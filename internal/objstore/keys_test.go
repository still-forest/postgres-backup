package objstore

import (
	"testing"
	"time"
)

func TestBuildKeyRoundTrip(t *testing.T) {
	ts := time.Date(2026, 9, 17, 23, 0, 14, 0, time.UTC)
	key := BuildKey("manor_db", "manor_production", ts)

	const want = "manor_db/manor_production/2026/09/manor_production-20260917T230014Z.dump"
	if key != want {
		t.Fatalf("BuildKey = %q, want %q", key, want)
	}

	db, got, ok := ParseKey(key)
	if !ok {
		t.Fatalf("ParseKey(%q) failed", key)
	}
	if db != "manor_production" {
		t.Errorf("db = %q, want manor_production", db)
	}
	if !got.Equal(ts) {
		t.Errorf("ts = %s, want %s", got, ts)
	}
}

func TestBuildKeyNormalisesToUTC(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	local := time.Date(2026, 9, 17, 19, 0, 14, 0, ny) // 23:00:14 UTC

	_, ts, ok := ParseKey(BuildKey("p", "db", local))
	if !ok {
		t.Fatal("ParseKey failed")
	}
	if want := local.UTC(); !ts.Equal(want) {
		t.Errorf("ts = %s, want %s", ts, want)
	}
}

func TestBuildKeyWithoutPrefix(t *testing.T) {
	key := BuildKey("", "db", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if want := "db/2026/01/db-20260102T030405Z.dump"; key != want {
		t.Fatalf("BuildKey = %q, want %q", key, want)
	}
	if _, _, ok := ParseKey(key); !ok {
		t.Fatal("ParseKey failed on unprefixed key")
	}
}

func TestParseKeyRejects(t *testing.T) {
	cases := map[string]string{
		"old kartoza flat layout": "manor_db/2026/September/PG_manor_17-September-2026.dmp",
		"globals sidecar":         "manor_db/db/2026/09/db-20260917T230014Z.globals.sql",
		"wrong extension":         "manor_db/db/2026/09/db-20260917T230014Z.sql",
		"missing timestamp":       "manor_db/db/2026/09/db.dump",
		"bad timestamp":           "manor_db/db/2026/09/db-20261317T230014Z.dump",
		"path month disagrees":    "manor_db/db/2026/10/db-20260917T230014Z.dump",
		"path year disagrees":     "manor_db/db/2025/09/db-20260917T230014Z.dump",
		"db name disagrees":       "manor_db/other/2026/09/db-20260917T230014Z.dump",
		"empty":                   "",
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, ok := ParseKey(key); ok {
				t.Errorf("ParseKey(%q) succeeded, want rejection", key)
			}
		})
	}
}

// Database names may contain hyphens, so the timestamp must be anchored to the
// end of the filename rather than found by splitting on the first hyphen.
func TestParseKeyHandlesHyphenatedDatabase(t *testing.T) {
	ts := time.Date(2026, 9, 17, 23, 0, 14, 0, time.UTC)
	db, got, ok := ParseKey(BuildKey("p", "manor-prod-eu", ts))
	if !ok {
		t.Fatal("ParseKey failed")
	}
	if db != "manor-prod-eu" {
		t.Errorf("db = %q, want manor-prod-eu", db)
	}
	if !got.Equal(ts) {
		t.Errorf("ts = %s, want %s", got, ts)
	}
}

func TestGlobalsKey(t *testing.T) {
	ts := time.Date(2026, 9, 17, 23, 0, 14, 0, time.UTC)
	got := GlobalsKey("manor_db", "db", ts)
	const want = "manor_db/db/2026/09/db-20260917T230014Z.globals.sql"
	if got != want {
		t.Fatalf("GlobalsKey = %q, want %q", got, want)
	}
}
