#!/usr/bin/env bash
# R6 P0-B: static Dockerfile / .dockerignore security audit (no daemon needed).
# This is NOT a container run; findings are configuration contracts only.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DF="$ROOT/Dockerfile"
IGN="$ROOT/.dockerignore"
PASS=0; FAIL=0; WARN=0
ok()   { PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
warn() { WARN=$((WARN+1)); printf 'WARN %s\n' "$1"; }

grep -q '^ARG GOLANG_IMAGE=golang:1.25-bookworm$' "$DF" \
  && grep -q '^ARG RUNTIME_IMAGE=debian:bookworm-slim$' "$DF" \
  && grep -q '^FROM ${GOLANG_IMAGE} AS build$' "$DF" \
  && grep -q '^FROM ${RUNTIME_IMAGE} AS runtime$' "$DF" \
  && ok "multi-stage build with version-governed base-image ARGs" || bad "multi-stage/base ARGs"
grep -qE '^FROM [^ ]+:latest([[:space:]]|$)' "$DF" && bad "base image uses :latest" || ok "no :latest base image"
grep -q '^ARG GOLANG_IMAGE=.*@sha256:' "$DF" && ok "Go base image digest-pinned" \
  || warn "Go base image tag-pinned (digest pinning policy in deploy/BASE_IMAGES.md)"
grep -q '^USER 10001:10001$' "$DF" && ok "runtime USER is 10001:10001" || bad "runtime USER"
grep -q '^HEALTHCHECK --interval=.*--start-period=' "$DF" && ok "HEALTHCHECK with start-period" || bad "HEALTHCHECK missing start-period"
grep -q 'CMD \["/app/onboardd", "healthcheck"\]' "$DF" && ok "HEALTHCHECK uses onboardd healthcheck (readyz-based)" || bad "HEALTHCHECK command"
grep -q '^ENTRYPOINT \["/app/onboardd"\]$' "$DF" && ok "ENTRYPOINT fixed to onboardd" || bad "ENTRYPOINT"
grep -q '^EXPOSE 8788$' "$DF" && ok "EXPOSE 8788" || bad "EXPOSE"
grep -q '^VOLUME \["/app/data"\]$' "$DF" && ok "VOLUME /app/data" || bad "VOLUME"
grep -qE 'ENV[^\n]*(PASSWORD|TOKEN|SECRET|KEY)=' "$DF" && bad "secret-looking ENV value in image" || ok "no secret values baked into image ENV"
grep -q 'deploy/secrets\|deploy/data\|\.sandbox' "$DF" && bad "Dockerfile copies runtime secrets/data" || ok "Dockerfile does not copy secrets/data"
grep -q 'rm -rf /var/lib/apt/lists/\*' "$DF" && ok "apt lists cleaned" || warn "apt lists cleanup not found"
grep -q 'trimpath' "$DF" && ok "go build uses -trimpath" || bad "go build missing -trimpath"
grep -q 'CGO_ENABLED=1' "$DF" && ok "CGO enabled for sqlite driver" || bad "CGO_ENABLED"
grep -qE '^RUN .*--privileged' "$DF" && bad "privileged build step" || ok "no privileged build step"
grep -qE '^FROM .* AS ' "$DF" && ok "named build stage" || warn "build stage not named"

# .dockerignore must keep secrets/runtime data and build output out of context.
check_ignore() { # $1 label, $2 regex matching an exclusion line
  if grep -qxE "$2" "$IGN"; then ok ".dockerignore excludes $1"; else bad ".dockerignore missing $1"; fi
}
check_ignore ".sandbox" '/\.sandbox/?'
check_ignore "root /data" '/data/?'
check_ignore "root /secrets" '/secrets/?'
check_ignore "deploy/data" '/deploy/data/?'
check_ignore "deploy/secrets" '/deploy/secrets/?'
check_ignore "bin" '/bin/?'
check_ignore ".git" '^\.git$'
check_ignore ".env" '^\.env'

# Supply-chain notes (not failures: pinned digests are a documented P2).
grep -qE '^FROM [^ ]+@sha256:' "$DF" && ok "base images pinned by digest" \
  || warn "base image tags are not pinned by digest (documented P2-6)"
grep -qE '^LABEL ' "$DF" && ok "OCI labels present" || warn "no OCI labels (documented P2-6)"

echo "SUMMARY pass=$PASS fail=$FAIL warn=$WARN"
[ "$FAIL" = "0" ]
