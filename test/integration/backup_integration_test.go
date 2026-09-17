//go:build integration

// Package integration exercises the real pg_dump, pg_restore and psql binaries
// against a live PostgreSQL server.
//
// Unit tests cover orchestration against fakes; what they cannot prove is that
// the arguments this service passes actually work, and that a dump it produces
// can be read back. That is what these tests are for. Object storage is still
// faked: the one thing no local S3 substitute reproduces is R2's checksum
// behaviour, which is covered by the manual smoke test in the README instead.
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/still-forest/manor/services/postgres-backup/internal/backup"
	"github.com/still-forest/manor/services/postgres-backup/internal/config"
	"github.com/still-forest/manor/services/postgres-backup/internal/exec"
	"github.com/still-forest/manor/services/postgres-backup/internal/objstore"
	"github.com/still-forest/manor/services/postgres-backup/internal/pgtool"
)

func env(t *testing.T, name, def string) string {
	t.Helper()
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func testConn(t *testing.T, db string) pgtool.Conn {
	t.Helper()
	port, err := strconv.Atoi(env(t, "PGBACKUP_TEST_PORT", "5432"))
	if err != nil {
		t.Fatalf("PGBACKUP_TEST_PORT: %v", err)
	}
	return pgtool.Conn{
		Host:     env(t, "PGBACKUP_TEST_HOST", "localhost"),
		Port:     port,
		User:     env(t, "PGBACKUP_TEST_USER", "postgres"),
		Password: env(t, "PGBACKUP_TEST_PASS", "integration"),
		Database: db,
	}
}

func mainDB(t *testing.T) string { return env(t, "PGBACKUP_TEST_DB", "pgbackup_integration") }

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// psqlExec runs a statement against db and fails the test if it errors.
func psqlExec(t *testing.T, db, sql string) {
	t.Helper()
	c := testConn(t, db)
	var errBuf strings.Builder
	err := exec.OS{}.Run(context.Background(), exec.Cmd{
		Path: "psql",
		Args: []string{"--no-psqlrc", "--quiet", "--dbname", db, "--command", sql},
		Env: []string{
			"PGPASSWORD=" + c.Password,
			"PGHOST=" + c.Host,
			"PGPORT=" + strconv.Itoa(c.Port),
			"PGUSER=" + c.User,
		},
		Stderr: &errBuf,
	})
	if err != nil {
		t.Fatalf("psql %q: %v: %s", sql, err, errBuf.String())
	}
}

func psqlQuery(t *testing.T, db, sql string) string {
	t.Helper()
	c := testConn(t, db)
	var out, errBuf strings.Builder
	err := exec.OS{}.Run(context.Background(), exec.Cmd{
		Path: "psql",
		Args: []string{"--no-psqlrc", "--tuples-only", "--no-align", "--dbname", db, "--command", sql},
		Env: []string{
			"PGPASSWORD=" + c.Password,
			"PGHOST=" + c.Host,
			"PGPORT=" + strconv.Itoa(c.Port),
			"PGUSER=" + c.User,
		},
		Stdout: &out,
		Stderr: &errBuf,
	})
	if err != nil {
		t.Fatalf("psql %q: %v: %s", sql, err, errBuf.String())
	}
	return strings.TrimSpace(out.String())
}

// seed populates the main database with enough content to clear the
// verification thresholds.
func seed(t *testing.T) {
	t.Helper()
	db := mainDB(t)
	psqlExec(t, db, `
		DROP TABLE IF EXISTS notes, users, tasks CASCADE;
		CREATE TABLE notes (id serial PRIMARY KEY, body text, created_at timestamptz DEFAULT now());
		CREATE TABLE users (id serial PRIMARY KEY, email text UNIQUE);
		CREATE TABLE tasks (id serial PRIMARY KEY, title text, done boolean DEFAULT false);
		CREATE INDEX idx_notes_created ON notes(created_at);
		INSERT INTO notes (body) SELECT 'note ' || g FROM generate_series(1,20000) g;
		INSERT INTO users (email) SELECT 'u' || g || '@example.com' FROM generate_series(1,2000) g;
		INSERT INTO tasks (title) SELECT 'task ' || g FROM generate_series(1,5000) g;
	`)
}

func TestClientIsExpectedMajorVersion(t *testing.T) {
	for _, bin := range []string{"pg_dump", "pg_restore"} {
		major, err := pgtool.ClientMajor(context.Background(), exec.OS{}, bin)
		if err != nil {
			t.Fatalf("ClientMajor(%s): %v", bin, err)
		}
		if major != backup.ExpectedPGMajor {
			t.Errorf("%s is major %d, want %d", bin, major, backup.ExpectedPGMajor)
		}
	}
}

func TestServerIsReachableAndNotNewerThanClient(t *testing.T) {
	c := testConn(t, mainDB(t))
	if err := pgtool.Ping(context.Background(), exec.OS{}, c); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	server, err := pgtool.ServerMajor(context.Background(), exec.OS{}, c)
	if err != nil {
		t.Fatalf("ServerMajor: %v", err)
	}
	if server > backup.ExpectedPGMajor {
		t.Fatalf("server is major %d, newer than the bundled client %d; dumps would be unusable",
			server, backup.ExpectedPGMajor)
	}
}

// The whole point of a backup: a dump this service produced can be loaded back
// and yields the same data.
func TestDumpVerifyRestoreRoundTrip(t *testing.T) {
	seed(t)
	ctx := context.Background()
	src := mainDB(t)
	dst := src + "_restored"

	want := map[string]string{}
	for _, table := range []string{"notes", "users", "tasks"} {
		want[table] = psqlQuery(t, src, "SELECT count(*) FROM "+table)
	}

	psqlExec(t, "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s", dst))
	psqlExec(t, "postgres", fmt.Sprintf("CREATE DATABASE %s", dst))
	t.Cleanup(func() {
		psqlExec(t, "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s", dst))
	})

	path := filepath.Join(t.TempDir(), "roundtrip.dump")
	if err := pgtool.Dump(ctx, exec.OS{}, testConn(t, src), path, "", quietLog()); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("dump file: %v", err)
	}
	t.Logf("dump is %d bytes", info.Size())

	entries, err := pgtool.TOCEntries(ctx, exec.OS{}, path)
	if err != nil {
		t.Fatalf("TOCEntries: %v", err)
	}
	if entries < 10 {
		t.Fatalf("archive lists %d objects, expected the seeded schema to produce more", entries)
	}

	if err := pgtool.Restore(ctx, exec.OS{}, pgtool.RestoreOpts{
		Conn: testConn(t, dst),
		Path: path,
	}, quietLog()); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	for table, expected := range want {
		if got := psqlQuery(t, dst, "SELECT count(*) FROM "+table); got != expected {
			t.Errorf("%s: restored %s rows, source had %s", table, got, expected)
		}
	}
}

