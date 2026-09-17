"""Retention policy: decide which backup objects to delete.

``plan`` is a pure function over a listing. It performs no I/O, so every
retention rule and every refusal below is exhaustively unit-testable.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta

from .keys import parse_key
from .store import Object


class PruneRefused(Exception):
    """A guard tripped. Nothing is deleted when this is raised."""


@dataclass(frozen=True)
class Policy:
    """How much history to retain."""

    daily_days: int = 30
    monthly_months: int = 12
    #: Refuses a plan that would leave fewer than this many backups, but only
    #: when the plan actually deletes something.
    min_keep: int = 3
    #: Refuses a plan deleting more than this many objects in one run.
    max_deletes: int = 50
    #: Refuses a plan deleting more than this fraction of the listing.
    max_delete_fraction: float = 0.5
    #: Lifts ``max_deletes`` and ``max_delete_fraction``.
    #:
    #: Those two guards assume a steady state, where a daily prune removes
    #: roughly one object. The first prune of a bucket that has accumulated
    #: unpruned history legitimately deletes most of it, so without an opt-in
    #: the guards would refuse forever and the bucket would grow unbounded.
    allow_bulk: bool = False


@dataclass
class Plan:
    """The outcome of planning a prune."""

    keep: list[Object] = field(default_factory=list)
    delete: list[Object] = field(default_factory=list)
    unknown: list[Object] = field(default_factory=list)

    def protect(self, key: str) -> None:
        """Raise if ``key`` is scheduled for deletion.

        Callers pass the object they just uploaded: a plan that would delete the
        current run's own backup indicates a bug in the key scheme or the clock,
        and must not execute.
        """
        if any(o.key == key for o in self.delete):
            raise PruneRefused(
                f"prune refused: plan includes the backup written by this run ({key})"
            )


_BULK_HINT = (
    "If this is the first prune of an unpruned bucket, set PRUNE_ALLOW_BULK=true for one run"
)


def plan(objects: list[Object], policy: Policy, now: datetime) -> Plan:
    """Partition ``objects`` into keep and delete sets under ``policy``.

    Raises PruneRefused, having selected nothing for deletion, if any guard
    trips: a prune that cannot be reasoned about deletes nothing at all.
    """
    if policy.daily_days < 1 or policy.monthly_months < 1:
        raise PruneRefused("prune refused: policy must retain at least one day and one month")

    if not objects:
        # A listing taken immediately after a successful upload is never empty.
        # An empty one means the prefix is wrong, and pruning it is meaningless.
        raise PruneRefused("prune refused: listing returned no objects")

    result = Plan()
    parsed: list[tuple[Object, datetime]] = []

    for obj in objects:
        found = parse_key(obj.key)
        if found is None:
            # Keys we cannot parse are never deletion candidates. This is what
            # keeps a prune from touching objects written by anything else,
            # including the tool this service replaces.
            result.unknown.append(obj)
            continue
        parsed.append((obj, found[1]))

    result.keep.extend(result.unknown)

    daily_cutoff = now - timedelta(days=policy.daily_days)
    monthly_cutoff = _months_ago(now, policy.monthly_months)

    # Newest first, so the first entry seen for a month is the one to keep.
    parsed.sort(key=lambda pair: pair[1], reverse=True)

    kept_month: set[str] = set()
    for obj, ts in parsed:
        if ts >= daily_cutoff:
            result.keep.append(obj)
        elif ts < monthly_cutoff:
            result.delete.append(obj)
        else:
            month = ts.strftime("%Y-%m")
            if month in kept_month:
                result.delete.append(obj)
            else:
                kept_month.add(month)
                result.keep.append(obj)

    # The guards below only make sense against a plan that actually deletes
    # something. A young bucket holding fewer than min_keep backups has nothing
    # to prune, and refusing there would fail every run until the floor is met.
    if not result.delete:
        result.keep.sort(key=lambda o: o.key)
        return result

    if len(result.keep) < policy.min_keep:
        raise PruneRefused(
            f"prune refused: plan would leave {len(result.keep)} backups, "
            f"below the minimum of {policy.min_keep}"
        )

    if not policy.allow_bulk:
        if len(result.delete) > policy.max_delete_fraction * len(objects):
            raise PruneRefused(
                f"prune refused: plan would delete {len(result.delete)} of {len(objects)} "
                f"objects, above the {policy.max_delete_fraction:.0%} ceiling. {_BULK_HINT}"
            )
        if len(result.delete) > policy.max_deletes:
            raise PruneRefused(
                f"prune refused: plan would delete {len(result.delete)} objects in one run, "
                f"above the limit of {policy.max_deletes}. {_BULK_HINT}"
            )

    result.keep.sort(key=lambda o: o.key)
    result.delete.sort(key=lambda o: o.key)
    return result


def _months_ago(when: datetime, months: int) -> datetime:
    """Return ``when`` shifted back by whole months, clamping short months."""
    year, month = when.year, when.month - months
    while month < 1:
        month += 12
        year -= 1

    day = when.day
    while day > 0:
        try:
            return when.replace(year=year, month=month, day=day)
        except ValueError:
            day -= 1
    raise AssertionError("unreachable: every month has a first day")
