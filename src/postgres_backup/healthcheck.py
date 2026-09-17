"""Reporting run outcomes to a Healthchecks-style endpoint."""

from __future__ import annotations

import time
import urllib.error
import urllib.request
from typing import Protocol

from .goconv import format_duration  # noqa: F401  (re-exported for callers)
from .logfmt import Log

_TIMEOUT = 10.0


class Pinger(Protocol):
    def start(self, body: str) -> None: ...

    def success(self, body: str) -> None: ...

    def fail(self, body: str) -> None: ...


class HttpPinger:
    """Pings a Healthchecks check by URL.

    Every ping is best-effort. A backup that succeeded but could not be reported
    is still a good backup, and the check going overdue raises the alarm anyway,
    so a ping failure is logged and never propagated to the caller.

    This class deliberately does not consult ``Shutdown``. That is the whole
    content of the Go original's "detached context": the /fail ping sent while
    the process is being torn down is the single most important one, and making
    it cancellation-aware would silently suppress exactly the report that
    matters most. Do not "tidy" that into cancellation handling.
    """

    def __init__(
        self,
        base_url: str,
        max_body: int = 10 * 1024,
        attempts: int = 3,
        log: Log | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.max_body = max_body
        self.attempts = max(attempts, 1)
        self._log = log or Log()

    def start(self, body: str) -> None:
        self._ping("/start", body)

    def success(self, body: str) -> None:
        self._ping("", body)

    def fail(self, body: str) -> None:
        self._ping("/fail", body)

    def _ping(self, suffix: str, body: str) -> None:
        url = self.base_url + suffix
        payload = _truncate(body, self.max_body)

        last_error: Exception | None = None
        for attempt in range(self.attempts):
            if attempt > 0:
                time.sleep(min(2**attempt, 4))
            try:
                self._post(url, payload)
                self._log.debug("healthcheck pinged", event="healthcheck.ping", url=url)
                return
            except Exception as exc:
                last_error = exc

        self._log.error(
            "healthcheck ping failed; backup outcome is unaffected",
            event="healthcheck.ping_failed",
            url=url,
            error=str(last_error),
        )

    @staticmethod
    def _post(url: str, body: str) -> None:
        request = urllib.request.Request(  # noqa: S310 - scheme validated at config load
            url,
            data=body.encode("utf-8"),
            method="POST",
            headers={"Content-Type": "text/plain; charset=utf-8"},
        )
        with urllib.request.urlopen(request, timeout=_TIMEOUT) as response:  # noqa: S310
            if not 200 <= response.status < 300:
                raise RuntimeError(f"status {response.status}")


class NoopPinger:
    """Discards every ping. Used when no HEALTHCHECKS_URL is configured."""

    def start(self, body: str) -> None: ...

    def success(self, body: str) -> None: ...

    def fail(self, body: str) -> None: ...


def _truncate(body: str, max_body: int) -> str:
    """Keep the last ``max_body`` bytes, which is where the error is."""
    if max_body <= 0:
        return body
    raw = body.encode("utf-8")
    if len(raw) <= max_body:
        return body
    return raw[-max_body:].decode("utf-8", "replace")


def build(base_url: str, max_body: int, log: Log | None = None) -> Pinger:
    if not base_url.strip():
        return NoopPinger()
    return HttpPinger(base_url, max_body=max_body, log=log)
