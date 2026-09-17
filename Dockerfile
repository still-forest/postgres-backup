# syntax=docker/dockerfile:1

FROM golang:1.27-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG GIT_SHA=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.gitSHA=${GIT_SHA}" \
      -o /out/pgbackup ./cmd/pgbackup

FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40

# The version assertion is the point of installing the client this way rather
# than inheriting the much larger postgres:18 image: if the package ever
# resolves to a different major, the build fails instead of the backup.
# The package version itself is deliberately not pinned, because apk indexes
# are mutable and an -r0 to -r1 bump would break the build for no benefit.
RUN apk add --no-cache ca-certificates postgresql18-client \
 && pg_dump    --version | grep -Eq '^pg_dump \(PostgreSQL\) 18\.' \
 && pg_restore --version | grep -Eq '^pg_restore \(PostgreSQL\) 18\.'

COPY --from=build /out/pgbackup /usr/local/bin/pgbackup

RUN adduser -D -u 10001 pgbackup \
 && mkdir -p /var/tmp/pgbackup \
 && chown pgbackup:pgbackup /var/tmp/pgbackup

USER 10001
ENV TMP_DIR=/var/tmp/pgbackup

# Keeps the dump off the container's writable layer. Mount a real volume here,
# sized for at least three times the compressed dump.
VOLUME ["/var/tmp/pgbackup"]

ENTRYPOINT ["/usr/local/bin/pgbackup"]
CMD ["serve"]
