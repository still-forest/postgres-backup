"""Structured logging: logfmt to stdout, plus a bounded tail for failure pings.

The ``event=`` key on every record is the contract Loki queries are written
against; ``tests/test_logfmt.py`` asserts the full set the service can emit.

Output mirrors what Go's ``log/slog`` TextHandler produced, so dashboards and
queries written against the Go build keep working:

    time=2026-09-17T23:00:14.123Z level=INFO msg="dumping database" \
        event=dump.start database=manor_production
"""

from __future__ import annotations

import logging
import sys
import threading
from datetime import UTC, datetime
from typing import Any

#: Structured fields are carried in a single LogRecord attribute rather than
#: splatted via ``extra=``. The logging module raises KeyError if ``extra``
#: collides with a reserved record attribute ("name", "module", "filename",
#: "process", ...), which would turn an innocuous new log field into a crash at
#: 23:00 -- the exact class of failure this service exists to eliminate.
_FIELDS_ATTR = "pgb_fields"

#: slog spells these WARN and Go's level names are what queries match on.
_LEVEL_NAMES = {
    logging.DEBUG: "DEBUG",
    logging.INFO: "INFO",
    logging.WARNING: "WARN",
    logging.ERROR: "ERROR",
    logging.CRITICAL: "ERROR",
}

_LEVELS = {
    "debug": logging.DEBUG,
    "info": logging.INFO,
    "warn": logging.WARNING,
    "warning": logging.WARNING,
    "error": logging.ERROR,
}


class Tail:
    """A fixed-capacity byte buffer keeping the most recent writes.

    Used to attach the end of a run's log to a Healthchecks failure ping, so the
    alert carries diagnostics rather than just "it failed". Safe for concurrent use.
    """

    def __init__(self, capacity: int) -> None:
        self._capacity = max(capacity, 1)
        self._buf = bytearray()
        self._dropped = False
        self._lock = threading.Lock()

    def write(self, data: str) -> None:
        raw = data.encode("utf-8", "replace")
        with self._lock:
            if len(raw) >= self._capacity:
                self._buf = bytearray(raw[-self._capacity :])
                self._dropped = True
                return
            overflow = len(self._buf) + len(raw) - self._capacity
            if overflow > 0:
                del self._buf[:overflow]
                self._dropped = True
            self._buf.extend(raw)

    def text(self) -> str:
        with self._lock:
            body = self._buf.decode("utf-8", "replace")
            if self._dropped:
                return "[...earlier output truncated...]\n" + body
            return body


def quote(value: Any) -> str:
    """Render a value for logfmt, quoting only when it would otherwise be ambiguous."""
    if value is None:
        return '""'
    if isinstance(value, bool):
        return "true" if value else "false"
    text = f"{value:.9g}" if isinstance(value, float) else str(value)
    if text == "":
        return '""'
    if any(c in text for c in ' ="') or any(c.isspace() for c in text):
        escaped = (
            text.replace("\\", "\\\\")
            .replace('"', '\\"')
            .replace("\n", "\\n")
            .replace("\t", "\\t")
            .replace("\r", "\\r")
        )
        return f'"{escaped}"'
    return text


class LogfmtFormatter(logging.Formatter):
    """Renders records as ``time=... level=INFO msg="..." key=value``."""

    def format(self, record: logging.LogRecord) -> str:
        dt = datetime.fromtimestamp(record.created, tz=UTC)
        stamp = dt.strftime("%Y-%m-%dT%H:%M:%S.") + f"{dt.microsecond // 1000:03d}Z"

        parts = [
            f"time={stamp}",
            f"level={_LEVEL_NAMES.get(record.levelno, record.levelname)}",
            f"msg={quote(record.getMessage())}",
        ]
        fields: dict[str, Any] = getattr(record, _FIELDS_ATTR, {})
        parts.extend(f"{key}={quote(value)}" for key, value in fields.items())

        line = " ".join(parts)
        if record.exc_info:
            line += "\n" + self.formatException(record.exc_info)
        return line


class TailHandler(logging.Handler):
    """Mirrors formatted records into a Tail."""

    def __init__(self, tail: Tail) -> None:
        super().__init__()
        self._tail = tail

    def emit(self, record: logging.LogRecord) -> None:
        try:
            self._tail.write(self.format(record) + "\n")
        except Exception:  # pragma: no cover - logging must never raise
            self.handleError(record)


class Log:
    """A slog-shaped facade, so call sites read ``log.info(msg, event=..., k=v)``."""

    def __init__(self, logger: logging.Logger | None = None) -> None:
        self._logger = logger or logging.getLogger("pgbackup")

    # The message is positional-only so that a structured field may itself be
    # called "msg" without colliding with the parameter name.
    def debug(self, message: str, /, **fields: Any) -> None:
        self._emit(logging.DEBUG, message, fields)

    def info(self, message: str, /, **fields: Any) -> None:
        self._emit(logging.INFO, message, fields)

    def warn(self, message: str, /, **fields: Any) -> None:
        self._emit(logging.WARNING, message, fields)

    def error(self, message: str, /, **fields: Any) -> None:
        self._emit(logging.ERROR, message, fields)

    def _emit(self, level: int, message: str, fields: dict[str, Any]) -> None:
        self._logger.log(level, message, extra={_FIELDS_ATTR: fields})


def configure(level: str, tail_bytes: int) -> tuple[Log, Tail]:
    """Install logfmt logging on stdout and return the facade and tail buffer.

    Container logs are the primary way a run is observed. Python block-buffers
    stdout when it is a pipe -- which is exactly what Docker provides -- so the
    image also sets PYTHONUNBUFFERED=1; without it the final lines of a crashing
    run are lost.
    """
    tail = Tail(tail_bytes)
    formatter = LogfmtFormatter()

    stream = logging.StreamHandler(sys.stdout)
    stream.setFormatter(formatter)

    mirror = TailHandler(tail)
    mirror.setFormatter(formatter)

    root = logging.getLogger()
    root.handlers.clear()
    root.addHandler(stream)
    root.addHandler(mirror)
    root.setLevel(_LEVELS.get(level.strip().lower(), logging.INFO))

    # boto3 is extremely chatty at DEBUG and would drown the run's own output.
    for noisy in ("boto3", "botocore", "s3transfer", "urllib3"):
        logging.getLogger(noisy).setLevel(logging.WARNING)

    return Log(), tail
