# postgres-backup

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Daily PostgreSQL 18 backups to S3-compatible storage (Cloudflare R2), with
verification, retention and restore.

It exists because the tool it replaced stopped backing up a production database
for a week after a credential rotation, and said nothing: its cron path could
fail before its logging was wired up, so there were no logs, no alert and no
non-zero exit. The guiding constraint here is therefore that **no failure mode
is allowed to be silent** — every table and check below follows from that.

## How it avoids silent failure

| Mechanism | What it catches |
| --- | --- |
| All configuration parsed and validated at startup | Missing or malformed settings, reported all at once, before the service starts waiting |
| Preflight on boot: database query, `pg_dump` version, bucket read/write probe | Rotated or wrong credentials, unreachable database, wrong client version — at deploy time, not at 23:00 |
| Self-scheduled in-process timer | No cron, no crontab, no marshalling the environment through a generated shell file |
| A SIGTERM handler, not the default disposition | Python's default would kill the interpreter without unwinding, so no `finally` runs and no failure is ever reported |
| Structured logs to stdout at every step | Ordinary container logs in logfmt, one `event=` key per step, ready for Loki/Grafana or anything else that reads stdout |
| `/fail` ping sent from a `finally` that catches `BaseException` | Any exit path, including SIGTERM or KeyboardInterrupt mid-run |
| Healthchecks period + grace | `SIGKILL`, OOM kill, host reboot, container that never starts — the cases no in-process code can report |

That last row is not optional. Configure the Healthchecks check with a
**period of 1 day and a grace of about 2 hours**, so a *missing* ping raises an
alert on its own. Without it, a dead container is still invisible.

## Commands

```
pgbackup serve              # default: run continuously on the daily schedule
pgbackup backup             # one backup now, then exit
pgbackup prune              # apply the retention policy, then exit
pgbackup list [--db] [--limit]
pgbackup verify --key KEY | --latest | --date YYYY-MM-DD
pgbackup restore (--key KEY | --latest | --date YYYY-MM-DD) [--execute] [--confirm-database NAME]
```

## When backups happen

`serve` backs up at `BACKUP_AT` every day and stays running. It will also run
once at startup, in either of two cases:

- **`RUN_ON_BOOT=true`** — always back up on startup, then continue on the
  schedule. The process stays alive; this is not the old container's
  `RUN_ONCE`, which ran a single backup and exited. For that behaviour, use the
  `backup` subcommand instead.
- **Otherwise, only if the last backup is stale** — at startup the service reads
  the newest backup's timestamp from the bucket, and runs immediately if it is
  older than `MAX_BACKUP_AGE` (26h). This is what stops a container that
  crash-looped through its window from silently skipping a day, without keeping
  any local state.

The default is deliberately `RUN_ON_BOOT=false`. On a platform that redeploys
on every push, each redeploy would otherwise dump production. The staleness
check covers the case that actually matters. Turn it on if you want the
guarantee that every deploy leaves a fresh backup behind, and are content to pay
for a dump per deploy.

A failed startup backup is reported (logged, and pinged to `/fail`) but does not
stop the service: it continues to the next scheduled run. Only a failed
*preflight* refuses to start, because a container that cannot reach its database
or bucket has nothing useful to wait for.

## What a run does

1. Ping `/start`.
2. Check free disk against the live database size.
3. `pg_dump --format=custom` to a temporary file.
4. `pg_dumpall --globals-only` — roles are not in a per-database dump, and a
   restore without them produces grants referencing roles that do not exist.
5. **Verify**, four independent ways: the file is at least `MIN_DUMP_BYTES`,
   `pg_restore --list` exits cleanly, the archive holds at least
   `MIN_TOC_ENTRIES` objects, and `pg_restore --file=/dev/null` reads every data
   block.

   The object-count check is what catches a dump of the wrong or an empty
   database. The full read is what catches truncation: a dump cut off halfway
   still carries a complete table of contents at its head, so `--list` alone
   reports it as healthy and exits zero. That was confirmed against a real
   archive, and it is the reason verification costs a full decompression pass.
6. Upload, then confirm via `HeadObject` that the size and SHA-256 round-tripped.
7. Prune.
8. Ping success, or `/fail` with the tail of the run log.

The dump is written to disk rather than streamed straight to storage because a
dump cannot be verified after it has already been handed over.

## Configuration

Required:

| Variable | Notes |
| --- | --- |
| `POSTGRES_HOST`, `POSTGRES_USER`, `POSTGRES_PASS` | |
| `POSTGRES_DB` | Comma-separated for multiple databases |
| `ACCESS_KEY_ID`, `SECRET_ACCESS_KEY` | R2 credentials |
| `BUCKET`, `HOST_BASE` | `HOST_BASE` is `https://<account>.r2.cloudflarestorage.com` |

