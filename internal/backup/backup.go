// Package backup orchestrates a single backup run: preflight, dump, verify,
// upload, prune.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/still-forest/manor/services/postgres-backup/internal/config"
	"github.com/still-forest/manor/services/postgres-backup/internal/exec"
	"github.com/still-forest/manor/services/postgres-backup/internal/healthcheck"
	"github.com/still-forest/manor/services/postgres-backup/internal/objstore"
	"github.com/still-forest/manor/services/postgres-backup/internal/pgtool"
	"github.com/still-forest/manor/services/postgres-backup/internal/prune"
)

// ExpectedPGMajor is the PostgreSQL major version this service is built against.
const ExpectedPGMajor = 18

// PruneFailedPrefix leads the failure message when the backup itself was
// stored successfully and only the prune afterwards failed.
const PruneFailedPrefix = "BACKUP UPLOADED OK - PRUNE FAILED"

// Runner performs backup runs. Every dependency is injected so a full run can
// be exercised in tests with no database, no bucket and no clock.
type Runner struct {
	Cfg    config.Config
	Store  objstore.Store
	Runner exec.Commander
	Ping   healthcheck.Pinger
	Log    *slog.Logger
	Now    func() time.Time
	// Tail supplies the recent log output attached to a failure ping.
	Tail func() string
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Runner) tail() string {
	if r.Tail != nil {
		return r.Tail()
	}
	return ""
}

func (r *Runner) conn(db string) pgtool.Conn {
	return pgtool.Conn{
		Host:     r.Cfg.Postgres.Host,
		Port:     r.Cfg.Postgres.Port,
		User:     r.Cfg.Postgres.User,
		Password: r.Cfg.Postgres.Password,
		Database: db,
	}
}

// Preflight verifies everything a run depends on, before the service starts
// waiting for its window.
//
// This is the check whose absence let a rotated R2 key go unnoticed for a week:
// bad credentials now fail at deploy time, loudly, instead of at 23:00 in
// silence.
func (r *Runner) Preflight(ctx context.Context) error {
	major, err := pgtool.ClientMajor(ctx, r.Runner, "pg_dump")
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if major != ExpectedPGMajor {
		return fmt.Errorf("preflight: pg_dump is major version %d, expected %d", major, ExpectedPGMajor)
	}
	r.Log.Info("pg_dump client verified", "event", "preflight.client", "major", major)

	for _, db := range r.Cfg.Postgres.Databases {
		c := r.conn(db)
		if err := pgtool.Ping(ctx, r.Runner, c); err != nil {
			return fmt.Errorf("preflight: database %q unreachable: %w", db, err)
		}
		server, err := pgtool.ServerMajor(ctx, r.Runner, c)
		if err != nil {
			return fmt.Errorf("preflight: reading server version for %q: %w", db, err)
		}
		if major < server {
			return fmt.Errorf("preflight: pg_dump %d is older than server %d for %q; the dump would be unusable",
				major, server, db)
		}
		r.Log.Info("database reachable",
			"event", "preflight.database", "database", db, "server_major", server)
	}

	if checker, ok := r.Store.(interface{ CheckAccess(context.Context) error }); ok {
		if err := checker.CheckAccess(ctx); err != nil {
			return fmt.Errorf("preflight: %w", err)
		}
	}
	probe := objstore.BuildKey(r.Cfg.R2.BucketPath, ".preflight", r.now()) + ".probe"
	if err := r.Store.Upload(ctx, probe, strings.NewReader("ok"), nil); err != nil {
		return fmt.Errorf("preflight: object storage is not writable: %w", err)
	}
	if err := r.Store.Delete(ctx, probe); err != nil {
		return fmt.Errorf("preflight: object storage is not deletable: %w", err)
	}
	r.Log.Info("object storage verified",
		"event", "preflight.storage", "bucket", r.Cfg.R2.Bucket, "prefix", r.Cfg.R2.BucketPath)

	return nil
}

