// Package schedule runs a job once a day at a fixed wall-clock time.
package schedule

import (
	"context"
	"fmt"
	"time"
)

// Clock abstracts time so the scheduler can be tested without waiting.
type Clock interface {
	Now() time.Time
	// NewTimer returns a channel that fires after d, and a stop function.
	NewTimer(d time.Duration) (<-chan time.Time, func() bool)
}

// RealClock is a Clock backed by the time package.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// Daily fires at HourMin in Loc, once per day.
type Daily struct {
	Hour  int
	Min   int
	Loc   *time.Location
	Clock Clock
}

// New returns a Daily parsed from an "HH:MM" string and an IANA zone name.
func New(at, zone string, clock Clock) (*Daily, error) {
	var h, m int
	if _, err := fmt.Sscanf(at, "%d:%d", &h, &m); err != nil {
		return nil, fmt.Errorf("invalid time %q, want HH:MM: %w", at, err)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return nil, fmt.Errorf("invalid time %q, want HH:MM within a day", at)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q: %w", zone, err)
	}
	if clock == nil {
		clock = RealClock{}
	}
	return &Daily{Hour: h, Min: m, Loc: loc, Clock: clock}, nil
}

// NextRun returns the next occurrence of the configured time strictly after now.
//
// It is recomputed from the wall clock before every sleep rather than advancing
// by a fixed 24h, so scheduling error cannot accumulate across runs and a
// DST transition shifts the next run instead of permanently offsetting it.
func (d *Daily) NextRun(now time.Time) time.Time {
	n := now.In(d.Loc)
	next := time.Date(n.Year(), n.Month(), n.Day(), d.Hour, d.Min, 0, 0, d.Loc)
	if !next.After(n) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// Run invokes job at each scheduled time until ctx is cancelled.
//
// beforeSleep, if set, is called with the upcoming run time so the caller can
// log it. Errors from job do not stop the loop: a failed backup must not
// prevent tomorrow's attempt.
func (d *Daily) Run(ctx context.Context, beforeSleep func(next time.Time), job func(context.Context)) {
	for {
		next := d.NextRun(d.Clock.Now())
		if beforeSleep != nil {
			beforeSleep(next)
		}

		ch, stop := d.Clock.NewTimer(next.Sub(d.Clock.Now()))
		select {
		case <-ctx.Done():
			stop()
			return
		case <-ch:
			job(ctx)
		}
	}
}
