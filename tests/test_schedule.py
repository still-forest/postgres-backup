"""Daily scheduling. Drift here silently moves the backup window."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from zoneinfo import ZoneInfo

import pytest

from postgres_backup.schedule import DailySchedule, load_zone, parse_hhmm

from .fakes import FakeClock


def schedule(at: str = "23:00", zone: str = "UTC", clock: FakeClock | None = None) -> DailySchedule:
    return DailySchedule(at, zone, clock or FakeClock())


def test_next_run_later_today() -> None:
    now = datetime(2026, 9, 17, 10, 0, tzinfo=UTC)

    assert schedule().next_run(now) == datetime(2026, 9, 17, 23, 0, tzinfo=UTC)


@pytest.mark.parametrize(
    "now",
    [
        pytest.param(datetime(2026, 9, 17, 23, 0, 1, tzinfo=UTC), id="just-after"),
        pytest.param(datetime(2026, 9, 17, 23, 0, 0, tzinfo=UTC), id="exactly-at"),
        pytest.param(datetime(2026, 9, 17, 23, 59, 59, tzinfo=UTC), id="late-at-night"),
    ],
)
def test_next_run_rolls_to_tomorrow(now: datetime) -> None:
    assert schedule().next_run(now) == datetime(2026, 9, 18, 23, 0, tzinfo=UTC)


@pytest.mark.parametrize(
    ("now", "expected"),
    [
        (
            datetime(2026, 9, 30, 23, 30, tzinfo=UTC),
            datetime(2026, 10, 1, 23, 0, tzinfo=UTC),
        ),
        (
            datetime(2026, 12, 31, 23, 30, tzinfo=UTC),
            datetime(2027, 1, 1, 23, 0, tzinfo=UTC),
        ),
        (
            datetime(2028, 2, 28, 23, 30, tzinfo=UTC),
            datetime(2028, 2, 29, 23, 0, tzinfo=UTC),
        ),
    ],
    ids=["month-end", "year-end", "leap-day"],
)
def test_next_run_crosses_month_and_year(now: datetime, expected: datetime) -> None:
    assert schedule().next_run(now) == expected


def test_schedule_does_not_drift() -> None:
    """A fixed 24h tick would slide by however long each backup takes."""
    clock = FakeClock(current=datetime(2026, 9, 17, 10, 0, tzinfo=UTC))
    sched = schedule(clock=clock)

    for _ in range(40):
        upcoming = sched.next_run(clock.now())
        assert (upcoming.hour, upcoming.minute) == (23, 0), (
            f"scheduled run drifted to {upcoming.isoformat()}"
        )
        # Simulate the run itself taking 47 minutes.
        clock.current = upcoming + timedelta(minutes=47)


def test_next_run_across_dst_spring_forward() -> None:
    """02:30 does not exist on 2026-03-08 in New York."""
    ny = ZoneInfo("America/New_York")
    sched = schedule(at="02:30", zone="America/New_York")
    now = datetime(2026, 3, 7, 12, 0, tzinfo=ny)

    upcoming = sched.next_run(now)

    assert upcoming > now
    assert upcoming.astimezone(ny).day == 8


def test_next_run_across_dst_fall_back() -> None:
    """01:30 occurs twice on 2026-11-01; it must resolve to exactly one instant."""
    ny = ZoneInfo("America/New_York")
    sched = schedule(at="01:30", zone="America/New_York")
    now = datetime(2026, 10, 31, 12, 0, tzinfo=ny)

    upcoming = sched.next_run(now)

    assert upcoming > now
    local = upcoming.astimezone(ny)
    assert (local.month, local.day) == (11, 1)


def test_run_stops_when_the_clock_reports_shutdown() -> None:
    clock = FakeClock(current=datetime(2026, 9, 17, 10, 0, tzinfo=UTC), stop_after=3)
    sched = schedule(clock=clock)
    runs = 0

    def job() -> None:
        nonlocal runs
        runs += 1

    sched.run(job=job)

    assert runs == 2, "the job must not run on the iteration that was interrupted"
    assert len(clock.slept) == 3


@pytest.mark.parametrize(
    ("at", "zone"),
    [
        pytest.param("24:00", "UTC", id="hour-out-of-range"),
        pytest.param("23:60", "UTC", id="minute-out-of-range"),
        pytest.param("midnight", "UTC", id="not-a-time"),
        pytest.param("9:00", "UTC", id="not-zero-padded"),
        pytest.param("23:00", "Mars/Olympus", id="unknown-zone"),
    ],
)
def test_construction_rejects_bad_input(at: str, zone: str) -> None:
    with pytest.raises(ValueError):
        DailySchedule(at, zone, FakeClock())


def test_parse_hhmm_requires_zero_padding() -> None:
    assert parse_hhmm("09:05") == (9, 5)
    with pytest.raises(ValueError):
        parse_hhmm("9:05")


def test_load_zone_accepts_a_real_zone() -> None:
    assert load_zone("America/New_York") is not None
