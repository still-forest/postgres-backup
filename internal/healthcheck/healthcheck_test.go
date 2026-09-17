package healthcheck

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type capture struct {
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func (c *capture) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, r.URL.Path)
	c.bodies = append(c.bodies, string(b))
}

func (c *capture) snapshot() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.paths...), append([]string(nil), c.bodies...)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newPinger(t *testing.T, h http.HandlerFunc) (*HTTP, *capture) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.record(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)

	return &HTTP{
		BaseURL:  srv.URL + "/ping/abc",
		Client:   srv.Client(),
		Log:      quietLogger(),
		MaxBody:  1024,
		Attempts: 3,
	}, c
}

func TestPingPaths(t *testing.T) {
	p, c := newPinger(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	ctx := context.Background()

	p.Start(ctx, "starting")
	p.Success(ctx, "done")
	p.Fail(ctx, "broke")

	paths, bodies := c.snapshot()
	want := []string{"/ping/abc/start", "/ping/abc", "/ping/abc/fail"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
	if bodies[2] != "broke" {
		t.Errorf("failure body = %q, want %q", bodies[2], "broke")
	}
}

func TestPingRetriesOnServerError(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	p, _ := newPinger(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	p.Success(context.Background(), "ok")

	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Errorf("made %d attempts, want a retry after the 5xx", calls)
	}
}

// A backup that succeeded but could not be reported is still a good backup,
// so a ping failure must never surface to the caller.
func TestPingFailureDoesNotPanicOrBlock(t *testing.T) {
	p := &HTTP{
		BaseURL:  "https://127.0.0.1:1/ping",
		Client:   &http.Client{Timeout: 50 * time.Millisecond},
		Log:      quietLogger(),
		Attempts: 2,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Fail(context.Background(), "body")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Fail blocked far longer than its timeout budget")
	}
}

func TestBodyIsTruncatedToLimit(t *testing.T) {
	p, c := newPinger(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	p.MaxBody = 20

	p.Fail(context.Background(), strings.Repeat("x", 500)+"TAIL")

	_, bodies := c.snapshot()
	if len(bodies[0]) != 20 {
		t.Errorf("body length = %d, want 20", len(bodies[0]))
	}
	// Truncation keeps the end, which is where the error is.
	if !strings.HasSuffix(bodies[0], "TAIL") {
		t.Errorf("body = %q, want it to retain the tail", bodies[0])
	}
}

func TestNoopWhenURLIsEmpty(t *testing.T) {
	p := New("", 1024, quietLogger())
	if _, ok := p.(Noop); !ok {
		t.Fatalf("New(\"\") returned %T, want Noop", p)
	}
	p.Fail(context.Background(), "should not panic")
}

// A cancelled context must not send the pinger into its full retry schedule:
// the /fail ping on shutdown is the one that has to get through promptly.
func TestCancelledContextStopsRetrying(t *testing.T) {
	p, c := newPinger(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	p.Fail(ctx, "body")

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Fail took %s on a cancelled context; it should give up quickly", elapsed)
	}
	paths, _ := c.snapshot()
	if len(paths) > 1 {
		t.Errorf("made %d attempts on a cancelled context, want at most 1", len(paths))
	}
}
