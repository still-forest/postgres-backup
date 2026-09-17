// Package pgtool drives pg_dump, pg_restore and psql.
package pgtool

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/still-forest/manor/services/postgres-backup/internal/exec"
)

// Conn identifies a target cluster and database.
type Conn struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
}

// env returns connection settings destined for the child process environment.
// The password is deliberately passed this way: anything in argv is visible in
// the process table and in logged command lines.
func (c Conn) env() []string {
	return []string{
		"PGPASSWORD=" + c.Password,
		"PGHOST=" + c.Host,
		"PGPORT=" + strconv.Itoa(c.Port),
		"PGUSER=" + c.User,
		"PGCONNECT_TIMEOUT=10",
		"PGAPPNAME=manor-pgbackup",
	}
}

var versionRE = regexp.MustCompile(`\(PostgreSQL\)\s+(\d+)\.`)

// ClientMajor returns the major version reported by the named binary.
func ClientMajor(ctx context.Context, runner exec.Commander, bin string) (int, error) {
	var out strings.Builder
	if err := runner.Run(ctx, exec.Cmd{Path: bin, Args: []string{"--version"}, Stdout: &out}); err != nil {
		return 0, fmt.Errorf("%s --version: %w", bin, err)
	}
	m := versionRE.FindStringSubmatch(out.String())
	if m == nil {
		return 0, fmt.Errorf("could not parse %s version from %q", bin, strings.TrimSpace(out.String()))
	}
	return strconv.Atoi(m[1])
}

// ServerMajor returns the major version of the connected server.
func ServerMajor(ctx context.Context, runner exec.Commander, c Conn) (int, error) {
	out, err := psql(ctx, runner, c, "SHOW server_version_num")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("unexpected server_version_num %q: %w", strings.TrimSpace(out), err)
	}
	return n / 10000, nil
}

// DatabaseSize returns the on-disk size of c.Database in bytes.
func DatabaseSize(ctx context.Context, runner exec.Commander, c Conn) (int64, error) {
	out, err := psql(ctx, runner, c, fmt.Sprintf("SELECT pg_database_size('%s')", c.Database))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// Ping verifies the database accepts queries.
func Ping(ctx context.Context, runner exec.Commander, c Conn) error {
	_, err := psql(ctx, runner, c, "SELECT 1")
	return err
}

func psql(ctx context.Context, runner exec.Commander, c Conn, query string) (string, error) {
	var out, errBuf strings.Builder
	err := runner.Run(ctx, exec.Cmd{
		Path:   "psql",
		Args:   []string{"--no-psqlrc", "--tuples-only", "--no-align", "--dbname", c.Database, "--command", query},
		Env:    c.env(),
		Stdout: &out,
		Stderr: &errBuf,
	})
	if err != nil {
		return "", fmt.Errorf("psql %q: %w: %s", query, err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}

// Dump writes a custom-format dump of c.Database to path.
func Dump(ctx context.Context, runner exec.Commander, c Conn, path, compress string, log *slog.Logger) error {
	args := []string{
		"--format=custom",
		"--no-owner",
		"--no-privileges",
		"--verbose",
		"--file=" + path,
		"--dbname=" + c.Database,
	}
	if compress != "" {
		args = append(args, "--compress="+compress)
	}

	lw := lineLogger(log, "pg_dump")
	defer lw.Flush()
	return runner.Run(ctx, exec.Cmd{Path: "pg_dump", Args: args, Env: c.env(), Stderr: lw})
}

// DumpGlobals writes cluster-wide roles and tablespaces to w. A per-database
// dump carries neither, so a restore without these produces a database whose
// grants reference roles that do not exist.
func DumpGlobals(ctx context.Context, runner exec.Commander, c Conn, w io.Writer, log *slog.Logger) error {
	lw := lineLogger(log, "pg_dumpall")
	defer lw.Flush()
	return runner.Run(ctx, exec.Cmd{
		Path:   "pg_dumpall",
		Args:   []string{"--globals-only", "--no-role-passwords"},
		Env:    c.env(),
		Stdout: w,
		Stderr: lw,
	})
}

// TOCEntries returns the number of table-of-contents entries in an archive,
// which is how a structurally valid but empty dump is detected.
func TOCEntries(ctx context.Context, runner exec.Commander, path string) (int, error) {
	var out, errBuf strings.Builder
	err := runner.Run(ctx, exec.Cmd{
		Path:   "pg_restore",
		Args:   []string{"--list", path},
		Stdout: &out,
		Stderr: &errBuf,
	})
	if err != nil {
		return 0, fmt.Errorf("pg_restore --list: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}

	n := 0
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		n++
	}
	return n, nil
}

// VerifyReadable reads the entire archive, decompressing every data block,
// and reports whether it is intact.
//
// This is a stronger check than TOCEntries, which only parses the table of
// contents at the head of the file: a dump truncated halfway through still
// lists its full contents and exits zero. Restoring to /dev/null is what
// actually forces the data to be read.
func VerifyReadable(ctx context.Context, runner exec.Commander, path string) error {
	var errBuf strings.Builder
	err := runner.Run(ctx, exec.Cmd{
		Path:   "pg_restore",
		Args:   []string{"--file=/dev/null", path},
		Stderr: &errBuf,
	})
	if err != nil {
		return fmt.Errorf("archive is not fully readable: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// RestoreOpts configures a restore.
type RestoreOpts struct {
	Conn Conn
	Path string
	Jobs int
}

// Restore loads an archive into the target database.
//
// --single-transaction and --exit-on-error together mean a failed restore
// leaves the target untouched rather than partially written. pg_restore
// otherwise logs errors and still exits zero, which is the restore-side
// equivalent of the silent failure this service exists to eliminate.
func Restore(ctx context.Context, runner exec.Commander, o RestoreOpts, log *slog.Logger) error {
	args := []string{
		"--clean",
		"--if-exists",
		"--no-owner",
		"--no-privileges",
		"--single-transaction",
		"--exit-on-error",
		"--verbose",
		"--dbname=" + o.Conn.Database,
		o.Path,
	}
	lw := lineLogger(log, "pg_restore")
	defer lw.Flush()
	return runner.Run(ctx, exec.Cmd{Path: "pg_restore", Args: args, Env: o.Conn.env(), Stderr: lw})
}

// lineWriter adapts a child process's stderr into the structured log, so
// --verbose output from pg_dump reaches both the container logs and the tail
// attached to a failure ping. It splits on newlines synchronously rather than
// via an io.Pipe goroutine, so no output can be lost when the command exits.
type lineWriter struct {
	log    *slog.Logger
	source string
	buf    []byte
}

func lineLogger(log *slog.Logger, source string) *lineWriter {
	return &lineWriter{log: log, source: source}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.log == nil {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.emit(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
}

// Flush emits any trailing output not terminated by a newline.
func (w *lineWriter) Flush() {
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}

func (w *lineWriter) emit(line string) {
	if line = strings.TrimSpace(line); line != "" {
		w.log.Debug(line, "event", "pg.output", "source", w.source)
	}
}
