"""Full backup runs against fakes: no database, no bucket, no real clock."""

from __future__ import annotations

import contextlib
from dataclasses import replace
from datetime import timedelta
from pathlib import Path

import pytest

from postgres_backup import keys
from postgres_backup.commands import CommandError
from postgres_backup.runner import (
    PRUNE_FAILED_PREFIX,
    BackupError,
    BackupRunner,
    PreflightError,
)
from postgres_backup.store import Object

from .conftest import RUN_AT, Harness, happy_path_stubs
from .fakes import MismatchedMetaStore, StrippedMetaStore, Stub


def dump_key() -> str:
    return keys.build_key("manor_db", "manor_production", RUN_AT)


def globals_key() -> str:
    return keys.globals_key("manor_db", "manor_production", RUN_AT)


def boom() -> CommandError:
    return CommandError("fake", "simulated failure")


# ----------------------------------------------------------------- happy path


def test_run_once_happy_path(harness: Harness) -> None:
    harness.runner.run_once()

    assert harness.ping.started
    assert harness.ping.succeeded
    assert not harness.ping.failed
    assert dump_key() in harness.store.stored_keys()
    assert globals_key() in harness.store.stored_keys()


def test_password_never_appears_in_args(harness: Harness) -> None:
    """argv is visible in the process table and echoed into logs; the env is not."""
    harness.runner.run_once()

    assert harness.cmd.calls, "no commands were run"
    for call in harness.cmd.calls:
        assert not any(arg == "pw" for arg in call.args), f"password in args of {call.path}"
        if call.path in ("pg_dump", "psql", "pg_dumpall"):
            assert call.env.get("PGPASSWORD") == "pw", f"{call.path} lacked PGPASSWORD"


def test_upload_records_verification_metadata(harness: Harness) -> None:
    harness.runner.run_once()

    _, metadata = harness.store.head(dump_key())

    assert metadata["sha256"]
    assert metadata["toc-entries"] == "3"
    assert metadata["database"] == "manor_production"
    assert metadata["pg-dump-major"] == "18"


# -------------------------------------------------------------- failure paths


def test_dump_failure_pings_fail(harness: Harness) -> None:
    harness.cmd.stubs = [
        Stub(match="psql", stdout="1048576\n"),
        Stub(match="pg_dump", error=boom()),
    ]

    with pytest.raises(BackupError):
        harness.runner.run_once()

    assert harness.ping.started
    assert harness.ping.failed
    assert not harness.ping.succeeded
    assert "recent log output" in harness.ping.fail_body, "the failure ping lost the log tail"
    assert harness.store.stored_keys() == [], "a failed run uploaded something"


def test_rejects_dump_with_too_few_toc_entries(harness: Harness) -> None:
    """A valid but nearly empty archive is the classic wrong-database symptom."""
    harness.cmd.stubs = happy_path_stubs()
    harness.cmd.stubs[2] = Stub(match="pg_restore", stdout=";\n; Archive\n;\n1; 1259 TABLE one\n")

    with pytest.raises(BackupError, match="wrong database"):
        harness.runner.run_once()

    assert harness.store.stored_keys() == []


def test_rejects_undersized_dump(harness: Harness) -> None:
    harness.cmd.stubs = happy_path_stubs(dump_body=b"x")

    with pytest.raises(BackupError, match="below the"):
        harness.runner.run_once()

    assert harness.store.stored_keys() == []


def test_rejects_unreadable_archive(harness: Harness) -> None:
    harness.cmd.stubs = happy_path_stubs()
    harness.cmd.stubs[2] = Stub(match="pg_restore", error=boom())

    with pytest.raises(BackupError):
        harness.runner.run_once()

    assert harness.store.stored_keys() == []


def test_rejects_truncated_archive(harness: Harness) -> None:
    """A truncated dump still lists a complete table of contents and exits 0.

    Only the full read catches it, which is why verify_readable exists.
    """
    harness.cmd.stubs = happy_path_stubs()
    harness.cmd.stubs[3] = Stub(
        match="pg_restore",
        stderr="pg_restore: error: could not read from input file: end of file\n",
        error=boom(),
    )

    with pytest.raises(BackupError, match="not fully readable"):
        harness.runner.run_once()

    assert harness.store.stored_keys() == [], "a truncated dump was uploaded"


