package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/still-forest/manor/services/postgres-backup/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// RUN_ON_BOOT must force a startup backup unconditionally, without consulting
// the bucket. Unlike the old container's RUN_ONCE, the caller then continues
// into the scheduler rather than exiting.
func TestRunOnBootForcesAStartupRun(t *testing.T) {
	cfg := config.Config{
		Schedule: config.Schedule{RunOnBoot: true, MaxBackupAge: 26 * time.Hour},
		Postgres: config.Postgres{Databases: []string{"manor_production"}},
	}

	due, why := staleness(context.Background(), cfg, nil, quietLogger())
	if !due {
		t.Fatal("RUN_ON_BOOT=true did not trigger a startup backup")
	}
	if !strings.Contains(why, "RUN_ON_BOOT") {
		t.Errorf("reason = %q, want it to cite RUN_ON_BOOT", why)
	}
}