// RunOnce performs one full backup of every configured database.
//
// It owns the Healthchecks lifecycle: /start on entry, success or /fail on
// exit. The caller is responsible only for logging the returned error.
func (r *Runner) RunOnce(ctx context.Context) (err error) {
	started := r.now()
	r.Log.Info("backup run starting",
		"event", "run.start",
		"databases", strings.Join(r.Cfg.Postgres.Databases, ","),
		"dry_run", r.Cfg.DryRun)

	r.Ping.Start(ctx, fmt.Sprintf("starting backup of %s at %s",
		strings.Join(r.Cfg.Postgres.Databases, ","), started.Format(time.RFC3339)))

	//nolint:contextcheck // The detached context below is the point, not an oversight.
	defer func() {
		// The ping must not ride on ctx: on SIGTERM ctx is already cancelled,
		// and a failure report sent on a dead context would silently do nothing
		// -- reintroducing exactly the blind spot this service exists to remove.
		pingCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			r.Log.Error("backup run panicked", "event", "run.panic", "panic", p)
		}

		dur := r.now().Sub(started)
		if err != nil {
			r.Log.Error("backup run failed",
				"event", "run.finish", "outcome", "failure",
				"duration_seconds", dur.Seconds(), "error", err)
			r.Ping.Fail(pingCtx, fmt.Sprintf("FAILED after %s: %v\n\n%s", dur.Round(time.Second), err, r.tail()))
			return
		}
		r.Log.Info("backup run succeeded",
			"event", "run.finish", "outcome", "success", "duration_seconds", dur.Seconds())
		r.Ping.Success(pingCtx, fmt.Sprintf("OK in %s", dur.Round(time.Second)))
	}()

	for _, db := range r.Cfg.Postgres.Databases {
		if err := r.backupDatabase(ctx, db, started); err != nil {
			return fmt.Errorf("database %q: %w", db, err)
		}
	}
	return nil
}

func (r *Runner) backupDatabase(ctx context.Context, db string, stamp time.Time) error {
	c := r.conn(db)

	if err := r.checkSpace(ctx, c, db); err != nil {
		return err
	}

	tmp := filepath.Join(r.Cfg.TmpDir, fmt.Sprintf("%s-%d.dump.part", db, stamp.UnixNano()))
	final := strings.TrimSuffix(tmp, ".part")
	defer func() {
		for _, p := range []string{tmp, final} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				r.Log.Warn("could not remove temporary file",
					"event", "cleanup.failed", "path", p, "error", err)
			}
		}
	}()

	dumpCtx, cancel := context.WithTimeout(ctx, r.Cfg.Postgres.Timeout)
	defer cancel()

	r.Log.Info("dumping database", "event", "dump.start", "database", db, "path", tmp)
	dumpStart := r.now()
	if err := pgtool.Dump(dumpCtx, r.Runner, c, tmp, r.Cfg.Postgres.Compress, r.Log); err != nil {
		return fmt.Errorf("pg_dump: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("renaming dump: %w", err)
	}

	size, sum, err := hashFile(final)
	if err != nil {
		return err
	}
	r.Log.Info("dump complete",
		"event", "dump.finish", "database", db,
		"bytes", size, "sha256", sum,
		"duration_seconds", r.now().Sub(dumpStart).Seconds())

	entries, err := r.verify(ctx, final, size)
	if err != nil {
		return err
	}

	key := objstore.BuildKey(r.Cfg.R2.BucketPath, db, stamp)
	meta := map[string]string{
		"sha256":         sum,
		"database":       db,
		"started-at":     stamp.Format(time.RFC3339),
		"toc-entries":    strconv.Itoa(entries),
		"pg-dump-major":  strconv.Itoa(ExpectedPGMajor),
		"uncompressed":   "false",
		"source-host":    r.Cfg.Postgres.Host,
		"content-length": strconv.FormatInt(size, 10),
	}

	if r.Cfg.DryRun {
		r.Log.Warn("dry run: skipping upload",
			"event", "upload.skipped", "database", db, "key", key, "bytes", size)
		return nil
	}

	if err := r.upload(ctx, key, final, meta, size, sum); err != nil {
		return err
	}
	if err := r.uploadGlobals(ctx, c, db, stamp); err != nil {
		return err
	}

	// The dump is safely stored by this point. A prune failure still has to
	// raise the alarm -- a bucket that silently grows forever is its own
	// outage -- but the alert must not read as a lost backup.
	if err := r.prune(ctx, db, key); err != nil {
		return fmt.Errorf("%s: %w", PruneFailedPrefix, err)
	}
	return nil
}

