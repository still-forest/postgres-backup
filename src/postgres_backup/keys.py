"""Object key scheme for backup artifacts.

The timestamp encoded in a key is the authority for a backup's age, never the
object's LastModified, which a copy or re-upload would silently reset.
"""

from __future__ import annotations

import re
from datetime import UTC, datetime

DUMP_EXT = ".dump"
GLOBALS_SUFFIX = ".globals.sql"

_STAMP_FORMAT = "%Y%m%dT%H%M%SZ"

# Keys look like:
#
#     manor_db/manor_production/2026/09/manor_production-20260917T230014Z.dump
#
# The timestamp is anchored to the end of the filename rather than found by
# splitting on the first hyphen, because database names may contain hyphens.
_KEY_RE = re.compile(
    r"^(?:(?P<prefix>.+)/)?"
    r"(?P<dir_db>[^/]+)/"
    r"(?P<year>\d{4})/"
    r"(?P<month>\d{2})/"
    r"(?P<file_db>[^/]+)-(?P<stamp>\d{8}T\d{6}Z)" + re.escape(DUMP_EXT) + r"$"
)


def build_key(prefix: str, db: str, ts: datetime) -> str:
    """Return the canonical key for a dump of ``db`` taken at ``ts``."""
    u = ts.astimezone(UTC)
    name = f"{db}-{u.strftime(_STAMP_FORMAT)}{DUMP_EXT}"
    parts = [p for p in (prefix.strip("/"), db, u.strftime("%Y"), u.strftime("%m"), name) if p]
    return "/".join(parts)


def globals_key(prefix: str, db: str, ts: datetime) -> str:
    """Return the key for the cluster-wide roles dump accompanying a backup.

    Roles and privileges are not carried in a per-database dump, so a restore
    without this produces grants referencing roles that do not exist.
    """
    return build_key(prefix, db, ts).removesuffix(DUMP_EXT) + GLOBALS_SUFFIX


def parse_key(key: str) -> tuple[str, datetime] | None:
    """Extract the database name and timestamp encoded in ``key``.

    Returns None for anything this service did not write. Callers treat that as
    "never a deletion candidate": the bucket also holds objects from the tool
    this service replaced.
    """
    m = _KEY_RE.match(key)
    if m is None:
        return None

    if m["dir_db"] != m["file_db"]:
        return None

    try:
        ts = datetime.strptime(m["stamp"], _STAMP_FORMAT).replace(tzinfo=UTC)
    except ValueError:
        return None

    if ts.strftime("%Y") != m["year"] or ts.strftime("%m") != m["month"]:
        return None

    return m["file_db"], ts
