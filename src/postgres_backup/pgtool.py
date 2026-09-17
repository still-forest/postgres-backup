"""Driving pg_dump, pg_restore and psql."""

from __future__ import annotations

import re
from dataclasses import dataclass

from .commands import Command, CommandError, CommandRunner

_VERSION_RE = re.compile(r"\(PostgreSQL\)\s+(\d+)\.")


@dataclass(frozen=True)
class Conn:
    """A target cluster and database."""

    host: str
    port: int
    user: str
    password: str
    database: str

    def env(self) -> dict[str, str]:
        """Connection settings destined for the child process environment.

        The password is deliberately passed this way: anything in argv is
        visible in the process table and in logged command lines.
        """
        return {
            "PGPASSWORD": self.password,
            "PGHOST": self.host,
            "PGPORT": str(self.port),
            "PGUSER": self.user,
            "PGCONNECT_TIMEOUT": "10",
            "PGAPPNAME": "manor-pgbackup",
        }


def client_major(runner: CommandRunner, binary: str) -> int:
    """Return the major version reported by ``binary``."""
    out = runner.run(Command(path=binary, args=["--version"])).stdout
    m = _VERSION_RE.search(out)
    if m is None:
        raise CommandError(binary, f"could not parse {binary} version from {out.strip()!r}")
    return int(m[1])


def server_major(runner: CommandRunner, conn: Conn) -> int:
    """Return the major version of the connected server."""
    raw = _psql(runner, conn, "SHOW server_version_num").strip()
    try:
        return int(raw) // 10000
    except ValueError as exc:
        raise CommandError("psql", f"unexpected server_version_num {raw!r}") from exc


def database_size(runner: CommandRunner, conn: Conn) -> int:
    """Return the on-disk size of the connected database in bytes."""
    raw = _psql(runner, conn, f"SELECT pg_database_size('{conn.database}')").strip()
    return int(raw)


def ping(runner: CommandRunner, conn: Conn) -> None:
    """Verify the database accepts queries."""
    _psql(runner, conn, "SELECT 1")


def _psql(runner: CommandRunner, conn: Conn, query: str) -> str:
    return runner.run(
        Command(
            path="psql",
            args=[
                "--no-psqlrc",
                "--tuples-only",
                "--no-align",
                "--dbname",
                conn.database,
                "--command",
                query,
            ],
            env=conn.env(),
        )
    ).stdout


def dump(
    runner: CommandRunner,
    conn: Conn,
    path: str,
    compress: str = "",
    timeout: float | None = None,
) -> None:
    """Write a custom-format dump of ``conn.database`` to ``path``."""
    args = [
        "--format=custom",
        "--no-owner",
        "--no-privileges",
        "--verbose",
        f"--file={path}",
        f"--dbname={conn.database}",
    ]
    if compress:
        args.append(f"--compress={compress}")

    runner.run(
        Command(path="pg_dump", args=args, env=conn.env(), log_stderr_as="pg_dump"),
        timeout=timeout,
    )


def dump_globals(runner: CommandRunner, conn: Conn, path: str) -> None:
    """Write cluster-wide roles and tablespaces to ``path``.

    A per-database dump carries neither, so a restore without these produces a
    database whose grants reference roles that do not exist.
    """
    runner.run(
        Command(
            path="pg_dumpall",
            args=["--globals-only", "--no-role-passwords"],
            env=conn.env(),
            stdout_path=path,
            log_stderr_as="pg_dumpall",
        )
    )


def toc_entries(runner: CommandRunner, path: str) -> int:
    """Count the table-of-contents entries in an archive.

    A structurally valid but nearly empty archive is the classic symptom of
    having dumped the wrong database.
    """
    out = runner.run(Command(path="pg_restore", args=["--list", path])).stdout
    return sum(1 for line in out.splitlines() if line.strip() and not line.startswith(";"))


def verify_readable(runner: CommandRunner, path: str) -> None:
    """Read the whole archive, decompressing every data block.

    This is a strictly stronger check than ``toc_entries``, which only parses the
    table of contents at the head of the file: a dump truncated halfway through
    still lists its full contents and exits zero. Restoring to /dev/null is what
    actually forces the data to be read, and it is the only check that catches
    truncation.
    """
    try:
        runner.run(Command(path="pg_restore", args=["--file=/dev/null", path]))
    except CommandError as exc:
        raise CommandError(
            "pg_restore",
            f"archive is not fully readable: {exc}: {exc.stderr}",
            stderr=exc.stderr,
        ) from exc


def restore(runner: CommandRunner, conn: Conn, path: str) -> None:
    """Load an archive into ``conn.database``.

    ``--single-transaction`` means a failed restore leaves the target untouched
    rather than partially written, and ``--exit-on-error`` prevents pg_restore's
    default of logging errors and still exiting zero -- which is the restore-side
    equivalent of the silent failure this service exists to eliminate.
    """
    runner.run(
        Command(
            path="pg_restore",
            args=[
                "--clean",
                "--if-exists",
                "--no-owner",
                "--no-privileges",
                "--single-transaction",
                "--exit-on-error",
                "--verbose",
                f"--dbname={conn.database}",
                path,
            ],
            env=conn.env(),
            log_stderr_as="pg_restore",
        )
    )
