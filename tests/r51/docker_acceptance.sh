#!/usr/bin/env bash
# R5.1-B Docker runtime acceptance (requires a working Docker daemon).
#
# Isolation contract (audited in docs/acceptance/R5.1-B.md):
#   - unique project/container/image names derived from PID+timestamp;
#   - temporary data and secret directories under .sandbox/r51/;
#   - loopback-only published ports 127.0.0.1:28788-28791;
#   - cleanup trap removes only its own containers, its compose project (-p) and
#     its own image; it never runs `docker system prune`, global rm, or touches
#     other containers/networks/volumes.
#
# Exit codes: 0 = all executed checks PASS; 1 = FAIL; 3 = NOT_RUN (no daemon).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if ! docker info >/dev/null 2>&1; then
  echo "NOT_RUN: Docker daemon is unavailable (docker info exit 1)."
  echo "Required: local Docker + Compose with permission to build/run this image."
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
  for name in "$PROJECT-app" "$PROJECT-app2" "$PROJECT-upgrade" "$PROJECT-degraded" \
              "$PROJECT-badkey" "$PROJECT-badkeydata" "$PROJECT-ro"; do
    docker rm -f "$name" >/dev/null 2>&1 || true
  done
  R51_IMAGE="$IMAGE" R51_DATA="$WORK/data" R51_MASTER_KEY="$WORK/secrets/master.key" \
    R51_ADMIN_TOKEN="$WORK/secrets/admin.token" \
    docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" \
    down -v >/dev/null 2>&1 || true
  docker image rm "$IMAGE" >/dev/null 2>&1 || true
}
trap cleanup EXIT
rm -rf "$WORK"; mkdir -p "$WORK"/{data,v2data,degraded,secrets}; umask 077
head -c 32 /dev/urandom > "$WORK/secrets/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/secrets/admin.token"
head -c 10 /dev/urandom > "$WORK/secrets/badkey"
# Test-only: the container runs as UID 10001 and bind-mounted host files keep
# host ownership; make them readable for the container. Production must make
# secrets readable by UID 10001 (Linux: chown 10001:10001 + 0400) or use a
# compose/runtime secret mapping; preflight.sh warns when the owner differs.
chmod 644 "$WORK/secrets/master.key" "$WORK/secrets/admin.token" "$WORK/secrets/badkey"
chmod 777 "$WORK/data" "$WORK/v2data" "$WORK/degraded" # test-only bind-mount write for UID 10001
TOKEN="$(cat "$WORK/secrets/admin.token")"
BASE="http://127.0.0.1:$PORT"
AUTH=(-H "Authorization: Bearer $TOKEN")
JSON=(-H "Content-Type: application/json")
ENVS=(-e ONBOARD_LISTEN=0.0.0.0:8788 -e ONBOARD_ALLOW_PUBLIC_LISTEN=1
      -e ONBOARD_DB_PATH=/app/data/state.sqlite -e ONBOARD_FAKE_PROVIDERS=true
      -e ONBOARD_DOMAINS=moonbyte.test -e ONBOARD_SENDER_ALLOWLIST=moonbyte.test
      -e ONBOARD_LOG_LEVEL=warn -e ONBOARD_SESSION_TTL=1h)
MOUNTS=(--mount type=bind,src="$WORK/secrets/master.key",dst=/run/secrets/onboarding_master_key,readonly
        --mount type=bind,src="$WORK/secrets/admin.token",dst=/run/secrets/onboard_admin_token,readonly)

run_app() { # $1 name, $2 port, $3 data dir, extra args...
  local name="$1" port="$2" data="$3"; shift 3
  docker run -d --name "$name" \
    --read-only --cap-drop ALL --security-opt no-new-privileges \
    --user 10001:10001 --tmpfs /tmp:rw,noexec,nosuid,size=16m \
    -p "127.0.0.1:$port:8788" -v "$data:/app/data" "${MOUNTS[@]}" "${ENVS[@]}" "$@" "$IMAGE" >/dev/null
}
wait_http() { # $1 port, $2 path, $3 expected code, $4 timeout seconds
  local port="$1" path="$2" want="$3" timeout="${4:-60}" code
  for _ in $(seq 1 $((timeout*4))); do
    code="$(curl -sS -m 1 -o /dev/null -w '%{http_code}' "${AUTH[@]}" "http://127.0.0.1:$port$path" 2>/dev/null || true)"
    [ "$code" = "$want" ] && return 0
    sleep 0.25
  done
  return 1
}
wait_health() { # $1 container, $2 timeout seconds
  local name="$1" timeout="${2:-90}" status
  for _ in $(seq 1 "$timeout"); do
    status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$name" 2>/dev/null || echo missing)"
    [ "$status" = "healthy" ] && return 0
    sleep 1
  done
  return 1
}
meta_schema() { curl -sS "${AUTH[@]}" "http://127.0.0.1:$1/v1/meta" | python3 -c 'import json,sys;print(json.load(sys.stdin)["schema_version"])'; }

