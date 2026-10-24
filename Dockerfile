# syntax=docker/dockerfile:1
# Multi-stage build for account-onboarding.
#
# The SQLite driver (mattn/go-sqlite3) uses cgo, so the builder stage needs a C
# toolchain and the runtime stage keeps the glibc runtime libraries.
#
# Runtime contract (see deploy/compose.yaml):
#   - non-root UID/GID 10001
#   - listens on 8788 inside the container; publish only 127.0.0.1 on the host
#   - /app/data is the only writable path (SQLite + WAL)
#   - /run/secrets/onboarding_master_key is the read-only master key
FROM golang:1.25-bookworm AS build

WORKDIR /src
RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/onboardd ./cmd/onboardd

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 10001 onboard \
    && useradd -u 10001 -g 10001 -M -s /usr/sbin/nologin onboard \
    && mkdir -p /app/data \
    && chown 10001:10001 /app/data

COPY --from=build /out/onboardd /app/onboardd

WORKDIR /app
USER 10001:10001

ENV ONBOARD_LISTEN=0.0.0.0:8788 \
    ONBOARD_DB_PATH=/app/data/state.sqlite \
    ONBOARD_MASTER_KEY_FILE=/run/secrets/onboarding_master_key

EXPOSE 8788
VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=10s \
    CMD ["/app/onboardd", "healthcheck"]

ENTRYPOINT ["/app/onboardd"]
