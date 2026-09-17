package config

import (
	"fmt"
	"strings"
	"testing"
)

// valid is the minimum environment for a successful Load.
func valid(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"POSTGRES_HOST":     "db.internal",
		"POSTGRES_USER":     "postgres",
		"POSTGRES_PASS":     "secret",
		"POSTGRES_DB":       "manor_production",
		"ACCESS_KEY_ID":     "abc123",
		"SECRET_ACCESS_KEY": "def456",
		"BUCKET":            "manor-postgres-backup",
		"BUCKET_PATH":       "manor_db",
		"HOST_BASE":         "https://acct.r2.cloudflarestorage.com",
		"TMP_DIR":           t.TempDir(),
	} {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	valid(t)

	c, warnings, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}

	if got := c.Schedule.At; got != "23:00" {
		t.Errorf("BACKUP_AT default = %q, want 23:00", got)
	}
	if got := c.Schedule.TZ; got != "UTC" {
		t.Errorf("BACKUP_TZ default = %q, want UTC", got)
	}
	if got := c.Retention.DailyDays; got != 30 {
		t.Errorf("RETENTION_DAILY_DAYS default = %d, want 30", got)
	}
	if got := c.Retention.MonthlyMonths; got != 12 {
		t.Errorf("RETENTION_MONTHLY_MONTHS default = %d, want 12", got)
	}
	if !c.PruneEnabled {
		t.Error("PRUNE_ENABLED should default to true")
	}
	if c.Retention.AllowBulk {
		t.Error("PRUNE_ALLOW_BULK should default to false")
	}
	if c.Postgres.Port != 5432 {
		t.Errorf("POSTGRES_PORT default = %d, want 5432", c.Postgres.Port)
	}
}

func TestLoadRejectsMissingRequired(t *testing.T) {
	for _, name := range []string{
		"POSTGRES_HOST", "POSTGRES_USER", "POSTGRES_PASS", "POSTGRES_DB",
		"ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "BUCKET", "HOST_BASE",
	} {
		t.Run(name, func(t *testing.T) {
			valid(t)
			t.Setenv(name, "")

			_, _, err := Load()
			if err == nil {
				t.Fatalf("Load succeeded with %s empty", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error does not name %s: %v", name, err)
			}
		})
	}
}

// Every problem should surface at once, so a misconfigured deploy is fixed in
// one pass rather than one restart per mistake.
func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	valid(t)
	t.Setenv("POSTGRES_HOST", "")
	t.Setenv("BUCKET", "")
	t.Setenv("BACKUP_AT", "nope")

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"POSTGRES_HOST", "BUCKET", "BACKUP_AT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error omits %s: %v", want, err)
		}
	}
}

func TestLoadRejectsMalformedValues(t *testing.T) {
	cases := map[string]struct{ key, val string }{
		"port not a number":     {"POSTGRES_PORT", "abc"},
		"port out of range":     {"POSTGRES_PORT", "70000"},
		"backup time bad":       {"BACKUP_AT", "25:00"},
		"backup time unpadded":  {"BACKUP_AT", "9:00"},
		"timezone unknown":      {"BACKUP_TZ", "Mars/Olympus"},
		"host base not a url":   {"HOST_BASE", "not a url"},
		"host base bad scheme":  {"HOST_BASE", "ftp://acct.r2.cloudflarestorage.com"},
		"healthchecks bad sch.": {"HEALTHCHECKS_URL", "ftp://example.com/ping"},
		"retention zero":        {"RETENTION_DAILY_DAYS", "0"},
		"retention negative":    {"RETENTION_MONTHLY_MONTHS", "-1"},
		"min keep zero":         {"RETENTION_MIN_KEEP", "0"},
		"max deletes zero":      {"PRUNE_MAX_DELETES", "0"},
		"bad boolean":           {"PRUNE_ENABLED", "yesplease"},
		"bad duration":          {"MAX_BACKUP_AGE", "one day"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			valid(t)
			t.Setenv(c.key, c.val)

			if _, _, err := Load(); err == nil {
				t.Fatalf("Load succeeded with %s=%q", c.key, c.val)
			}
		})
	}
}

func TestLoadParsesMultipleDatabases(t *testing.T) {
	valid(t)
	t.Setenv("POSTGRES_DB", " manor_production , manor_analytics ,, ")

	c, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"manor_production", "manor_analytics"}
	if len(c.Postgres.Databases) != len(want) {
		t.Fatalf("databases = %v, want %v", c.Postgres.Databases, want)
	}
	for i := range want {
		if c.Postgres.Databases[i] != want[i] {
			t.Errorf("databases[%d] = %q, want %q", i, c.Postgres.Databases[i], want[i])
		}
	}
}

func TestLoadTrimsBucketPath(t *testing.T) {
	valid(t)
	t.Setenv("BUCKET_PATH", "/manor_db/")

	c, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.R2.BucketPath != "manor_db" {
		t.Errorf("BucketPath = %q, want manor_db", c.R2.BucketPath)
	}
}

// A stale Coolify environment should be visible rather than silently ignored.
func TestLoadWarnsAboutLegacyVars(t *testing.T) {
	valid(t)
	t.Setenv("DEFAULT_REGION", "apac")
	t.Setenv("CONSOLE_LOGGING", "True")
	t.Setenv("RUN_ONCE", "true")

	_, warnings, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"DEFAULT_REGION", "CONSOLE_LOGGING", "RUN_ONCE"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning for %s in:\n%s", want, joined)
		}
	}
}

func TestRedactedOmitsSecrets(t *testing.T) {
	valid(t)
	t.Setenv("POSTGRES_PASS", "pgpw-sentinel-9f3a")
	t.Setenv("SECRET_ACCESS_KEY", "r2secret-sentinel-4b7c")
	t.Setenv("ACCESS_KEY_ID", "r2keyid-sentinel-1d2e")

	c, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rendered := fmt.Sprint(c.Redacted()...)
	for _, secret := range []string{"pgpw-sentinel-9f3a", "r2secret-sentinel-4b7c", "r2keyid-sentinel-1d2e"} {
		if strings.Contains(rendered, secret) {
			t.Errorf("Redacted leaked %q", secret)
		}
	}

	// Sanity-check that the rendering is actually populated, so the assertion
	// above cannot pass by rendering nothing at all.
	if !strings.Contains(rendered, "db.internal") {
		t.Errorf("Redacted did not include the host; rendering may be empty: %s", rendered)
	}
}

func TestUnwritableTmpDirIsRejected(t *testing.T) {
	valid(t)
	t.Setenv("TMP_DIR", "/proc/definitely-not-writable/pgbackup")

	if _, _, err := Load(); err == nil {
		t.Fatal("Load accepted an unwritable TMP_DIR")
	}
}

// http must work for local testing against MinIO, but never pass unremarked.
func TestHTTPEndpointsAreAllowedButWarned(t *testing.T) {
	valid(t)
	t.Setenv("HOST_BASE", "http://localhost:9000")

	c, warnings, err := Load()
	if err != nil {
		t.Fatalf("Load rejected an http endpoint: %v", err)
	}
	if c.R2.HostBase != "http://localhost:9000" {
		t.Errorf("HostBase = %q", c.R2.HostBase)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "HOST_BASE uses http") {
		t.Errorf("no warning for an http endpoint: %v", warnings)
	}
}