echo "=== build / image metadata ==="
docker build -t "$IMAGE" "$ROOT"
IMG_ID="$(docker image inspect --format '{{.Id}}' "$IMAGE")"
ARCH="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$IMAGE")"
IMG_USER="$(docker image inspect --format '{{.Config.User}}' "$IMAGE")"
ok "image built: id=$IMG_ID platform=$ARCH user=${IMG_USER:-<unset>}"

echo "=== run + health + hardening ==="
run_app "$PROJECT-app" "$PORT" "$WORK/data"
curl -fsS -m 3 "$BASE/healthz" >/dev/null 2>&1 || true
wait_http "$PORT" /healthz 200 60 && ok "/healthz 200" || bad "/healthz"
wait_http "$PORT" /readyz 200 60 && ok "/readyz 200 (schema/revocation-table/master-key ready)" || bad "/readyz"
wait_health "$PROJECT-app" 90 && ok "Docker HEALTHCHECK healthy (readyz-based)" || bad "Docker HEALTHCHECK"
docker exec "$PROJECT-app" /app/onboardd healthcheck >/dev/null 2>&1 \
  && ok "in-container 'onboardd healthcheck' exit 0" || bad "in-container healthcheck"
[ "$(meta_schema "$PORT")" = "3" ] && ok "fresh database migrated to schema 3" || bad "fresh schema"
[ "$(docker exec "$PROJECT-app" id -u)" = "10001" ] && ok "runs as UID 10001 (non-root)" || bad "non-root"
[ "$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$PROJECT-app")" = "true" ] \
  && ok "read-only root filesystem (HostConfig)" || bad "ReadonlyRootfs"
docker exec "$PROJECT-app" sh -c 'touch /app/forbidden 2>/dev/null' >/dev/null 2>&1 \
  && bad "rootfs is writable" || ok "write outside /app/data refused"
docker inspect --format '{{json .HostConfig.CapDrop}}' "$PROJECT-app" | grep -q 'ALL' && ok "cap_drop ALL" || bad "cap_drop ALL"
docker inspect --format '{{json .HostConfig.SecurityOpt}}' "$PROJECT-app" | grep -q 'no-new-privileges' \
  && ok "no-new-privileges" || bad "no-new-privileges"
