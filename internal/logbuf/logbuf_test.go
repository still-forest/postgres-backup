package logbuf

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestTailKeepsMostRecentBytes(t *testing.T) {
	tail := NewTail(10)
	if _, err := tail.Write([]byte("abcdefghijklmno")); err != nil {
		t.Fatal(err)
	}

	got := tail.String()
	if !strings.HasSuffix(got, "fghijklmno") {
		t.Errorf("tail = %q, want it to end with the last 10 bytes", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("tail did not mark that earlier output was dropped")
	}
}

func TestTailUnderCapacityIsVerbatim(t *testing.T) {
	tail := NewTail(100)
	if _, err := tail.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got := tail.String(); got != "hello" {
		t.Errorf("tail = %q, want %q", got, "hello")
	}
}

func TestTailEvictsAcrossWrites(t *testing.T) {
	tail := NewTail(10)
	for _, s := range []string{"aaaa", "bbbb", "cccc"} {
		if _, err := tail.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}

	got := tail.String()
	if !strings.HasSuffix(got, "bbcccc") {
		t.Errorf("tail = %q, want it to end with the most recent bytes", got)
	}
	if strings.Contains(got, "aaaa") {
		t.Error("tail retained evicted output")
	}
}

// The tail exists to be attached to a failure ping, so it must contain what
// the container logs contain.
func TestHandlerWritesToBothDestinations(t *testing.T) {
	var out bytes.Buffer
	tail := NewTail(4096)
	log := slog.New(New(&out, tail, slog.LevelInfo))

	log.Info("dumping database", "event", "dump.start", "database", "manor_production")

	for name, got := range map[string]string{"stdout": out.String(), "tail": tail.String()} {
		if !strings.Contains(got, "dumping database") {
			t.Errorf("%s missing the message: %q", name, got)
		}
		if !strings.Contains(got, "manor_production") {
			t.Errorf("%s missing the attributes: %q", name, got)
		}
	}
}

func TestHandlerRespectsLevel(t *testing.T) {
	var out bytes.Buffer
	tail := NewTail(4096)
	log := slog.New(New(&out, tail, slog.LevelWarn))

	log.Debug("noisy pg_dump output")
	log.Warn("disk nearly full")

	if strings.Contains(out.String(), "noisy") {
		t.Error("a debug record was emitted at warn level")
	}
	if !strings.Contains(out.String(), "disk nearly full") {
		t.Error("the warn record was dropped")
	}
}

func TestTailIsConcurrencySafe(t *testing.T) {
	tail := NewTail(1024)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_, _ = tail.Write([]byte("concurrent write\n"))
				_ = tail.String()
			}
		}()
	}
	wg.Wait()

	if len(tail.String()) == 0 {
		t.Error("tail is empty after concurrent writes")
	}
}
