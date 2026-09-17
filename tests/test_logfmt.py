"""Log formatting and the bounded tail attached to failure pings."""

from __future__ import annotations

import logging
import threading

import pytest

from postgres_backup.logfmt import Log, LogfmtFormatter, Tail, quote


def render(level: int, message: str, fields: dict[str, object] | None = None) -> str:
    record = logging.LogRecord("t", level, "f", 1, message, None, None)
    record.pgb_fields = fields or {}  # type: ignore[attr-defined]
    return LogfmtFormatter().format(record)


# ------------------------------------------------------------------- the tail


def test_tail_keeps_the_most_recent_bytes() -> None:
    tail = Tail(10)

    tail.write("abcdefghijklmno")

    text = tail.text()
    assert text.endswith("fghijklmno")
    assert "truncated" in text, "the tail did not mark that earlier output was dropped"


def test_tail_under_capacity_is_verbatim() -> None:
    tail = Tail(100)
    tail.write("hello")
    assert tail.text() == "hello"


def test_tail_evicts_across_writes() -> None:
    tail = Tail(10)
    for chunk in ("aaaa", "bbbb", "cccc"):
        tail.write(chunk)

    text = tail.text()
    assert text.endswith("bbcccc")
    assert "aaaa" not in text


def test_tail_is_concurrency_safe() -> None:
    tail = Tail(1024)

    def hammer() -> None:
        for _ in range(20):
            tail.write("concurrent write\n")
            tail.text()

    threads = [threading.Thread(target=hammer) for _ in range(20)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    assert tail.text()


# -------------------------------------------------------------- the formatter


def test_record_renders_as_logfmt() -> None:
    line = render(logging.INFO, "dumping database", {"event": "dump.start", "database": "app"})

    assert "level=INFO" in line
    assert 'msg="dumping database"' in line
    assert "event=dump.start" in line
    assert "database=app" in line


def test_warning_is_rendered_as_warn() -> None:
    """slog spells it WARN, and Loki queries match on the Go build's spelling."""
    assert "level=WARN" in render(logging.WARNING, "careful")
    assert "level=WARNING" not in render(logging.WARNING, "careful")


def test_fields_keep_their_insertion_order() -> None:
    line = render(logging.INFO, "m", {"event": "e", "first": 1, "second": 2, "third": 3})

    assert line.index("first=") < line.index("second=") < line.index("third=")


@pytest.mark.parametrize(
    ("value", "expected"),
    [
        (True, "true"),
        (False, "false"),
        (None, '""'),
        ("", '""'),
        ("plain", "plain"),
        ("has space", '"has space"'),
        ("has=equals", '"has=equals"'),
        (42, "42"),
        ('say "hi"', '"say \\"hi\\""'),
    ],
)
def test_quote(value: object, expected: str) -> None:
    assert quote(value) == expected


def test_timestamp_is_utc_with_milliseconds() -> None:
    line = render(logging.INFO, "m")
    stamp = line.split(" ", 1)[0].removeprefix("time=")

    assert stamp.endswith("Z"), "slog renders UTC as Z, not +00:00"
    assert len(stamp) == len("2026-09-17T23:00:14.123Z")


# ------------------------------------------------------------------- the facade


@pytest.mark.parametrize(
    "name", ["name", "module", "filename", "process", "msg", "args", "levelname", "lineno"]
)
def test_a_field_named_after_a_record_attribute_does_not_crash(
    name: str, caplog: pytest.LogCaptureFixture
) -> None:
    """Splatting these through `extra=` raises KeyError inside logging itself.

    Nesting the fields under one attribute is what makes the whole namespace
    safe, so a future log field cannot take the service down at 23:00.
    """
    log = Log(logging.getLogger("pgbackup.collide"))

    with caplog.at_level(logging.INFO, logger="pgbackup.collide"):
        log.info("m", **{name: "value"})

    assert render(logging.INFO, "m", {name: "value"}).endswith(f"{name}=value")
    assert caplog.records


def test_log_facade_emits_through_the_logging_module(
    caplog: pytest.LogCaptureFixture,
) -> None:
    log = Log(logging.getLogger("pgbackup.test"))

    with caplog.at_level(logging.INFO, logger="pgbackup.test"):
        log.info("hello", event="test.event", count=3)

    assert caplog.records
    fields = caplog.records[0].pgb_fields  # type: ignore[attr-defined]
    assert fields == {"event": "test.event", "count": 3}


# -------------------------------------------------------------- the log contract


#: Every event key the service can emit. Loki queries are written against these,
#: so a rename is a breaking change and must be a deliberate one.
EXPECTED_EVENTS = {
    "boot",
    "cleanup.failed",
    "config.no_healthchecks",
    "config.warning",
    "dump.finish",
    "dump.start",
    "globals.uploaded",
    "healthcheck.ping",
    "healthcheck.ping_failed",
    "pg.output",
    "preflight.client",
    "preflight.database",
    "preflight.disk",
    "preflight.ok",
    "preflight.size",
    "preflight.size_failed",
    "preflight.storage",
    "prune.deleted",
    "prune.dry_run",
    "prune.finish",
    "prune.plan",
    "prune.skipped",
    "restore.download",
    "restore.finish",
    "restore.production",
    "restore.start",
    "restore.unverified",
    "restore.verified",
    "run.error",
    "run.finish",
    "run.panic",
    "run.start",
    "schedule.catchup",
    "schedule.staleness_ok",
    "schedule.staleness_unknown",
    "schedule.waiting",
    "shutdown",
    "upload.finish",
    "upload.skipped",
    "upload.start",
    "verify.ok",
}


def test_event_keys_match_the_published_contract() -> None:
    """Fails if an event key is added, removed or renamed without intent."""
    import re
    from pathlib import Path

    import postgres_backup

    source_dir = Path(postgres_backup.__file__).parent
    found: set[str] = set()
    pattern = re.compile(r'event="([a-z_.]+)"')
    for path in source_dir.glob("*.py"):
        found.update(pattern.findall(path.read_text()))

    assert found == EXPECTED_EVENTS, (
        f"missing from code: {EXPECTED_EVENTS - found}\n"
        f"undocumented in the contract: {found - EXPECTED_EVENTS}"
    )
