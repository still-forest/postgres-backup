"""Go-compatible parsing and formatting for values that cross the config boundary.

The service this ports from was written in Go, and its environment variables are
already set in production. ``MAX_BACKUP_AGE=2h45m`` and ``PRUNE_ENABLED=T`` are
both valid Go and must keep working, so these helpers reproduce
``time.ParseDuration``, ``strconv.ParseBool`` and ``Duration.String`` rather than
approximating them.
"""

from __future__ import annotations

import re

#: strconv.ParseBool accepts exactly these, case-sensitively for the words but
#: Go also accepts the capitalised forms; lowercasing first covers all of them.
_TRUE = frozenset({"1", "t", "true"})
_FALSE = frozenset({"0", "f", "false"})

_UNIT_SECONDS = {
    "ns": 1e-9,
    "us": 1e-6,
    "µs": 1e-6,  # U+00B5 MICRO SIGN, which Go accepts
    "μs": 1e-6,  # U+03BC GREEK SMALL LETTER MU, which Go also accepts
    "ms": 1e-3,
    "s": 1.0,
    "m": 60.0,
    "h": 3600.0,
}

_TERM_RE = re.compile(r"(\d+(?:\.\d+)?)(ns|us|µs|μs|ms|s|m|h)")
_DURATION_RE = re.compile(r"^[+-]?(?:\d+(?:\.\d+)?(?:ns|us|µs|μs|ms|s|m|h))+$")


def parse_bool(value: str) -> bool | None:
    """Parse a Go ``strconv.ParseBool`` literal. Returns None if invalid."""
    lowered = value.strip().lower()
    if lowered in _TRUE:
        return True
    if lowered in _FALSE:
        return False
    return None


def parse_duration(value: str) -> float | None:
    """Parse a Go ``time.ParseDuration`` string into seconds.

    Handles compound durations such as ``2h45m`` and every unit Go accepts.
    Returns None if invalid. A bare number is invalid in Go, and is here too.
    """
    text = value.strip()
    if not _DURATION_RE.match(text):
        return None

    negative = text.startswith("-")
    body = text.lstrip("+-")

    total = 0.0
    consumed = 0
    for m in _TERM_RE.finditer(body):
        if m.start() != consumed:
            return None
        total += float(m[1]) * _UNIT_SECONDS[m[2]]
        consumed = m.end()
    if consumed != len(body):
        return None

    return -total if negative else total


def format_duration(seconds: float) -> str:
    """Render seconds the way Go's ``Duration.Round(time.Second).String()`` does.

    This text appears in Healthchecks failure bodies, so keeping it identical
    keeps the alerts identical.
    """
    total = round(seconds)
    if total == 0:
        return "0s"

    sign = "-" if total < 0 else ""
    total = abs(total)

    hours, remainder = divmod(total, 3600)
    minutes, secs = divmod(remainder, 60)

    if hours:
        return f"{sign}{hours}h{minutes}m{secs}s"
    if minutes:
        return f"{sign}{minutes}m{secs}s"
    return f"{sign}{secs}s"
