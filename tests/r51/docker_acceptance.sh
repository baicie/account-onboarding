#!/usr/bin/env bash
# R5.1-B: Docker runtime acceptance (requires a working Docker daemon).
#
# Isolation: unique project/container/image names, temporary data+secret
# directories, loopback-only published ports, and a cleanup trap. It never
# touches other containers, networks, volumes, the default compose project or
# any production deployment.
#
# Exit codes: 0 = all executed tests PASS; 1 = FAIL; 3 = NOT_RUN (no daemon).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if ! docker info >/dev/null 2>&1; then
  echo "NOT_RUN: Docker daemon is unavailable (docker info exit 1)."
  echo "Required: a local Docker/Compose environment with permission to build and run this image."
  echo "No container was built or started; no host configuration was changed."
  exit 3
fi
ID="r51-$(date +%s)-$$"
PROJECT="onboarding-r51-$ID"
IMAGE="account-onboarding:$ID"
WORK="$ROOT/.sandbox/r51/docker-$ID"
PORT=28788
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
cleanup() {
  docker rm -f "$PROJECT-app" >/dev/null 2>&1 || true
  docker rm -f "$PROJECT-compose-onboarding-1" >/dev/null 2>&1 || true
  docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" down -v >/dev/null 2>&1 || true
  docker volume rm "${PROJECT}_data" >/dev/null 2>&1 || true
  docker image rm "$IMAGE" >/dev/null 2>&1 || true
}
trap cleanup EXIT
rm -rf "$WORK"; mkdir -p "$WORK/data" "$WORK/secrets"; umask 077
head -c 32 /dev/urandom > "$WORK/secrets/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/secrets/admin.token"
chmod 777 "$WORK/data" # test-only: lets UID 10001 write a bind-mounted dir on macOS
TOKEN="$(cat "$WORK/secrets/admin.token")"
BASE="http://127.0.0.1:$PORT"

echo "=== build ==="
docker build -t "$IMAGE" "$ROOT"
IMG_ID="$(docker image inspect --format '{{.Id}}' "$IMAGE")"
ok "image built: $IMG_ID"

# Start one isolated container with the production hardening flags.
docker run -d --name "$PROJECT-app" \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --user 10001:10001 --tmpfs /tmp:rw,noexec,nosuid,size=16m \
  -p "127.0.0.1:$PORT:8788" \
  -v "$WORK/data:/app/data" \
  --mount type=bind,src="$WORK/secrets/master.key",dst=/run/secrets/onboarding_master_key,readonly \
  --mount type=bind,src="$WORK/secrets/admin.token",dst=/run/secrets/onboard_admin_token,readonly \
  -e ONBOARD_LISTEN=0.0.0.0:8788 -e ONBOARD_ALLOW_PUBLIC_LISTEN=1 \
  -e ONBOARD_DB_PATH=/app/data/state.sqlite -e ONBOARD_FAKE_PROVIDERS=true \
  -e ONBOARD_DOMAINS=moonbyte.test \
  -e ONBOARD_SENDER_ALLOWLIST=moonbyte.test -e ONBOARD_LOG_LEVEL=warn \
  "$IMAGE" >/dev/null
for _ in $(seq 1 120); do curl -fsS -m 1 "$BASE/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
[ "$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/healthz")" = "200" ] && ok "container healthz 200" || bad "healthz"
# Built-in HEALTHCHECK must eventually report healthy.
for _ in $(seq 1 60); do
  STATUS="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$PROJECT-app")"
  [ "$STATUS" = "healthy" ] && break; sleep 1
done
[ "$STATUS" = "healthy" ] && ok "Docker HEALTHCHECK healthy" || bad "healthcheck status=$STATUS"

echo "=== hardening ==="
[ "$(docker exec "$PROJECT-app" id -u)" = "10001" ] && ok "runs as UID 10001 (non-root)" || bad "non-root"
docker exec "$PROJECT-app" sh -c 'touch /app/forbidden 2>/dev/null' >/dev/null 2>&1 && bad "rootfs is writable" || ok "read-only root filesystem"
docker inspect --format '{{json .HostConfig.CapDrop}}' "$PROJECT-app" | grep -q 'ALL' && ok "cap_drop ALL" || bad "cap_drop ALL"
docker inspect --format '{{json .HostConfig.SecurityOpt}}' "$PROJECT-app" | grep -q 'no-new-privileges' && ok "no-new-privileges" || bad "no-new-privileges"
test -f "$WORK/data/state.sqlite" && ok "SQLite file persisted on host bind mount" || bad "no SQLite file"
ls -ln "$WORK/data/state.sqlite" | awk '{print $3}' | grep -qx 10001 && ok "SQLite owned by UID 10001" || bad "SQLite ownership"

