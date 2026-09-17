"""Reporting run outcomes. A ping failure must never affect the backup."""

from __future__ import annotations

import threading
import time
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from postgres_backup.healthcheck import HttpPinger, NoopPinger, build
from postgres_backup.shutdown import Shutdown


class Capture:
    def __init__(self) -> None:
        self.paths: list[str] = []
        self.bodies: list[str] = []
        self.status = 200
        self.fail_first = 0
        self._lock = threading.Lock()

    def record(self, path: str, body: str) -> int:
        with self._lock:
            self.paths.append(path)
            self.bodies.append(body)
            if self.fail_first > 0:
                self.fail_first -= 1
                return 500
            return self.status


@pytest.fixture
def server() -> Iterator[tuple[str, Capture]]:
    capture = Capture()

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(length).decode()
            status = capture.record(self.path, body)
            self.send_response(status)
            self.send_header("Content-Length", "0")
            self.end_headers()

        def log_message(self, *args: object) -> None:
            pass

    httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    host, port = httpd.server_address[:2]
    try:
        yield f"http://{host}:{port}/ping/abc", capture
    finally:
        httpd.shutdown()
        httpd.server_close()


def test_ping_paths(server: tuple[str, Capture]) -> None:
    url, capture = server
    pinger = HttpPinger(url)

    pinger.start("starting")
    pinger.success("done")
    pinger.fail("broke")

    assert capture.paths == ["/ping/abc/start", "/ping/abc", "/ping/abc/fail"]
    assert capture.bodies[2] == "broke"


def test_ping_retries_on_server_error(server: tuple[str, Capture]) -> None:
    url, capture = server
    capture.fail_first = 1
    pinger = HttpPinger(url, attempts=3)

    pinger.success("ok")

    assert len(capture.paths) >= 2, "no retry after the 5xx"


def test_body_is_truncated_to_the_limit(server: tuple[str, Capture]) -> None:
    url, capture = server
    pinger = HttpPinger(url, max_body=20)

    pinger.fail("x" * 500 + "TAIL")

    assert len(capture.bodies[0]) == 20
    assert capture.bodies[0].endswith("TAIL"), "truncation must keep the end, where the error is"


def test_ping_failure_does_not_raise_or_hang() -> None:
    """A backup that succeeded but could not be reported is still a good backup."""
    pinger = HttpPinger("http://127.0.0.1:1/ping", attempts=2)

    started = time.monotonic()
    pinger.fail("body")  # must not raise
    assert time.monotonic() - started < 30


def test_noop_when_url_is_empty() -> None:
    pinger = build("", 1024)

    assert isinstance(pinger, NoopPinger)
    pinger.fail("should not raise")


def test_fail_ping_is_sent_even_after_shutdown_is_requested(
    server: tuple[str, Capture],
) -> None:
    """The report sent while the process is being torn down is the vital one.

    The pinger deliberately does not consult Shutdown; making it
    cancellation-aware would suppress exactly the alert that matters most.
    """
    url, capture = server
    shutdown = Shutdown()
    shutdown.set()

    HttpPinger(url).fail("terminated by SIGTERM")

    assert capture.paths == ["/ping/abc/fail"]
    assert capture.bodies[0] == "terminated by SIGTERM"
