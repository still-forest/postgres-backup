// Package logbuf provides an slog handler that tees structured log output to a
// writer and to a bounded in-memory buffer, so the tail of a run's log can be
// attached to a Healthchecks ping.
package logbuf

import (
	"context"
	"io"
	"log/slog"
	"sync"
)

// Tail is a fixed-capacity byte buffer that keeps the most recent writes and
// discards the oldest. It is safe for concurrent use.
type Tail struct {
	mu       sync.Mutex
	buf      []byte
	capacity int
	dropped  bool
}

// NewTail returns a Tail retaining at most capacity bytes.
func NewTail(capacity int) *Tail {
	if capacity < 1 {
		capacity = 1
	}
	return &Tail{capacity: capacity, buf: make([]byte, 0, capacity)}
}

// Write appends p, evicting from the front once capacity is exceeded.
func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(p) >= t.capacity {
		t.buf = append(t.buf[:0], p[len(p)-t.capacity:]...)
		t.dropped = true
		return len(p), nil
	}

	if len(t.buf)+len(p) > t.capacity {
		drop := len(t.buf) + len(p) - t.capacity
		t.buf = append(t.buf[:0], t.buf[drop:]...)
		t.dropped = true
	}
	t.buf = append(t.buf, p...)
	return len(p), nil
}

// String returns the retained tail, prefixed with a truncation marker if any
// earlier output was evicted.
func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.dropped {
		return "[...earlier output truncated...]\n" + string(t.buf)
	}
	return string(t.buf)
}

// Handler wraps an slog.Handler so every record is also written to a Tail.
type Handler struct {
	inner slog.Handler
}

// New returns an slog.Handler writing text records to w and to tail, and the
// Tail itself for later retrieval.
func New(w io.Writer, tail *Tail, level slog.Level) slog.Handler {
	return &Handler{inner: slog.NewTextHandler(io.MultiWriter(w, tail), &slog.HandlerOptions{Level: level})}
}

func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool  { return h.inner.Enabled(ctx, l) }
func (h *Handler) Handle(ctx context.Context, r slog.Record) error { return h.inner.Handle(ctx, r) }
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	return &Handler{inner: h.inner.WithAttrs(as)}
}
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name)}
}
