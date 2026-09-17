package main

import (
	"strings"
	"testing"

	"github.com/still-forest/manor/services/postgres-backup/internal/config"
)

func sourceCfg() config.Config {
	return config.Config{
		Postgres: config.Postgres{
			Host:      "db.internal",
			Databases: []string{"manor_production", "manor_analytics"},
		},
	}
}

// The primary guard is that restore reads a wholly separate set of environment
// variables, so the production connection settings have no path to pg_restore.
func TestRestoreRequiresItsOwnEnvironment(t *testing.T) {
	t.Setenv("POSTGRES_HOST", "db.internal")
	t.Setenv("POSTGRES_USER", "postgres")
	t.Setenv("POSTGRES_PASS", "productionpw")
	t.Setenv("POSTGRES_DB", "manor_production")

	_, err := loadRestoreTarget()
	if err == nil {
		t.Fatal("loadRestoreTarget succeeded using the POSTGRES_* settings")
	}
	for _, want := range []string{"RESTORE_HOST", "RESTORE_USER", "RESTORE_PASS", "RESTORE_DB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

func TestLoadRestoreTargetReadsRestoreVars(t *testing.T) {
	t.Setenv("RESTORE_HOST", "scratch.internal")
	t.Setenv("RESTORE_USER", "restorer")
	t.Setenv("RESTORE_PASS", "pw")
	t.Setenv("RESTORE_DB", "scratch")
	t.Setenv("RESTORE_PORT", "5433")

	got, err := loadRestoreTarget()
	if err != nil {
		t.Fatalf("loadRestoreTarget: %v", err)
	}
	if got.Host != "scratch.internal" || got.Database != "scratch" || got.Port != 5433 {
		t.Errorf("target = %+v", got)
	}
}

func TestLoadRestoreTargetDefaultsPort(t *testing.T) {
	t.Setenv("RESTORE_HOST", "h")
	t.Setenv("RESTORE_USER", "u")
	t.Setenv("RESTORE_PASS", "p")
	t.Setenv("RESTORE_DB", "d")

	got, err := loadRestoreTarget()
	if err != nil {
		t.Fatalf("loadRestoreTarget: %v", err)
	}
	if got.Port != 5432 {
		t.Errorf("Port = %d, want 5432", got.Port)
	}
}

func TestRefusesToRestoreOverABackedUpDatabase(t *testing.T) {
	cfg := sourceCfg()

	for name, target := range map[string]restoreTarget{
		"primary database":   {Host: "db.internal", Database: "manor_production"},
		"secondary database": {Host: "db.internal", Database: "manor_analytics"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkNotSource(target, cfg, false)
			if err == nil {
				t.Fatal("restore into a backed-up database was permitted")
			}
			if !strings.Contains(err.Error(), "--force-production") {
				t.Errorf("error should name the override: %v", err)
			}
		})
	}
}

func TestPermitsRestoreToADifferentTarget(t *testing.T) {
	cfg := sourceCfg()

	for name, target := range map[string]restoreTarget{
		"different host":     {Host: "scratch.internal", Database: "manor_production"},
		"different database": {Host: "db.internal", Database: "manor_scratch"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkNotSource(target, cfg, false); err != nil {
				t.Errorf("restore to a non-source target was refused: %v", err)
			}
		})
	}
}

func TestExplicitOverrideAllowsProductionRestore(t *testing.T) {
	target := restoreTarget{Host: "db.internal", Database: "manor_production"}
	if err := checkNotSource(target, sourceCfg(), true); err != nil {
		t.Errorf("the explicit override did not take effect: %v", err)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:                    "512 B",
		1024:                   "1.0 KB",
		1536:                   "1.5 KB",
		1024 * 1024:            "1.0 MB",
		3 * 1024 * 1024 * 1024: "3.0 GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
