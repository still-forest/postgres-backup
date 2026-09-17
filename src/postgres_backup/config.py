"""Loading and validating every setting at process start.

Nothing here is read lazily. The service this replaces discovered its
configuration inside a cron job at 23:00, where a failure produced no output;
loading and validating everything up front means a misconfigured container fails
immediately and visibly at deploy time instead.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from .goconv import parse_bool, parse_duration
from .schedule import load_zone, parse_hhmm

GIB = 1024 * 1024 * 1024
MIB = 1024 * 1024


class ConfigError(Exception):
    """Configuration is unusable. The message lists every problem found."""


@dataclass(frozen=True)
class Postgres:
    host: str
    port: int
    user: str
    password: str
    databases: list[str]
    compress: str
    timeout: float


@dataclass(frozen=True)
class R2:
    access_key_id: str
    secret_access_key: str
    bucket: str
    bucket_path: str
    host_base: str


@dataclass(frozen=True)
class Schedule:
    at: str
    tz: str
    run_on_boot: bool
    max_backup_age: float


@dataclass(frozen=True)
class Retention:
    daily_days: int
    monthly_months: int
    min_keep: int
    max_deletes: int
    allow_bulk: bool


@dataclass(frozen=True)
class Limits:
    min_free_bytes: int
    min_dump_bytes: int
    min_toc_entries: int


@dataclass(frozen=True)
class Config:
    postgres: Postgres
    r2: R2
    schedule: Schedule
    retention: Retention
    limits: Limits
    healthchecks_url: str
    log_level: str
    log_tail_bytes: int
    tmp_dir: str
    dry_run: bool
    prune_enabled: bool

    def redacted(self) -> dict[str, Any]:
        """The effective configuration for the startup log, with secrets removed."""
        return {
            "postgres_host": self.postgres.host,
            "postgres_port": self.postgres.port,
            "postgres_user": self.postgres.user,
            "databases": ",".join(self.postgres.databases),
            "bucket": self.r2.bucket,
            "bucket_path": self.r2.bucket_path,
            "host_base": self.r2.host_base,
            "backup_at": self.schedule.at,
            "backup_tz": self.schedule.tz,
            "run_on_boot": self.schedule.run_on_boot,
            "max_backup_age": f"{self.schedule.max_backup_age:g}s",
            "retention_daily_days": self.retention.daily_days,
            "retention_monthly_months": self.retention.monthly_months,
            "retention_min_keep": self.retention.min_keep,
            "prune_max_deletes": self.retention.max_deletes,
            "prune_allow_bulk": self.retention.allow_bulk,
            "prune_enabled": self.prune_enabled,
            "dry_run": self.dry_run,
            "tmp_dir": self.tmp_dir,
            "healthchecks_configured": bool(self.healthchecks_url),
        }


#: Variables meaningful to the kartoza-based container this service replaces.
#: They are reported at startup so a stale environment is visible rather than
#: silently ignored.
LEGACY_VARS: dict[str, str] = {
    "DEFAULT_REGION": (
        'R2 signs with region "auto"; DEFAULT_REGION was a jurisdiction hint, '
        "never a signing region"
    ),
    "HOST_BUCKET": "only meaningful to s3cmd",
    "CONSOLE_LOGGING": "logs always go to stdout",
    "STORAGE_BACKEND": "only S3-compatible storage is supported",
    "RUN_ONCE": (
        "for a single backup that exits, use the `backup` subcommand; "
        "to back up at startup and then stay running on the schedule, set RUN_ON_BOOT=true"
    ),
    "POSTGRES_MAJOR_VERSION": "baked into the image",
    "POSTGIS_MAJOR_VERSION": "baked into the image",
    "POSTGIS_MINOR_RELEASE": "baked into the image",
    "DB_DUMP_ENCRYPTION": "not supported; R2 encrypts at rest",
    "CRON_SCHEDULE": "use BACKUP_AT and BACKUP_TZ",
}


@dataclass
class _Loader:
    errors: list[str] = field(default_factory=list)

    def fail(self, message: str) -> None:
        self.errors.append(message)

    def string(self, name: str, default: str = "") -> str:
        value = os.environ.get(name, "")
        return value if value.strip() else default

    def required(self, name: str) -> str:
        value = os.environ.get(name, "")
        if not value.strip():
            self.fail(f"{name} is required but empty")
        return value

    def integer(self, name: str, default: int) -> int:
        raw = os.environ.get(name, "")
        if not raw.strip():
            return default
        try:
            return int(raw.strip())
        except ValueError:
            self.fail(f"{name} must be an integer, got {raw!r}")
            return default

    def boolean(self, name: str, default: bool) -> bool:
        raw = os.environ.get(name, "")
        if not raw.strip():
            return default
        parsed = parse_bool(raw)
        if parsed is None:
            self.fail(f"{name} must be a boolean, got {raw!r}")
            return default
        return parsed

    def duration(self, name: str, default: float) -> float:
        raw = os.environ.get(name, "")
        if not raw.strip():
            return default
        seconds = parse_duration(raw)
        if seconds is None or seconds < 0:
            self.fail(f"{name} must be a non-negative duration like 26h, got {raw!r}")
            return default
        return seconds


def load() -> tuple[Config, list[str]]:
    """Read configuration from the environment and validate it.

    Every problem is collected and reported together, so a misconfigured deploy
    is fixed in one pass rather than one restart per mistake.
    """
    ld = _Loader()

    postgres_host = ld.required("POSTGRES_HOST")
    postgres_user = ld.required("POSTGRES_USER")
    postgres_pass = ld.required("POSTGRES_PASS")
    access_key_id = ld.required("ACCESS_KEY_ID")
    secret_access_key = ld.required("SECRET_ACCESS_KEY")
    bucket = ld.required("BUCKET")
    host_base = ld.required("HOST_BASE")

    databases = [p.strip() for p in os.environ.get("POSTGRES_DB", "").split(",") if p.strip()]
    if not databases:
        ld.fail("POSTGRES_DB is required (comma-separated for multiple databases)")

    port = ld.integer("POSTGRES_PORT", 5432)
    if not 1 <= port <= 65535:
        ld.fail(f"POSTGRES_PORT must be 1-65535, got {port}")

    healthchecks_url = ld.string("HEALTHCHECKS_URL")

    # http is permitted so the service can be exercised against a local MinIO or
    # a local ping listener; production use of either is flagged below.
    insecure: list[str] = []
    for name, value in (("HOST_BASE", host_base), ("HEALTHCHECKS_URL", healthchecks_url)):
        if not value:
            continue
        parsed = urlparse(value)
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            ld.fail(f"{name} must be an absolute http or https URL, got {value!r}")
        elif parsed.scheme == "http":
            insecure.append(name)

    backup_at = ld.string("BACKUP_AT", "23:00")
    try:
        parse_hhmm(backup_at)
    except ValueError:
        ld.fail(f"BACKUP_AT must be zero-padded HH:MM within a day, got {backup_at!r}")

    backup_tz = ld.string("BACKUP_TZ", "UTC")
    try:
        load_zone(backup_tz)
    except ValueError:
        ld.fail(f"BACKUP_TZ is not a known IANA timezone: {backup_tz!r}")

    daily_days = ld.integer("RETENTION_DAILY_DAYS", 30)
    monthly_months = ld.integer("RETENTION_MONTHLY_MONTHS", 12)
    min_keep = ld.integer("RETENTION_MIN_KEEP", 3)
    max_deletes = ld.integer("PRUNE_MAX_DELETES", 50)
    for setting, count in (
        ("RETENTION_DAILY_DAYS", daily_days),
        ("RETENTION_MONTHLY_MONTHS", monthly_months),
        ("RETENTION_MIN_KEEP", min_keep),
        ("PRUNE_MAX_DELETES", max_deletes),
    ):
        if count < 1:
            ld.fail(f"{setting} must be at least 1, got {count}")

    # A dedicated directory owned by the container user and backed by a mounted
    # volume, not shared scratch space: the dump must stay off the writable
    # overlay layer. Writability is verified below before anything relies on it.
    tmp_dir = ld.string("TMP_DIR", "/var/tmp/pgbackup")  # noqa: S108
    problem = check_writable(tmp_dir)
    if problem is not None:
        ld.fail(f"TMP_DIR {tmp_dir!r} is not writable: {problem}")

    cfg = Config(
        postgres=Postgres(
            host=postgres_host,
            port=port,
            user=postgres_user,
            password=postgres_pass,
            databases=databases,
            compress=ld.string("PGDUMP_COMPRESS"),
            timeout=ld.duration("PGDUMP_TIMEOUT", 2 * 3600),
        ),
        r2=R2(
            access_key_id=access_key_id,
            secret_access_key=secret_access_key,
            bucket=bucket,
            bucket_path=os.environ.get("BUCKET_PATH", "").strip("/"),
            host_base=host_base,
        ),
        schedule=Schedule(
            at=backup_at,
            tz=backup_tz,
            run_on_boot=ld.boolean("RUN_ON_BOOT", False),
            max_backup_age=ld.duration("MAX_BACKUP_AGE", 26 * 3600),
        ),
        retention=Retention(
            daily_days=daily_days,
            monthly_months=monthly_months,
            min_keep=min_keep,
            max_deletes=max_deletes,
            allow_bulk=ld.boolean("PRUNE_ALLOW_BULK", False),
        ),
        limits=Limits(
            min_free_bytes=ld.integer("MIN_FREE_BYTES", 2 * GIB),
            min_dump_bytes=ld.integer("MIN_DUMP_BYTES", MIB),
            min_toc_entries=ld.integer("MIN_TOC_ENTRIES", 10),
        ),
        healthchecks_url=healthchecks_url,
        log_level=ld.string("LOG_LEVEL", "info"),
        log_tail_bytes=ld.integer("LOG_TAIL_BYTES", 10 * 1024),
        tmp_dir=tmp_dir,
        dry_run=ld.boolean("DRY_RUN", False),
        prune_enabled=ld.boolean("PRUNE_ENABLED", True),
    )

    if ld.errors:
        raise ConfigError("invalid configuration:\n  - " + "\n  - ".join(ld.errors))

    warnings = legacy_warnings()
    warnings += [
        f"{name} uses http, not https; its traffic would cross the network in the clear"
        for name in insecure
    ]
    return cfg, warnings


def check_writable(directory: str) -> str | None:
    """Verify the directory exists and accepts a write.

    An actual create-and-remove, not a stat: a stat alone passes on a read-only
    mount, and the failure would then surface mid-dump instead of at startup.
    """
    probe = Path(directory) / ".pgbackup-write-probe"
    try:
        Path(directory).mkdir(parents=True, exist_ok=True)
        probe.touch()
        probe.unlink()
    except OSError as exc:
        return str(exc)
    return None


def legacy_warnings() -> list[str]:
    return [
        f"{name} is set but ignored: {why}"
        for name, why in LEGACY_VARS.items()
        if name in os.environ
    ]
