package backup

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/still-forest/manor/services/postgres-backup/internal/config"
	"github.com/still-forest/manor/services/postgres-backup/internal/exec"
	"github.com/still-forest/manor/services/postgres-backup/internal/objstore"
)

var runAt = time.Date(2026, 9, 17, 23, 0, 0, 0, time.UTC)

// recordingPinger captures the Healthchecks calls a run makes.
type recordingPinger struct {
	started  bool
	success  bool
	failed   bool
	failBody string
}

func (p *recordingPinger) Start(context.Context, string)    { p.started = true }
func (p *recordingPinger) Success(context.Context, string)  { p.success = true }
func (p *recordingPinger) Fail(_ context.Context, b string) { p.failed = true; p.failBody = b }

type harness struct {
	runner *Runner
	store  *objstore.Fake
	cmd    *exec.Fake
	ping   *recordingPinger
	tmp    string
}

// newHarness wires a Runner whose external world is entirely faked, and stubs
// the command sequence of a successful run.
func newHarness(t *testing.T) *harness {
	t.Helper()
	tmp := t.TempDir()

	cfg := config.Config{
		Postgres: config.Postgres{
			Host: "db.internal", Port: 5432, User: "postgres",
			Password: "pw", Databases: []string{"manor_production"},
			Timeout: time.Hour,
		},
		R2:           config.R2{Bucket: "b", BucketPath: "manor_db"},
		Retention:    config.Retention{DailyDays: 30, MonthlyMonths: 12, MinKeep: 1, MaxDeletes: 50},
		Limits:       config.Limits{MinFreeBytes: 1, MinDumpBytes: 4, MinTOCEntries: 2},
		TmpDir:       tmp,
		PruneEnabled: true,
	}

	h := &harness{
		store: objstore.NewFake(),
		cmd:   &exec.Fake{},
		ping:  &recordingPinger{},
		tmp:   tmp,
	}
	h.store.Now = func() time.Time { return runAt }

	h.runner = &Runner{
		Cfg:    cfg,
		Store:  h.store,
		Runner: h.cmd,
		Ping:   h.ping,
		Log:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Now:    func() time.Time { return runAt },
		Tail:   func() string { return "recent log output" },
	}
	h.stubHappyPath("")
	return h
}

// stubHappyPath queues the commands a successful run issues. dumpBody controls
// the contents of the file pg_dump is pretended to have written.
func (h *harness) stubHappyPath(dumpBody string) {
	if dumpBody == "" {
		dumpBody = strings.Repeat("dump-bytes", 64)
	}
	h.cmd.Stubs = []exec.Stub{
		{Match: "psql", Stdout: "1048576\n"}, // pg_database_size
		{Match: "pg_dump", Do: func(c exec.Cmd) error {
			return os.WriteFile(dumpPath(c), []byte(dumpBody), 0o600)
		}},
		{Match: "pg_restore", Stdout: ";\n; Archive created at\n;\n1; 1259 TABLE notes\n2; 1259 TABLE users\n3; 1259 TABLE tasks\n"},
		{Match: "pg_restore"}, // VerifyReadable: full read of the archive
		{Match: "pg_dumpall", Stdout: "CREATE ROLE manor;\n"},
	}
}

// dumpPath extracts the --file= target from a recorded pg_dump invocation.
func dumpPath(c exec.Cmd) string {
	for _, a := range c.Args {
		if strings.HasPrefix(a, "--file=") {
			return strings.TrimPrefix(a, "--file=")
		}
	}
	return ""
}

func TestRunOnceHappyPath(t *testing.T) {
	h := newHarness(t)

	if err := h.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !h.ping.started {
		t.Error("no /start ping was sent")
	}
	if !h.ping.success {
		t.Error("no success ping was sent")
	}
	if h.ping.failed {
		t.Error("a /fail ping was sent for a successful run")
	}

	wantKey := objstore.BuildKey("manor_db", "manor_production", runAt)
	if !contains(h.store.Keys(), wantKey) {
		t.Errorf("dump was not uploaded; keys = %v", h.store.Keys())
	}
	if !contains(h.store.Keys(), objstore.GlobalsKey("manor_db", "manor_production", runAt)) {
		t.Errorf("globals were not uploaded; keys = %v", h.store.Keys())
	}
}

// The password must reach pg_dump through the environment only: argv is visible
// in the process table and is echoed into logs.
func TestPasswordNeverAppearsInArgs(t *testing.T) {
	h := newHarness(t)
	if err := h.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(h.cmd.Calls) == 0 {
		t.Fatal("no commands were run")
	}
	for _, c := range h.cmd.Calls {
		for _, a := range c.Args {
			if strings.Contains(a, "pw") && strings.Contains(a, "PGPASSWORD") {
				t.Errorf("password leaked into args of %s", exec.ArgsString(c))
			}
		}
		if c.Path == "pg_dump" || c.Path == "psql" {
			if !hasEnv(c.Env, "PGPASSWORD=pw") {
				t.Errorf("%s did not receive PGPASSWORD via the environment", c.Path)
			}
		}
	}
}

