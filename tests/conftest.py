"""Shared fixtures."""

from __future__ import annotations

from collections.abc import Iterator
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path

import pytest

from postgres_backup.commands import Command
from postgres_backup.config import R2, Config, Limits, Postgres, Retention, Schedule
from postgres_backup.logfmt import Log
from postgres_backup.runner import BackupRunner

from .fakes import FakeRunner, FakeStore, RecordingPinger, Stub

#: The instant every backup test pretends it is.
RUN_AT = datetime(2026, 9, 17, 23, 0, 0, tzinfo=UTC)

#: Every variable the service reads, cleared so a developer's shell cannot leak
#: into a config test. The Go suite got this for free from t.Setenv.
_MANAGED_VARS = [
    "POSTGRES_HOST",
    "POSTGRES_PORT",
    "POSTGRES_USER",
    "POSTGRES_PASS",
    "POSTGRES_DB",
    "ACCESS_KEY_ID",
    "SECRET_ACCESS_KEY",
    "BUCKET",
    "BUCKET_PATH",
    "HOST_BASE",
    "HEALTHCHECKS_URL",
    "BACKUP_AT",
    "BACKUP_TZ",
    "RUN_ON_BOOT",
    "MAX_BACKUP_AGE",
    "RETENTION_DAILY_DAYS",
    "RETENTION_MONTHLY_MONTHS",
    "RETENTION_MIN_KEEP",
    "PRUNE_ENABLED",
    "PRUNE_MAX_DELETES",
    "PRUNE_ALLOW_BULK",
    "DRY_RUN",
    "TMP_DIR",
    "MIN_FREE_BYTES",
    "MIN_DUMP_BYTES",
    "MIN_TOC_ENTRIES",
    "PGDUMP_COMPRESS",
    "PGDUMP_TIMEOUT",
    "LOG_LEVEL",
    "LOG_TAIL_BYTES",
    "RESTORE_HOST",
    "RESTORE_PORT",
    "RESTORE_USER",
    "RESTORE_PASS",
    "RESTORE_DB",
    "ALLOW_RESTORE_TO_PRODUCTION",
    "DEFAULT_REGION",
    "HOST_BUCKET",
    "CONSOLE_LOGGING",
    "STORAGE_BACKEND",
    "RUN_ONCE",
    "POSTGRES_MAJOR_VERSION",
    "POSTGIS_MAJOR_VERSION",
    "POSTGIS_MINOR_RELEASE",
    "DB_DUMP_ENCRYPTION",
    "CRON_SCHEDULE",
]


@pytest.fixture(autouse=True)
def clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    for name in _MANAGED_VARS:
        monkeypatch.delenv(name, raising=False)


@pytest.fixture
def valid_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """The minimum environment for a successful config load."""
    for name, value in {
        "POSTGRES_HOST": "db.internal",
        "POSTGRES_USER": "postgres",
        "POSTGRES_PASS": "secret",
        "POSTGRES_DB": "app_production",
        "ACCESS_KEY_ID": "abc123",
        "SECRET_ACCESS_KEY": "def456",
        "BUCKET": "example-backups",
        "BUCKET_PATH": "backups",
        "HOST_BASE": "https://acct.r2.cloudflarestorage.com",
        "TMP_DIR": str(tmp_path),
    }.items():
        monkeypatch.setenv(name, value)


def make_config(tmp_dir: Path, **overrides: object) -> Config:
    base = Config(
        postgres=Postgres(
            host="db.internal",
            port=5432,
            user="postgres",
            password="pw",
            databases=["app_production"],
            compress="",
            timeout=3600.0,
        ),
        r2=R2(
            access_key_id="key",
            secret_access_key="secret",
            bucket="b",
            bucket_path="backups",
            host_base="https://acct.r2.cloudflarestorage.com",
        ),
        schedule=Schedule(at="23:00", tz="UTC", run_on_boot=False, max_backup_age=26 * 3600),
        retention=Retention(
            daily_days=30, monthly_months=12, min_keep=1, max_deletes=50, allow_bulk=False
        ),
        limits=Limits(min_free_bytes=1, min_dump_bytes=4, min_toc_entries=2),
        healthchecks_url="",
        log_level="error",
        log_tail_bytes=4096,
        tmp_dir=str(tmp_dir),
        dry_run=False,
        prune_enabled=True,
    )
    if overrides:
        from dataclasses import replace

        return replace(base, **overrides)  # type: ignore[arg-type]
    return base


@dataclass
class Harness:
    runner: BackupRunner
    store: FakeStore
    cmd: FakeRunner
    ping: RecordingPinger
    tmp: Path
    cfg: Config


def dump_path(command: Command) -> str:
    """The --file= target of a recorded pg_dump invocation."""
    for arg in command.args:
        if arg.startswith("--file="):
            return arg.removeprefix("--file=")
    return ""


def happy_path_stubs(dump_body: bytes | None = None) -> list[Stub]:
    """The command sequence a successful run issues."""
    body = dump_body if dump_body is not None else b"dump-bytes" * 64

    def write_dump(command: Command) -> None:
        Path(dump_path(command)).write_bytes(body)

    def write_globals(command: Command) -> None:
        if command.stdout_path:
            Path(command.stdout_path).write_text("CREATE ROLE app;\n")

    return [
        Stub(match="psql", stdout="1048576\n"),  # pg_database_size
        Stub(match="pg_dump", do=write_dump),
        Stub(
            match="pg_restore",
            stdout=(
                ";\n; Archive created at\n;\n"
                "1; 1259 TABLE notes\n2; 1259 TABLE users\n3; 1259 TABLE tasks\n"
            ),
        ),
        Stub(match="pg_restore"),  # verify_readable: full read of the archive
        Stub(match="pg_dumpall", do=write_globals),
    ]


@pytest.fixture
def harness(tmp_path: Path) -> Iterator[Harness]:
    store = FakeStore()
    store.now = lambda: RUN_AT
    cmd = FakeRunner(happy_path_stubs())
    ping = RecordingPinger()
    cfg = make_config(tmp_path)

    runner = BackupRunner(
        cfg=cfg,
        store=store,
        runner=cmd,
        ping=ping,
        log=Log(),
        now=lambda: RUN_AT,
        tail=lambda: "recent log output",
    )
    yield Harness(runner=runner, store=store, cmd=cmd, ping=ping, tmp=tmp_path, cfg=cfg)
