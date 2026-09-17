# syntax=docker/dockerfile:1

# alpine3.24 specifically: it is the Alpine release carrying postgresql18-client.
# The default python:3.14-alpine tag sits on an older Alpine that only has
# postgresql17-client, which would produce dumps the server cannot vouch for.
ARG PYTHON_IMAGE=python:3.14-alpine3.24@sha256:c6ead215bfd31f1e433d968853b7a769989117115b728874824e6c0a27cb96fc

FROM ${PYTHON_IMAGE} AS build

COPY --from=ghcr.io/astral-sh/uv:0.12.3@sha256:2d890623d310b57771ce840f0da5eed5fc6d657da05ffaa45d82797b53fa3abc /uv /usr/local/bin/uv

ENV UV_PYTHON_DOWNLOADS=never \
    UV_LINK_MODE=copy \
    UV_COMPILE_BYTECODE=1 \
    UV_PROJECT_ENVIRONMENT=/opt/venv

WORKDIR /src

# Dependencies first, so a source-only change does not re-resolve them.
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project

COPY README.md ./
COPY src/ src/
RUN uv sync --frozen --no-dev --no-editable


FROM ${PYTHON_IMAGE}

# The version assertion is the point of installing the client this way rather
# than inheriting the much larger postgres:18 image: if the package ever
# resolves to a different major, the build fails instead of the backup. All four
# binaries are checked because the service shells out to all four.
#
# The package version itself is deliberately not pinned: apk indexes are mutable
# and an -r0 to -r1 bump would break the build for no benefit.
RUN apk add --no-cache ca-certificates postgresql18-client \
 && pg_dump    --version | grep -Eq '^pg_dump \(PostgreSQL\) 18\.' \
 && pg_restore --version | grep -Eq '^pg_restore \(PostgreSQL\) 18\.' \
 && pg_dumpall --version | grep -Eq '^pg_dumpall \(PostgreSQL\) 18\.' \
 && psql       --version | grep -Eq '^psql \(PostgreSQL\) 18\.'

COPY --from=build /opt/venv /opt/venv

ARG GIT_SHA=unknown

ENV PATH="/opt/venv/bin:${PATH}" \
    PGBACKUP_GIT_SHA=${GIT_SHA} \
    TMP_DIR=/var/tmp/pgbackup \
    # Python block-buffers stdout when it is a pipe, which is exactly what a
    # container gives it. Without this the final log lines of a crashing run are
    # lost -- a silent-failure mode introduced by the runtime, not the code.
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    # Stop a stale ambient AWS profile or config file from changing SDK
    # behaviour; credentials come from the configuration, and nowhere else.
    AWS_EC2_METADATA_DISABLED=true \
    AWS_CONFIG_FILE=/dev/null \
    AWS_SHARED_CREDENTIALS_FILE=/dev/null

RUN adduser -D -u 10001 pgbackup \
 && mkdir -p /var/tmp/pgbackup \
 && chown pgbackup:pgbackup /var/tmp/pgbackup

USER 10001

# Keeps the dump off the container's writable layer. Mount a real volume here,
# sized for at least three times the compressed dump.
VOLUME ["/var/tmp/pgbackup"]

ENTRYPOINT ["pgbackup"]
CMD ["serve"]
