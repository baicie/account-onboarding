#!/usr/bin/env bash
# Runs ON the target host over ssh (fed to `bash -s` with positional arguments).
#
# Arguments:
#   $1 image repository        (ghcr.io/owner/repo)
#   $2 tag                     (v0.1.0)
#   $3 digest                  (sha256:... of the pushed index; logged, not trusted)
#   $4 container name          (account-onboarding)
#   $5 deploy directory        (/opt/account-onboarding)
#   $6 host port               (18788)
#   $7 compose file path       (already uploaded by the workflow)
#
# Contract: idempotent, fails closed, never leaves the service on a half-deployed
# image, and never prints secret material.
set -euo pipefail

IMAGE="$1"; TAG="$2"; DIGEST="$3"; CONTAINER="$4"; DIR="$5"; HOST_PORT="$6"; COMPOSE="$7"; DOMAINS="${8:-moonbyte.test}"
DATA="$DIR/data"; SECRETS_SRC="$DIR/secrets-src"; SECRETS="$DIR/secrets"; STATE="$DIR/.current-tag"

echo "==> target: $IMAGE:$TAG -> $DIR (container $CONTAINER, 127.0.0.1:$HOST_PORT)"

# docker access: cloud images often have the user in sudo but not in the docker group
if docker info >/dev/null 2>&1; then
  DOCKER=(docker); echo "==> docker: direct"
elif sudo -n true >/dev/null 2>&1; then
  DOCKER=(sudo -n docker); echo "==> docker: via sudo -n"
else
  echo "ERROR: no docker access and no passwordless sudo; add the user to the docker group" >&2
  exit 1
fi
dk() { "${DOCKER[@]}" "$@"; }

# ---- preflight: fail before touching anything ---------------------------------
for f in onboarding_master_key onboard_admin_token; do
  if [ ! -f "$SECRETS_SRC/$f" ]; then
    echo "ERROR: missing $SECRETS_SRC/$f (provision it once, see docs/runbooks/TAG-DEPLOY.md)" >&2
    exit 1
  fi
  mode=$(stat -c "%a" "$SECRETS_SRC/$f" 2>/dev/null || stat -f "%Lp" "$SECRETS_SRC/$f")
  if [ "$mode" != "444" ]; then
    echo "ERROR: $SECRETS_SRC/$f must be mode 0444 (it is $mode); the materializer refuses writable sources" >&2
    exit 1
  fi
done
[ -f "$COMPOSE" ] || { echo "ERROR: compose file $COMPOSE missing" >&2; exit 1; }
mkdir -p "$DATA" "$SECRETS"
chmod 0700 "$SECRETS"
if ! dk image inspect "$IMAGE:$TAG" >/dev/null 2>&1; then
  if [ -f "$DIR/.ghcr-token" ]; then
    echo "==> pulling with the host-stored registry token"
    dk login ghcr.io -u "${GHCR_USER:-token}" --password-stdin < "$DIR/.ghcr-token" >/dev/null
  else
    echo "==> pulling (package assumed public)"
  fi
  dk pull "$IMAGE:$TAG"
else
  echo "==> image already present locally"
fi
pulled=$(dk image inspect "$IMAGE:$TAG" --format "{{.Id}}" 2>/dev/null || true)
echo "==> local image id: $pulled (index digest from CI: $DIGEST)"

# ---- materialize secrets: 0400/10001 files the service accepts -----------------
# The service rejects 0444 sources and symlinks, so the root-owned projection is
# copied by the shipped materializer (the same path the Docker/Kind gates verify).
dk run --rm --user 0:0 \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
  --security-opt no-new-privileges:true \
  -v "$SECRETS_SRC:/src:ro" -v "$SECRETS:/run/secrets" \
  --entrypoint /bin/sh "$IMAGE:$TAG" \
  -c "exec /app/scripts/materialize_secrets.sh --source-dir /src --target-dir /run/secrets --uid 10001 --gid 10001"

# ---- deploy with rollback -----------------------------------------------------
previous=""
if [ -f "$STATE" ]; then previous=$(cat "$STATE"); fi
echo "==> previous tag: ${previous:-<none>}"

# compose needs the four variables; sudo -n clears the environment, so pass them
# explicitly on that path instead of relying on export.
compose_up() {
  local tag="$1"
  if [ "${DOCKER[0]}" = "sudo" ]; then
    sudo -n env IMAGE_REPO="$IMAGE" IMAGE_TAG="$tag" CONTAINER_NAME="$CONTAINER" ONBOARD_DOMAINS="$DOMAINS" \
      HOST_PORT="$HOST_PORT" HOST_DIR="$DIR" \
      docker compose -p "$CONTAINER" -f "$COMPOSE" up -d --remove-orphans
  else
    IMAGE_REPO="$IMAGE" IMAGE_TAG="$tag" CONTAINER_NAME="$CONTAINER" ONBOARD_DOMAINS="$DOMAINS" \
      HOST_PORT="$HOST_PORT" HOST_DIR="$DIR" \
      docker compose -p "$CONTAINER" -f "$COMPOSE" up -d --remove-orphans
  fi
}

compose_up "$TAG"

healthy=0; code=""
for _ in $(seq 1 60); do
  code=$(curl -s -o /dev/null -m 2 -w "%{http_code}" "http://127.0.0.1:$HOST_PORT/healthz" || true)
  if [ "$code" = "200" ]; then healthy=1; break; fi
  sleep 1
done

if [ "$healthy" != "1" ]; then
  echo "ERROR: $IMAGE:$TAG did not become healthy on 127.0.0.1:$HOST_PORT (last http=$code)" >&2
  dk logs --tail 40 "$CONTAINER" >&2 || true
  if [ -n "$previous" ] && [ "$previous" != "$TAG" ]; then
    echo "==> rolling back to $previous" >&2
    compose_up "$previous" || true
  fi
  exit 1
fi

printf "%s" "$TAG" > "$STATE"
echo "==> deployed $TAG, healthy on 127.0.0.1:$HOST_PORT"
dk image prune -f --filter "until=168h" >/dev/null 2>&1 || true