MODES="$(docker exec "$PROJECT-app" stat -c '%a' /run/secrets/onboarding_master_key /run/secrets/onboard_admin_token 2>/dev/null || true)"
SECRET_OK=1
while IFS= read -r m; do
  [ -n "$m" ] || continue
  if [ $((8#$m & 022)) -ne 0 ]; then SECRET_OK=0; fi
done <<< "$MODES"
docker exec "$PROJECT-app" test -r /run/secrets/onboarding_master_key \
  && docker exec "$PROJECT-app" test -r /run/secrets/onboard_admin_token \
  && SECRET_READ=1 || SECRET_READ=0
[ "$SECRET_OK" = "1" ] && [ "$SECRET_READ" = "1" ] \
  && ok "secret files readable by UID 10001 and not group/world writable (modes: $(echo "$MODES" | tr '\n' ' '))" \
  || bad "secret permissions/readability (modes: $MODES readable=$SECRET_READ)"
test -f "$WORK/data/state.sqlite" && ok "SQLite persisted on host bind mount" || bad "no SQLite file"
[ "$(docker exec "$PROJECT-app" stat -c '%u' /app/data/state.sqlite)" = "10001" ] \
  && ok "SQLite owned by UID 10001 inside the container" || bad "SQLite ownership"
docker exec "$PROJECT-app" sh -c 'ls /app/data' | grep -q 'state.sqlite-wal' \
  && ok "WAL file present in writable data volume" || bad "WAL file missing"
docker inspect --format '{{json .HostConfig.PortBindings}}' "$PROJECT-app" | grep -q '127.0.0.1' \
  && ok "published port bound to 127.0.0.1" || bad "port binding"
[ "$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$PROJECT-app")" != "host" ] \
  && ok "container network is not host mode" || bad "host network"

echo "=== schema upgrade from v2 database ==="
python3 - "$ROOT" "$WORK/v2data/state.sqlite" <<'PY'
import sqlite3, sys
root, path = sys.argv[1], sys.argv[2]
db = sqlite3.connect(path)
for name in ("0001_initial.sql", "0002_operator_fields.sql"):
    db.executescript(open(f"{root}/internal/storage/migrations/{name}").read())
db.commit()
ver = db.execute("select max(version) from schema_migrations").fetchone()[0]
assert ver == 2, ver
db.close()
PY
run_app "$PROJECT-upgrade" 28790 "$WORK/v2data"
wait_http 28790 /readyz 200 60 && ok "v2 database upgraded and /readyz 200" || bad "v2 upgrade readyz"
[ "$(meta_schema 28790)" = "3" ] && ok "v2 -> schema 3 after container start" || bad "v2 upgrade schema"
docker rm -f "$PROJECT-upgrade" >/dev/null 2>&1 || true

echo "=== runs + idempotency ==="
BODY='{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox"}'
RUN1="$(curl -sS "${AUTH[@]}" "${JSON[@]}" -X POST "$BASE/v1/runs" -d "$BODY" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
RUN2="$(curl -sS "${AUTH[@]}" "${JSON[@]}" -X POST "$BASE/v1/runs" -d "$BODY" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
[ -n "$RUN1" ] && [ "$RUN1" != "$RUN2" ] && ok "distinct runs without idempotency key" || bad "run creation"
IDEM="$(curl -sS "${AUTH[@]}" "${JSON[@]}" -X POST "$BASE/v1/runs" -H 'Idempotency-Key: r51b-idem' -d "$BODY" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
IDEM2="$(curl -sS "${AUTH[@]}" "${JSON[@]}" -X POST "$BASE/v1/runs" -H 'Idempotency-Key: r51b-idem' -d "$BODY" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
[ "$IDEM" = "$IDEM2" ] && ok "same Idempotency-Key -> one run" || bad "idempotency key created two runs"
for _ in $(seq 1 60); do
  STATUS="$(curl -sS "${AUTH[@]}" "$BASE/v1/runs/$RUN1" | python3 -c 'import json,sys;print(json.load(sys.stdin)["status"])')"
  case "$STATUS" in COMPLETED|FAILED_TERMINAL|CANCELLED) break;; esac
  sleep 0.5
done
[ "$STATUS" = "COMPLETED" ] && ok "sandbox run COMPLETED inside container" || bad "run status=$STATUS"

echo "=== session revocation + SIGTERM/SIGKILL ==="
curl -sS -c "$WORK/jar" -o /dev/null -X POST "$BASE/ui/login" -H "Origin: $BASE" --data-urlencode "token=$TOKEN"
COOKIE="$(awk '$6=="onboard_session"{print $7}' "$WORK/jar")"
curl -sS -b "$WORK/jar" -o "$WORK/dash.html" "$BASE/ui/"
CSRF="$(grep -o 'name="csrf" value="[^"]*"' "$WORK/dash.html" | head -1 | sed 's/.*value="//;s/"//')"
[ -n "$COOKIE" ] && [ -n "$CSRF" ] && ok "session cookie + CSRF acquired" || bad "session/CSRF"
curl -sS -b "$WORK/jar" -o /dev/null -X POST "$BASE/ui/logout" -H "Origin: $BASE" --data-urlencode "csrf=$CSRF"
ROWS="$(python3 - "$WORK/data/state.sqlite" <<'PY'
import sqlite3,sys
print(sqlite3.connect(sys.argv[1]).execute("select count(*) from session_revocations").fetchone()[0])
PY
)"
[ "$ROWS" -ge 1 ] && ok "revocation row persisted in SQLite ($ROWS)" || bad "no revocation row"
docker stop --time 10 "$PROJECT-app" >/dev/null
EXIT_CODE="$(docker inspect --format '{{.State.ExitCode}}' "$PROJECT-app")"
[ "$EXIT_CODE" = "0" ] && ok "SIGTERM graceful stop exit code 0" || bad "SIGTERM exit=$EXIT_CODE"
docker start "$PROJECT-app" >/dev/null
wait_http "$PORT" /readyz 200 60 && ok "restart after SIGTERM healthy" || bad "restart after SIGTERM"
C="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' -H "Cookie: onboard_session=$COOKIE" "$BASE/ui/")"
case "$C" in "303 "*"/ui/login") ok "revocation survives restart";; *) bad "post-restart replay: $C";; esac
docker kill -s KILL "$PROJECT-app" >/dev/null
docker start "$PROJECT-app" >/dev/null
wait_http "$PORT" /readyz 200 60 && ok "restart after SIGKILL healthy" || bad "restart after SIGKILL"
curl -sS "${AUTH[@]}" "$BASE/v1/runs/$RUN1" | grep -q '"id"' && ok "run data preserved after SIGKILL" || bad "run lost after SIGKILL"

