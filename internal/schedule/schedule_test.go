package schedule

import (
	"context"
	"testing"
	"time"
)

// fakeClock advances only when a test tells it to.
type fakeClock struct {
	now   time.Time
	slept []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch, func() bool { return true }
}

func mustNew(t *testing.T, at, zone string) *Daily {
	t.Helper()
	d, err := New(at, zone, &fakeClock{})
	if err != nil {
		t.Fatalf("New(%q, %q): %v", at, zone, err)
	}
	return d
}

func TestNextRunLaterToday(t *testing.T) {
	d := mustNew(t, "23:00", "UTC")
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	got := d.NextRun(now)
	want := time.Date(2026, 9, 17, 23, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("NextRun = %s, want %s", got, want)
	}
}

func TestNextRunRollsToTomorrow(t *testing.T) {
	d := mustNew(t, "23:00", "UTC")

	for name, now := range map[string]time.Time{
		"just after the window": time.Date(2026, 9, 17, 23, 0, 1, 0, time.UTC),
		"exactly at":            time.Date(2026, 9, 17, 23, 0, 0, 0, time.UTC),
		"late at night":         time.Date(2026, 9, 17, 23, 59, 59, 0, time.UTC),
	} {
		t.Run(name, func(t *testing.T) {
			got := d.NextRun(now)
			want := time.Date(2026, 9, 18, 23, 0, 0, 0, time.UTC)
			if !got.Equal(want) {
				t.Errorf("NextRun(%s) = %s, want %s", now, got, want)
			}
		})
	}
}

func TestNextRunCrossesMonthAndYear(t *testing.T) {
	d := mustNew(t, "23:00", "UTC")

	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 9, 30, 23, 30, 0, 0, time.UTC), time.Date(2026, 10, 1, 23, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 23, 30, 0, 0, time.UTC), time.Date(2027, 1, 1, 23, 0, 0, 0, time.UTC)},
		{time.Date(2028, 2, 28, 23, 30, 0, 0, time.UTC), time.Date(2028, 2, 29, 23, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := d.NextRun(c.now); !got.Equal(c.want) {
			t.Errorf("NextRun(%s) = %s, want %s", c.now, got, c.want)
		}
	}
}

// Recomputing from the wall clock is what keeps the run time from drifting;
// a fixed 24h tick would slide by however long each backup takes.
func TestScheduleDoesNotDrift(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)}
	d, err := New("23:00", "UTC", clock)
	if err != nil {
		t.Fatal(err)
	}

	for range 40 {
		next := d.NextRun(clock.Now())
		// Simulate the run itself taking 47 minutes.
		clock.now = next.Add(47 * time.Minute)

		if next.Hour() != 23 || next.Minute() != 0 {
			t.Fatalf("scheduled run drifted to %s", next.Format(time.RFC3339))
		}
	}
}

func TestNextRunAcrossDSTSpringForward(t *testing.T) {
	// 02:30 does not exist on 2026-03-08 in New York.
	d := mustNew(t, "02:30", "America/New_York")
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}

	now := time.Date(2026, 3, 7, 12, 0, 0, 0, ny)
	got := d.NextRun(now)

	if !got.After(now) {
		t.Fatalf("NextRun(%s) = %s, which is not in the future", now, got)
	}
	// Go normalises the non-existent local time forward rather than erroring.
	if got.Day() != 8 {
		t.Errorf("NextRun landed on day %d, want 8", got.Day())
	}
}

func TestNextRunAcrossDSTFallBack(t *testing.T) {
	d := mustNew(t, "01:30", "America/New_York")
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}

	// 01:30 occurs twice on 2026-11-01; either is acceptable for a daily backup,
	// but it must resolve to exactly one future instant.
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	got := d.NextRun(now)

	if !got.After(now) {
		t.Fatalf("NextRun(%s) = %s, which is not in the future", now, got)
	}
	if got.Day() != 1 || got.Month() != time.November {
		t.Errorf("NextRun = %s, want 2026-11-01", got)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)}
	d, err := New("23:00", "UTC", clock)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runs := 0

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx, nil, func(context.Context) {
			runs++
			if runs == 3 {
				cancel()
			}
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
	if runs < 3 {
		t.Errorf("ran %d times, want at least 3", runs)
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	for name, c := range map[string]struct{ at, zone string }{
		"hour out of range":   {"24:00", "UTC"},
		"minute out of range": {"23:60", "UTC"},
		"not a time":          {"midnight", "UTC"},
		"unknown zone":        {"23:00", "Mars/Olympus"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(c.at, c.zone, &fakeClock{}); err == nil {
				t.Errorf("New(%q, %q) succeeded, want an error", c.at, c.zone)
			}
		})
	}
}