func (r *Runner) checkSpace(ctx context.Context, c pgtool.Conn, db string) error {
	free, err := freeBytes(r.Cfg.TmpDir)
	if err != nil {
		return fmt.Errorf("checking free space on %s: %w", r.Cfg.TmpDir, err)
	}

	required := r.Cfg.Limits.MinFreeBytes
	if dbSize, err := pgtool.DatabaseSize(ctx, r.Runner, c); err == nil {
		// A custom-format dump compresses, so the live size is a generous
		// upper bound; requiring it outright keeps a full disk from being
		// discovered halfway through a dump.
		if dbSize > required {
			required = dbSize
		}
		r.Log.Info("database size measured",
			"event", "preflight.size", "database", db, "bytes", dbSize)
	} else {
		r.Log.Warn("could not measure database size; falling back to MIN_FREE_BYTES",
			"event", "preflight.size_failed", "database", db, "error", err)
	}

	if free < required {
		return fmt.Errorf("insufficient space in %s: %d bytes free, need %d",
			r.Cfg.TmpDir, free, required)
	}
	r.Log.Info("disk space sufficient",
		"event", "preflight.disk", "free_bytes", free, "required_bytes", required)
	return nil
}

// verify runs four independent checks on the archive: a size floor, a readable
// table of contents, a minimum object count, and a full read of every data
// block. Together they catch a truncated dump, an unreadable dump, and a
// structurally valid dump of the wrong (or an empty) database.
func (r *Runner) verify(ctx context.Context, path string, size int64) (int, error) {
	if size < r.Cfg.Limits.MinDumpBytes {
		return 0, fmt.Errorf("dump is %d bytes, below the %d byte minimum", size, r.Cfg.Limits.MinDumpBytes)
	}

	entries, err := pgtool.TOCEntries(ctx, r.Runner, path)
	if err != nil {
		return 0, fmt.Errorf("verifying dump: %w", err)
	}
	if entries < r.Cfg.Limits.MinTOCEntries {
		return 0, fmt.Errorf("dump lists %d objects, below the %d minimum; it may be of the wrong database",
			entries, r.Cfg.Limits.MinTOCEntries)
	}

	// Reading the archive in full is the only check that catches truncation:
	// a half-written dump still carries a complete table of contents and
	// passes the check above.
	if err := pgtool.VerifyReadable(ctx, r.Runner, path); err != nil {
		return 0, err
	}

	r.Log.Info("dump verified",
		"event", "verify.ok", "toc_entries", entries, "bytes", size)
	return entries, nil
}

func (r *Runner) upload(ctx context.Context, key, path string, meta map[string]string, size int64, sum string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening dump for upload: %w", err)
	}
	defer func() { _ = f.Close() }()

	r.Log.Info("uploading dump", "event", "upload.start", "key", key, "bytes", size)
	start := r.now()
	if err := r.Store.Upload(ctx, key, f, meta); err != nil {
		return fmt.Errorf("uploading dump: %w", err)
	}

	obj, gotMeta, err := r.Store.Head(ctx, key)
	if err != nil {
		return fmt.Errorf("confirming upload: %w", err)
	}
	if obj.Size != size {
		return fmt.Errorf("uploaded object is %d bytes, expected %d", obj.Size, size)
	}
	// ETag is deliberately not compared: for a multipart upload it is not an
	// MD5 of the content, so the round-tripped checksum metadata is the check
	// that actually means something. Absence is treated as a failure rather
	// than skipped, because this run set it moments ago -- silently accepting
	// a missing checksum would be the same blind spot in a new place.
	got, ok := gotMeta["sha256"]
	if !ok {
		return fmt.Errorf("uploaded object has no sha256 metadata; it cannot be verified")
	}
	if got != sum {
		return fmt.Errorf("uploaded object checksum %s does not match local %s", got, sum)
	}

	r.Log.Info("upload confirmed",
		"event", "upload.finish", "key", key, "bytes", obj.Size,
		"duration_seconds", r.now().Sub(start).Seconds())
	return nil
}

