// Package healthcheck reports run outcomes to a Healthchecks-style endpoint.
package healthcheck

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Pinger signals the start, success or failure of a run.
type Pinger interface {
	Start(ctx context.Context, body string)
	Success(ctx context.Context, body string)
	Fail(ctx context.Context, body string)
}

// HTTP pings a Healthchecks check by URL.
type HTTP struct {
	BaseURL  string
	Client   *http.Client
	Log      *slog.Logger
	MaxBody  int
	Attempts int
}

// New returns an HTTP pinger. A blank base URL yields a no-op pinger.
func New(baseURL string, maxBody int, log *slog.Logger) Pinger {
	if strings.TrimSpace(baseURL) == "" {
		return Noop{}
	}
	return &HTTP{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Client:   &http.Client{Timeout: 10 * time.Second},
		Log:      log,
		MaxBody:  maxBody,
		Attempts: 3,
	}
}

func (h *HTTP) Start(ctx context.Context, body string)   { h.ping(ctx, "/start", body) }
func (h *HTTP) Success(ctx context.Context, body string) { h.ping(ctx, "", body) }
func (h *HTTP) Fail(ctx context.Context, body string)    { h.ping(ctx, "/fail", body) }

// ping is best-effort by design. A backup that succeeded but could not be
// reported is still a good backup, and the check going overdue raises the alarm
// anyway, so a ping failure is logged and never propagated to the caller.
func (h *HTTP) ping(ctx context.Context, suffix, body string) {
	url := h.BaseURL + suffix
	if h.MaxBody > 0 && len(body) > h.MaxBody {
		body = body[len(body)-h.MaxBody:]
	}

	attempts := max(h.Attempts, 1)

	var lastErr error
retry:
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
				break retry
			case <-time.After(time.Duration(1<<i) * time.Second):
			}
		}

		err := h.post(ctx, url, body)
		if err == nil {
			h.logger().Debug("healthcheck pinged", "event", "healthcheck.ping", "url", url)
			return
		}
		lastErr = err
	}

	h.logger().Error("healthcheck ping failed; backup outcome is unaffected",
		"event", "healthcheck.ping_failed", "url", url, "error", lastErr)
}

func (h *HTTP) post(ctx context.Context, url, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func (h *HTTP) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

// Noop discards every ping. Used when no HEALTHCHECKS_URL is configured.
type Noop struct{}

func (Noop) Start(context.Context, string)   {}
func (Noop) Success(context.Context, string) {}
func (Noop) Fail(context.Context, string)    {}