func TestDumpFailurePingsFail(t *testing.T) {
	h := newHarness(t)
	h.cmd.Stubs = []exec.Stub{
		{Match: "psql", Stdout: "1048576\n"},
		{Match: "pg_dump", Err: errUnavailable{}},
	}

	err := h.runner.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce succeeded despite pg_dump failing")
	}
	if !h.ping.started {
		t.Error("no /start ping was sent")
	}
	if !h.ping.failed {
		t.Fatal("no /fail ping was sent for a failed run")
	}
	if h.ping.success {
		t.Error("a success ping was sent for a failed run")
	}
	if !strings.Contains(h.ping.failBody, "recent log output") {
		t.Error("the failure ping did not carry the log tail")
	}
	if len(h.store.Keys()) != 0 {
		t.Errorf("a failed run uploaded something: %v", h.store.Keys())
	}
}

// A dump that is structurally valid but nearly empty is the classic symptom of
// backing up the wrong database, and must not be accepted.
func TestRejectsDumpWithTooFewTOCEntries(t *testing.T) {
	h := newHarness(t)
	h.stubHappyPath("")
	h.cmd.Stubs[2] = exec.Stub{Match: "pg_restore", Stdout: ";\n; Archive\n;\n1; 1259 TABLE only_one\n"}

	err := h.runner.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce accepted a dump with too few objects")
	}
	if !strings.Contains(err.Error(), "wrong database") {
		t.Errorf("error should explain the likely cause: %v", err)
	}
	if len(h.store.Keys()) != 0 {
		t.Errorf("an unverified dump was uploaded: %v", h.store.Keys())
	}
}

func TestRejectsUndersizedDump(t *testing.T) {
	h := newHarness(t)
	h.stubHappyPath("x")

	err := h.runner.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce accepted a one-byte dump")
	}
	if len(h.store.Keys()) != 0 {
		t.Errorf("an undersized dump was uploaded: %v", h.store.Keys())
	}
}

func TestRejectsUnreadableArchive(t *testing.T) {
	h := newHarness(t)
	h.stubHappyPath("")
	h.cmd.Stubs[2] = exec.Stub{Match: "pg_restore", Err: errUnavailable{}}

	if err := h.runner.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce accepted an archive pg_restore could not read")
	}
	if len(h.store.Keys()) != 0 {
		t.Errorf("an unverified dump was uploaded: %v", h.store.Keys())
	}
}

// Temporary files must not survive any exit path; the container's volume is
// sized for one dump.
func TestTempFilesAreRemoved(t *testing.T) {
	for name, broken := range map[string]bool{"success": false, "failure": true} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if broken {
				h.cmd.Stubs[2] = exec.Stub{Match: "pg_restore", Err: errUnavailable{}}
			}

			_ = h.runner.RunOnce(context.Background())

			entries, err := os.ReadDir(h.tmp)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.Contains(e.Name(), ".dump") {
					t.Errorf("left behind %s", filepath.Join(h.tmp, e.Name()))
				}
			}
		})
	}
}

func TestUploadChecksumMismatchFails(t *testing.T) {
	h := newHarness(t)
	h.runner.Store = mismatchStore{h.store}

	if err := h.runner.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce accepted an upload whose checksum did not round-trip")
	}
}

// A prune failure after a good upload must still raise the alarm, but the body
// has to make clear the backup itself is safe.
func TestPruneFailureStillReportsUploadSucceeded(t *testing.T) {
	h := newHarness(t)
	h.store.ListErr = errUnavailable{}

	err := h.runner.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce succeeded despite the prune failing")
	}
	if !h.ping.failed {
		t.Error("no /fail ping was sent")
	}
	if !contains(h.store.Keys(), objstore.BuildKey("manor_db", "manor_production", runAt)) {
		t.Error("the dump should still have been uploaded before the prune ran")
	}
	// The alert must not read as a lost backup.
	if !strings.Contains(h.ping.failBody, PruneFailedPrefix) {
		t.Errorf("failure body should lead with %q, got: %s", PruneFailedPrefix, h.ping.failBody)
	}
}

// The first nights after a deploy have fewer stored backups than MinKeep, and
// must not be reported as failures.
func TestFreshBucketRunSucceeds(t *testing.T) {
	h := newHarness(t)
	h.runner.Cfg.Retention.MinKeep = 3

	if err := h.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the first run against an empty bucket failed: %v", err)
	}
	if !h.ping.success {
		t.Error("no success ping was sent")
	}
	if h.ping.failed {
		t.Errorf("a /fail ping was sent: %s", h.ping.failBody)
	}
}

func TestDryRunUploadsNothing(t *testing.T) {
	h := newHarness(t)
	h.runner.Cfg.DryRun = true

	if err := h.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(h.store.Keys()) != 0 {
		t.Errorf("a dry run wrote to the store: %v", h.store.Keys())
	}
	if !h.ping.success {
		t.Error("a dry run should still report success")
	}
}

