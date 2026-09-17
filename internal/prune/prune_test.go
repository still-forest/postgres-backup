package prune

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/still-forest/manor/services/postgres-backup/internal/objstore"
)

var now = time.Date(2026, 9, 17, 23, 30, 0, 0, time.UTC)

func defaultPolicy() Policy {
	return Policy{DailyDays: 30, MonthlyMonths: 12, MinKeep: 3}
}

// at builds an object for a backup taken d days before now.
func at(daysAgo int) objstore.Object {
	ts := now.AddDate(0, 0, -daysAgo)
	return objstore.Object{Key: objstore.BuildKey("manor_db", "db", ts), Size: 1024, LastModified: ts}
}

func keys(objs []objstore.Object) map[string]bool {
	m := make(map[string]bool, len(objs))
	for _, o := range objs {
		m[o.Key] = true
	}
	return m
}

func TestKeepsEverythingInsideDailyWindow(t *testing.T) {
	var objs []objstore.Object
	for d := range 30 {
		objs = append(objs, at(d))
	}

	got, err := Plan(objs, defaultPolicy(), now)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(got.Delete) != 0 {
		t.Errorf("deleted %d objects inside the daily window, want 0", len(got.Delete))
	}
	if len(got.Keep) != 30 {
		t.Errorf("kept %d, want 30", len(got.Keep))
	}
}

// The boundary is the rule most likely to be off by one, so pin both sides.
func TestDailyWindowBoundary(t *testing.T) {
	var objs []objstore.Object
	for d := range 200 {
		objs = append(objs, at(d))
	}

	p := defaultPolicy()
	p.AllowBulk = true
	got, err := Plan(objs, p, now)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	kept := keys(got.Keep)

	if !kept[at(29).Key] {
		t.Error("backup 29 days old was deleted; it is inside the 30-day window")
	}
	if !kept[at(30).Key] {
		t.Error("backup exactly 30 days old was deleted; the cutoff is inclusive")
	}
	if kept[at(31).Key] && kept[at(32).Key] {
		t.Error("consecutive backups past the daily window were both kept; only the newest per month should survive")
	}
}