func (r *Runner) uploadGlobals(ctx context.Context, c pgtool.Conn, db string, stamp time.Time) error {
	var buf strings.Builder
	if err := pgtool.DumpGlobals(ctx, r.Runner, c, &buf, r.Log); err != nil {
		return fmt.Errorf("pg_dumpall --globals-only: %w", err)
	}

	key := objstore.GlobalsKey(r.Cfg.R2.BucketPath, db, stamp)
	if err := r.Store.Upload(ctx, key, strings.NewReader(buf.String()), map[string]string{"database": db}); err != nil {
		return fmt.Errorf("uploading globals: %w", err)
	}
	r.Log.Info("globals uploaded", "event", "globals.uploaded", "key", key, "bytes", buf.Len())
	return nil
}

func (r *Runner) prune(ctx context.Context, db, currentKey string) error {
	if !r.Cfg.PruneEnabled {
		r.Log.Info("pruning disabled", "event", "prune.skipped", "database", db)
		return nil
	}

	prefix := r.Cfg.R2.BucketPath + "/" + db
	objs, err := r.Store.List(ctx, strings.TrimPrefix(prefix, "/"))
	if err != nil {
		return fmt.Errorf("listing for prune: %w", err)
	}

	// The globals sidecars share the prefix but not the dump key format; they
	// would be classed unknown and kept forever, so drop them from the listing
	// and let each one follow the dump it belongs to.
	var dumps []objstore.Object
	globals := map[string]objstore.Object{}
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".globals.sql") {
			globals[strings.TrimSuffix(o.Key, ".globals.sql")+objstore.DumpExt] = o
			continue
		}
		dumps = append(dumps, o)
	}

	plan, err := prune.Plan(dumps, prune.Policy{
		DailyDays:     r.Cfg.Retention.DailyDays,
		MonthlyMonths: r.Cfg.Retention.MonthlyMonths,
		MinKeep:       r.Cfg.Retention.MinKeep,
		MaxDeletes:    r.Cfg.Retention.MaxDeletes,
		AllowBulk:     r.Cfg.Retention.AllowBulk,
	}, r.now())
	if err != nil {
		return err
	}
	if err := plan.Protect(currentKey); err != nil {
		return err
	}

	r.Log.Info("prune planned",
		"event", "prune.plan", "database", db,
		"keep", len(plan.Keep), "delete", len(plan.Delete), "unknown", len(plan.Unknown))

	if r.Cfg.DryRun {
		for _, o := range plan.Delete {
			r.Log.Warn("dry run: would delete", "event", "prune.dry_run", "key", o.Key)
		}
		return nil
	}

	for _, o := range plan.Delete {
		if err := r.Store.Delete(ctx, o.Key); err != nil {
			return fmt.Errorf("pruning %s: %w", o.Key, err)
		}
		r.Log.Info("pruned", "event", "prune.deleted", "key", o.Key, "bytes", o.Size)

		if g, ok := globals[o.Key]; ok {
			if err := r.Store.Delete(ctx, g.Key); err != nil {
				return fmt.Errorf("pruning %s: %w", g.Key, err)
			}
			r.Log.Info("pruned", "event", "prune.deleted", "key", g.Key, "bytes", g.Size)
		}
	}
	return nil
}

// NewestBackup returns the timestamp of the most recent parseable backup of db,
// used to decide whether a restart has left the schedule stale.
func (r *Runner) NewestBackup(ctx context.Context, db string) (time.Time, error) {
	objs, err := r.Store.List(ctx, strings.TrimPrefix(r.Cfg.R2.BucketPath+"/"+db, "/"))
	if err != nil {
		return time.Time{}, err
	}

	var newest time.Time
	for _, o := range objs {
		if _, ts, ok := objstore.ParseKey(o.Key); ok && ts.After(newest) {
			newest = ts
		}
	}
	return newest, nil
}

// HashFile returns the size and SHA-256 of a local file.
func HashFile(path string) (int64, string, error) { return hashFile(path) }

func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("opening dump: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", fmt.Errorf("hashing dump: %w", err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// PruneDatabase applies the retention policy to db outside of a backup run.
func (r *Runner) PruneDatabase(ctx context.Context, db string) error {
	return r.prune(ctx, db, "")
}
