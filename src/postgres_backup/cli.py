"""Command-line entry point."""

from __future__ import annotations

import argparse
import os
import sys
import traceback
from datetime import UTC, datetime

from . import restore as restore_cmd
from .commands import Subprocess
from .config import Config, ConfigError
from .config import load as load_config
from .goconv import format_duration
from .healthcheck import build as build_pinger
from .logfmt import Log, configure
from .runner import BackupRunner
from .schedule import Clock, DailySchedule
from .shutdown import Shutdown
from .store import R2Store

#: Set at build time via the PGBACKUP_GIT_SHA environment variable.
GIT_SHA_ENV = "PGBACKUP_GIT_SHA"

USAGE = """pgbackup - PostgreSQL backups to S3-compatible storage

Usage:
  pgbackup serve             Run continuously, backing up on the daily schedule (default)
  pgbackup backup            Run one backup immediately and exit
  pgbackup prune             Apply the retention policy and exit
  pgbackup list              List stored backups
  pgbackup verify --key KEY  Download a backup and check it is restorable
  pgbackup restore ...       Restore a backup into a target database

Configuration is read from the environment; see README.md.
"""


class Parser(argparse.ArgumentParser):
    """An ArgumentParser that exits 1 rather than argparse's default of 2.

    Every other failure in this service exits 1; a bad flag should not be the
    one thing that reports differently.
    """

    def error(self, message: str) -> None:  # type: ignore[override]
        self.print_usage(sys.stderr)
        sys.stderr.write(f"pgbackup: {message}\n")
        raise SystemExit(1)


class RealClock:
    """A Clock backed by the wall clock, interruptible by shutdown."""

    def __init__(self, shutdown: Shutdown) -> None:
        self._shutdown = shutdown

    def now(self) -> datetime:
        return datetime.now(UTC)

    def sleep_until_or_stop(self, seconds: float) -> bool:
        return self._shutdown.wait(seconds)


def main(argv: list[str] | None = None) -> int:
    args = list(sys.argv[1:] if argv is None else argv)

    if args and args[0] in ("-h", "--help", "help"):
        sys.stdout.write(USAGE)
        return 0

    command = args[0] if args else "serve"
    rest = args[1:]

    try:
        return _run(command, rest)
    except SystemExit as exc:  # argparse and explicit exits
        return int(exc.code or 0)
    except Exception as exc:
        # The logger may not exist yet if configuration failed, so report to
        # stderr unconditionally: a container that cannot start must still
        # say why.
        sys.stderr.write(f"pgbackup: {exc}\n")
        return 1


def _run(command: str, args: list[str]) -> int:
    try:
        cfg, warnings = load_config()
    except ConfigError as exc:
        sys.stderr.write(f"pgbackup: {exc}\n")
        return 1

    log, tail = configure(cfg.log_level, cfg.log_tail_bytes)

    log.info(
        "pgbackup starting",
        event="boot",
        git_sha=os.environ.get(GIT_SHA_ENV, "unknown"),
        command=command,
        **cfg.redacted(),
    )
    for warning in warnings:
        log.warn(warning, event="config.warning")
    if not cfg.healthchecks_url:
        log.warn(
            "HEALTHCHECKS_URL is not set; run outcomes will not be reported externally",
            event="config.no_healthchecks",
        )

    shutdown = Shutdown()
    shutdown.install()

    store = R2Store(
        access_key_id=cfg.r2.access_key_id,
        secret_access_key=cfg.r2.secret_access_key,
        bucket=cfg.r2.bucket,
        host_base=cfg.r2.host_base,
    )
    pinger = build_pinger(cfg.healthchecks_url, cfg.log_tail_bytes, log)
    subprocess_runner = Subprocess(log=log, shutdown=shutdown)

    backup_runner = BackupRunner(
        cfg=cfg,
        store=store,
        runner=subprocess_runner,
        ping=pinger,
        log=log,
        tail=tail.text,
    )

    handlers = {
        "serve": lambda: serve(cfg, backup_runner, log, shutdown),
        "backup": lambda: _one_shot(backup_runner),
        "prune": lambda: prune_only(cfg, backup_runner, log),
        "list": lambda: restore_cmd.list_backups(cfg, store, args),
        "verify": lambda: restore_cmd.verify(cfg, store, subprocess_runner, args, log),
        "restore": lambda: restore_cmd.restore(cfg, store, subprocess_runner, args, log),
    }

    handler = handlers.get(command)
    if handler is None:
        sys.stdout.write(USAGE)
        sys.stderr.write(f"pgbackup: unknown command {command!r}\n")
        return 1

    try:
        return handler()
    except Exception as exc:
        sys.stderr.write(f"pgbackup: {exc}\n")
        return 1


