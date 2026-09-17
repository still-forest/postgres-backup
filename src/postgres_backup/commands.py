"""Running external commands, behind an interface so callers stay testable.

Everything that shells out to pg_dump, pg_restore or psql goes through
``CommandRunner``, so orchestration can be exercised without those binaries.
"""

from __future__ import annotations

import os
import subprocess
import threading
from dataclasses import dataclass, field
from typing import IO, Protocol

from .logfmt import Log
from .shutdown import Shutdown


class CommandError(Exception):
    """An external command failed."""

    def __init__(self, command: str, message: str, stderr: str = "") -> None:
        self.command = command
        self.stderr = stderr
        super().__init__(message)


@dataclass
class Command:
    """A single external command invocation.

    ``env`` is layered on top of the parent environment. Secrets belong here and
    never in ``args``: args get echoed into logs and show up in the process
    table, the environment does not.
    """

    path: str
    args: list[str] = field(default_factory=list)
    env: dict[str, str] = field(default_factory=dict)
    stdout_path: str | None = None
    #: When set, the child's stderr is logged line by line at DEBUG under this
    #: source name, so pg_dump --verbose progress reaches the container logs and
    #: the tail attached to a failure ping.
    log_stderr_as: str | None = None


@dataclass
class Result:
    stdout: str
    stderr: str


class CommandRunner(Protocol):
    def run(self, command: Command, timeout: float | None = None) -> Result: ...


class Subprocess:
    """A CommandRunner backed by :mod:`subprocess`.

    Tracks the running child so a shutdown signal can terminate it, which is
    what lets a SIGTERM mid-dump unwind normally and still report the failure.
    """

    def __init__(self, log: Log | None = None, shutdown: Shutdown | None = None) -> None:
        self._log = log or Log()
        self._shutdown = shutdown
        self._current: subprocess.Popen[str] | None = None
        self._lock = threading.Lock()

    def run(self, command: Command, timeout: float | None = None) -> Result:
        env = {**os.environ, **command.env}
        stdout_file: IO[str] | None = None

        try:
            if command.stdout_path is not None:
                stdout_file = open(command.stdout_path, "w")  # noqa: SIM115, PTH123

            proc = subprocess.Popen(
                [command.path, *command.args],
                env=env,
                stdout=stdout_file or subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            with self._lock:
                self._current = proc

            try:
                # communicate() drains both pipes concurrently; reading them
                # serially would deadlock on a chatty pg_dump --verbose.
                out, err = proc.communicate(timeout=timeout)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.communicate()
                raise CommandError(
                    command.path, f"{command.path} exceeded its {timeout}s timeout"
                ) from None
            finally:
                with self._lock:
                    self._current = None
        finally:
            if stdout_file is not None:
                stdout_file.close()

        err = err or ""
        if command.log_stderr_as is not None:
            self._log_lines(err, command.log_stderr_as)

        if proc.returncode != 0:
            if self._shutdown is not None and self._shutdown.requested:
                raise CommandError(
                    command.path,
                    f"{command.path} was terminated by shutdown",
                    stderr=err.strip(),
                )
            raise CommandError(
                command.path,
                f"{command.path} exited {proc.returncode}",
                stderr=err.strip(),
            )
        return Result(stdout=out or "", stderr=err)

    def terminate_current(self) -> None:
        """Terminate the running child, if any. Called on shutdown."""
        with self._lock:
            proc = self._current
        if proc is not None and proc.poll() is None:
            proc.terminate()

    def _log_lines(self, text: str, source: str) -> None:
        for line in text.splitlines():
            stripped = line.strip()
            if stripped:
                self._log.debug(stripped, event="pg.output", source=source)
