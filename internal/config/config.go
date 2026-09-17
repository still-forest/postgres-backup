// Package config loads and validates every setting at process start.
//
// Nothing here is read lazily. The service this replaces discovered its
// configuration inside a cron job at 23:00, where a failure produced no output;
// loading and validating everything up front means a misconfigured container
// fails immediately and visibly at deploy time instead.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	Postgres  Postgres
	R2        R2
	Schedule  Schedule
	Retention Retention
	Limits    Limits

	HealthchecksURL string
	LogLevel        string
	LogTailBytes    int
	TmpDir          string
	DryRun          bool
	PruneEnabled    bool
}

// Postgres holds connection settings for the cluster being backed up.
type Postgres struct {
	Host      string
	Port      int
	User      string
	Password  string
	Databases []string
	Compress  string
	Timeout   time.Duration
}

// R2 holds Cloudflare R2 (S3-compatible) settings.
type R2 struct {
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	BucketPath      string
	HostBase        string
}

// Schedule holds the daily run window.
type Schedule struct {
	At           string
	TZ           string
	RunOnBoot    bool
	MaxBackupAge time.Duration
}

// Retention holds the pruning policy.
type Retention struct {
	DailyDays     int
	MonthlyMonths int
	MinKeep       int
	MaxDeletes    int
	AllowBulk     bool
}

// Limits holds the preflight and verification thresholds.
type Limits struct {
	MinFreeBytes  int64
	MinDumpBytes  int64
	MinTOCEntries int
}

// legacyVars were meaningful to the kartoza-based container this service
// replaces. They are reported at startup so a stale Coolify environment is
// visible rather than silently ignored.
var legacyVars = map[string]string{
	"DEFAULT_REGION":  "R2 signs with region \"auto\"; DEFAULT_REGION was a jurisdiction hint, never a signing region",
	"HOST_BUCKET":     "only meaningful to s3cmd",
	"CONSOLE_LOGGING": "logs always go to stdout",
	"STORAGE_BACKEND": "only S3-compatible storage is supported",
	"RUN_ONCE": "for a single backup that exits, use the `backup` subcommand; " +
		"to back up at startup and then stay running on the schedule, set RUN_ON_BOOT=true",
	"POSTGRES_MAJOR_VERSION": "baked into the image",
	"POSTGIS_MAJOR_VERSION":  "baked into the image",
	"POSTGIS_MINOR_RELEASE":  "baked into the image",
	"DB_DUMP_ENCRYPTION":     "not supported; R2 encrypts at rest",
	"CRON_SCHEDULE":          "use BACKUP_AT and BACKUP_TZ",
}

