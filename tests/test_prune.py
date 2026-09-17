"""Retention policy. This is the only code in the service that deletes."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta

import pytest

from postgres_backup import keys
from postgres_backup.prune import Plan, Policy, PruneRefused, plan
from postgres_backup.store import Object

NOW = datetime(2026, 9, 17, 23, 30, tzinfo=UTC)


def default_policy(**overrides: object) -> Policy:
    base = {"daily_days": 30, "monthly_months": 12, "min_keep": 3}
    return Policy(**{**base, **overrides})  # type: ignore[arg-type]


def at(days_ago: int) -> Object:
    """An object for a backup taken ``days_ago`` days before NOW."""
    ts = NOW - timedelta(days=days_ago)
    return Object(key=keys.build_key("manor_db", "db", ts), size=1024, last_modified=ts)


def key_set(objects: list[Object]) -> set[str]:
    return {o.key for o in objects}


def test_keeps_everything_inside_daily_window() -> None:
    objects = [at(d) for d in range(30)]

    result = plan(objects, default_policy(), NOW)

    assert result.delete == []
    assert len(result.keep) == 30


def test_daily_window_boundary() -> None:
    """The boundary is the rule most likely to be off by one, so pin both sides."""
    objects = [at(d) for d in range(200)]

    result = plan(objects, default_policy(allow_bulk=True), NOW)
    kept = key_set(result.keep)

    assert at(29).key in kept, "29 days old is inside the 30-day window"
    assert at(30).key in kept, "exactly 30 days old: the cutoff is inclusive"
    assert not (at(31).key in kept and at(32).key in kept), (
        "consecutive backups past the daily window were both kept; "
        "only the newest per month should survive"
    )


def test_keeps_newest_per_month_outside_daily_window() -> None:
    objects = [at(d) for d in range(31, 300)]

    result = plan(
        objects, Policy(daily_days=30, monthly_months=12, min_keep=1, allow_bulk=True), NOW
    )

    seen: dict[str, int] = {}
    for obj in result.keep:
        found = keys.parse_key(obj.key)
        assert found is not None, f"kept an unparseable key {obj.key!r}"
        month = found[1].strftime("%Y-%m")
        seen[month] = seen.get(month, 0) + 1

    assert seen, "no monthly backups retained"
    assert all(count == 1 for count in seen.values()), f"a month kept more than one: {seen}"


def test_deletes_beyond_monthly_window() -> None:
    objects = [at(0), at(1), at(2)]
    objects += [at(d) for d in range(40, 800, 30)]

    result = plan(
        objects, Policy(daily_days=30, monthly_months=12, min_keep=1, allow_bulk=True), NOW
    )

    cutoff = NOW.replace(year=NOW.year - 1)
    for obj in result.keep:
        found = keys.parse_key(obj.key)
        if found is not None:
            assert found[1] >= cutoff, f"kept {obj.key}, older than the 12-month window"
    assert result.delete, "nothing deleted despite backups older than the monthly window"


def test_unknown_keys_are_never_deleted() -> None:
    """The bucket holds output from the tool this service replaced."""
    legacy = Object(
        key="manor_db/2024/September/PG_manor_17-September-2024.dmp",
        size=100,
        last_modified=NOW.replace(year=2024),
    )
    objects = [legacy, at(0), at(1), at(2), at(400)]

    result = plan(objects, Policy(daily_days=30, monthly_months=12, min_keep=1), NOW)

    assert legacy.key not in key_set(result.delete)
    assert legacy.key in key_set(result.keep)
    assert len(result.unknown) == 1


def test_refuses_empty_listing() -> None:
    with pytest.raises(PruneRefused):
        plan([], default_policy(), NOW)


def test_refuses_when_too_few_would_survive() -> None:
    objects = [at(400), at(500), at(600)]

    with pytest.raises(PruneRefused, match="below the minimum"):
        plan(
            objects,
            Policy(daily_days=30, monthly_months=1, min_keep=3, allow_bulk=True),
            NOW,
        )


def test_refuses_when_plan_deletes_too_large_a_fraction() -> None:
    # Four recent backups plus twelve within a single older month: eleven of the
    # sixteen are consolidated away, which is over half but well under the
    # per-run cap, so only the fraction ceiling can refuse.
    objects = [at(d) for d in range(4)] + [at(d) for d in range(35, 47)]

    with pytest.raises(PruneRefused, match="ceiling"):
        plan(objects, default_policy(), NOW)


def test_refuses_too_many_deletes_in_one_run() -> None:
    objects = [at(d) for d in range(200)]

    # The fraction ceiling is lifted so only the per-run cap can refuse, which
    # is what this test is about.
    with pytest.raises(PruneRefused, match="in one run"):
        plan(
            objects,
            Policy(
                daily_days=30,
                monthly_months=12,
                min_keep=1,
                max_deletes=10,
                max_delete_fraction=0.99,
            ),
            NOW,
        )


@pytest.mark.parametrize(
    "policy",
    [
        Policy(daily_days=0, monthly_months=12, min_keep=1),
        Policy(daily_days=30, monthly_months=0, min_keep=1),
    ],
    ids=["zero-daily", "zero-monthly"],
)
def test_refuses_invalid_policy(policy: Policy) -> None:
    with pytest.raises(PruneRefused):
        plan([at(0)], policy, NOW)


def test_refusal_never_yields_a_partial_plan() -> None:
    """A refused prune must not hand back anything a caller could act on."""
    with pytest.raises(PruneRefused):
        plan([], default_policy(), NOW)


def test_protect_rejects_the_current_runs_key() -> None:
    old = at(400)
    result = plan(
        [at(0), at(1), at(2), old],
        Policy(daily_days=30, monthly_months=1, min_keep=1),
        NOW,
    )

    with pytest.raises(PruneRefused, match="written by this run"):
        result.protect(old.key)

    result.protect(at(0).key)  # not scheduled for deletion; must not raise


def test_single_backup_survives() -> None:
    result = plan([at(0)], Policy(daily_days=30, monthly_months=12, min_keep=1), NOW)
    assert result.delete == []


def test_allow_bulk_lifts_steady_state_guards() -> None:
    """Without the opt-in, a bucket with accumulated history is unprunable forever."""
    objects = [at(0), at(1), at(2)] + [at(d) for d in range(400, 500)]

    with pytest.raises(PruneRefused):
        plan(objects, default_policy(), NOW)

    result = plan(objects, default_policy(allow_bulk=True), NOW)
    assert result.delete, "allow_bulk produced no deletions"


@pytest.mark.parametrize("count", [1, 2, 3])
def test_young_bucket_below_min_keep_is_not_refused(count: int) -> None:
    """A fresh bucket has nothing to prune.

    Refusing here would fail every run for the first three nights after a deploy,
    which is exactly what happened before the floor was made conditional.
    """
    objects = [at(d) for d in range(count)]

    result = plan(objects, default_policy(), NOW)

    assert result.delete == []
    assert len(result.keep) == count


def test_min_keep_still_guards_real_deletions() -> None:
    objects = [at(0), at(400), at(500)]

    with pytest.raises(PruneRefused, match="below the minimum"):
        plan(
            objects,
            Policy(daily_days=30, monthly_months=1, min_keep=3, allow_bulk=True),
            NOW,
        )


def test_steady_state_daily_prune_is_allowed() -> None:
    """One new backup a day, one falling out of the window: must never need an override."""
    objects = [at(d) for d in range(45)]

    result = plan(objects, default_policy(), NOW)

    assert result.delete, "expected backups past the daily window to be consolidated"


def test_empty_plan_protect_is_a_noop() -> None:
    assert Plan().delete == []
    Plan().protect("anything")
