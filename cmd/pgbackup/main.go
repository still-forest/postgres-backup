// Command pgbackup backs up PostgreSQL databases to S3-compatible storage on a
// daily schedule, verifies each dump, prunes old backups and can restore one.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	// Embeds the IANA timezone database so BACKUP_TZ resolves without tzdata
	// installed in the image.
	_ "time/tzdata"

	"github.com/still-forest/manor/services/postgres-backup/internal/backup"
	"github.com/still-forest/manor/services/postgres-backup/internal/config"
	"github.com/still-forest/manor/services/postgres-backup/internal/exec"
	"github.com/still-forest/manor/services/postgres-backup/internal/healthcheck"
	"github.com/still-forest/manor/services/postgres-backup/internal/logbuf"
	"github.com/still-forest/manor/services/postgres-backup/internal/objstore"
	"github.com/still-forest/manor/services/postgres-backup/internal/schedule"
)

// gitSHA is set at build time via -ldflags.
var gitSHA = "unknown"

const usage = `pgbackup - PostgreSQL backups to S3-compatible storage

Usage:
  pgbackup serve             Run continuously, backing up on the daily schedule (default)
  pgbackup backup            Run one backup immediately and exit
  pgbackup prune             Apply the retention policy and exit
  pgbackup list              List stored backups
  pgbackup verify --key KEY  Download a backup and check it is restorable
  pgbackup restore ...       Restore a backup into a target database

Configuration is read from the environment; see README.md.
`

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		fmt.Print(usage)
		return
	}

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	if err := run(cmd, os.Args[2:]); err != nil {
		// The logger may not exist yet if configuration failed, so report to
		// stderr unconditionally: a container that cannot start must still say why.
		fmt.Fprintf(os.Stderr, "pgbackup: %v\n", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	cfg, warnings, err := config.Load()
	if err != nil {
		return err
	}

	tail := logbuf.NewTail(cfg.LogTailBytes)
	log := slog.New(logbuf.New(os.Stdout, tail, parseLevel(cfg.LogLevel)))
	slog.SetDefault(log)

	log.Info("pgbackup starting",
		append([]any{"event", "boot", "git_sha", gitSHA, "command", cmd}, cfg.Redacted()...)...)
	for _, w := range warnings {
		log.Warn(w, "event", "config.warning")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := objstore.NewR2(ctx, objstore.R2Options{
		AccessKeyID:     cfg.R2.AccessKeyID,
		SecretAccessKey: cfg.R2.SecretAccessKey,
		Bucket:          cfg.R2.Bucket,
		HostBase:        cfg.R2.HostBase,
	})
	if err != nil {
		return err
	}

	pinger := healthcheck.New(cfg.HealthchecksURL, cfg.LogTailBytes, log)
	if cfg.HealthchecksURL == "" {
		log.Warn("HEALTHCHECKS_URL is not set; run outcomes will not be reported externally",
			"event", "config.no_healthchecks")
	}

	runner := &backup.Runner{
		Cfg:    cfg,
		Store:  store,
		Runner: exec.OS{},
		Ping:   pinger,
		Log:    log,
		Tail:   tail.String,
	}

	switch cmd {
	case "serve":
		return serve(ctx, cfg, runner, log)
	case "backup":
		return runner.RunOnce(ctx)
	case "prune":
		return pruneOnly(ctx, cfg, runner, log)
	case "list":
		return list(ctx, cfg, store, args)
	case "verify":
		return verify(ctx, cfg, store, args, log)
	case "restore":
		return restore(ctx, cfg, store, args, log)
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func serve(ctx context.Context, cfg config.Config, runner *backup.Runner, log *slog.Logger) error {
	if err := runner.Preflight(ctx); err != nil {
		// Refusing to start is the point: a container that cannot back up
		// should crash-loop visibly rather than sit idle until 23:00.
		return err
	}
	log.Info("preflight passed", "event", "preflight.ok")

	sched, err := schedule.New(cfg.Schedule.At, cfg.Schedule.TZ, schedule.RealClock{})
	if err != nil {
		return err
	}

	if due, why := staleness(ctx, cfg, runner, log); due {
		log.Info("running immediately", "event", "schedule.catchup", "reason", why)
		logRun(ctx, runner, log)
	}

	sched.Run(ctx,
		func(next time.Time) {
			log.Info("waiting for next scheduled backup",
				"event", "schedule.waiting",
				"next_run", next.Format(time.RFC3339),
				"in", time.Until(next).Round(time.Second).String())
		},
		func(runCtx context.Context) { logRun(runCtx, runner, log) },
	)

	log.Info("shutting down", "event", "shutdown")
	return nil
}

// logRun executes a backup and swallows the error: a failed run is already
// reported to Healthchecks and the logs, and must not stop tomorrow's attempt.
func logRun(ctx context.Context, runner *backup.Runner, log *slog.Logger) {
	defer func() {
		if p := recover(); p != nil {
			log.Error("scheduler recovered from panic",
				"event", "run.panic", "panic", p, "stack", string(debug.Stack()))
		}
	}()
	if err := runner.RunOnce(ctx); err != nil {
		log.Error("scheduled backup failed", "event", "run.error", "error", err)
	}
}

// staleness decides whether a restart has left the schedule behind.
//
// Always backing up on boot would dump production on every deploy; never doing
// so means a container that crash-loops through its window silently skips a
// day. Asking the bucket how old the newest backup is settles it without
// keeping any local state.
func staleness(ctx context.Context, cfg config.Config, runner *backup.Runner, log *slog.Logger) (bool, string) {
	if cfg.Schedule.RunOnBoot {
		return true, "RUN_ON_BOOT is set"
	}

	for _, db := range cfg.Postgres.Databases {
		newest, err := runner.NewestBackup(ctx, db)
		if err != nil {
			// A transient storage error at boot must not cancel the schedule.
			log.Warn("could not determine last backup age; deferring to the schedule",
				"event", "schedule.staleness_unknown", "database", db, "error", err)
			return false, ""
		}
		if newest.IsZero() {
			return true, fmt.Sprintf("no existing backup found for %q", db)
		}
		if age := time.Since(newest); age > cfg.Schedule.MaxBackupAge {
			return true, fmt.Sprintf("newest backup of %q is %s old, over the %s threshold",
				db, age.Round(time.Minute), cfg.Schedule.MaxBackupAge)
		}
		log.Info("existing backup is current",
			"event", "schedule.staleness_ok", "database", db,
			"newest", newest.Format(time.RFC3339))
	}
	return false, ""
}

func pruneOnly(ctx context.Context, cfg config.Config, runner *backup.Runner, log *slog.Logger) error {
	var errs []error
	for _, db := range cfg.Postgres.Databases {
		if err := runner.PruneDatabase(ctx, db); err != nil {
			errs = append(errs, fmt.Errorf("database %q: %w", db, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	log.Info("prune complete", "event", "prune.finish")
	return nil
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