echo "=== second container on the same volume must fail (single writer) ==="
run_app "$PROJECT-app2" 28789 "$WORK/data" >/dev/null 2>&1 || true
sleep 4
STATE="$(docker inspect --format '{{.State.Status}}' "$PROJECT-app2" 2>/dev/null || echo missing)"
LOGS="$(docker logs "$PROJECT-app2" 2>&1 || true)"
if echo "$LOGS" | grep -qi "single-writer"; then
  ok "second container refused by single-writer lock (state=$STATE)"
else
  bad "second container: state=$STATE logs=$(echo "$LOGS" | tail -2 | tr '\\n' ' ')"
fi
docker rm -f "$PROJECT-app2" >/dev/null 2>&1 || true

echo "=== encrypted backup / restore ==="
ONBOARD_BACKUP_ENCRYPT=1 ONBOARD_KEYRING="$WORK/secrets/master.key" \
  bash "$ROOT/scripts/backup.sh" --db "$WORK/data/state.sqlite" --out-dir "$WORK/backup" \
  --encrypt --keyring "$WORK/secrets/master.key" >/dev/null 2>&1 \
  && ok "encrypted snapshot of container DB" || bad "encrypted backup"
ENC="$(ls "$WORK"/backup/*.sqlite.enc 2>/dev/null | head -1 || true)"
if [ -n "$ENC" ] && bash "$ROOT/scripts/verify_backup.sh" "$ENC" --keyring "$WORK/secrets/master.key" >/dev/null 2>&1; then
  ok "verify encrypted snapshot"
else
  bad "verify snapshot"
fi
mkdir -p "$WORK/restored"
if CONFIRM_RESTORE=1 bash "$ROOT/scripts/restore.sh" --snapshot "$ENC" \
     --target "$WORK/restored/state.sqlite" --keyring "$WORK/secrets/master.key" --force >/dev/null 2>&1; then
  ok "restore encrypted snapshot"
  COUNTS="$(python3 - "$WORK/restored/state.sqlite" <<'PY'
import sqlite3,sys
db=sqlite3.connect(sys.argv[1])
print(db.execute("select count(*) from runs").fetchone()[0], db.execute("select count(*) from session_revocations").fetchone()[0])
PY
)"
  RUNS_R="$(echo "$COUNTS" | awk '{print $1}')"; REV_R="$(echo "$COUNTS" | awk '{print $2}')"
  [ "$RUNS_R" -ge 2 ] && [ "$REV_R" -ge 1 ] && ok "restored DB keeps runs=$RUNS_R revocations=$REV_R" || bad "restored counts: $COUNTS"
else
  bad "restore snapshot"
fi

echo "=== degraded DB: /readyz 503 + healthcheck red ==="
docker stop --time 10 "$PROJECT-app" >/dev/null
cp "$WORK/data/state.sqlite" "$WORK/degraded/state.sqlite"
python3 - "$WORK/degraded/state.sqlite" <<'PY'
import sqlite3,sys
db=sqlite3.connect(sys.argv[1]); db.execute("DROP TABLE session_revocations"); db.commit(); db.close()
PY
chmod 666 "$WORK/degraded/state.sqlite"
run_app "$PROJECT-degraded" 28791 "$WORK/degraded"
wait_http 28791 /healthz 200 30 || bad "degraded healthz"
wait_http 28791 /readyz 503 30 && ok "degraded DB -> /readyz 503" || bad "degraded readyz"
docker exec "$PROJECT-degraded" /app/onboardd healthcheck >/dev/null 2>&1 \
  && bad "degraded healthcheck reported ready" || ok "degraded healthcheck exit non-zero"
docker rm -f "$PROJECT-degraded" >/dev/null 2>&1 || true

echo "=== fail-closed configurations ==="
mkdir -p "$WORK/bad-data" "$WORK/badkey-data" "$WORK/badkeydata"; chmod 777 "$WORK/bad-data" "$WORK/badkey-data" "$WORK/badkeydata"
docker run -d --name "$PROJECT-badkey" --read-only --cap-drop ALL --user 10001:10001 \
  --tmpfs /tmp:size=8m -v "$WORK/bad-data:/app/data" "${ENVS[@]}" "$IMAGE" >/dev/null 2>&1 || true
sleep 3
docker inspect --format '{{.State.Status}}' "$PROJECT-badkey" | grep -q 'exited' \
  && ok "missing master key container exits" || bad "missing key container state"