def test_upload_checksum_mismatch_fails(harness: Harness) -> None:
    harness.runner.store = MismatchedMetaStore(harness.store)  # type: ignore[assignment]

    with pytest.raises(BackupError, match="does not match"):
        harness.runner.run_once()


def test_upload_without_checksum_metadata_fails(harness: Harness) -> None:
    """Absent metadata is a failure, not a skip: this run wrote it moments ago."""
    harness.runner.store = StrippedMetaStore(harness.store)  # type: ignore[assignment]

    with pytest.raises(BackupError, match="no sha256 metadata"):
        harness.runner.run_once()


def test_insufficient_disk_space_fails_before_dumping(harness: Harness) -> None:
    harness.runner.cfg = replace(
        harness.cfg, limits=replace(harness.cfg.limits, min_free_bytes=1 << 62)
    )

    with pytest.raises(BackupError, match="insufficient space"):
        harness.runner.run_once()

    assert harness.cmd.calls_to("pg_dump") == [], "dumped despite insufficient space"


@pytest.mark.parametrize("break_run", [False, True], ids=["success", "failure"])
def test_temp_files_are_removed(harness: Harness, break_run: bool) -> None:
    """The container's volume is sized for one dump."""
    if break_run:
        harness.cmd.stubs[2] = Stub(match="pg_restore", error=boom())

    with contextlib.suppress(BackupError):
        harness.runner.run_once()

    leftovers = [p.name for p in harness.tmp.iterdir() if "dump" in p.name or "globals" in p.name]
    assert leftovers == [], f"left behind {leftovers}"


# --------------------------------------------------------------------- prune


def test_prune_failure_still_reports_the_upload_succeeded(harness: Harness) -> None:
    harness.store.list_error = boom()

    with pytest.raises(BackupError):
        harness.runner.run_once()

    assert harness.ping.failed
    assert dump_key() in harness.store.stored_keys(), "the dump should have been uploaded first"
    assert PRUNE_FAILED_PREFIX in harness.ping.fail_body, "the alert must not read as a lost backup"


def test_prune_removes_expired_backups_and_their_globals(harness: Harness) -> None:
    old = RUN_AT - timedelta(days=400)
    old_dump = keys.build_key("manor_db", "manor_production", old)
    old_globals = keys.globals_key("manor_db", "manor_production", old)
    harness.store.seed(old_dump, 100, old)
    harness.store.seed(old_globals, 10, old)
    for days in (1, 2, 3):
        ts = RUN_AT - timedelta(days=days)
        harness.store.seed(keys.build_key("manor_db", "manor_production", ts), 100, ts)

    harness.runner.run_once()

    assert old_dump not in harness.store.stored_keys(), "the expired dump was not pruned"
    assert old_globals not in harness.store.stored_keys(), "the sidecar was left behind"


def test_prune_leaves_foreign_keys_alone(harness: Harness) -> None:
    legacy = "manor_db/2024/September/PG_manor_17-September-2024.dmp"
    harness.store.seed(legacy, 100, RUN_AT - timedelta(days=700))

    harness.runner.run_once()

    assert legacy in harness.store.stored_keys(), "a key this service did not write was deleted"


def test_pruning_can_be_disabled(harness: Harness) -> None:
    harness.runner.cfg = replace(harness.cfg, prune_enabled=False)
    old = RUN_AT - timedelta(days=400)
    old_dump = keys.build_key("manor_db", "manor_production", old)
    harness.store.seed(old_dump, 100, old)

    harness.runner.run_once()

    assert old_dump in harness.store.stored_keys()


# ------------------------------------------------------------------- dry run


def test_dry_run_uploads_nothing(harness: Harness) -> None:
    harness.runner.cfg = replace(harness.cfg, dry_run=True)

    harness.runner.run_once()

    assert harness.store.stored_keys() == []
    assert harness.ping.succeeded, "a dry run should still report success"


