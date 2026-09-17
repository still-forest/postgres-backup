"""The object key scheme. Keys are the authority for a backup's age."""

from __future__ import annotations

from datetime import UTC, datetime
from zoneinfo import ZoneInfo

import pytest

from postgres_backup.keys import build_key, globals_key, parse_key


def test_build_key_round_trip() -> None:
    ts = datetime(2026, 9, 17, 23, 0, 14, tzinfo=UTC)
    key = build_key("manor_db", "manor_production", ts)

    assert key == "manor_db/manor_production/2026/09/manor_production-20260917T230014Z.dump"

    found = parse_key(key)
    assert found is not None
    assert found == ("manor_production", ts)


def test_build_key_normalises_to_utc() -> None:
    local = datetime(2026, 9, 17, 19, 0, 14, tzinfo=ZoneInfo("America/New_York"))

    found = parse_key(build_key("p", "db", local))

    assert found is not None
    assert found[1] == local.astimezone(UTC)


def test_build_key_without_prefix() -> None:
    key = build_key("", "db", datetime(2026, 1, 2, 3, 4, 5, tzinfo=UTC))

    assert key == "db/2026/01/db-20260102T030405Z.dump"
    assert parse_key(key) is not None


def test_parse_key_handles_hyphenated_database() -> None:
    """The stamp is anchored at the end precisely so this works."""
    ts = datetime(2026, 9, 17, 23, 0, 14, tzinfo=UTC)

    found = parse_key(build_key("p", "manor-prod-eu", ts))

    assert found == ("manor-prod-eu", ts)


@pytest.mark.parametrize(
    "key",
    [
        pytest.param(
            "manor_db/2026/September/PG_manor_17-September-2026.dmp", id="old-kartoza-layout"
        ),
        pytest.param("manor_db/db/2026/09/db-20260917T230014Z.globals.sql", id="globals-sidecar"),
        pytest.param("manor_db/db/2026/09/db-20260917T230014Z.sql", id="wrong-extension"),
        pytest.param("manor_db/db/2026/09/db.dump", id="missing-timestamp"),
        pytest.param("manor_db/db/2026/09/db-20261317T230014Z.dump", id="month-13"),
        pytest.param("manor_db/db/2026/10/db-20260917T230014Z.dump", id="path-month-disagrees"),
        pytest.param("manor_db/db/2025/09/db-20260917T230014Z.dump", id="path-year-disagrees"),
        pytest.param("manor_db/other/2026/09/db-20260917T230014Z.dump", id="db-name-disagrees"),
        pytest.param("", id="empty"),
    ],
)
def test_parse_key_rejects(key: str) -> None:
    assert parse_key(key) is None


def test_globals_key() -> None:
    ts = datetime(2026, 9, 17, 23, 0, 14, tzinfo=UTC)

    assert (
        globals_key("manor_db", "db", ts) == "manor_db/db/2026/09/db-20260917T230014Z.globals.sql"
    )


def test_preflight_probe_key_is_deliberately_unparseable() -> None:
    """A leaked probe object must be classified unknown and kept, not pruned."""
    probe = build_key("manor_db", ".preflight", datetime(2026, 9, 17, 23, 0, tzinfo=UTC)) + ".probe"

    assert parse_key(probe) is None