def _one_shot(runner: BackupRunner) -> int:
    runner.run_once()
    return 0


def serve(cfg: Config, runner: BackupRunner, log: Log, shutdown: Shutdown) -> int:
    # Refusing to start is the point: a container that cannot back up should
    # crash-loop visibly rather than sit idle until 23:00.
    runner.preflight()
    log.info("preflight passed", event="preflight.ok")

    clock: Clock = RealClock(shutdown)
    schedule = DailySchedule(cfg.schedule.at, cfg.schedule.tz, clock)

    due, why = staleness(cfg, runner, log)
    if due:
        log.info("running immediately", event="schedule.catchup", reason=why)
        _log_run(runner, log)

    def announce(next_run: datetime) -> None:
        remaining = (next_run - clock.now()).total_seconds()
        log.info(
            "waiting for next scheduled backup",
            event="schedule.waiting",
            next_run=next_run.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ"),
            **{"in": format_duration(remaining)},
        )

    schedule.run(job=lambda: _log_run(runner, log), before_sleep=announce)

    log.info("shutting down", event="shutdown")
    return 0


def _log_run(runner: BackupRunner, log: Log) -> None:
    """Execute a backup and swallow the error.

    A failed run is already reported to Healthchecks and the logs, and must not
    stop tomorrow's attempt.
    """
    try:
        runner.run_once()
    except Exception as exc:
        log.error("scheduled backup failed", event="run.error", error=str(exc))
    except BaseException as exc:
        log.error(
            "scheduler recovered from unexpected exit",
            event="run.panic",
            error=str(exc),
            stack=traceback.format_exc(),
        )


def staleness(cfg: Config, runner: BackupRunner, log: Log) -> tuple[bool, str]:
    """Decide whether a restart has left the schedule behind.

    Always backing up on boot would dump production on every deploy; never doing
    so means a container that crash-loops through its window silently skips a
    day. Asking the bucket how old the newest backup is settles it without
    keeping any local state.
    """
    if cfg.schedule.run_on_boot:
        # Deliberately short-circuits before touching the bucket.
        return True, "RUN_ON_BOOT is set"

    for db in cfg.postgres.databases:
        try:
            newest = runner.newest_backup(db)
        except Exception as exc:
            # A transient storage error at boot must not cancel the schedule.
            log.warn(
                "could not determine last backup age; deferring to the schedule",
                event="schedule.staleness_unknown",
                database=db,
                error=str(exc),
            )
            return False, ""

        if newest is None:
            return True, f"no existing backup found for {db!r}"

        age = (runner.now() - newest).total_seconds()
        if age > cfg.schedule.max_backup_age:
            return True, (
                f"newest backup of {db!r} is {format_duration(age)} old, "
                f"over the {format_duration(cfg.schedule.max_backup_age)} threshold"
            )

        log.info(
            "existing backup is current",
            event="schedule.staleness_ok",
            database=db,
            newest=newest.strftime("%Y-%m-%dT%H:%M:%SZ"),
        )

    return False, ""


def prune_only(cfg: Config, runner: BackupRunner, log: Log) -> int:
    errors: list[str] = []
    for db in cfg.postgres.databases:
        try:
            runner.prune_database(db)
        except Exception as exc:
            errors.append(f"database {db!r}: {exc}")
    if errors:
        raise RuntimeError("; ".join(errors))
    log.info("prune complete", event="prune.finish")
    return 0
