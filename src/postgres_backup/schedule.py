"""Run a job once a day at a fixed wall-clock time."""

from __future__ import annotations

import re
from collections.abc import Callable
from datetime import datetime, timedelta
from typing import Protocol
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

_HHMM = re.compile(r"^(\d{2}):(\d{2})$")


class Clock(Protocol):
    """Abstracts time so the scheduler can be tested without waiting."""

    def now(self) -> datetime: ...

    def sleep_until_or_stop(self, seconds: float) -> bool:
        """Wait up to ``seconds``. Returns True if asked to stop instead."""
        ...


class DailySchedule:
    """Fires at a fixed local time, once per day."""

    def __init__(self, at: str, zone: str, clock: Clock) -> None:
        self.hour, self.minute = parse_hhmm(at)
        self.zone = load_zone(zone)
        self.clock = clock

    def next_run(self, now: datetime) -> datetime:
        """Return the next occurrence of the configured time strictly after ``now``.

        Recomputed from the wall clock before every sleep rather than advancing
        by a fixed 24h, so scheduling error cannot accumulate across runs and a
        DST transition shifts the next run instead of permanently offsetting it.
        """
        local = now.astimezone(self.zone)
        candidate = local.replace(hour=self.hour, minute=self.minute, second=0, microsecond=0)
        if candidate <= local:
            candidate += timedelta(days=1)
        return candidate

    def run(
        self,
        job: Callable[[], None],
        before_sleep: Callable[[datetime], None] | None = None,
    ) -> None:
        """Invoke ``job`` at each scheduled time until asked to stop.

        Exceptions from ``job`` are the caller's to handle: a failed backup must
        not prevent tomorrow's attempt.
        """
        while True:
            upcoming = self.next_run(self.clock.now())
            if before_sleep is not None:
                before_sleep(upcoming)

            delay = (upcoming - self.clock.now()).total_seconds()
            if self.clock.sleep_until_or_stop(max(delay, 0)):
                return
            job()


def parse_hhmm(value: str) -> tuple[int, int]:
    """Parse a zero-padded ``HH:MM`` string."""
    m = _HHMM.match(value)
    if m is None:
        raise ValueError(f"invalid time {value!r}, want zero-padded HH:MM")
    hour, minute = int(m[1]), int(m[2])
    if hour > 23 or minute > 59:
        raise ValueError(f"invalid time {value!r}, want HH:MM within a day")
    return hour, minute


def load_zone(name: str) -> ZoneInfo:
    """Load an IANA timezone, reporting a missing database clearly."""
    try:
        return ZoneInfo(name)
    except ZoneInfoNotFoundError as exc:
        raise ValueError(f"unknown timezone {name!r}: {exc}") from exc
