package pgtool

import (
	"context"
	"strings"
	"testing"

	"github.com/still-forest/manor/services/postgres-backup/internal/exec"
)

func testConn() Conn {
	return Conn{Host: "db.internal", Port: 5432, User: "postgres", Password: "pw", Database: "manor_production"}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestClientMajorParsesVersion(t *testing.T) {
	cases := map[string]int{
		"pg_dump (PostgreSQL) 18.6\n":             18,
		"pg_dump (PostgreSQL) 17.11 (Homebrew)\n": 17,
		"pg_restore (PostgreSQL) 18.0\n":          18,
	}
	for out, want := range cases {
		t.Run(strings.TrimSpace(out), func(t *testing.T) {
			f := &exec.Fake{Stubs: []exec.Stub{{Stdout: out}}}
			got, err := ClientMajor(context.Background(), f, "pg_dump")
			if err != nil {
				t.Fatalf("ClientMajor: %v", err)
			}
			if got != want {
				t.Errorf("major = %d, want %d", got, want)
			}
		})
	}
}

func TestClientMajorRejectsUnparseableOutput(t *testing.T) {
	f := &exec.Fake{Stubs: []exec.Stub{{Stdout: "not a version string\n"}}}
	if _, err := ClientMajor(context.Background(), f, "pg_dump"); err == nil {
		t.Fatal("ClientMajor accepted unparseable output")
	}
}

func TestServerMajorConvertsVersionNum(t *testing.T) {
	f := &exec.Fake{Stubs: []exec.Stub{{Stdout: "180006\n"}}}
	got, err := ServerMajor(context.Background(), f, testConn())
	if err != nil {
		t.Fatalf("ServerMajor: %v", err)
	}
	if got != 18 {
		t.Errorf("major = %d, want 18", got)
	}
}

// The password must travel in the environment, never in argv, which is visible
// in the process table and echoed into logs.
func TestConnectionSecretsTravelInEnvironment(t *testing.T) {
	f := &exec.Fake{}
	if err := Ping(context.Background(), f, testConn()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	calls := f.CallsTo("psql")
	if len(calls) != 1 {
		t.Fatalf("psql called %d times, want 1", len(calls))
	}
	c := calls[0]

	for _, a := range c.Args {
		if strings.Contains(a, "pw") {
			t.Errorf("password appeared in args: %s", exec.ArgsString(c))
		}
	}
	for _, want := range []string{"PGPASSWORD=pw", "PGHOST=db.internal", "PGPORT=5432", "PGUSER=postgres"} {
		if !hasArg(c.Env, want) {
			t.Errorf("env missing %q; got %v", want, c.Env)
		}
	}
	if !hasArg(c.Env, "PGCONNECT_TIMEOUT=10") {
		t.Error("no connect timeout was set; a hung connection would stall the run")
	}
}

func TestDumpArgs(t *testing.T) {
	f := &exec.Fake{}
	if err := Dump(context.Background(), f, testConn(), "/tmp/x.dump", "", nil); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	calls := f.CallsTo("pg_dump")
	if len(calls) != 1 {
		t.Fatalf("pg_dump called %d times, want 1", len(calls))
	}
	args := calls[0].Args

	for _, want := range []string{"--format=custom", "--no-owner", "--no-privileges", "--file=/tmp/x.dump", "--dbname=manor_production"} {
		if !hasArg(args, want) {
			t.Errorf("missing %q in %v", want, args)
		}
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--compress") {
			t.Errorf("compression was set without being configured: %q", a)
		}
	}
}

func TestDumpPassesCompressionWhenConfigured(t *testing.T) {
	f := &exec.Fake{}
	if err := Dump(context.Background(), f, testConn(), "/tmp/x.dump", "zstd:3", nil); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if !hasArg(f.CallsTo("pg_dump")[0].Args, "--compress=zstd:3") {
		t.Error("configured compression was not passed to pg_dump")
	}
}

func TestTOCEntriesCountsOnlyRealEntries(t *testing.T) {
	const listing = `;
; Archive created at 2026-09-17 23:00:14 UTC
;     dbname: manor_production
;
; Selected TOC Entries:
;
215; 1259 16456 TABLE public notes manor
216; 1259 16462 TABLE public users manor
217; 1259 16468 TABLE public tasks manor
`
	f := &exec.Fake{Stubs: []exec.Stub{{Stdout: listing}}}

	got, err := TOCEntries(context.Background(), f, "/tmp/x.dump")
	if err != nil {
		t.Fatalf("TOCEntries: %v", err)
	}
	if got != 3 {
		t.Errorf("entries = %d, want 3 (comments and blanks must not count)", got)
	}
}

func TestTOCEntriesOnEmptyArchive(t *testing.T) {
	f := &exec.Fake{Stubs: []exec.Stub{{Stdout: ";\n; Archive created\n;\n"}}}
	got, err := TOCEntries(context.Background(), f, "/tmp/x.dump")
	if err != nil {
		t.Fatalf("TOCEntries: %v", err)
	}
	if got != 0 {
		t.Errorf("entries = %d, want 0", got)
	}
}

// pg_restore otherwise logs errors and still exits zero, which would make a
// half-applied restore look successful.
func TestRestoreIsAtomicAndFailsLoudly(t *testing.T) {
	f := &exec.Fake{}
	err := Restore(context.Background(), f, RestoreOpts{Conn: testConn(), Path: "/tmp/x.dump"}, nil)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	args := f.CallsTo("pg_restore")[0].Args
	for _, want := range []string{"--single-transaction", "--exit-on-error", "--clean", "--if-exists"} {
		if !hasArg(args, want) {
			t.Errorf("missing %q in %v", want, args)
		}
	}
}

func TestDumpGlobalsRequestsOnlyGlobals(t *testing.T) {
	f := &exec.Fake{Stubs: []exec.Stub{{Match: "pg_dumpall", Stdout: "CREATE ROLE manor;\n"}}}
	var out strings.Builder

	if err := DumpGlobals(context.Background(), f, testConn(), &out, nil); err != nil {
		t.Fatalf("DumpGlobals: %v", err)
	}
	if !hasArg(f.CallsTo("pg_dumpall")[0].Args, "--globals-only") {
		t.Error("pg_dumpall was not restricted to globals")
	}
	if !strings.Contains(out.String(), "CREATE ROLE") {
		t.Errorf("globals output was not captured: %q", out.String())
	}
}

func TestDatabaseSizeParsesBytes(t *testing.T) {
	f := &exec.Fake{Stubs: []exec.Stub{{Stdout: "8589934592\n"}}}
	got, err := DatabaseSize(context.Background(), f, testConn())
	if err != nil {
		t.Fatalf("DatabaseSize: %v", err)
	}
	if got != 8589934592 {
		t.Errorf("size = %d, want 8589934592", got)
	}
}
