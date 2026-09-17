"""Test doubles for the three seams: commands, object storage and the clock."""

from __future__ import annotations

import io
import threading
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from typing import BinaryIO

from postgres_backup.commands import Command, Result
from postgres_backup.store import Object


@dataclass
class Stub:
    """A canned response for one matched invocation."""

    #: Compared against Command.path; empty matches any path.
    match: str = ""
    stdout: str = ""
    stderr: str = ""
    error: Exception | None = None
    #: Runs after the canned output, for tests that need to materialise the file
    #: the real command would have written.
    do: Callable[[Command], None] | None = None


class FakeRunner:
    """A CommandRunner that records invocations and replays stubs.

    Stubs are consumed in order; any invocation past the end of the list
    succeeds silently.
    """

    def __init__(self, stubs: list[Stub] | None = None) -> None:
        self.stubs: list[Stub] = list(stubs or [])
        self.calls: list[Command] = []

    def run(self, command: Command, timeout: float | None = None) -> Result:
        self.calls.append(command)

        for index, stub in enumerate(self.stubs):
            if stub.match and stub.match != command.path:
                continue
            del self.stubs[index]
            if stub.do is not None:
                stub.do(command)
            if stub.error is not None:
                raise stub.error
            return Result(stdout=stub.stdout, stderr=stub.stderr)

        return Result(stdout="", stderr="")

    def calls_to(self, path: str) -> list[Command]:
        return [c for c in self.calls if c.path == path]


class FakeStore:
    """An in-memory ObjectStore."""

    def __init__(self) -> None:
        self._objects: dict[str, tuple[bytes, dict[str, str], datetime]] = {}
        self._lock = threading.Lock()
        self.list_error: Exception | None = None
        self.upload_error: Exception | None = None
        self.delete_error: Exception | None = None
        self.deleted: list[str] = []
        self.now: Callable[[], datetime] = lambda: datetime.now(UTC)

    def seed(self, key: str, size: int, modified: datetime) -> None:
        """Insert an object directly, bypassing upload_error."""
        with self._lock:
            self._objects[key] = (b"\0" * size, {}, modified)

    def upload(self, key: str, body: BinaryIO, metadata: dict[str, str]) -> None:
        if self.upload_error is not None:
            raise self.upload_error
        data = body.read()
        with self._lock:
            self._objects[key] = (data, dict(metadata or {}), self.now())

    def list_objects(self, prefix: str) -> list[Object]:
        if self.list_error is not None:
            raise self.list_error
        with self._lock:
            return sorted(
                (
                    Object(key=k, size=len(v[0]), last_modified=v[2])
                    for k, v in self._objects.items()
                    if k.startswith(prefix)
                ),
                key=lambda o: o.key,
            )

    def download(self, key: str, dest: BinaryIO) -> None:
        with self._lock:
            entry = self._objects.get(key)
        if entry is None:
            raise KeyError(f"no such key {key!r}")
        dest.write(entry[0])

    def head(self, key: str) -> tuple[Object, dict[str, str]]:
        with self._lock:
            entry = self._objects.get(key)
        if entry is None:
            raise KeyError(f"no such key {key!r}")
        data, metadata, modified = entry
        return Object(key=key, size=len(data), last_modified=modified), dict(metadata)

    def delete(self, key: str) -> None:
        if self.delete_error is not None:
            raise self.delete_error
        with self._lock:
            self._objects.pop(key, None)
            self.deleted.append(key)

    def stored_keys(self) -> list[str]:
        with self._lock:
            return sorted(self._objects)


class StrippedMetaStore:
    """Wraps a FakeStore but reports no user metadata.

    Exercises the rule that an absent checksum is a failure, not a skip.
    """

    def __init__(self, inner: FakeStore) -> None:
        self._inner = inner

    def __getattr__(self, name: str) -> object:
        return getattr(self._inner, name)

    def head(self, key: str) -> tuple[Object, dict[str, str]]:
        obj, _ = self._inner.head(key)
        return obj, {}


class MismatchedMetaStore:
    """Wraps a FakeStore but reports a checksum that disagrees with the upload."""

    def __init__(self, inner: FakeStore) -> None:
        self._inner = inner

    def __getattr__(self, name: str) -> object:
        return getattr(self._inner, name)

    def head(self, key: str) -> tuple[Object, dict[str, str]]:
        obj, metadata = self._inner.head(key)
        metadata["sha256"] = "0" * 64
        return obj, metadata


@dataclass
class RecordingPinger:
    """Captures the Healthchecks calls a run makes."""

    started: bool = False
    succeeded: bool = False
    failed: bool = False
    start_body: str = ""
    success_body: str = ""
    fail_body: str = ""

    def start(self, body: str) -> None:
        self.started = True
        self.start_body = body

    def success(self, body: str) -> None:
        self.succeeded = True
        self.success_body = body

    def fail(self, body: str) -> None:
        self.failed = True
        self.fail_body = body


@dataclass
class FakeClock:
    """A Clock that advances only when the scheduler sleeps."""

    current: datetime = field(default_factory=lambda: datetime(2026, 9, 17, 10, 0, tzinfo=UTC))
    slept: list[float] = field(default_factory=list)
    #: Number of sleeps to allow before reporting a shutdown request.
    stop_after: int | None = None

    def now(self) -> datetime:
        return self.current

    def sleep_until_or_stop(self, seconds: float) -> bool:
        self.slept.append(seconds)
        self.current = self.current + timedelta(seconds=seconds)
        return self.stop_after is not None and len(self.slept) >= self.stop_after


def reader(data: bytes) -> BinaryIO:
    return io.BytesIO(data)