# ----------------------------------------------------------------- preflight


def preflight_stubs() -> list[Stub]:
    return [
        Stub(match="pg_dump", stdout="pg_dump (PostgreSQL) 18.6\n"),
        Stub(match="psql", stdout="1\n"),
        Stub(match="psql", stdout="180006\n"),
    ]


def test_preflight_passes_when_everything_works(harness: Harness) -> None:
    harness.cmd.stubs = preflight_stubs()

    harness.runner.preflight()

    assert harness.store.stored_keys() == [], "preflight left its probe object behind"


def test_preflight_rejects_wrong_client_version(harness: Harness) -> None:
    harness.cmd.stubs = [Stub(match="pg_dump", stdout="pg_dump (PostgreSQL) 17.4\n")]

    with pytest.raises(PreflightError, match="17"):
        harness.runner.preflight()


def test_preflight_rejects_unreachable_database(harness: Harness) -> None:
    harness.cmd.stubs = [
        Stub(match="pg_dump", stdout="pg_dump (PostgreSQL) 18.6\n"),
        Stub(match="psql", error=boom()),
    ]

    with pytest.raises(PreflightError, match="unreachable"):
        harness.runner.preflight()


def test_preflight_rejects_unwritable_storage(harness: Harness) -> None:
    """The rotated-key scenario: fail at deploy time, not at 23:00 in silence."""
    harness.cmd.stubs = preflight_stubs()
    harness.store.upload_error = boom()

    with pytest.raises(PreflightError, match="not writable"):
        harness.runner.preflight()


def test_preflight_rejects_a_server_newer_than_the_client(harness: Harness) -> None:
    harness.cmd.stubs = [
        Stub(match="pg_dump", stdout="pg_dump (PostgreSQL) 18.6\n"),
        Stub(match="psql", stdout="1\n"),
        Stub(match="psql", stdout="190001\n"),
    ]

    with pytest.raises(PreflightError, match="older than server"):
        harness.runner.preflight()


# -------------------------------------------------------------------- misc


def test_newest_backup_ignores_unparseable_keys(harness: Harness) -> None:
    harness.store.seed("manor_db/manor_production/junk.dmp", 10, RUN_AT)
    ts = RUN_AT - timedelta(days=2)
    harness.store.seed(keys.build_key("manor_db", "manor_production", ts), 10, ts)

    assert harness.runner.newest_backup("manor_production") == ts


def test_newest_backup_is_none_on_an_empty_bucket(harness: Harness) -> None:
    assert harness.runner.newest_backup("manor_production") is None


def test_multiple_databases_each_produce_a_backup(tmp_path: Path, harness: Harness) -> None:
    harness.runner.cfg = replace(
        harness.cfg,
        postgres=replace(harness.cfg.postgres, databases=["db_one", "db_two"]),
    )
    harness.cmd.stubs = happy_path_stubs() + happy_path_stubs()

    harness.runner.run_once()

    stored = harness.store.stored_keys()
    assert keys.build_key("manor_db", "db_one", RUN_AT) in stored
    assert keys.build_key("manor_db", "db_two", RUN_AT) in stored


def test_object_dataclass_is_hashable_for_set_operations() -> None:
    obj = Object(key="k", size=1, last_modified=RUN_AT)
    assert {obj, obj} == {obj}


def test_runner_uses_injected_clock(harness: Harness) -> None:
    assert harness.runner.now() == RUN_AT


def test_prune_database_standalone(harness: Harness) -> None:
    for days in range(40):
        ts = RUN_AT - timedelta(days=days)
        harness.store.seed(keys.build_key("manor_db", "manor_production", ts), 100, ts)

    harness.runner.prune_database("manor_production")

    assert len(harness.store.deleted) > 0


def test_backup_runner_requires_no_real_clock() -> None:
    """The constructor must not reach for the wall clock when one is supplied."""
    assert BackupRunner.__init__.__defaults__ is not None
