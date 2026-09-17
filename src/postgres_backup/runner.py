"""Orchestrating a backup run: preflight, dump, verify, upload, prune."""

from __future__ import annotations

import hashlib
import io
import shutil
from collections.abc import Callable
from datetime import UTC, datetime
from pathlib import Path

from . import keys, pgtool, prune
from .commands import CommandRunner
from .config import Config
from .goconv import format_duration
from .healthcheck import Pinger
from .logfmt import Log
from .store import Object, ObjectStore

#: The PostgreSQL major version this service is built against.
EXPECTED_PG_MAJOR = 18

#: Leads the failure message when the backup itself was stored successfully and
#: only the prune afterwards failed.
PRUNE_FAILED_PREFIX = "BACKUP UPLOADED OK - PRUNE FAILED"


class PreflightError(Exception):
    """A dependency the service needs is unavailable or wrong."""


class BackupError(Exception):
    """A backup run failed."""


class BackupRunner:
    """Performs backup runs.

    Every dependency is injected so a full run can be exercised in tests with no
    database, no bucket and no clock.
    """

    def __init__(
        self,
        cfg: Config,
        store: ObjectStore,
        runner: CommandRunner,
        ping: Pinger,
        log: Log,
        now: Callable[[], datetime] | None = None,
        tail: Callable[[], str] | None = None,
    ) -> None:
        self.cfg = cfg
        self.store = store
        self.runner = runner
        self.ping = ping
        self.log = log
        self._now = now or (lambda: datetime.now(UTC))
        self._tail = tail or (lambda: "")

    def now(self) -> datetime:
        return self._now().astimezone(UTC)

    def conn(self, database: str) -> pgtool.Conn:
        pg = self.cfg.postgres
        return pgtool.Conn(
            host=pg.host,
            port=pg.port,
            user=pg.user,
            password=pg.password,
            database=database,
        )

    # ------------------------------------------------------------------ preflight

    def preflight(self) -> None:
        """Verify everything a run depends on, before waiting for the window.

        This is the check whose absence let a rotated R2 key go unnoticed for a
        week: bad credentials now fail at deploy time, loudly, instead of at
        23:00 in silence.
        """
        try:
            major = pgtool.client_major(self.runner, "pg_dump")
        except Exception as exc:
            raise PreflightError(f"preflight: {exc}") from exc
        if major != EXPECTED_PG_MAJOR:
            raise PreflightError(
                f"preflight: pg_dump is major version {major}, expected {EXPECTED_PG_MAJOR}"
            )
        self.log.info("pg_dump client verified", event="preflight.client", major=major)

        for db in self.cfg.postgres.databases:
            conn = self.conn(db)
            try:
                pgtool.ping(self.runner, conn)
            except Exception as exc:
                raise PreflightError(f"preflight: database {db!r} unreachable: {exc}") from exc
            try:
                server = pgtool.server_major(self.runner, conn)
            except Exception as exc:
                raise PreflightError(
                    f"preflight: reading server version for {db!r}: {exc}"
                ) from exc
            if major < server:
                raise PreflightError(
                    f"preflight: pg_dump {major} is older than server {server} for {db!r}; "
                    "the dump would be unusable"
                )
            self.log.info(
                "database reachable",
                event="preflight.database",
                database=db,
                server_major=server,
            )

        check_access = getattr(self.store, "check_access", None)
        if callable(check_access):
            try:
                check_access()
            except Exception as exc:
                raise PreflightError(f"preflight: {exc}") from exc

        # Deliberately not a parseable backup key: if this probe ever leaks, it
        # is classified unknown and kept rather than silently pruned.
        probe = keys.build_key(self.cfg.r2.bucket_path, ".preflight", self.now()) + ".probe"
        try:
            self.store.upload(probe, io.BytesIO(b"ok"), {})
        except Exception as exc:
            raise PreflightError(f"preflight: object storage is not writable: {exc}") from exc
        try:
            self.store.delete(probe)
        except Exception as exc:
            raise PreflightError(f"preflight: object storage is not deletable: {exc}") from exc

        self.log.info(
            "object storage verified",
            event="preflight.storage",
            bucket=self.cfg.r2.bucket,
            prefix=self.cfg.r2.bucket_path,
        )

    # ------------------------------------------------------------------- run once

    def run_once(self) -> None:
        """Perform one full backup of every configured database.

        Owns the Healthchecks lifecycle: /start on entry, success or /fail on
        exit. The caller only has to log the exception.
        """
        started = self.now()
        databases = ",".join(self.cfg.postgres.databases)
        self.log.info(
            "backup run starting",
            event="run.start",
            databases=databases,
            dry_run=self.cfg.dry_run,
        )
        self.ping.start(
            f"starting backup of {databases} at {started.strftime('%Y-%m-%dT%H:%M:%SZ')}"
        )

        try:
            for db in self.cfg.postgres.databases:
                try:
                    self._backup_database(db, started)
                except Exception as exc:
                    raise BackupError(f"database {db!r}: {exc}") from exc
        except BaseException as exc:
            # BaseException, not Exception: a KeyboardInterrupt or a SystemExit
            # mid-run must still report. This is the Python analogue of the Go
            # original's deferred recover().
            duration = (self.now() - started).total_seconds()
            self.log.error(
                "backup run failed",
                event="run.finish",
                outcome="failure",
                duration_seconds=duration,
                error=str(exc),
            )
            self.ping.fail(f"FAILED after {format_duration(duration)}: {exc}\n\n{self._tail()}")
            raise

        duration = (self.now() - started).total_seconds()
        self.log.info(
            "backup run succeeded",
            event="run.finish",
            outcome="success",
            duration_seconds=duration,
        )
        self.ping.success(f"OK in {format_duration(duration)}")

    # -------------------------------------------------------------- one database

    def _backup_database(self, db: str, stamp: datetime) -> None:
        conn = self.conn(db)
        self._check_space(conn, db)

        tmp_dir = Path(self.cfg.tmp_dir)
        part = tmp_dir / f"{db}-{int(stamp.timestamp() * 1e9)}.dump.part"
        final = part.with_suffix("")  # strips ".part"
        globals_path = tmp_dir / f"{db}-{int(stamp.timestamp() * 1e9)}.globals.sql"

        try:
            self.log.info("dumping database", event="dump.start", database=db, path=str(part))
            dump_started = self.now()
            pgtool.dump(
                self.runner,
                conn,
                str(part),
                self.cfg.postgres.compress,
                timeout=self.cfg.postgres.timeout,
            )
            part.rename(final)

            size, digest = _hash_file(final)
            self.log.info(
                "dump complete",
                event="dump.finish",
                database=db,
                bytes=size,
                sha256=digest,
                duration_seconds=(self.now() - dump_started).total_seconds(),
            )

            entries = self._verify(str(final), size)

            key = keys.build_key(self.cfg.r2.bucket_path, db, stamp)
            metadata = {
                "sha256": digest,
                "database": db,
                "started-at": stamp.strftime("%Y-%m-%dT%H:%M:%SZ"),
                "toc-entries": str(entries),
                "pg-dump-major": str(EXPECTED_PG_MAJOR),
                "source-host": self.cfg.postgres.host,
                "content-length": str(size),
            }

            if self.cfg.dry_run:
                self.log.warn(
                    "dry run: skipping upload",
                    event="upload.skipped",
                    database=db,
                    key=key,
                    bytes=size,
                )
                return

            self._upload(key, final, metadata, size, digest)
            self._upload_globals(conn, db, stamp, globals_path)

            # The dump is safely stored by this point. A prune failure still has
            # to raise the alarm -- a bucket that grows forever is its own
            # outage -- but the alert must not read as a lost backup.
            try:
                self._prune(db, key)
            except Exception as exc:
                raise BackupError(f"{PRUNE_FAILED_PREFIX}: {exc}") from exc
        finally:
            for path in (part, final, globals_path):
                try:
                    path.unlink(missing_ok=True)
                except OSError as exc:
                    self.log.warn(
                        "could not remove temporary file",
                        event="cleanup.failed",
                        path=str(path),
                        error=str(exc),
                    )

    def _check_space(self, conn: pgtool.Conn, db: str) -> None:
        free = shutil.disk_usage(self.cfg.tmp_dir).free
        required = self.cfg.limits.min_free_bytes

        try:
            db_size = pgtool.database_size(self.runner, conn)
        except Exception as exc:
            self.log.warn(
                "could not measure database size; falling back to MIN_FREE_BYTES",
                event="preflight.size_failed",
                database=db,
                error=str(exc),
            )
        else:
            # A custom-format dump compresses, so the live size is a generous
            # upper bound; requiring it outright keeps a full disk from being
            # discovered halfway through a dump.
            required = max(required, db_size)
            self.log.info(
                "database size measured", event="preflight.size", database=db, bytes=db_size
            )

        if free < required:
            raise BackupError(
                f"insufficient space in {self.cfg.tmp_dir}: {free} bytes free, need {required}"
            )
        self.log.info(
            "disk space sufficient",
            event="preflight.disk",
            free_bytes=free,
            required_bytes=required,
        )

    def _verify(self, path: str, size: int) -> int:
        """Four independent checks on the archive.

        Together they catch a truncated dump, an unreadable dump, and a
        structurally valid dump of the wrong (or an empty) database.
        """
        if size < self.cfg.limits.min_dump_bytes:
            raise BackupError(
                f"dump is {size} bytes, below the {self.cfg.limits.min_dump_bytes} byte minimum"
            )

        entries = pgtool.toc_entries(self.runner, path)
        if entries < self.cfg.limits.min_toc_entries:
            raise BackupError(
                f"dump lists {entries} objects, below the "
                f"{self.cfg.limits.min_toc_entries} minimum; it may be of the wrong database"
            )

        # Reading the archive in full is the only check that catches truncation:
        # a half-written dump still carries a complete table of contents and
        # passes the check above.
        pgtool.verify_readable(self.runner, path)

        self.log.info("dump verified", event="verify.ok", toc_entries=entries, bytes=size)
        return entries

    def _upload(
        self, key: str, path: Path, metadata: dict[str, str], size: int, digest: str
    ) -> None:
        self.log.info("uploading dump", event="upload.start", key=key, bytes=size)
        started = self.now()

        with path.open("rb") as handle:
            self.store.upload(key, handle, metadata)

        obj, stored_meta = self.store.head(key)
        if obj.size != size:
            raise BackupError(f"uploaded object is {obj.size} bytes, expected {size}")

        # ETag is deliberately not compared: for a multipart upload it is not an
        # MD5 of the content, so the round-tripped checksum metadata is the
        # check that actually means something. Absence is treated as a failure
        # rather than skipped, because this run set it moments ago -- silently
        # accepting a missing checksum would be the same blind spot in a new place.
        if "sha256" not in stored_meta:
            raise BackupError("uploaded object has no sha256 metadata; it cannot be verified")
        if stored_meta["sha256"] != digest:
            raise BackupError(
                f"uploaded object checksum {stored_meta['sha256']} does not match local {digest}"
            )

        self.log.info(
            "upload confirmed",
            event="upload.finish",
            key=key,
            bytes=obj.size,
            duration_seconds=(self.now() - started).total_seconds(),
        )

    def _upload_globals(self, conn: pgtool.Conn, db: str, stamp: datetime, path: Path) -> None:
        pgtool.dump_globals(self.runner, conn, str(path))
        key = keys.globals_key(self.cfg.r2.bucket_path, db, stamp)
        size = path.stat().st_size
        with path.open("rb") as handle:
            self.store.upload(key, handle, {"database": db})
        self.log.info("globals uploaded", event="globals.uploaded", key=key, bytes=size)

    # ----------------------------------------------------------------------- prune

    def _prune(self, db: str, current_key: str = "") -> None:
        if not self.cfg.prune_enabled:
            self.log.info("pruning disabled", event="prune.skipped", database=db)
            return

        prefix = f"{self.cfg.r2.bucket_path}/{db}".lstrip("/")
        objects = self.store.list_objects(prefix)

        # The globals sidecars share the prefix but not the dump key format, so
        # they would be classed unknown and kept forever. Drop them from the
        # listing and let each one follow the dump it belongs to. This also means
        # the percentage guard is computed over dumps only, which is intended.
        dumps: list[Object] = []
        sidecars: dict[str, Object] = {}
        for obj in objects:
            if obj.key.endswith(keys.GLOBALS_SUFFIX):
                dump_key = obj.key.removesuffix(keys.GLOBALS_SUFFIX) + keys.DUMP_EXT
                sidecars[dump_key] = obj
            else:
                dumps.append(obj)

        policy = prune.Policy(
            daily_days=self.cfg.retention.daily_days,
            monthly_months=self.cfg.retention.monthly_months,
            min_keep=self.cfg.retention.min_keep,
            max_deletes=self.cfg.retention.max_deletes,
            allow_bulk=self.cfg.retention.allow_bulk,
        )
        plan = prune.plan(dumps, policy, self.now())
        if current_key:
            plan.protect(current_key)

        self.log.info(
            "prune planned",
            event="prune.plan",
            database=db,
            keep=len(plan.keep),
            delete=len(plan.delete),
            unknown=len(plan.unknown),
        )

        if self.cfg.dry_run:
            for obj in plan.delete:
                self.log.warn("dry run: would delete", event="prune.dry_run", key=obj.key)
            return

        for obj in plan.delete:
            self.store.delete(obj.key)
            self.log.info("pruned", event="prune.deleted", key=obj.key, bytes=obj.size)

            sidecar = sidecars.get(obj.key)
            if sidecar is not None:
                self.store.delete(sidecar.key)
                self.log.info("pruned", event="prune.deleted", key=sidecar.key, bytes=sidecar.size)

    def prune_database(self, db: str) -> None:
        """Apply the retention policy to ``db`` outside of a backup run."""
        self._prune(db)

    def newest_backup(self, db: str) -> datetime | None:
        """Timestamp of the most recent parseable backup of ``db``, if any.

        Used to decide whether a restart has left the schedule stale.
        """
        prefix = f"{self.cfg.r2.bucket_path}/{db}".lstrip("/")
        newest: datetime | None = None
        for obj in self.store.list_objects(prefix):
            found = keys.parse_key(obj.key)
            if found is not None and (newest is None or found[1] > newest):
                newest = found[1]
        return newest


def hash_file(path: str | Path) -> tuple[int, str]:
    """Return the size and SHA-256 of a local file."""
    return _hash_file(Path(path))


def _hash_file(path: Path) -> tuple[int, str]:
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
            size += len(chunk)
    return size, digest.hexdigest()


__all__ = [
    "EXPECTED_PG_MAJOR",
    "PRUNE_FAILED_PREFIX",
    "BackupError",
    "BackupRunner",
    "PreflightError",
    "hash_file",
]