func TestPruneRemovesExpiredBackupsAndTheirGlobals(t *testing.T) {
	h := newHarness(t)

	old := runAt.AddDate(0, 0, -400)
	oldKey := objstore.BuildKey("manor_db", "manor_production", old)
	oldGlobals := objstore.GlobalsKey("manor_db", "manor_production", old)
	h.store.Seed(oldKey, 100, old)
	h.store.Seed(oldGlobals, 10, old)
	for d := 1; d <= 3; d++ {
		ts := runAt.AddDate(0, 0, -d)
		h.store.Seed(objstore.BuildKey("manor_db", "manor_production", ts), 100, ts)
	}

	if err := h.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if contains(h.store.Keys(), oldKey) {
		t.Error("the expired dump was not pruned")
	}
	if contains(h.store.Keys(), oldGlobals) {
		t.Error("the expired dump's globals sidecar was left behind")
	}
}

// Objects written by the tool this service replaces must survive untouched.
func TestPruneLeavesForeignKeysAlone(t *testing.T) {
	h := newHarness(t)
	const legacy = "manor_db/2024/September/PG_manor_17-September-2024.dmp"
	h.store.Seed(legacy, 100, runAt.AddDate(-2, 0, 0))

	if err := h.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !contains(h.store.Keys(), legacy) {
		t.Error("a key this service did not write was deleted")
	}
}

func TestPreflightRejectsWrongClientVersion(t *testing.T) {
	h := newHarness(t)
	h.cmd.Stubs = []exec.Stub{{Match: "pg_dump", Stdout: "pg_dump (PostgreSQL) 17.4\n"}}

	err := h.runner.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight accepted a pg_dump of the wrong major version")
	}
	if !strings.Contains(err.Error(), "17") {
		t.Errorf("error should name the version found: %v", err)
	}
}

func TestPreflightRejectsUnreachableStorage(t *testing.T) {
	h := newHarness(t)
	h.cmd.Stubs = []exec.Stub{
		{Match: "pg_dump", Stdout: "pg_dump (PostgreSQL) 18.6\n"},
		{Match: "psql", Stdout: "1\n"},
		{Match: "psql", Stdout: "180006\n"},
	}
	h.store.UploadErr = errUnavailable{}

	err := h.runner.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight passed despite object storage being unwritable")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("error should say storage is unwritable: %v", err)
	}
}

func TestPreflightPassesWhenEverythingWorks(t *testing.T) {
	h := newHarness(t)
	h.cmd.Stubs = []exec.Stub{
		{Match: "pg_dump", Stdout: "pg_dump (PostgreSQL) 18.6\n"},
		{Match: "psql", Stdout: "1\n"},
		{Match: "psql", Stdout: "180006\n"},
	}

	if err := h.runner.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(h.store.Keys()) != 0 {
		t.Errorf("preflight left its probe object behind: %v", h.store.Keys())
	}
}

type errUnavailable struct{}

func (errUnavailable) Error() string { return "simulated failure" }

// mismatchStore reports a checksum that disagrees with what was uploaded.
type mismatchStore struct{ *objstore.Fake }

func (m mismatchStore) Head(ctx context.Context, key string) (objstore.Object, map[string]string, error) {
	o, meta, err := m.Fake.Head(ctx, key)
	if meta != nil {
		meta["sha256"] = "0000000000000000000000000000000000000000000000000000000000000000"
	}
	return o, meta, err
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func hasEnv(env []string, want string) bool { return contains(env, want) }

// Absent checksum metadata must fail rather than be skipped: this run wrote it
// moments earlier, so its absence means something is wrong.
func TestUploadWithoutChecksumMetadataFails(t *testing.T) {
	h := newHarness(t)
	h.runner.Store = strippedMetaStore{h.store}

	err := h.runner.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce accepted an upload with no checksum metadata")
	}
	if !strings.Contains(err.Error(), "no sha256 metadata") {
		t.Errorf("error should name the missing checksum: %v", err)
	}
}

type strippedMetaStore struct{ *objstore.Fake }

func (m strippedMetaStore) Head(ctx context.Context, key string) (objstore.Object, map[string]string, error) {
	o, _, err := m.Fake.Head(ctx, key)
	return o, map[string]string{}, err
}

// A dump truncated partway still carries a complete table of contents, so the
// TOC check alone cannot catch it; only a full read of the archive can.
func TestRejectsTruncatedArchive(t *testing.T) {
	h := newHarness(t)
	h.cmd.Stubs[3] = exec.Stub{
		Match:  "pg_restore",
		Stderr: "pg_restore: error: could not read from input file: end of file\n",
		Err:    errUnavailable{},
	}

	err := h.runner.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce accepted a truncated archive")
	}
	if !strings.Contains(err.Error(), "not fully readable") {
		t.Errorf("error should identify the archive as unreadable: %v", err)
	}
	if len(h.store.Keys()) != 0 {
		t.Errorf("a truncated dump was uploaded: %v", h.store.Keys())
	}
}