// Load reads configuration from the environment and validates it.
//
// The returned error aggregates every problem found, so a misconfigured deploy
// surfaces all of them at once instead of one per restart.
func Load() (Config, []string, error) {
	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	c := Config{
		HealthchecksURL: os.Getenv("HEALTHCHECKS_URL"),
		LogLevel:        envStr("LOG_LEVEL", "info"),
		TmpDir:          envStr("TMP_DIR", "/var/tmp/pgbackup"),
	}

	c.Postgres = Postgres{
		Host:     os.Getenv("POSTGRES_HOST"),
		User:     os.Getenv("POSTGRES_USER"),
		Password: os.Getenv("POSTGRES_PASS"),
		Compress: os.Getenv("PGDUMP_COMPRESS"),
	}
	c.R2 = R2{
		AccessKeyID:     os.Getenv("ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("SECRET_ACCESS_KEY"),
		Bucket:          os.Getenv("BUCKET"),
		BucketPath:      strings.Trim(os.Getenv("BUCKET_PATH"), "/"),
		HostBase:        os.Getenv("HOST_BASE"),
	}

	for _, r := range []struct{ name, val string }{
		{"POSTGRES_HOST", c.Postgres.Host},
		{"POSTGRES_USER", c.Postgres.User},
		{"POSTGRES_PASS", c.Postgres.Password},
		{"ACCESS_KEY_ID", c.R2.AccessKeyID},
		{"SECRET_ACCESS_KEY", c.R2.SecretAccessKey},
		{"BUCKET", c.R2.Bucket},
		{"HOST_BASE", c.R2.HostBase},
	} {
		if strings.TrimSpace(r.val) == "" {
			fail("%s is required but empty", r.name)
		}
	}

	c.Postgres.Databases = splitList(os.Getenv("POSTGRES_DB"))
	if len(c.Postgres.Databases) == 0 {
		fail("POSTGRES_DB is required (comma-separated for multiple databases)")
	}

	c.Postgres.Port = envInt("POSTGRES_PORT", 5432, &errs)
	if c.Postgres.Port < 1 || c.Postgres.Port > 65535 {
		fail("POSTGRES_PORT must be 1-65535, got %d", c.Postgres.Port)
	}
	c.Postgres.Timeout = envDur("PGDUMP_TIMEOUT", 2*time.Hour, &errs)

	// http is permitted so the service can be exercised against a local MinIO
	// or a local ping listener; production use of either is flagged below.
	var insecure []string
	for _, u := range []struct{ name, val string }{
		{"HOST_BASE", c.R2.HostBase},
		{"HEALTHCHECKS_URL", c.HealthchecksURL},
	} {
		if u.val == "" {
			continue
		}
		parsed, err := url.Parse(u.val)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			fail("%s must be an absolute http or https URL, got %q", u.name, u.val)
			continue
		}
		if parsed.Scheme == "http" {
			insecure = append(insecure, u.name)
		}
	}

	c.Schedule = Schedule{
		At:           envStr("BACKUP_AT", "23:00"),
		TZ:           envStr("BACKUP_TZ", "UTC"),
		RunOnBoot:    envBool("RUN_ON_BOOT", false, &errs),
		MaxBackupAge: envDur("MAX_BACKUP_AGE", 26*time.Hour, &errs),
	}
	if !validHHMM(c.Schedule.At) {
		fail("BACKUP_AT must be HH:MM within a day, got %q", c.Schedule.At)
	}
	if _, err := time.LoadLocation(c.Schedule.TZ); err != nil {
		fail("BACKUP_TZ is not a known IANA timezone: %q", c.Schedule.TZ)
	}

	c.Retention = Retention{
		DailyDays:     envInt("RETENTION_DAILY_DAYS", 30, &errs),
		MonthlyMonths: envInt("RETENTION_MONTHLY_MONTHS", 12, &errs),
		MinKeep:       envInt("RETENTION_MIN_KEEP", 3, &errs),
		MaxDeletes:    envInt("PRUNE_MAX_DELETES", 50, &errs),
		AllowBulk:     envBool("PRUNE_ALLOW_BULK", false, &errs),
	}
	if c.Retention.DailyDays < 1 {
		fail("RETENTION_DAILY_DAYS must be at least 1, got %d", c.Retention.DailyDays)
	}
	if c.Retention.MonthlyMonths < 1 {
		fail("RETENTION_MONTHLY_MONTHS must be at least 1, got %d", c.Retention.MonthlyMonths)
	}
	if c.Retention.MinKeep < 1 {
		fail("RETENTION_MIN_KEEP must be at least 1, got %d", c.Retention.MinKeep)
	}
	if c.Retention.MaxDeletes < 1 {
		fail("PRUNE_MAX_DELETES must be at least 1, got %d", c.Retention.MaxDeletes)
	}

	c.Limits = Limits{
		MinFreeBytes:  int64(envInt("MIN_FREE_BYTES", 2*1024*1024*1024, &errs)),
		MinDumpBytes:  int64(envInt("MIN_DUMP_BYTES", 1024*1024, &errs)),
		MinTOCEntries: envInt("MIN_TOC_ENTRIES", 10, &errs),
	}
	c.LogTailBytes = envInt("LOG_TAIL_BYTES", 10*1024, &errs)
	c.DryRun = envBool("DRY_RUN", false, &errs)
	c.PruneEnabled = envBool("PRUNE_ENABLED", true, &errs)

	if err := checkWritable(c.TmpDir); err != nil {
		fail("TMP_DIR %q is not writable: %v", c.TmpDir, err)
	}

	if len(errs) > 0 {
		return Config{}, nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	warnings := legacyWarnings()
	for _, name := range insecure {
		warnings = append(warnings, fmt.Sprintf(
			"%s uses http, not https; its traffic would cross the network in the clear", name))
	}
	return c, warnings, nil
}

// Redacted renders the effective configuration for the startup log with
// secrets removed.
func (c Config) Redacted() []any {
	return []any{
		"postgres_host", c.Postgres.Host,
		"postgres_port", c.Postgres.Port,
		"postgres_user", c.Postgres.User,
		"databases", strings.Join(c.Postgres.Databases, ","),
		"bucket", c.R2.Bucket,
		"bucket_path", c.R2.BucketPath,
		"host_base", c.R2.HostBase,
		"backup_at", c.Schedule.At,
		"backup_tz", c.Schedule.TZ,
		"run_on_boot", c.Schedule.RunOnBoot,
		"max_backup_age", c.Schedule.MaxBackupAge.String(),
		"retention_daily_days", c.Retention.DailyDays,
		"retention_monthly_months", c.Retention.MonthlyMonths,
		"retention_min_keep", c.Retention.MinKeep,
		"prune_max_deletes", c.Retention.MaxDeletes,
		"prune_allow_bulk", c.Retention.AllowBulk,
		"prune_enabled", c.PruneEnabled,
		"dry_run", c.DryRun,
		"tmp_dir", c.TmpDir,
		"healthchecks_configured", c.HealthchecksURL != "",
	}
}

// checkWritable verifies the directory exists and accepts a write, by actually
// creating and removing a file. A stat alone would pass on a read-only mount.
func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".pgbackup-write-probe")
	f, err := os.Create(probe)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(probe)
}

func legacyWarnings() []string {
	var out []string
	for name, why := range legacyVars {
		if _, ok := os.LookupEnv(name); ok {
			out = append(out, fmt.Sprintf("%s is set but ignored: %s", name, why))
		}
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validHHMM(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil && h >= 0 && h <= 23 && m >= 0 && m <= 59
}

func envStr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func envInt(name string, def int, errs *[]string) int {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s must be an integer, got %q", name, v))
		return def
	}
	return n
}

func envBool(name string, def bool, errs *[]string) bool {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.ToLower(strings.TrimSpace(v)))
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s must be a boolean, got %q", name, v))
		return def
	}
	return b
}

func envDur(name string, def time.Duration, errs *[]string) time.Duration {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s must be a duration like 26h, got %q", name, v))
		return def
	}
	return d
}
