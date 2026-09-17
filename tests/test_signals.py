"""Signal handling, exercised with real signals rather than reasoned about.

Python's default SIGTERM disposition kills the interpreter without unwinding, so
no ``finally`` runs and no failure ping is sent. The handler is what makes
reporting a shutdown possible at all, and that it actually interrupts a long
wait is too load-bearing to assert from a fake clock.
"""

from __future__ import annotations

import os
import signal
import subprocess
import sys
import threading
import time

from postgres_backup.shutdown import Shutdown


def test_wait_returns_false_when_no_signal_arrives() -> None:
    assert Shutdown().wait(0.01) is False


def test_wait_returns_immediately_once_set() -> None:
    shutdown = Shutdown()
    shutdown.set()

    started = time.monotonic()
    assert shutdown.wait(30) is True
    assert time.monotonic() - started < 1, "a set event must not sleep out the timeout"


def test_requested_reflects_the_flag() -> None:
    shutdown = Shutdown()
    assert shutdown.requested is False
    shutdown.set()
    assert shutdown.requested is True


def test_a_real_sigterm_interrupts_a_long_wait() -> None:
    """The scheduler sleeps on this for up to a day; SIGTERM must not wait it out."""
    shutdown = Shutdown()
    shutdown.install()

    result: list[bool] = []

    def waiter() -> None:
        result.append(shutdown.wait(30))

    # The handler only runs on the main thread, so the wait happens there and the
    # signal is delivered from a helper.
    def fire() -> None:
        time.sleep(0.2)
        os.kill(os.getpid(), signal.SIGTERM)

    original = signal.getsignal(signal.SIGTERM)
    try:
        threading.Thread(target=fire, daemon=True).start()
        started = time.monotonic()
        interrupted = shutdown.wait(30)
        elapsed = time.monotonic() - started
    finally:
        signal.signal(signal.SIGTERM, original)

    assert interrupted is True
    assert elapsed < 5, f"SIGTERM took {elapsed:.1f}s to interrupt the wait"
    assert shutdown.signal_number == signal.SIGTERM
    assert result == []


def test_serve_exits_promptly_on_sigterm() -> None:
    """End to end: a running container must stop when Docker asks it to."""
    code = (
        "import sys, time;"
        "sys.path.insert(0, 'src');"
        "from postgres_backup.shutdown import Shutdown;"
        "s = Shutdown(); s.install();"
        "print('ready', flush=True);"
        "s.wait(120);"
        "print('stopped', flush=True);"
        "sys.exit(0)"
    )
    proc = subprocess.Popen(
        [sys.executable, "-c", code],
        stdout=subprocess.PIPE,
        text=True,
        cwd=_repo_root(),
    )
    try:
        assert proc.stdout is not None
        assert proc.stdout.readline().strip() == "ready"

        proc.send_signal(signal.SIGTERM)
        returncode = proc.wait(timeout=10)
        remaining = proc.stdout.read()
    finally:
        if proc.poll() is None:  # pragma: no cover - only on failure
            proc.kill()

    assert returncode == 0
    assert "stopped" in remaining, "the process died without unwinding"


def _repo_root() -> str:
    from pathlib import Path

    return str(Path(__file__).resolve().parent.parent)