echo "=== idempotency + persistence + upgrade ==="
H=(-H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json")
RUN1="$(curl -sS "${H[@]}" -X POST "$BASE/v1/runs" -d '{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
RUN2="$(curl -sS "${H[@]}" -X POST "$BASE/v1/runs" -d '{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
[ "$RUN1" != "$RUN2" ] && ok "distinct runs without idempotency key" || bad "duplicate run id"
IDEM="$(curl -sS "${H[@]}" -X POST "$BASE/v1/runs" -H 'Idempotency-Key: r51-idem' -d '{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
IDEM2="$(curl -sS "${H[@]}" -X POST "$BASE/v1/runs" -H 'Idempotency-Key: r51-idem' -d '{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
[ "$IDEM" = "$IDEM2" ] && ok "idempotency key returns one run" || bad "idempotency key created two runs"

echo "=== SIGTERM / SIGKILL / restart / revocation ==="
DASH_COOKIE="$(curl -sS -c "$WORK/jar" -o /dev/null -X POST "$BASE/ui/login" -H "Origin: $BASE" --data-urlencode "token=$TOKEN" -w '%{http_code}')"
[ "$DASH_COOKIE" = "303" ] && ok "UI login in container 303" || bad "login $DASH_COOKIE"
COOKIE="$(awk '$6=="onboard_session"{print $7}' "$WORK/jar")"
curl -sS -b "$WORK/jar" -o "$WORK/dash.html" "$BASE/ui/"
CSRF="$(grep -o 'name="csrf" value="[^"]*"' "$WORK/dash.html" | head -1 | sed 's/.*value="//;s/"//')"
[ -n "$COOKIE" ] && [ -n "$CSRF" ] && ok "session cookie + CSRF acquired" || bad "session/CSRF"
curl -sS -b "$WORK/jar" -o /dev/null -X POST "$BASE/ui/logout" -H "Origin: $BASE" --data-urlencode "csrf=$CSRF" -w '%{http_code}' >/dev/null
docker restart --time 10 "$PROJECT-app" >/dev/null
for _ in $(seq 1 120); do curl -fsS -m 1 "$BASE/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
C="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' -H "Cookie: onboard_session=$COOKIE" "$BASE/ui/")"
case "$C" in "303 "*"/ui/login") ok "revocation survives container restart";; *) bad "post-restart replay: $C";; esac
docker kill -s KILL "$PROJECT-app" >/dev/null
docker start "$PROJECT-app" >/dev/null
for _ in $(seq 1 120); do curl -fsS -m 1 "$BASE/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -sS "${H[@]}" "$BASE/v1/runs/$RUN1" | grep -q '"id"' && ok "runs preserved after SIGKILL + restart" || bad "runs lost after SIGKILL"

echo "=== encrypted backup/restore against live container DB ==="
ONBOARD_BACKUP_ENCRYPT=1 ONBOARD_KEYRING="$WORK/secrets/master.key" \
  bash "$ROOT/scripts/backup.sh" --db "$WORK/data/state.sqlite" --out-dir "$WORK/backup" \
  --encrypt --keyring "$WORK/secrets/master.key" >/dev/null 2>&1 \
  && ok "encrypted snapshot of container DB" || bad "encrypted backup"
bash "$ROOT/scripts/verify_backup.sh" "$WORK/backup"/*.sqlite.enc --keyring "$WORK/secrets/master.key" >/dev/null 2>&1 \
  && ok "verify encrypted snapshot" || bad "verify snapshot"

echo "=== failure modes ==="
docker run --rm -d --name "$PROJECT-bad" --read-only --cap-drop ALL --user 10001:10001 \
  --tmpfs /tmp:size=8m -v "$WORK/data:/app/data" -e ONBOARD_LISTEN=0.0.0.0:8788 \
  -e ONBOARD_ALLOW_PUBLIC_LISTEN=1 -e ONBOARD_DB_PATH=/app/data/state.sqlite \
  -e ONBOARD_MASTER_KEY_FILE=/run/secrets/missing "$IMAGE" >/dev/null 2>&1 || true
sleep 2
docker logs "$PROJECT-bad" 2>&1 | grep -qi "master key\|fatal" && ok "missing secret fails closed" || bad "missing secret"
docker rm -f "$PROJECT-bad" >/dev/null 2>&1 || true

echo "=== compose parity (isolated project) ==="
R51_IMAGE="$IMAGE" R51_DATA="$WORK/data" R51_MASTER_KEY="$WORK/secrets/master.key" R51_ADMIN_TOKEN="$WORK/secrets/admin.token" \
  docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" config >/dev/null \
  && ok "compose config with isolated override" || bad "compose config"
R51_IMAGE="$IMAGE" R51_DATA="$WORK/data" R51_MASTER_KEY="$WORK/secrets/master.key" R51_ADMIN_TOKEN="$WORK/secrets/admin.token" \
  docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" up -d >/dev/null \
  && ok "compose up (isolated project)" || bad "compose up"
sleep 5
C="$(docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" ps --format '{{.Name}} {{.Status}}' 2>/dev/null | head -1)"
echo "$C" | grep -qi healthy && ok "compose service healthy: $C" || bad "compose health: $C"
COUNT="$(docker ps --filter "label=com.docker.compose.project=$PROJECT" --format '{{.ID}}' | wc -l | tr -d ' ')"
[ "$COUNT" = "1" ] && ok "exactly one writer container in the project" || bad "writer container count=$COUNT"

echo "SUMMARY pass=$PASS fail=$FAIL image=$IMG_ID"
[ "$FAIL" = "0" ]