docker rm -f "$PROJECT-badkey" >/dev/null 2>&1 || true

docker run -d --name "$PROJECT-badkeydata" --read-only --cap-drop ALL --user 10001:10001 \
  --tmpfs /tmp:size=8m -v "$WORK/badkeydata:/app/data" \
  --mount type=bind,src="$WORK/secrets/badkey",dst=/run/secrets/onboarding_master_key,readonly \
  --mount type=bind,src="$WORK/secrets/admin.token",dst=/run/secrets/onboard_admin_token,readonly \
  "${ENVS[@]}" "$IMAGE" >/dev/null 2>&1 || true
sleep 3
docker inspect --format '{{.State.Status}}' "$PROJECT-badkeydata" | grep -q 'exited' \
  && ok "invalid master key container exits" || bad "invalid key container state"
docker rm -f "$PROJECT-badkeydata" >/dev/null 2>&1 || true

mkdir -p "$WORK/rodata"; chmod 500 "$WORK/rodata"
docker run -d --name "$PROJECT-ro" --read-only --cap-drop ALL --user 10001:10001 \
  --tmpfs /tmp:size=8m -v "$WORK/rodata:/app/data" "${MOUNTS[@]}" "${ENVS[@]}" "$IMAGE" >/dev/null 2>&1 || true
sleep 4
RO_STATE="$(docker inspect --format '{{.State.Status}}' "$PROJECT-ro" 2>/dev/null || echo missing)"
if [ "$RO_STATE" = "exited" ]; then
  ok "unwritable data directory fails closed (container exited)"
else
  RO_CODE="$(curl -sS -m 2 -o /dev/null -w '%{http_code}' "${AUTH[@]}" "http://127.0.0.1:$PORT/readyz" 2>/dev/null || true)"
  [ "$RO_CODE" != "200" ] && ok "unwritable data directory not ready (probe=$RO_CODE)" || bad "unwritable data dir reported ready"
fi
docker rm -f "$PROJECT-ro" >/dev/null 2>&1 || true

echo "=== compose parity (isolated project) ==="
docker rm -f "$PROJECT-app" >/dev/null 2>&1 || true
R51_IMAGE="$IMAGE" R51_DATA="$WORK/data" R51_MASTER_KEY="$WORK/secrets/master.key" \
  R51_ADMIN_TOKEN="$WORK/secrets/admin.token" \
  docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" config >/dev/null \
  && ok "compose config (isolated override)" || bad "compose config"
R51_IMAGE="$IMAGE" R51_DATA="$WORK/data" R51_MASTER_KEY="$WORK/secrets/master.key" \
  R51_ADMIN_TOKEN="$WORK/secrets/admin.token" \
  docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" up -d >/dev/null \
  && ok "compose up (isolated project)" || bad "compose up"
for _ in $(seq 1 90); do
  C="$(docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" ps --format '{{.Status}}' 2>/dev/null | head -1)"
  echo "$C" | grep -qi healthy && break; sleep 1
done
echo "$C" | grep -qi healthy && ok "compose service healthy: $C" || bad "compose health: $C"
COMPOSE_CID="$(docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" ps -q onboarding 2>/dev/null | head -1)"
if [ -n "$COMPOSE_CID" ] && docker exec "$COMPOSE_CID" test -r /run/secrets/onboarding_master_key \
     && docker exec "$COMPOSE_CID" test -r /run/secrets/onboard_admin_token; then
  ok "compose secrets readable by UID 10001"
else
  bad "compose secrets not readable by UID 10001 (check host ownership / compose secret mapping)"
fi
COUNT="$(docker ps --filter "label=com.docker.compose.project=$PROJECT" --format '{{.ID}}' | wc -l | tr -d ' ')"
[ "$COUNT" = "1" ] && ok "exactly one writer container in the compose project" || bad "writer count=$COUNT"
COMPOSE_PORT="$(docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" port onboarding 8788 2>/dev/null || true)"
case "$COMPOSE_PORT" in 127.0.0.1:*) ok "compose published port loopback-only ($COMPOSE_PORT)";; *) bad "compose port: $COMPOSE_PORT";; esac
R51_IMAGE="$IMAGE" R51_DATA="$WORK/data" R51_MASTER_KEY="$WORK/secrets/master.key" \
  R51_ADMIN_TOKEN="$WORK/secrets/admin.token" \
  docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" down -v >/dev/null 2>&1 || true

echo "SUMMARY pass=$PASS fail=$FAIL image=$IMG_ID platform=$ARCH"
[ "$FAIL" = "0" ]
