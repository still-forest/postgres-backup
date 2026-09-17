# postgres-backup

Daily PostgreSQL 18 backups to S3-compatible storage (Cloudflare R2), with
verification, retention and restore.

Replaces a fork of `kartoza/docker-pg-backup`, whose cron path could fail
without producing any output at all. The guiding constraint here is that
**no failure mode is allowed to be silent.**

## How it avoids silent failure

| Mechanism | What it catches |
| --- | --- |
| All configuration parsed and validated at startup | Missing or malformed settings, reported all at once, before the service starts waiting |
| Preflight on boot: database query, `pg_dump` version, bucket read/write probe | Rotated or wrong credentials, unreachable database, wrong client version — at deploy time, not at 23:00 |
| Self-scheduled in-process timer | No cron, no crontab, no marshalling the environment through a generated shell file |
| Structured logs to stdout at every step | Ordinary container logs, one `event=` key per step; picked up by Alloy and shipped to Loki automatically |
| `/fail` ping from a `defer` that also recovers panics | Any exit path, including a panic or SIGTERM mid-run |
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

The default is deliberately `RUN_ON_BOOT=false`. Coolify redeploys can happen
several times a day, and each one would otherwise dump production. The staleness
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
  -e RESTORE_PASS=... -e RESTORE_DB=manor_scratch \
  ghcr.io/still-forest/manor-postgres-backup:production \
  restore --latest --execute --confirm-database manor_scratch
```

## Development

Requires Go (see `go.mod`) and Docker.

```sh
go test ./... -race          # unit tests; no database or bucket needed
go vet ./...
docker build -t manor-postgres-backup:test .
```

The interfaces in `internal/exec`, `internal/objstore` and `internal/schedule`
exist so the orchestration, retention rules and schedule arithmetic can be
tested against fakes rather than a live database, bucket or clock.

To exercise it end to end against a throwaway database and a local MinIO, see
the procedure in `test/integration`.

> `docker compose up postgres` from the repo root does not work: the root
> `docker-compose.yml` still references `./services/postgres`, which no longer
> exists. Run `docker run --rm -e POSTGRES_PASSWORD=... postgres:18` directly.
