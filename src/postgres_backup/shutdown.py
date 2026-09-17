"""Cooperative cancellation for SIGINT and SIGTERM.

Python's default SIGTERM disposition terminates the interpreter without
unwinding: no ``finally`` runs, no ``atexit`` fires, and no failure ping is sent.
Installing a handler is therefore not a nicety -- it is the only thing that makes
reporting a shutdown mid-run possible at all.

The Go original got this from ``signal.NotifyContext``, and sent its ``/fail``
ping on a *detached* context because the run context was already cancelled.
The Python equivalent of that detachment is simply that the pinger never
consults this object. See ``healthcheck.HttpPinger``.
"""

from __future__ import annotations

import signal
import threading
from types import FrameType


class Shutdown:
    """A flag set by SIGINT/SIGTERM, with an interruptible wait."""

    def __init__(self) -> None:
        self._event = threading.Event()
        self.signal_number: int | None = None

    def install(self) -> None:
        """Register the handler. Must be called from the main thread."""
        for sig in (signal.SIGINT, signal.SIGTERM):
            signal.signal(sig, self._handle)

    def _handle(self, signum: int, _frame: FrameType | None) -> None:
        self.signal_number = signum
        self._event.set()

    @property
    def requested(self) -> bool:
        return self._event.is_set()

    def wait(self, seconds: float) -> bool:
        """Sleep up to ``seconds``. Returns True if shutdown was requested.

        A signal arriving while the main thread blocks here runs the Python-level
        handler, which sets the event and releases the wait, so a SIGTERM at
        10:00 does not sit out the remaining thirteen hours until the backup
        window. That behaviour is asserted by a real-signal subprocess test
        rather than reasoned about.
        """
        return self._event.wait(max(seconds, 0))

    def set(self) -> None:
        """Request shutdown programmatically. For tests."""
        self._event.set()
