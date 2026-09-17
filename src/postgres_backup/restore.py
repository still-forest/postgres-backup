"""The list, verify and restore subcommands."""

from __future__ import annotations

import argparse
import os
import sys
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path

from . import keys, pgtool
from .commands import CommandRunner
from .config import Config
from .logfmt import Log
from .runner import hash_file
from .store import ObjectStore


@dataclass(frozen=True)
class RestoreTarget:
    """Where a restore writes.

    Read from a separate ``RESTORE_*`` namespace, never from the ``POSTGRES_*``
    variables the service backs up. This is the guard against restoring over
    production, and it is structural rather than a prompt: there is no code path
    by which the production connection settings can reach pg_restore. Recovering
    production requires deliberately putting the production host into
    differently-named variables.
    """

    host: str
    port: int
    user: str
    password: str
    database: str


def load_restore_target() -> RestoreTarget:
    port_raw = os.environ.get("RESTORE_PORT", "").strip() or "5432"
    try:
        port = int(port_raw)
    except ValueError:
        raise ValueError(f"RESTORE_PORT must be an integer, got {port_raw!r}") from None

    values = {
        name: os.environ.get(f"RESTORE_{name}", "") for name in ("HOST", "USER", "PASS", "DB")
    }
    missing = [f"RESTORE_{name}" for name, value in values.items() if not value.strip()]
    if missing:
        raise ValueError(
            f"restore requires {', '.join(missing)}; these are deliberately separate from "
            "the POSTGRES_* settings so that running this container with its normal "
            "environment cannot overwrite the database it backs up"
        )

    return RestoreTarget(
        host=values["HOST"],
        port=port,
        user=values["USER"],
        password=values["PASS"],
        database=values["DB"],
    )


def is_source(target: RestoreTarget, cfg: Config) -> bool:
    """Whether the restore target is a database this service backs up.

    The primary guard is the separate variable namespace; this is the second
    line, for the case where an operator has filled those in with production
    values.
    """
    return target.host == cfg.postgres.host and target.database in cfg.postgres.databases


def check_not_source(target: RestoreTarget, cfg: Config, overridden: bool) -> None:
    if not is_source(target, cfg) or overridden:
        return
    raise ValueError(
        f"refusing to restore into {target.host}/{target.database}: it is a database this "
        "service backs up. Pass --force-production and set "
        "ALLOW_RESTORE_TO_PRODUCTION=true if that is genuinely intended"
    )


def _parser(name: str) -> argparse.ArgumentParser:
    from .cli import Parser

    return Parser(prog=f"pgbackup {name}", allow_abbrev=False, add_help=True)


