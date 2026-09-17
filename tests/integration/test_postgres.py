"""Exercises the real pg_dump, pg_restore and psql binaries against a live server.

Unit tests cover orchestration against fakes; what they cannot prove is that the
arguments this service passes actually work, and that a dump it produces can be
read back. That is what these are for.

Object storage is still faked: the one thing no local S3 substitute reproduces
is R2's checksum behaviour, which the README's smoke test covers instead.
"""

from __future__ import annotations

import os
from datetime import UTC, datetime
from pathlib import Path

import pytest

from postgres_backup import keys, pgtool
from postgres_backup.commands import Command, CommandError, Subprocess
from postgres_backup.logfmt import Log
from postgres_backup.runner import EXPECTED_PG_MAJOR, BackupRunner
from tests.conftest import make_config
from tests.fakes import FakeStore, RecordingPinger

pytestmark = pytest.mark.integration

RUN_AT = datetime(2026, 9, 17, 23, 0, tzinfo=UTC)


def env(name: str, default: str) -> str:
    return os.environ.get(name) or default


def conn(database: str) -> pgtool.Conn:
    return pgtool.Conn(
        host=env("PGBACKUP_TEST_HOST", "localhost"),
        port=int(env("PGBACKUP_TEST_PORT", "5432")),
        user=env("PGBACKUP_TEST_USER", "postgres"),
        password=env("PGBACKUP_TEST_PASS", "integration"),
        database=database,
    )


def main_db() -> str:
    return env("PGBACKUP_TEST_DB", "pgbackup_integration")


@pytest.fixture(scope="module")
def runner() -> Subprocess:
    return Subprocess(log=Log())


def psql_exec(runner: Subprocess, database: str, sql: str) -> None:
    target = conn(database)
    runner.run(
        Command(
            path="psql",
            args=["--no-psqlrc", "--quiet", "--dbname", database, "--command", sql],
            env=target.env(),
        )
    )


def psql_query(runner: Subprocess, database: str, sql: str) -> str:
    target = conn(database)
    return runner.run(
        Command(
            path="psql",
            args=[
                "--no-psqlrc",
                "--tuples-only",
                "--no-align",
                "--dbname",
                database,
                "--command",
                sql,
            ],
            env=target.env(),
        )
    ).stdout.strip()


@pytest.fixture
def seeded(runner: Subprocess) -> str:
    """A database with enough content to clear the verification thresholds."""
    database = main_db()
    psql_exec(
        runner,
        database,
        """
        DROP TABLE IF EXISTS notes, users, tasks CASCADE;
        CREATE TABLE notes (id serial PRIMARY KEY, body text, created_at timestamptz DEFAULT now());
        CREATE TABLE users (id serial PRIMARY KEY, email text UNIQUE);
        CREATE TABLE tasks (id serial PRIMARY KEY, title text, done boolean DEFAULT false);
        CREATE INDEX idx_notes_created ON notes(created_at);
        INSERT INTO notes (body) SELECT 'note ' || g FROM generate_series(1,20000) g;
        INSERT INTO users (email) SELECT 'u' || g || '@example.com' FROM generate_series(1,2000) g;
        INSERT INTO tasks (title) SELECT 'task ' || g FROM generate_series(1,5000) g;
        """,
    )
    return database


def test_client_is_the_expected_major_version(runner: Subprocess) -> None:
    for binary in ("pg_dump", "pg_restore"):
        assert pgtool.client_major(runner, binary) == EXPECTED_PG_MAJOR


def test_server_is_reachable_and_not_newer_than_the_client(runner: Subprocess) -> None:
    target = conn(main_db())
    pgtool.ping(runner, target)

    server = pgtool.server_major(runner, target)

    assert server <= EXPECTED_PG_MAJOR, (
        f"server is major {server}, newer than the bundled client; dumps would be unusable"
    )


