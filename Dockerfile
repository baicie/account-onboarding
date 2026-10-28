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
ARG GOLANG_IMAGE=golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af
ARG RUNTIME_IMAGE=debian:bookworm-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
# Frontend assets (Vite + React + Tailwind) are compiled into internal/api/webdist and
# embedded by go:embed, so the runtime image still ships a single binary.
FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm ci --no-audit --no-fund || npm install --no-audit --no-fund
COPY web/ ./
RUN npm run build
FROM ${GOLANG_IMAGE} AS build

ARG VERSION=0.1.0
ARG COMMIT=unknown
WORKDIR /src
RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY . .

COPY --from=web /internal/api/webdist ./internal/api/webdist
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/baicie/account-onboarding/internal/api.Version=${VERSION} -X github.com/baicie/account-onboarding/internal/api.Commit=${COMMIT}" \
    -o /out/onboardd ./cmd/onboardd

FROM ${RUNTIME_IMAGE} AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata bash util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 10001 onboard \
    && useradd -u 10001 -g 10001 -M -s /usr/sbin/nologin onboard \
    && mkdir -p /app/data \
    && chown 10001:10001 /app/data

COPY --from=build /out/onboardd /app/onboardd
COPY scripts/materialize_secrets.sh /app/scripts/materialize_secrets.sh
RUN chmod 0755 /app/scripts/materialize_secrets.sh

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
