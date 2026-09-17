"""Argument construction and output parsing for the PostgreSQL binaries."""

from __future__ import annotations

import pytest

from postgres_backup import pgtool
from postgres_backup.commands import CommandError

from .fakes import FakeRunner, Stub


def conn(database: str = "manor_production") -> pgtool.Conn:
    return pgtool.Conn(
        host="db.internal", port=5432, user="postgres", password="pw", database=database
    )


@pytest.mark.parametrize(
    ("output", "expected"),
    [
        ("pg_dump (PostgreSQL) 18.6\n", 18),
        ("pg_dump (PostgreSQL) 17.11 (Homebrew)\n", 17),
        ("pg_restore (PostgreSQL) 18.0\n", 18),
    ],
)
def test_client_major_parses_version(output: str, expected: int) -> None:
    runner = FakeRunner([Stub(stdout=output)])

    assert pgtool.client_major(runner, "pg_dump") == expected


def test_client_major_rejects_unparseable_output() -> None:
    runner = FakeRunner([Stub(stdout="not a version string\n")])

    with pytest.raises(CommandError):
        pgtool.client_major(runner, "pg_dump")


def test_server_major_converts_version_num() -> None:
    runner = FakeRunner([Stub(stdout="180006\n")])

    assert pgtool.server_major(runner, conn()) == 18


def test_connection_secrets_travel_in_the_environment() -> None:
    """argv is visible in the process table and echoed into logs; the env is not."""
    runner = FakeRunner()

    pgtool.ping(runner, conn())

    calls = runner.calls_to("psql")
    assert len(calls) == 1
    call = calls[0]

    assert "pw" not in call.args
    assert call.env["PGPASSWORD"] == "pw"
    assert call.env["PGHOST"] == "db.internal"
    assert call.env["PGPORT"] == "5432"
    assert call.env["PGUSER"] == "postgres"
    assert call.env["PGCONNECT_TIMEOUT"] == "10", "a hung connection would stall the run"


def test_dump_args() -> None:
    runner = FakeRunner()

    pgtool.dump(runner, conn(), "/tmp/x.dump")

    args = runner.calls_to("pg_dump")[0].args
    for expected in (
        "--format=custom",
        "--no-owner",
        "--no-privileges",
        "--file=/tmp/x.dump",
        "--dbname=manor_production",
    ):
        assert expected in args
    assert not any(a.startswith("--compress") for a in args), (
        "compression was set without being configured"
    )


def test_dump_passes_compression_when_configured() -> None:
    runner = FakeRunner()

    pgtool.dump(runner, conn(), "/tmp/x.dump", "zstd:3")

    assert "--compress=zstd:3" in runner.calls_to("pg_dump")[0].args


def test_toc_entries_counts_only_real_entries() -> None:
    listing = (
        ";\n"
        "; Archive created at 2026-09-17 23:00:14 UTC\n"
        ";     dbname: manor_production\n"
        ";\n"
        "; Selected TOC Entries:\n"
        ";\n"
        "215; 1259 16456 TABLE public notes manor\n"
        "216; 1259 16462 TABLE public users manor\n"
        "217; 1259 16468 TABLE public tasks manor\n"
    )
    runner = FakeRunner([Stub(stdout=listing)])

    assert pgtool.toc_entries(runner, "/tmp/x.dump") == 3


def test_toc_entries_on_an_empty_archive() -> None:
    runner = FakeRunner([Stub(stdout=";\n; Archive created\n;\n")])

    assert pgtool.toc_entries(runner, "/tmp/x.dump") == 0


def test_verify_readable_reads_the_whole_archive() -> None:
    """--list only parses the header; this is what catches truncation."""
    runner = FakeRunner()

    pgtool.verify_readable(runner, "/tmp/x.dump")

    args = runner.calls_to("pg_restore")[0].args
    assert "--file=/dev/null" in args
    assert "--list" not in args


def test_verify_readable_reports_a_truncated_archive() -> None:
    runner = FakeRunner(
        [Stub(match="pg_restore", error=CommandError("pg_restore", "pg_restore exited 1"))]
    )

    with pytest.raises(CommandError, match="not fully readable"):
        pgtool.verify_readable(runner, "/tmp/x.dump")


def test_restore_is_atomic_and_fails_loudly() -> None:
    """pg_restore otherwise logs errors and still exits zero."""
    runner = FakeRunner()

    pgtool.restore(runner, conn(), "/tmp/x.dump")

    args = runner.calls_to("pg_restore")[0].args
    for expected in ("--single-transaction", "--exit-on-error", "--clean", "--if-exists"):
        assert expected in args


def test_dump_globals_requests_only_globals(tmp_path: object) -> None:
    runner = FakeRunner()

    pgtool.dump_globals(runner, conn(), "/tmp/globals.sql")

    call = runner.calls_to("pg_dumpall")[0]
    assert "--globals-only" in call.args
    assert call.stdout_path == "/tmp/globals.sql"


def test_database_size_parses_bytes() -> None:
    runner = FakeRunner([Stub(stdout="8589934592\n")])

    assert pgtool.database_size(runner, conn()) == 8589934592