func TestKeepsNewestPerMonthOutsideDailyWindow(t *testing.T) {
	var objs []objstore.Object
	for d := 31; d < 300; d++ {
		objs = append(objs, at(d))
	}

	got, err := Plan(objs, Policy{DailyDays: 30, MonthlyMonths: 12, MinKeep: 1, AllowBulk: true}, now)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	seen := map[string]int{}
	for _, o := range got.Keep {
		_, ts, ok := objstore.ParseKey(o.Key)
		if !ok {
			t.Fatalf("kept an unparseable key %q", o.Key)
		}
		seen[ts.Format("2006-01")]++
	}
	for month, n := range seen {
		if n != 1 {
			t.Errorf("month %s kept %d backups, want exactly 1", month, n)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no monthly backups retained")
	}
}

func TestDeletesBeyondMonthlyWindow(t *testing.T) {
	objs := []objstore.Object{at(0), at(1), at(2)}
	for d := 40; d < 800; d += 30 {
		objs = append(objs, at(d))
	}

	got, err := Plan(objs, Policy{DailyDays: 30, MonthlyMonths: 12, MinKeep: 1, AllowBulk: true}, now)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	cutoff := now.AddDate(0, -12, 0)
	for _, o := range got.Keep {
		if _, ts, ok := objstore.ParseKey(o.Key); ok && ts.Before(cutoff) {
			t.Errorf("kept %s, which is older than the 12-month window", o.Key)
		}
	}
	if len(got.Delete) == 0 {
		t.Fatal("nothing was deleted despite backups older than the monthly window")
	}
}

// Objects whose keys this service did not write must never be deletion
// candidates: the bucket holds output from the tool being replaced.
func TestUnknownKeysAreNeverDeleted(t *testing.T) {
	legacy := objstore.Object{
		Key:          "manor_db/2024/September/PG_manor_17-September-2024.dmp",
		LastModified: now.AddDate(-2, 0, 0),
	}
	objs := []objstore.Object{legacy, at(0), at(1), at(2), at(400)}

	got, err := Plan(objs, Policy{DailyDays: 30, MonthlyMonths: 12, MinKeep: 1}, now)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if keys(got.Delete)[legacy.Key] {
		t.Error("an unparseable legacy key was scheduled for deletion")
	}
	if !keys(got.Keep)[legacy.Key] {
		t.Error("an unparseable legacy key was not reported as kept")
	}
	if len(got.Unknown) != 1 {
		t.Errorf("Unknown = %d, want 1", len(got.Unknown))
	}
}

func TestRefusesEmptyListing(t *testing.T) {
	_, err := Plan(nil, defaultPolicy(), now)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

func TestRefusesWhenTooFewWouldSurvive(t *testing.T) {
	objs := []objstore.Object{at(400), at(500), at(600)}

	_, err := Plan(objs, Policy{DailyDays: 30, MonthlyMonths: 1, MinKeep: 3, AllowBulk: true}, now)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

func TestRefusesWhenPlanDeletesTooLargeAFraction(t *testing.T) {
	var objs []objstore.Object
	objs = append(objs, at(0), at(1))
	for d := 400; d < 460; d++ {
		objs = append(objs, at(d))
	}

	_, err := Plan(objs, defaultPolicy(), now)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

func TestRefusesInvalidPolicy(t *testing.T) {
	for name, p := range map[string]Policy{
		"zero daily":   {DailyDays: 0, MonthlyMonths: 12, MinKeep: 1},
		"zero monthly": {DailyDays: 30, MonthlyMonths: 0, MinKeep: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Plan([]objstore.Object{at(0)}, p, now); !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
		})
	}
}

func TestRefusalPlanIsEmpty(t *testing.T) {
	got, err := Plan(nil, defaultPolicy(), now)
	if err == nil {
		t.Fatal("expected refusal")
	}
	if len(got.Delete) != 0 || len(got.Keep) != 0 {
		t.Error("a refused plan returned a non-empty result; nothing may be acted on")
	}
}

func TestProtectRejectsCurrentRunsKey(t *testing.T) {
	old := at(400)
	got, err := Plan([]objstore.Object{at(0), at(1), at(2), old},
		Policy{DailyDays: 30, MonthlyMonths: 1, MinKeep: 1}, now)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := got.Protect(old.Key); !errors.Is(err, ErrRefused) {
		t.Fatalf("Protect = %v, want ErrRefused", err)
	}
	if err := got.Protect(at(0).Key); err != nil {
		t.Fatalf("Protect rejected a key that is not scheduled for deletion: %v", err)
	}
}

func TestSingleBackupSurvives(t *testing.T) {
	got, err := Plan([]objstore.Object{at(0)}, Policy{DailyDays: 30, MonthlyMonths: 12, MinKeep: 1}, now)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(got.Delete) != 0 {
		t.Error("the only backup was scheduled for deletion")
	}
}

// The steady-state guards would otherwise make a bucket with accumulated
// history impossible to prune, so the opt-in must actually lift them.
func TestAllowBulkLiftsSteadyStateGuards(t *testing.T) {
	var objs []objstore.Object
	objs = append(objs, at(0), at(1), at(2))
	for d := 400; d < 500; d++ {
		objs = append(objs, at(d))
	}

	if _, err := Plan(objs, defaultPolicy(), now); !errors.Is(err, ErrRefused) {
		t.Fatal("expected the steady-state guards to refuse a bulk prune")
	}

	p := defaultPolicy()
	p.AllowBulk = true
	got, err := Plan(objs, p, now)
	if err != nil {
		t.Fatalf("Plan with AllowBulk: %v", err)
	}
	if len(got.Delete) == 0 {
		t.Error("AllowBulk produced no deletions")
	}
}

func TestRefusesTooManyDeletesInOneRun(t *testing.T) {
	var objs []objstore.Object
	for d := range 200 {
		objs = append(objs, at(d))
	}

	// The fraction ceiling is lifted so that only the per-run cap can refuse,
	// which is what this test is about.
	p := Policy{DailyDays: 30, MonthlyMonths: 12, MinKeep: 1, MaxDeletes: 10, MaxDeleteFraction: 0.99}
	_, err := Plan(objs, p, now)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if !strings.Contains(err.Error(), "in one run") {
		t.Errorf("refusal cited the wrong guard: %v", err)
	}
}

// Steady state is the case that must never need an override: one new backup a
// day, one falling out of the window.
func TestSteadyStateDailyPruneIsAllowed(t *testing.T) {
	var objs []objstore.Object
	for d := range 45 {
		objs = append(objs, at(d))
	}

	got, err := Plan(objs, defaultPolicy(), now)
	if err != nil {
		t.Fatalf("a routine daily prune was refused: %v", err)
	}
	if len(got.Delete) == 0 {
		t.Error("expected the backups past the daily window to be consolidated")
	}
}

// A bucket younger than MinKeep has nothing to prune. Refusing there would fail
// every run until the floor was met, which is the first days after a deploy.
func TestYoungBucketBelowMinKeepIsNotRefused(t *testing.T) {
	for n := 1; n <= 3; n++ {
		var objs []objstore.Object
		for d := range n {
			objs = append(objs, at(d))
		}

		got, err := Plan(objs, defaultPolicy(), now)
		if err != nil {
			t.Fatalf("Plan refused a bucket holding %d backups with nothing to delete: %v", n, err)
		}
		if len(got.Delete) != 0 {
			t.Errorf("with %d backups, %d were scheduled for deletion", n, len(got.Delete))
		}
		if len(got.Keep) != n {
			t.Errorf("kept %d of %d", len(got.Keep), n)
		}
	}
}

// The floor must still hold once a plan genuinely proposes deletions.
func TestMinKeepStillGuardsRealDeletions(t *testing.T) {
	objs := []objstore.Object{at(0), at(400), at(500)}

	_, err := Plan(objs, Policy{DailyDays: 30, MonthlyMonths: 1, MinKeep: 3, AllowBulk: true}, now)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}