Optional:

| Variable | Default | Notes |
| --- | --- | --- |
| `POSTGRES_PORT` | `5432` | |
| `BUCKET_PATH` | *(none)* | Key prefix within the bucket |
| `HEALTHCHECKS_URL` | *(none)* | Strongly recommended |
| `BACKUP_AT` | `23:00` | `HH:MM`, zero-padded |
| `BACKUP_TZ` | `UTC` | IANA zone |
| `RUN_ON_BOOT` | `false` | Back up at startup, then stay running on the schedule |
| `MAX_BACKUP_AGE` | `26h` | Staleness threshold for the catch-up check |
| `RETENTION_DAILY_DAYS` | `30` | |
| `RETENTION_MONTHLY_MONTHS` | `12` | |
| `RETENTION_MIN_KEEP` | `3` | Floor, applied only when a prune would delete something |
| `PRUNE_ENABLED` | `true` | |
| `PRUNE_MAX_DELETES` | `50` | Per-run ceiling |
| `PRUNE_ALLOW_BULK` | `false` | See *First prune* below |
| `DRY_RUN` | `false` | Plan and log, change nothing |
| `TMP_DIR` | `/var/tmp/pgbackup` | Mount a volume here |
| `MIN_FREE_BYTES` | `2147483648` | |
| `MIN_DUMP_BYTES` | `1048576` | **Tune to your database** — see below |
| `MIN_TOC_ENTRIES` | `10` | |
| `PGDUMP_COMPRESS` | *(none)* | e.g. `zstd:3`; `zstd` and `lz4` are both available |
| `PGDUMP_TIMEOUT` | `2h` | |
| `LOG_LEVEL` | `info` | `debug` includes `pg_dump --verbose` output |
| `LOG_TAIL_BYTES` | `10240` | Log tail attached to a failure ping |

> `MIN_DUMP_BYTES` defaults to 1 MiB. A custom-format dump compresses hard — a
> 14 MB database produced a 382 KB dump in testing — so check your real dump
> size and set this to roughly half of it. Too high and every run fails; too low
> and a truncated dump slips through.

### Dropped since the kartoza container

These are ignored, and their presence is logged as a warning at startup:
`DEFAULT_REGION`, `HOST_BUCKET`, `CONSOLE_LOGGING`, `STORAGE_BACKEND`,
`RUN_ONCE`, `CRON_SCHEDULE`, `POSTGRES_MAJOR_VERSION`, `POSTGIS_*`.

`DEFAULT_REGION` is the one behavioural change worth knowing about. R2's S3 API
signs with region `auto`; `apac` was a jurisdiction hint used at bucket
creation, never a signing region.

## Object layout

```
<BUCKET_PATH>/<database>/<YYYY>/<MM>/<database>-<YYYYMMDD>T<HHMMSS>Z.dump
<BUCKET_PATH>/<database>/<YYYY>/<MM>/<database>-<YYYYMMDD>T<HHMMSS>Z.globals.sql
```

The timestamp in the key is the authority for a backup's age, never
`LastModified`, which a copy or re-upload would reset.

## Retention

Everything within `RETENTION_DAILY_DAYS` is kept. Beyond that, the newest backup
of each month survives for `RETENTION_MONTHLY_MONTHS`. Each dump's `globals.sql`
follows it.

A prune aborts entirely, deleting nothing, if any of these hold:

- the listing errored or paged incompletely
- fewer than `RETENTION_MIN_KEEP` backups would survive a plan that deletes something
- more than `PRUNE_MAX_DELETES` objects, or more than half the listing, would go
- the backup written by the current run appears in the delete set

**Keys this service did not write are never deletion candidates.** Objects left
by the previous tool are classified as unknown and kept.

### First prune

The per-run and percentage ceilings assume a steady state of roughly one
deletion a day. The first prune of a bucket with accumulated history will
legitimately exceed both and be refused. Set `PRUNE_ALLOW_BULK=true` for a
single run, ideally alongside `DRY_RUN=true` first to read the plan.

## Restore

The guard against restoring over production is structural rather than a prompt:
**`restore` does not read `POSTGRES_*` at all.** It reads `RESTORE_HOST`,
`RESTORE_PORT`, `RESTORE_USER`, `RESTORE_PASS` and `RESTORE_DB`. Running this
container with its normal environment cannot overwrite the database it backs up,
because the target is simply absent.

