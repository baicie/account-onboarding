#!/usr/bin/env bash
# Runs ON the target host over ssh (fed to `bash -s` with positional arguments).
#
# Arguments:
#   $1 image repository   $2 tag        $3 digest      $4 container name
#   $5 deploy dir         $6 host port  $7 domains     $8 compose (base64)
#
# One SSH connection does everything: install the compose file, bootstrap the
# directories and the secret projection, pull, materialize, deploy, health check and
# roll back on failure. Failures are explicit; secret material is never printed.
set -euo pipefail

IMAGE="$1"; TAG="$2"; DIGEST="$3"; CONTAINER="$4"; DIR="$5"; HOST_PORT="$6"; DOMAINS="${7:-moonbyte.test}"; COMPOSE_B64="${8:-}"
DATA="$DIR/data"; SECRETS_SRC="$DIR/secrets-src"; SECRETS="$DIR/secrets"; STATE="$DIR/.current-tag"
COMPOSE="$DIR/docker-compose.deploy.yml"

echo "==> target: $IMAGE:$TAG -> $DIR (container $CONTAINER, 127.0.0.1:$HOST_PORT)"

# ---- root helper (the deploy user is usually in sudo, not root) -------------------
if [ "$(id -u)" = "0" ]; then
  as_root() { "$@"; }
elif sudo -n true >/dev/null 2>&1; then
  as_root() { sudo -n "$@"; }
else
  echo "ERROR: need root or passwordless sudo on the host to own the deploy directory" >&2
  exit 1
fi

# ---- directories + compose file ---------------------------------------------------
[ -n "$COMPOSE_B64" ] || { echo "ERROR: compose payload missing" >&2; exit 1; }
as_root mkdir -p "$DATA" "$SECRETS" "$SECRETS_SRC"
as_root chown "$(id -u):$(id -g)" "$DIR" "$DATA"
as_root chmod 0700 "$SECRETS"
printf "%s" "$COMPOSE_B64" | base64 -d > "$COMPOSE.tmp"
mv -f "$COMPOSE.tmp" "$COMPOSE"
chmod 0644 "$COMPOSE"
echo "==> compose installed: $COMPOSE"

# ---- secret projection: generated here, once, kept on re-deploy -------------------
if ! as_root test -f "$SECRETS_SRC/onboarding_master_key"; then
  echo "==> generating the master keyring on the host"
  as_root sh -c "openssl rand 32 > $SECRETS_SRC/onboarding_master_key"
fi
if ! as_root test -f "$SECRETS_SRC/onboard_admin_token"; then
  echo "==> generating the admin token on the host"
  as_root sh -c "openssl rand -hex 32 > $SECRETS_SRC/onboard_admin_token"
fi
for f in onboarding_master_key onboard_admin_token; do
  as_root chown root:root "$SECRETS_SRC/$f"
  as_root chmod 0444 "$SECRETS_SRC/$f"
  mode=$(as_root stat -c "%a" "$SECRETS_SRC/$f")
  if [ "$mode" != "444" ]; then
    echo "ERROR: $SECRETS_SRC/$f must be 0444 (it is $mode)" >&2
    exit 1
  fi
done

# ---- docker access ----------------------------------------------------------------
if docker info >/dev/null 2>&1; then
  DOCKER=(docker); echo "==> docker: direct"
elif sudo -n true >/dev/null 2>&1; then
  DOCKER=(sudo -n docker); echo "==> docker: via sudo -n"
else
  echo "ERROR: no docker access and no passwordless sudo; run sudo usermod -aG docker <user>" >&2
  exit 1
fi
dk() { "${DOCKER[@]}" "$@"; }

# ---- pull ------------------------------------------------------------------------
if ! dk image inspect "$IMAGE:$TAG" >/dev/null 2>&1; then
  if [ -f "$DIR/.ghcr-token" ]; then
    echo "==> pulling with the host-stored registry token"
    dk login ghcr.io -u "${GHCR_USER:-token}" --password-stdin < "$DIR/.ghcr-token" >/dev/null
  else
    echo "==> pulling (package must be publicly readable)"
  fi
  if ! dk pull "$IMAGE:$TAG"; then
    echo "ERROR: cannot pull $IMAGE:$TAG" >&2
    echo "       make the GHCR package public, or store a read:packages token in" >&2
    echo "       $DIR/.ghcr-token (mode 0600) so this script can log in" >&2
    exit 1
  fi
fi
echo "==> image id: $(dk image inspect "$IMAGE:$TAG" --format "{{.Id}}" 2>/dev/null || echo unknown) (CI index digest: $DIGEST)"

# ---- materialize the 0400 files the service accepts -------------------------------
dk run --rm --user 0:0 \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
  --security-opt no-new-privileges:true \
  -v "$SECRETS_SRC:/src:ro" -v "$SECRETS:/run/secrets" \
  --entrypoint /bin/sh "$IMAGE:$TAG" \
  -c "exec /app/scripts/materialize_secrets.sh --source-dir /src --target-dir /run/secrets --uid 10001 --gid 10001"

# ---- deploy with rollback ---------------------------------------------------------
previous=""
if [ -f "$STATE" ]; then previous=$(cat "$STATE"); fi
echo "==> previous tag: ${previous:-<none>}"

compose_up() { # compose_up <tag>
  local tag="$1"
  if [ "${DOCKER[0]}" = "sudo" ]; then
    sudo -n env IMAGE_REPO="$IMAGE" IMAGE_TAG="$tag" CONTAINER_NAME="$CONTAINER" \
      HOST_PORT="$HOST_PORT" HOST_DIR="$DIR" ONBOARD_DOMAINS="$DOMAINS" \
      docker compose -p "$CONTAINER" -f "$COMPOSE" up -d --remove-orphans
  else
    IMAGE_REPO="$IMAGE" IMAGE_TAG="$tag" CONTAINER_NAME="$CONTAINER" \
      HOST_PORT="$HOST_PORT" HOST_DIR="$DIR" ONBOARD_DOMAINS="$DOMAINS" \
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

echo "$TAG" > "$STATE"
echo "==> deployed $TAG, healthy on 127.0.0.1:$HOST_PORT"
echo "==> admin token lives on the host: sudo cat $SECRETS/onboard_admin_token"
dk image prune -f --filter "until=168h" >/dev/null 2>&1 || true
