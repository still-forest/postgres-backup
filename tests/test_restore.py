"""The restore guard, and the CLI behaviours around it."""

from __future__ import annotations

from pathlib import Path

import pytest

from postgres_backup.cli import staleness
from postgres_backup.restore import (
    RestoreTarget,
    check_not_source,
    human_bytes,
    load_restore_target,
)

from .conftest import make_config


def source_cfg(tmp_path: Path) -> object:
    from dataclasses import replace

    cfg = make_config(tmp_path)
    return replace(
        cfg,
        postgres=replace(
            cfg.postgres,
            host="db.internal",
            databases=["manor_production", "manor_analytics"],
        ),
    )


def test_restore_requires_its_own_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    """The POSTGRES_* settings must have no path to pg_restore."""
    monkeypatch.setenv("POSTGRES_HOST", "db.internal")
    monkeypatch.setenv("POSTGRES_USER", "postgres")
    monkeypatch.setenv("POSTGRES_PASS", "productionpw")
    monkeypatch.setenv("POSTGRES_DB", "manor_production")

    with pytest.raises(ValueError) as excinfo:
        load_restore_target()

    message = str(excinfo.value)
    for expected in ("RESTORE_HOST", "RESTORE_USER", "RESTORE_PASS", "RESTORE_DB"):
        assert expected in message


def test_load_restore_target_reads_restore_vars(monkeypatch: pytest.MonkeyPatch) -> None:
    for name, value in {
        "RESTORE_HOST": "scratch.internal",
        "RESTORE_USER": "restorer",
        "RESTORE_PASS": "pw",
        "RESTORE_DB": "scratch",
        "RESTORE_PORT": "5433",
    }.items():
        monkeypatch.setenv(name, value)

    target = load_restore_target()

    assert target.host == "scratch.internal"
    assert target.database == "scratch"
    assert target.port == 5433


def test_load_restore_target_defaults_port(monkeypatch: pytest.MonkeyPatch) -> None:
    for name, value in {
        "RESTORE_HOST": "h",
        "RESTORE_USER": "u",
        "RESTORE_PASS": "p",
        "RESTORE_DB": "d",
    }.items():
        monkeypatch.setenv(name, value)

    assert load_restore_target().port == 5432


def test_load_restore_target_rejects_a_bad_port(monkeypatch: pytest.MonkeyPatch) -> None:
    for name, value in {
        "RESTORE_HOST": "h",
        "RESTORE_USER": "u",
        "RESTORE_PASS": "p",
        "RESTORE_DB": "d",
        "RESTORE_PORT": "not-a-port",
    }.items():
        monkeypatch.setenv(name, value)

    with pytest.raises(ValueError, match="RESTORE_PORT"):
        load_restore_target()


@pytest.mark.parametrize("database", ["manor_production", "manor_analytics"])
def test_refuses_to_restore_over_a_backed_up_database(tmp_path: Path, database: str) -> None:
    target = RestoreTarget(host="db.internal", port=5432, user="u", password="p", database=database)

    with pytest.raises(ValueError, match="--force-production"):
        check_not_source(target, source_cfg(tmp_path), overridden=False)  # type: ignore[arg-type]


@pytest.mark.parametrize(
    ("host", "database"),
    [("scratch.internal", "manor_production"), ("db.internal", "manor_scratch")],
    ids=["different-host", "different-database"],
)
def test_permits_restore_to_a_different_target(tmp_path: Path, host: str, database: str) -> None:
    target = RestoreTarget(host=host, port=5432, user="u", password="p", database=database)

    check_not_source(target, source_cfg(tmp_path), overridden=False)  # type: ignore[arg-type]


def test_explicit_override_allows_a_production_restore(tmp_path: Path) -> None:
    target = RestoreTarget(
        host="db.internal", port=5432, user="u", password="p", database="manor_production"
    )

    check_not_source(target, source_cfg(tmp_path), overridden=True)  # type: ignore[arg-type]


@pytest.mark.parametrize(
    ("value", "expected"),
    [
        (512, "512 B"),
        (1024, "1.0 KB"),
        (1536, "1.5 KB"),
        (1024 * 1024, "1.0 MB"),
        (3 * 1024 * 1024 * 1024, "3.0 GB"),
    ],
)
def test_human_bytes(value: int, expected: str) -> None:
    assert human_bytes(value) == expected


def test_run_on_boot_forces_a_startup_run(tmp_path: Path) -> None:
    """Unlike the old container's RUN_ONCE, the caller then keeps running.

    The runner is None: if the short-circuit ever stopped happening before the
    bucket is consulted, this would raise instead of passing.
    """
    from dataclasses import replace

    from postgres_backup.logfmt import Log

    cfg = make_config(tmp_path)
    cfg = replace(cfg, schedule=replace(cfg.schedule, run_on_boot=True))

    due, why = staleness(cfg, None, Log())  # type: ignore[arg-type]

    assert due
    assert "RUN_ON_BOOT" in why