def _selection_flags(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--key", default="", help="object key to act on")
    parser.add_argument("--date", default="", help="select the backup taken on YYYY-MM-DD")
    parser.add_argument("--db", default="", help="restrict selection to one database")
    parser.add_argument("--latest", action="store_true", help="select the most recent backup")


def list_backups(cfg: Config, store: ObjectStore, args: list[str]) -> int:
    parser = _parser("list")
    parser.add_argument("--db", default="", help="restrict to one database")
    parser.add_argument("--limit", type=int, default=20, help="maximum rows to show")
    opts = parser.parse_args(args)

    prefix = cfg.r2.bucket_path
    if opts.db:
        prefix = f"{prefix}/{opts.db}".lstrip("/")

    rows: list[tuple[str, datetime, int, str]] = []
    for obj in store.list_objects(prefix):
        found = keys.parse_key(obj.key)
        if found is None:
            continue
        rows.append((found[0], found[1], obj.size, obj.key))
    rows.sort(key=lambda r: r[1], reverse=True)

    if not rows:
        sys.stdout.write(f"No backups found under {prefix!r}.\n")
        return 0

    if opts.limit > 0:
        rows = rows[: opts.limit]

    sys.stdout.write(f"{'DATABASE':<20}  {'TAKEN (UTC)':<24}  {'SIZE':>12}  KEY\n")
    for db, ts, size, key in rows:
        stamp = ts.strftime("%Y-%m-%dT%H:%M:%SZ")
        sys.stdout.write(f"{db:<20}  {stamp:<24}  {human_bytes(size):>12}  {key}\n")
    return 0


def resolve_key(cfg: Config, store: ObjectStore, key: str, date: str, db: str, latest: bool) -> str:
    """Turn the selection flags into exactly one key."""
    if key:
        return key
    if not latest and not date:
        raise ValueError("specify one of --key, --date or --latest")

    prefix = cfg.r2.bucket_path
    if db:
        prefix = f"{prefix}/{db}".lstrip("/")

    best: str = ""
    best_ts: datetime | None = None
    for obj in store.list_objects(prefix):
        found = keys.parse_key(obj.key)
        if found is None:
            continue
        if date and found[1].strftime("%Y-%m-%d") != date:
            continue
        if best_ts is None or found[1] > best_ts:
            best, best_ts = obj.key, found[1]

    if not best:
        raise ValueError(f"no backup matched (prefix {prefix!r}, date {date!r})")
    return best


def fetch(cfg: Config, store: ObjectStore, key: str, log: Log) -> Path:
    """Download a backup and check it against the checksum recorded at upload."""
    _, metadata = store.head(key)

    path = Path(cfg.tmp_dir) / Path(key).name
    log.info("downloading backup", event="restore.download", key=key, path=str(path))
    with path.open("wb") as handle:
        store.download(key, handle)

    size, digest = hash_file(path)
    recorded = metadata.get("sha256")
    if recorded is None:
        # An explicitly-named key may predate this service, so this is a loud
        # warning rather than a refusal.
        log.warn(
            "backup has no recorded checksum; its integrity cannot be confirmed",
            event="restore.unverified",
            key=key,
        )
    elif recorded != digest:
        raise ValueError(
            f"checksum mismatch for {key}: object metadata says {recorded}, "
            f"downloaded file is {digest}"
        )
    else:
        log.info(
            "download verified",
            event="restore.verified",
            key=key,
            bytes=size,
            sha256=digest,
        )
    return path


def verify(
    cfg: Config, store: ObjectStore, runner: CommandRunner, args: list[str], log: Log
) -> int:
    parser = _parser("verify")
    _selection_flags(parser)
    opts = parser.parse_args(args)

    key = resolve_key(cfg, store, opts.key, opts.date, opts.db, opts.latest)
    path = fetch(cfg, store, key, log)
    try:
        entries = pgtool.toc_entries(runner, str(path))
        pgtool.verify_readable(runner, str(path))
    finally:
        path.unlink(missing_ok=True)

    sys.stdout.write(f"{key} is restorable: {entries} objects in the archive.\n")
    return 0


def restore(
    cfg: Config, store: ObjectStore, runner: CommandRunner, args: list[str], log: Log
) -> int:
    parser = _parser("restore")
    _selection_flags(parser)
    parser.add_argument("--confirm-database", default="", help="must match RESTORE_DB")
    parser.add_argument(
        "--execute",
        action="store_true",
        help="actually write to the target; without this the command is a dry run",
    )
    parser.add_argument(
        "--force-production",
        action="store_true",
        help="permit a target identical to the backup source",
    )
    opts = parser.parse_args(args)

    target = load_restore_target()
    overridden = opts.force_production and (
        os.environ.get("ALLOW_RESTORE_TO_PRODUCTION", "").strip().lower() == "true"
    )
    check_not_source(target, cfg, overridden)
    if is_source(target, cfg):
        log.warn(
            "restoring into the backup source database",
            event="restore.production",
            host=target.host,
            database=target.database,
        )

    key = resolve_key(cfg, store, opts.key, opts.date, opts.db, opts.latest)
    path = fetch(cfg, store, key, log)

    try:
        entries = pgtool.toc_entries(runner, str(path))
        pgtool.verify_readable(runner, str(path))

        sys.stdout.write(f"\nBackup:   {key}\n")
        sys.stdout.write(f"Archive:  {entries} objects\n")
        sys.stdout.write(
            f"Target:   {target.user}@{target.host}:{target.port}/{target.database}\n\n"
        )

        if not opts.execute:
            sys.stdout.write("Dry run: nothing was written. Re-run with --execute to restore.\n")
            return 0

        if opts.confirm_database != target.database:
            raise ValueError(
                f"--confirm-database {opts.confirm_database!r} does not match "
                f"RESTORE_DB {target.database!r}"
            )

        log.info(
            "restoring",
            event="restore.start",
            key=key,
            host=target.host,
            database=target.database,
        )
        conn = pgtool.Conn(
            host=target.host,
            port=target.port,
            user=target.user,
            password=target.password,
            database=target.database,
        )
        try:
            pgtool.restore(runner, conn, str(path))
        except Exception as exc:
            raise ValueError(
                f"restore failed; --single-transaction means the target is unchanged: {exc}"
            ) from exc

        log.info("restore complete", event="restore.finish", key=key, database=target.database)
        return 0
    finally:
        path.unlink(missing_ok=True)


def human_bytes(n: int) -> str:
    unit = 1024
    if n < unit:
        return f"{n} B"
    div, exp = unit, 0
    while n / div >= unit:
        div *= unit
        exp += 1
    return f"{n / div:.1f} {'KMGTPE'[exp]}B"
