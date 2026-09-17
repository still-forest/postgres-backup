package objstore

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// DumpExt is the suffix every backup artifact carries.
const DumpExt = ".dump"

const stampLayout = "20060102T150405Z"

// Keys look like:
//
//	manor_db/manor_production/2026/09/manor_production-20260917T230014Z.dump
//
// The timestamp is anchored to the end of the filename rather than split on the
// first hyphen, because database names may themselves contain hyphens.
var keyRE = regexp.MustCompile(`^(?:(.+)/)?([^/]+)/(\d{4})/(\d{2})/([^/]+)-(\d{8}T\d{6}Z)\` + DumpExt + `$`)

// BuildKey returns the canonical key for a dump of db taken at ts.
func BuildKey(prefix, db string, ts time.Time) string {
	u := ts.UTC()
	name := fmt.Sprintf("%s-%s%s", db, u.Format(stampLayout), DumpExt)
	return path.Join(prefix, db, u.Format("2006"), u.Format("01"), name)
}

// GlobalsKey returns the key for the cluster-wide roles dump accompanying a
// backup. Roles and passwords are not carried in a per-database dump, so a
// restore needs this alongside the dump itself.
func GlobalsKey(prefix, db string, ts time.Time) string {
	return strings.TrimSuffix(BuildKey(prefix, db, ts), DumpExt) + ".globals.sql"
}

// ParseKey extracts the database name and timestamp encoded in a key.
//
// The key is the authority for a backup's age, never the object's
// LastModified, which a copy or re-upload would silently reset.
func ParseKey(key string) (db string, ts time.Time, ok bool) {
	m := keyRE.FindStringSubmatch(key)
	if m == nil {
		return "", time.Time{}, false
	}

	dirDB, year, month, fileDB, stamp := m[2], m[3], m[4], m[5], m[6]
	if dirDB != fileDB {
		return "", time.Time{}, false
	}

	t, err := time.ParseInLocation(stampLayout, stamp, time.UTC)
	if err != nil {
		return "", time.Time{}, false
	}
	if t.Format("2006") != year || t.Format("01") != month {
		return "", time.Time{}, false
	}
	return fileDB, t, true
}