// A truncated archive must be rejected by verification, not discovered at
// restore time.
func TestVerificationRejectsTruncatedArchive(t *testing.T) {
	seed(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "truncated.dump")

	if err := pgtool.Dump(ctx, exec.OS{}, testConn(t, mainDB(t)), path, "", quietLog()); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()/2); err != nil {
		t.Fatal(err)
	}

	// The table of contents lives at the head of the archive and survives
	// truncation, so this check alone passes -- which is precisely why
	// VerifyReadable exists.
	if _, err := pgtool.TOCEntries(ctx, exec.OS{}, path); err != nil {
		t.Logf("TOCEntries also rejected the truncated archive: %v", err)
	}

	if err := pgtool.VerifyReadable(ctx, exec.OS{}, path); err == nil {
		t.Fatal("verification accepted a truncated archive")
	}
}

// The corresponding positive case: an intact archive must pass the full read.
func TestVerificationAcceptsIntactArchive(t *testing.T) {
	seed(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "intact.dump")

	if err := pgtool.Dump(ctx, exec.OS{}, testConn(t, mainDB(t)), path, "", quietLog()); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if err := pgtool.VerifyReadable(ctx, exec.OS{}, path); err != nil {
		t.Fatalf("VerifyReadable rejected an intact archive: %v", err)
	}
}

// A dump of an empty database is structurally valid, which is why the TOC-entry
// floor exists: it is the signal that the wrong database was dumped.
func TestEmptyDatabaseProducesFewTOCEntries(t *testing.T) {
	ctx := context.Background()
	empty := mainDB(t) + "_empty"

	psqlExec(t, "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s", empty))
	psqlExec(t, "postgres", fmt.Sprintf("CREATE DATABASE %s", empty))
	t.Cleanup(func() {
		psqlExec(t, "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s", empty))
	})

	path := filepath.Join(t.TempDir(), "empty.dump")
	if err := pgtool.Dump(ctx, exec.OS{}, testConn(t, empty), path, "", quietLog()); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	entries, err := pgtool.TOCEntries(ctx, exec.OS{}, path)
	if err != nil {
		t.Fatalf("TOCEntries: %v", err)
	}
	if entries >= 10 {
		t.Errorf("an empty database produced %d TOC entries; the default floor of 10 would not catch it", entries)
	}
}

// The full orchestration against real pg binaries, with storage faked.
func TestFullRunAgainstRealPostgres(t *testing.T) {
	seed(t)
	c := testConn(t, mainDB(t))
	store := objstore.NewFake()
	now := time.Date(2026, 9, 17, 23, 0, 0, 0, time.UTC)

	runner := &backup.Runner{
		Cfg: config.Config{
			Postgres: config.Postgres{
				Host: c.Host, Port: c.Port, User: c.User, Password: c.Password,
				Databases: []string{c.Database}, Timeout: 10 * time.Minute,
			},
			R2:           config.R2{Bucket: "test", BucketPath: "manor_db"},
			Retention:    config.Retention{DailyDays: 30, MonthlyMonths: 12, MinKeep: 1, MaxDeletes: 50},
			Limits:       config.Limits{MinFreeBytes: 1, MinDumpBytes: 1024, MinTOCEntries: 10},
			TmpDir:       t.TempDir(),
			PruneEnabled: true,
		},
		Store:  store,
		Runner: exec.OS{},
		Ping:   noopPinger{},
		Log:    quietLog(),
		Now:    func() time.Time { return now },
		Tail:   func() string { return "" },
	}

	if err := runner.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	wantKey := objstore.BuildKey("manor_db", c.Database, now)
	found := false
	for _, k := range store.Keys() {
		if k == wantKey {
			found = true
		}
	}
	if !found {
		t.Errorf("dump was not stored; keys = %v", store.Keys())
	}

	obj, meta, err := store.Head(context.Background(), wantKey)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if obj.Size == 0 {
		t.Error("stored dump is empty")
	}
	if meta["sha256"] == "" {
		t.Error("stored dump has no checksum metadata")
	}
	if meta["toc-entries"] == "" {
		t.Error("stored dump has no TOC entry count")
	}
}

type noopPinger struct{}

func (noopPinger) Start(context.Context, string)   {}
func (noopPinger) Success(context.Context, string) {}
func (noopPinger) Fail(context.Context, string)    {}
