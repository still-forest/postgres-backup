package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/still-forest/manor/services/postgres-backup/internal/backup"
	"github.com/still-forest/manor/services/postgres-backup/internal/config"
	"github.com/still-forest/manor/services/postgres-backup/internal/exec"
	"github.com/still-forest/manor/services/postgres-backup/internal/objstore"
	"github.com/still-forest/manor/services/postgres-backup/internal/pgtool"
)

// restoreTarget is read from a separate RESTORE_* namespace, never from the
// POSTGRES_* variables the service backs up.
//
// This is the guard against restoring over production, and it is structural
// rather than a prompt: there is no code path by which the production
// connection settings can reach pg_restore. Recovering production requires
// deliberately putting the production host into differently-named variables.
type restoreTarget struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
}

func loadRestoreTarget() (restoreTarget, error) {
	t := restoreTarget{
		Host:     os.Getenv("RESTORE_HOST"),
		User:     os.Getenv("RESTORE_USER"),
		Password: os.Getenv("RESTORE_PASS"),
		Database: os.Getenv("RESTORE_DB"),
		Port:     5432,
	}
	if p := os.Getenv("RESTORE_PORT"); p != "" {
		if _, err := fmt.Sscanf(p, "%d", &t.Port); err != nil {
			return t, fmt.Errorf("RESTORE_PORT must be an integer, got %q", p)
		}
	}

	var missing []string
	for _, r := range []struct{ name, val string }{
		{"RESTORE_HOST", t.Host},
		{"RESTORE_USER", t.User},
		{"RESTORE_PASS", t.Password},
		{"RESTORE_DB", t.Database},
	} {
		if strings.TrimSpace(r.val) == "" {
			missing = append(missing, r.name)
		}
	}
	if len(missing) > 0 {
		return t, fmt.Errorf("restore requires %s; these are deliberately separate from the "+
			"POSTGRES_* settings so that running this container with its normal environment "+
			"cannot overwrite the database it backs up", strings.Join(missing, ", "))
	}
	return t, nil
}

// isSource reports whether the restore target is a database this service backs
// up. The primary guard is that restore reads a different set of environment
// variables entirely; this is the second line, for the case where an operator
// has filled those in with production values.
func isSource(t restoreTarget, cfg config.Config) bool {
	return t.Host == cfg.Postgres.Host && contains(cfg.Postgres.Databases, t.Database)
}

func checkNotSource(t restoreTarget, cfg config.Config, overridden bool) error {
	if !isSource(t, cfg) || overridden {
		return nil
	}
	return fmt.Errorf("refusing to restore into %s/%s: it is a database this service backs up. "+
		"Pass --force-production and set ALLOW_RESTORE_TO_PRODUCTION=true if that is genuinely intended",
		t.Host, t.Database)
}

func list(ctx context.Context, cfg config.Config, store objstore.Store, args []string) error {
	fs := newFlagSet("list")
	db := fs.String("db", "", "restrict to one database")
	limit := fs.Int("limit", 20, "maximum rows to show")
	if err := fs.Parse(args); err != nil {
		return err
	}

	prefix := cfg.R2.BucketPath
	if *db != "" {
		prefix = strings.TrimPrefix(prefix+"/"+*db, "/")
	}

	objs, err := store.List(ctx, prefix)
	if err != nil {
		return err
	}

	type row struct {
		key  string
		db   string
		ts   time.Time
		size int64
	}
	var rows []row
	for _, o := range objs {
		name, ts, ok := objstore.ParseKey(o.Key)
		if !ok {
			continue
		}
		rows = append(rows, row{key: o.Key, db: name, ts: ts, size: o.Size})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ts.After(rows[j].ts) })

	if len(rows) == 0 {
		fmt.Printf("No backups found under %q.\n", prefix)
		return nil
	}
	if *limit > 0 && len(rows) > *limit {
		rows = rows[:*limit]
	}

	fmt.Printf("%-20s  %-24s  %12s  %s\n", "DATABASE", "TAKEN (UTC)", "SIZE", "KEY")
	for _, r := range rows {
		fmt.Printf("%-20s  %-24s  %12s  %s\n",
			r.db, r.ts.Format(time.RFC3339), humanBytes(r.size), r.key)
	}
	return nil
}

// resolveKey turns the selection flags into exactly one key.
func resolveKey(ctx context.Context, cfg config.Config, store objstore.Store, key, date, db string, latest bool) (string, error) {
	if key != "" {
		return key, nil
	}
	if !latest && date == "" {
		return "", fmt.Errorf("specify one of --key, --date or --latest")
	}

	prefix := cfg.R2.BucketPath
	if db != "" {
		prefix = strings.TrimPrefix(prefix+"/"+db, "/")
	}
	objs, err := store.List(ctx, prefix)
	if err != nil {
		return "", err
	}

	var best string
	var bestTS time.Time
	for _, o := range objs {
		_, ts, ok := objstore.ParseKey(o.Key)
		if !ok {
			continue
		}
		if date != "" && ts.Format("2006-01-02") != date {
			continue
		}
		if best == "" || ts.After(bestTS) {
			best, bestTS = o.Key, ts
		}
	}
	if best == "" {
		return "", fmt.Errorf("no backup matched (prefix %q, date %q)", prefix, date)
	}
	return best, nil
}