def test_dump_verify_restore_round_trip(runner: Subprocess, seeded: str, tmp_path: Path) -> None:
    """The whole point of a backup: it can be loaded back and yields the same data."""
    target = f"{seeded}_restored"
    expected = {
        table: psql_query(runner, seeded, f"SELECT count(*) FROM {table}")
        for table in ("notes", "users", "tasks")
    }

    psql_exec(runner, "postgres", f"DROP DATABASE IF EXISTS {target}")
    psql_exec(runner, "postgres", f"CREATE DATABASE {target}")
    try:
        path = tmp_path / "roundtrip.dump"
        pgtool.dump(runner, conn(seeded), str(path))

        assert path.stat().st_size > 0
        assert pgtool.toc_entries(runner, str(path)) >= 10
        pgtool.verify_readable(runner, str(path))

        pgtool.restore(runner, conn(target), str(path))

        for table, count in expected.items():
            assert psql_query(runner, target, f"SELECT count(*) FROM {table}") == count
    finally:
        psql_exec(runner, "postgres", f"DROP DATABASE IF EXISTS {target}")


def test_verification_rejects_a_truncated_archive(
    runner: Subprocess, seeded: str, tmp_path: Path
) -> None:
    """The finding that shaped verification, asserted from both sides.

    A truncated archive still carries a complete table of contents at its head,
    so --list reports it as healthy. Only a full read catches it.
    """
    path = tmp_path / "truncated.dump"
    pgtool.dump(runner, conn(seeded), str(path))
    os.truncate(path, path.stat().st_size // 2)

    listed_ok = True
    try:
        pgtool.toc_entries(runner, str(path))
    except CommandError:
        listed_ok = False

    with pytest.raises(CommandError, match="not fully readable"):
        pgtool.verify_readable(runner, str(path))

    assert listed_ok, (
        "pg_restore --list rejected the truncated archive; if that is now reliable, "
        "the full-read check may be reconsidered -- but verify it across versions first"
    )


def test_verification_accepts_an_intact_archive(
    runner: Subprocess, seeded: str, tmp_path: Path
) -> None:
    path = tmp_path / "intact.dump"
    pgtool.dump(runner, conn(seeded), str(path))

    pgtool.verify_readable(runner, str(path))


def test_an_empty_database_produces_too_few_toc_entries(runner: Subprocess, tmp_path: Path) -> None:
    """This is what the TOC floor exists to catch: a dump of the wrong database."""
    empty = f"{main_db()}_empty"
    psql_exec(runner, "postgres", f"DROP DATABASE IF EXISTS {empty}")
    psql_exec(runner, "postgres", f"CREATE DATABASE {empty}")
    try:
        path = tmp_path / "empty.dump"
        pgtool.dump(runner, conn(empty), str(path))

        assert pgtool.toc_entries(runner, str(path)) < 10
    finally:
        psql_exec(runner, "postgres", f"DROP DATABASE IF EXISTS {empty}")


def test_globals_dump_contains_roles(runner: Subprocess, tmp_path: Path) -> None:
    path = tmp_path / "globals.sql"

    pgtool.dump_globals(runner, conn(main_db()), str(path))

    assert "ROLE" in path.read_text().upper()


def test_full_run_against_real_postgres(runner: Subprocess, seeded: str, tmp_path: Path) -> None:
    """The whole orchestration against real binaries, with storage faked."""
    from dataclasses import replace

    store = FakeStore()
    store.now = lambda: RUN_AT
    target = conn(seeded)

    cfg = make_config(tmp_path)
    cfg = replace(
        cfg,
        postgres=replace(
            cfg.postgres,
            host=target.host,
            port=target.port,
            user=target.user,
            password=target.password,
            databases=[seeded],
            timeout=600.0,
        ),
        limits=replace(cfg.limits, min_dump_bytes=1024, min_toc_entries=10),
    )

    backup = BackupRunner(
        cfg=cfg,
        store=store,
        runner=runner,
        ping=RecordingPinger(),
        log=Log(),
        now=lambda: RUN_AT,
    )

    backup.preflight()
    backup.run_once()

    key = keys.build_key(cfg.r2.bucket_path, seeded, RUN_AT)
    assert key in store.stored_keys()

    obj, metadata = store.head(key)
    assert obj.size > 0
    assert metadata["sha256"]
    assert int(metadata["toc-entries"]) >= 10
    assert metadata["pg-dump-major"] == str(EXPECTED_PG_MAJOR)
