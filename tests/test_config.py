"""Configuration loading. A misconfigured deploy must fail loudly and at once."""

from __future__ import annotations

import pytest

from postgres_backup.config import ConfigError, load


def test_load_defaults(valid_env: None) -> None:
    cfg, warnings = load()

    assert warnings == []
    assert cfg.schedule.at == "23:00"
    assert cfg.schedule.tz == "UTC"
    assert cfg.schedule.run_on_boot is False
    assert cfg.schedule.max_backup_age == 26 * 3600
    assert cfg.retention.daily_days == 30
    assert cfg.retention.monthly_months == 12
    assert cfg.retention.min_keep == 3
    assert cfg.retention.max_deletes == 50
    assert cfg.retention.allow_bulk is False
    assert cfg.prune_enabled is True
    assert cfg.postgres.port == 5432
    assert cfg.postgres.timeout == 2 * 3600


@pytest.mark.parametrize(
    "name",
    [
        "POSTGRES_HOST",
        "POSTGRES_USER",
        "POSTGRES_PASS",
        "POSTGRES_DB",
        "ACCESS_KEY_ID",
        "SECRET_ACCESS_KEY",
        "BUCKET",
        "HOST_BASE",
    ],
)
def test_load_rejects_missing_required(
    valid_env: None, monkeypatch: pytest.MonkeyPatch, name: str
) -> None:
    monkeypatch.setenv(name, "")

    with pytest.raises(ConfigError, match=name):
        load()


def test_load_reports_all_problems_at_once(
    valid_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Fixed in one pass, rather than one restart per mistake."""
    monkeypatch.setenv("POSTGRES_HOST", "")
    monkeypatch.setenv("BUCKET", "")
    monkeypatch.setenv("BACKUP_AT", "nope")

    with pytest.raises(ConfigError) as excinfo:
        load()

    message = str(excinfo.value)
    for expected in ("POSTGRES_HOST", "BUCKET", "BACKUP_AT"):
        assert expected in message


@pytest.mark.parametrize(
    ("key", "value"),
    [
        pytest.param("POSTGRES_PORT", "abc", id="port-not-a-number"),
        pytest.param("POSTGRES_PORT", "70000", id="port-out-of-range"),
        pytest.param("BACKUP_AT", "25:00", id="hour-out-of-range"),
        pytest.param("BACKUP_AT", "9:00", id="not-zero-padded"),
        pytest.param("BACKUP_TZ", "Mars/Olympus", id="unknown-timezone"),
        pytest.param("HOST_BASE", "not a url", id="host-base-not-a-url"),
        pytest.param("HOST_BASE", "ftp://acct.example.com", id="host-base-bad-scheme"),
        pytest.param("HEALTHCHECKS_URL", "ftp://example.com/ping", id="ping-bad-scheme"),
        pytest.param("RETENTION_DAILY_DAYS", "0", id="retention-zero"),
        pytest.param("RETENTION_MONTHLY_MONTHS", "-1", id="retention-negative"),
        pytest.param("RETENTION_MIN_KEEP", "0", id="min-keep-zero"),
        pytest.param("PRUNE_MAX_DELETES", "0", id="max-deletes-zero"),
        pytest.param("PRUNE_ENABLED", "yesplease", id="bad-boolean"),
        pytest.param("MAX_BACKUP_AGE", "one day", id="bad-duration"),
        pytest.param("MAX_BACKUP_AGE", "26", id="duration-without-unit"),
        pytest.param("PGDUMP_TIMEOUT", "2d", id="duration-unknown-unit"),
    ],
)
def test_load_rejects_malformed_values(
    valid_env: None, monkeypatch: pytest.MonkeyPatch, key: str, value: str
) -> None:
    monkeypatch.setenv(key, value)

    with pytest.raises(ConfigError):
        load()


@pytest.mark.parametrize(
    ("value", "expected"),
    [("T", True), ("true", True), ("1", True), ("FALSE", False), ("f", False), ("0", False)],
)
def test_boolean_accepts_go_literals(
    valid_env: None, monkeypatch: pytest.MonkeyPatch, value: str, expected: bool
) -> None:
    """These env vars are already set in production by the Go build."""
    monkeypatch.setenv("RUN_ON_BOOT", value)

    cfg, _ = load()

    assert cfg.schedule.run_on_boot is expected


@pytest.mark.parametrize(
    ("value", "seconds"),
    [("26h", 93600.0), ("2h45m", 9900.0), ("1.5h", 5400.0), ("90m", 5400.0), ("30s", 30.0)],
)
def test_duration_accepts_go_literals(
    valid_env: None, monkeypatch: pytest.MonkeyPatch, value: str, seconds: float
) -> None:
    monkeypatch.setenv("MAX_BACKUP_AGE", value)

    cfg, _ = load()

    assert cfg.schedule.max_backup_age == seconds


def test_load_parses_multiple_databases(valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("POSTGRES_DB", " manor_production , manor_analytics ,, ")

    cfg, _ = load()

    assert cfg.postgres.databases == ["manor_production", "manor_analytics"]


def test_load_trims_bucket_path(valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("BUCKET_PATH", "/manor_db/")

    cfg, _ = load()

    assert cfg.r2.bucket_path == "manor_db"


def test_load_warns_about_legacy_vars(valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
    """A stale environment should be visible rather than silently ignored."""
    monkeypatch.setenv("DEFAULT_REGION", "apac")
    monkeypatch.setenv("CONSOLE_LOGGING", "True")
    monkeypatch.setenv("RUN_ONCE", "true")

    _, warnings = load()

    joined = "\n".join(warnings)
    for expected in ("DEFAULT_REGION", "CONSOLE_LOGGING", "RUN_ONCE"):
        assert expected in joined


def test_run_once_warning_points_at_both_replacements(
    valid_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("RUN_ONCE", "true")

    _, warnings = load()

    warning = next(w for w in warnings if w.startswith("RUN_ONCE"))
    assert "backup` subcommand" in warning
    assert "RUN_ON_BOOT=true" in warning


def test_http_endpoints_are_allowed_but_warned(
    valid_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    """http must work for local MinIO testing, but never pass unremarked."""
    monkeypatch.setenv("HOST_BASE", "http://localhost:9000")

    cfg, warnings = load()

    assert cfg.r2.host_base == "http://localhost:9000"
    assert any("HOST_BASE uses http" in w for w in warnings)


def test_redacted_omits_secrets(valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("POSTGRES_PASS", "pgpw-sentinel-9f3a")
    monkeypatch.setenv("SECRET_ACCESS_KEY", "r2secret-sentinel-4b7c")
    monkeypatch.setenv("ACCESS_KEY_ID", "r2keyid-sentinel-1d2e")

    cfg, _ = load()
    rendered = " ".join(f"{k}={v}" for k, v in cfg.redacted().items())

    for secret in ("pgpw-sentinel-9f3a", "r2secret-sentinel-4b7c", "r2keyid-sentinel-1d2e"):
        assert secret not in rendered
    # Guards against the assertion above passing on an empty rendering.
    assert "db.internal" in rendered


def test_unwritable_tmp_dir_is_rejected(valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("TMP_DIR", "/proc/definitely-not-writable/pgbackup")

    with pytest.raises(ConfigError, match="TMP_DIR"):
        load()