On top of that: restore is a dry run unless `--execute` is passed;
`--confirm-database` must match `RESTORE_DB`; the downloaded checksum is checked
against the recorded one before the database is touched; and if the target does
resolve to a backed-up database anyway, it is refused unless both
`--force-production` and `ALLOW_RESTORE_TO_PRODUCTION=true` are set.

`pg_restore` runs with `--single-transaction --exit-on-error`, so a failed
restore leaves the target untouched instead of half-written.

```sh
docker run --rm --env-file prod.env \
  -e RESTORE_HOST=scratch.internal -e RESTORE_USER=postgres \
  -e RESTORE_PASS=... -e RESTORE_DB=myapp_scratch \
  ghcr.io/still-forest/postgres-backup:latest \
  restore --latest --execute --confirm-database myapp_scratch
```

## Development

Requires [uv](https://docs.astral.sh/uv/) and Docker.

```sh
uv sync
uv run pytest                 # unit tests; no database or bucket needed
uv run pytest -m integration  # needs a live PostgreSQL 18 (see below)
uv run ruff check src tests
uv run ruff format src tests
uv run mypy
docker build -t pgbackup:test .
```

The `CommandRunner`, `ObjectStore` and `Clock` protocols exist so the
orchestration, retention rules and schedule arithmetic can be tested against
fakes rather than a live database, bucket or clock. That testability is the
point: the tool this replaces had none.

For the integration suite, point it at a throwaway server:

```sh
docker run --rm -d --name pgb -p 55433:5432 \
  -e POSTGRES_PASSWORD=integration -e POSTGRES_DB=pgbackup_integration postgres:18

PGBACKUP_TEST_HOST=localhost PGBACKUP_TEST_PORT=55433 \
PGBACKUP_TEST_USER=postgres PGBACKUP_TEST_PASS=integration \
PGBACKUP_TEST_DB=pgbackup_integration \
  uv run pytest -m integration
```

### Smoke-testing against real R2

Local MinIO does **not** reproduce R2's rejection of the AWS SDK's default
multi-checksum behaviour, so the botocore configuration in `store.py` is only
truly exercised against R2 itself. Before trusting a change there:

1. Use a scratch prefix (`BUCKET_PATH=..._test`), never the production one.
2. Back up a dump larger than 16 MiB so the multipart path actually runs —
   single-part success does not prove multipart works.
3. Confirm with `pgbackup list` and a `restore --latest` dry run that the
   `sha256` metadata round-tripped.

Two things there remain unverified against production R2 and should be checked
on first deploy: that R2 preserves user metadata across a **multipart** upload
(the upload confirmation would fail loudly if not), and that `HeadBucket`
succeeds with a **bucket-scoped** API token rather than an account-scoped one.
If the latter 403s, the preflight probe should fall back to
`list_objects_v2(MaxKeys=1)`.

## Notes on the port from Go

This service was first written in Go. The Python port preserves the external
contract exactly — the same environment variables, subcommands, object key
scheme, `event=` log keys and exit codes — and equivalence was verified by
running both builds against the same PostgreSQL and MinIO and diffing the
results. Three deliberate differences:

- **`uncompressed` object metadata was dropped.** It was always the constant
  string `"false"` and conveyed nothing. Nothing had consumed it.
- **`GIT_SHA` moves from a linker flag to the `PGBACKUP_GIT_SHA` environment
  variable**, set by the Dockerfile. The `git_sha` log field is unchanged.
- **A SIGTERM during a large multipart upload may take up to the socket timeout
  to unwind**, because `s3transfer`'s worker threads are not interruptible the
  way Go's context cancellation was. `read_timeout` bounds it at 60s.

Three Python-specific hazards are load-bearing and should not be "tidied":

- `PYTHONUNBUFFERED=1` in the Dockerfile. Python block-buffers stdout when it is
  a pipe, which is what a container gives it; without this the final log lines
  of a crashing run are lost.
- The `tzdata` dependency. Alpine ships no zone database, so `BACKUP_TZ` would
  fail to resolve without it.
- `HttpPinger` never consults `Shutdown`. The `/fail` ping sent while the
  process is being torn down is the most important one there is, and making it
  cancellation-aware would suppress exactly the alert that matters most.

## Acknowledgements

This service replaces a deployment of
[kartoza/docker-pg-backup](https://github.com/kartoza/docker-pg-backup), which
is GPL-2.0 licensed. No code from that project is used here: this is an
independent implementation written against the observed behaviour of the tool it
replaced, and it is licensed independently. The environment variable names are
kept compatible so an existing deployment can be migrated without rewriting its
configuration, and several comments describe what the previous tool did in order
to explain why a check exists.

## License

[MIT](LICENSE) © 2026 John Szymanowski
