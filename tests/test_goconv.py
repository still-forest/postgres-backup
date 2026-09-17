"""Go-compatible value parsing.

These env vars are already set in production by the Go build, so the literals it
accepted must keep working.
"""

from __future__ import annotations

import pytest

from postgres_backup.goconv import format_duration, parse_bool, parse_duration


@pytest.mark.parametrize(
    ("value", "expected"),
    [
        ("1", True),
        ("t", True),
        ("T", True),
        ("true", True),
        ("TRUE", True),
        ("True", True),
        ("0", False),
        ("f", False),
        ("F", False),
        ("false", False),
        ("FALSE", False),
        ("False", False),
    ],
)
def test_parse_bool_accepts_go_literals(value: str, expected: bool) -> None:
    assert parse_bool(value) is expected


@pytest.mark.parametrize("value", ["yes", "no", "on", "off", "y", "n", "", "2", "maybe"])
def test_parse_bool_rejects_everything_else(value: str) -> None:
    assert parse_bool(value) is None


@pytest.mark.parametrize(
    ("value", "seconds"),
    [
        ("26h", 93600.0),
        ("2h", 7200.0),
        ("1.5h", 5400.0),
        ("90m", 5400.0),
        ("300ms", 0.3),
        ("30s", 30.0),
        ("2h45m", 9900.0),
        ("2h0m0s", 7200.0),
        ("1h30m15s", 5415.0),
        ("500us", 0.0005),
        ("1000ns", 1e-06),
        ("-5m", -300.0),
    ],
)
def test_parse_duration_accepts_go_literals(value: str, seconds: float) -> None:
    result = parse_duration(value)
    assert result is not None
    assert result == pytest.approx(seconds)


@pytest.mark.parametrize(
    "value",
    ["", "26", "1d", "abc", "h", "2x", "one day", "2 h", "--5m"],
    ids=[
        "empty",
        "bare-number",
        "days-not-a-go-unit",
        "letters",
        "unit-without-amount",
        "unknown-unit",
        "prose",
        "internal-space",
        "double-sign",
    ],
)
def test_parse_duration_rejects_everything_else(value: str) -> None:
    assert parse_duration(value) is None


def test_parse_duration_accepts_the_micro_sign_and_mu() -> None:
    """Go accepts both U+00B5 and U+03BC for microseconds."""
    assert parse_duration("5µs") == pytest.approx(5e-06)
    assert parse_duration("5μs") == pytest.approx(5e-06)


@pytest.mark.parametrize(
    ("seconds", "expected"),
    [
        (0, "0s"),
        (5, "5s"),
        (59, "59s"),
        (60, "1m0s"),
        (90, "1m30s"),
        (3600, "1h0m0s"),
        (7245.6, "2h0m46s"),
        (-90, "-1m30s"),
    ],
)
def test_format_duration_matches_go(seconds: float, expected: str) -> None:
    """This text appears in Healthchecks failure bodies."""
    assert format_duration(seconds) == expected