// fetch downloads a backup to a local file and checks it against the checksum
// recorded at upload time.
func fetch(ctx context.Context, store objstore.Store, cfg config.Config, key string, log *slog.Logger) (string, error) {
	_, meta, err := store.Head(ctx, key)
	if err != nil {
		return "", err
	}

	path := filepath.Join(cfg.TmpDir, filepath.Base(key))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}

	log.Info("downloading backup", "event", "restore.download", "key", key, "path", path)
	if err := store.Download(ctx, key, f); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	size, sum, err := hashLocal(path)
	if err != nil {
		return "", err
	}
	want, ok := meta["sha256"]
	switch {
	case !ok:
		// An explicitly-named key may predate this service, so this is a loud
		// warning rather than a refusal.
		log.Warn("backup has no recorded checksum; its integrity cannot be confirmed",
			"event", "restore.unverified", "key", key)
	case want != sum:
		return "", fmt.Errorf("checksum mismatch for %s: object metadata says %s, downloaded file is %s",
			key, want, sum)
	}
	log.Info("download verified",
		"event", "restore.verified", "key", key, "bytes", size, "sha256", sum)
	return path, nil
}

func verify(ctx context.Context, cfg config.Config, store objstore.Store, args []string, log *slog.Logger) error {
	fs := newFlagSet("verify")
	key := fs.String("key", "", "object key to verify")
	date := fs.String("date", "", "select the backup taken on YYYY-MM-DD")
	db := fs.String("db", "", "restrict selection to one database")
	latest := fs.Bool("latest", false, "select the most recent backup")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolved, err := resolveKey(ctx, cfg, store, *key, *date, *db, *latest)
	if err != nil {
		return err
	}

	path, err := fetch(ctx, store, cfg, resolved, log)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()

	entries, err := pgtool.TOCEntries(ctx, exec.OS{}, path)
	if err != nil {
		return err
	}
	fmt.Printf("%s is restorable: %d objects in the archive.\n", resolved, entries)
	return nil
}

func restore(ctx context.Context, cfg config.Config, store objstore.Store, args []string, log *slog.Logger) error {
	fs := newFlagSet("restore")
	key := fs.String("key", "", "object key to restore")
	date := fs.String("date", "", "select the backup taken on YYYY-MM-DD")
	db := fs.String("db", "", "restrict selection to one database")
	latest := fs.Bool("latest", false, "select the most recent backup")
	confirmDB := fs.String("confirm-database", "", "must match RESTORE_DB")
	execute := fs.Bool("execute", false, "actually write to the target; without this the command is a dry run")
	forceProd := fs.Bool("force-production", false, "permit a target identical to the backup source")
	if err := fs.Parse(args); err != nil {
		return err
	}

	target, err := loadRestoreTarget()
	if err != nil {
		return err
	}

	overridden := *forceProd && strings.EqualFold(os.Getenv("ALLOW_RESTORE_TO_PRODUCTION"), "true")
	if err := checkNotSource(target, cfg, overridden); err != nil {
		return err
	}
	if isSource(target, cfg) {
		log.Warn("restoring into the backup source database",
			"event", "restore.production", "host", target.Host, "database", target.Database)
	}

	resolved, err := resolveKey(ctx, cfg, store, *key, *date, *db, *latest)
	if err != nil {
		return err
	}

	path, err := fetch(ctx, store, cfg, resolved, log)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()

	entries, err := pgtool.TOCEntries(ctx, exec.OS{}, path)
	if err != nil {
		return err
	}

	fmt.Printf("\nBackup:   %s\n", resolved)
	fmt.Printf("Archive:  %d objects\n", entries)
	fmt.Printf("Target:   %s@%s:%d/%s\n\n", target.User, target.Host, target.Port, target.Database)

	if !*execute {
		fmt.Println("Dry run: nothing was written. Re-run with --execute to restore.")
		return nil
	}
	if *confirmDB != target.Database {
		return fmt.Errorf("--confirm-database %q does not match RESTORE_DB %q", *confirmDB, target.Database)
	}

	log.Info("restoring", "event", "restore.start", "key", resolved,
		"host", target.Host, "database", target.Database)

	err = pgtool.Restore(ctx, exec.OS{}, pgtool.RestoreOpts{
		Conn: pgtool.Conn{
			Host:     target.Host,
			Port:     target.Port,
			User:     target.User,
			Password: target.Password,
			Database: target.Database,
		},
		Path: path,
	}, log)
	if err != nil {
		return fmt.Errorf("restore failed; --single-transaction means the target is unchanged: %w", err)
	}

	log.Info("restore complete", "event", "restore.finish",
		"key", resolved, "database", target.Database)
	return nil
}

func hashLocal(path string) (int64, string, error) { return backup.HashFile(path) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
