// Package prune decides which backup objects to delete under a
// daily-plus-monthly retention policy.
//
// Plan is a pure function over a listing: it performs no I/O, so every
// retention rule and every refusal below is exhaustively unit-testable.
package prune

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/still-forest/manor/services/postgres-backup/internal/objstore"
)

// Policy describes how much history to retain.
type Policy struct {
	// DailyDays keeps every backup taken within this many days.
	DailyDays int
	// MonthlyMonths keeps the newest backup of each month for this many months.
	MonthlyMonths int
	// MinKeep refuses the whole plan if fewer than this many backups would survive.
	MinKeep int
	// MaxDeleteFraction refuses the plan if it would delete more than this
	// fraction of the listing. Zero means the default of 0.5.
	MaxDeleteFraction float64
	// MaxDeletes refuses the plan if it would delete more than this many
	// objects in one run. Zero means the default of 50.
	MaxDeletes int
	// AllowBulk lifts MaxDeleteFraction and MaxDeletes.
	//
	// Those two guards assume a steady state, where a daily prune removes
	// roughly one object. The first prune of a bucket that has accumulated
	// unpruned history legitimately deletes most of it, so without an opt-in
	// the guards would refuse forever and the bucket would grow unbounded.
	AllowBulk bool
}

// ErrRefused is the class of every guard that aborts a plan with no deletions.
var ErrRefused = errors.New("prune refused")

// Result is the outcome of planning a prune.
type Result struct {
	Keep    []objstore.Object
	Delete  []objstore.Object
	Unknown []objstore.Object
}

// Plan partitions objs into keep and delete sets under p, as of now.
//
// Any guard that trips returns an error wrapping ErrRefused and an empty
// Result: a prune that cannot be reasoned about deletes nothing at all.
func Plan(objs []objstore.Object, p Policy, now time.Time) (Result, error) {
	if p.DailyDays < 1 || p.MonthlyMonths < 1 {
		return Result{}, fmt.Errorf("%w: policy must retain at least one day and one month", ErrRefused)
	}
	if len(objs) == 0 {
		// A listing taken immediately after a successful upload is never empty.
		// An empty one means the prefix is wrong, and pruning it is meaningless.
		return Result{}, fmt.Errorf("%w: listing returned no objects", ErrRefused)
	}

	frac := p.MaxDeleteFraction
	if frac == 0 {
		frac = 0.5
	}
	maxDeletes := p.MaxDeletes
	if maxDeletes == 0 {
		maxDeletes = 50
	}

	var res Result
	type parsed struct {
		obj objstore.Object
		ts  time.Time
	}
	var known []parsed

	for _, o := range objs {
		_, ts, ok := objstore.ParseKey(o.Key)
		if !ok {
			// Keys we cannot parse are never deletion candidates. This is what
			// keeps a prune from touching objects written by anything else,
			// including the tool this service replaces.
			res.Unknown = append(res.Unknown, o)
			continue
		}
		known = append(known, parsed{obj: o, ts: ts})
	}

	res.Keep = append(res.Keep, res.Unknown...)

	dailyCutoff := now.Add(-time.Duration(p.DailyDays) * 24 * time.Hour)
	monthlyCutoff := now.AddDate(0, -p.MonthlyMonths, 0)

	// Newest first, so the first entry seen for a month is the one to keep.
	sort.Slice(known, func(i, j int) bool { return known[i].ts.After(known[j].ts) })

	keptMonth := map[string]bool{}
	for _, k := range known {
		switch {
		case !k.ts.Before(dailyCutoff):
			res.Keep = append(res.Keep, k.obj)
		case k.ts.Before(monthlyCutoff):
			res.Delete = append(res.Delete, k.obj)
		default:
			month := k.ts.UTC().Format("2006-01")
			if keptMonth[month] {
				res.Delete = append(res.Delete, k.obj)
				continue
			}
			keptMonth[month] = true
			res.Keep = append(res.Keep, k.obj)
		}
	}

	// The guards below only make sense against a plan that actually deletes
	// something. A young bucket holding fewer than MinKeep backups has nothing
	// to prune, and refusing there would fail every run until the floor is met.
	if len(res.Delete) == 0 {
		sort.Slice(res.Keep, func(i, j int) bool { return res.Keep[i].Key < res.Keep[j].Key })
		return res, nil
	}

	if len(res.Keep) < p.MinKeep {
		return Result{}, fmt.Errorf("%w: plan would leave %d backups, below the minimum of %d",
			ErrRefused, len(res.Keep), p.MinKeep)
	}
	if !p.AllowBulk {
		if float64(len(res.Delete)) > frac*float64(len(objs)) {
			return Result{}, fmt.Errorf(
				"%w: plan would delete %d of %d objects, above the %.0f%% ceiling. "+
					"If this is the first prune of an unpruned bucket, set PRUNE_ALLOW_BULK=true for one run",
				ErrRefused, len(res.Delete), len(objs), frac*100)
		}
		if len(res.Delete) > maxDeletes {
			return Result{}, fmt.Errorf(
				"%w: plan would delete %d objects in one run, above the limit of %d. "+
					"If this is the first prune of an unpruned bucket, set PRUNE_ALLOW_BULK=true for one run",
				ErrRefused, len(res.Delete), maxDeletes)
		}
	}

	sort.Slice(res.Keep, func(i, j int) bool { return res.Keep[i].Key < res.Keep[j].Key })
	sort.Slice(res.Delete, func(i, j int) bool { return res.Delete[i].Key < res.Delete[j].Key })
	return res, nil
}

// Protect returns an error if key appears in the delete set. Callers pass the
// object they just uploaded: a plan that would delete the current run's own
// backup indicates a bug in the key scheme or the clock, and must not execute.
func (r Result) Protect(key string) error {
	for _, o := range r.Delete {
		if o.Key == key {
			return fmt.Errorf("%w: plan includes the backup written by this run (%s)", ErrRefused, key)
		}
	}
	return nil
}
