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
SELF_TEST=0
case "${1:-}" in
  --self-test) SELF_TEST=1 ;;
  "") ;;
  *) echo "usage: $0 [--self-test]" >&2; exit 2 ;;
esac
if [ "$SELF_TEST" = "0" ] && ! docker info >/dev/null 2>&1; then
  echo "NOT_RUN: Docker daemon is unavailable (docker info exit 1)."
  echo "Required: local Docker + Compose with permission to build/run this image."
  echo "No container was built or started; no host configuration was changed."
  exit 3
fi
ID="r51-$(date +%s)-$$"
PROJECT="onboarding-r51-$ID"
IMAGE="account-onboarding:$ID"
WORK="$ROOT/.sandbox/r51/docker-$ID"
free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}
PORT="$(free_port)"
PORT2="$(free_port)"
PORT_UPGRADE="$(free_port)"
PORT_DEGRADED="$(free_port)"
PORT_RO="$(free_port)"
PORT_RESTORED="$(free_port)"
while [ "$PORT2" = "$PORT" ] || [ "$PORT_UPGRADE" = "$PORT" ] || [ "$PORT_UPGRADE" = "$PORT2" ] || \
      [ "$PORT_DEGRADED" = "$PORT" ] || [ "$PORT_DEGRADED" = "$PORT2" ] || [ "$PORT_DEGRADED" = "$PORT_UPGRADE" ] || \
      [ "$PORT_RO" = "$PORT" ] || [ "$PORT_RO" = "$PORT2" ] || [ "$PORT_RO" = "$PORT_UPGRADE" ] || [ "$PORT_RO" = "$PORT_DEGRADED" ] || \
      [ "$PORT_RESTORED" = "$PORT" ] || [ "$PORT_RESTORED" = "$PORT2" ] || [ "$PORT_RESTORED" = "$PORT_UPGRADE" ] || \
      [ "$PORT_RESTORED" = "$PORT_DEGRADED" ] || [ "$PORT_RESTORED" = "$PORT_RO" ]; do
  PORT2="$(free_port)"; PORT_UPGRADE="$(free_port)"; PORT_DEGRADED="$(free_port)"; PORT_RO="$(free_port)"; PORT_RESTORED="$(free_port)"
done
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
cleanup() {
  if [ "$SELF_TEST" = "1" ]; then
    [ -n "${MOCK_PID:-}" ] && kill "$MOCK_PID" 2>/dev/null || true
    return 0
  fi
  for name in "$PROJECT-app" "$PROJECT-app2" "$PROJECT-upgrade" "$PROJECT-degraded" \
              "$PROJECT-badkey" "$PROJECT-badkeydata" "$PROJECT-ro" "$PROJECT-restored"; do
    docker rm -f "$name" >/dev/null 2>&1 || true
  done
  R51_IMAGE="$IMAGE" R51_DATA="$WORK/data" R51_MASTER_KEY="$WORK/secrets/master.key" \
    R51_ADMIN_TOKEN="$WORK/secrets/admin.token" \
    docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" \
    down -v >/dev/null 2>&1 || true
  docker volume rm "$PROJECT-secrets" >/dev/null 2>&1 || true
  docker image rm "$IMAGE" >/dev/null 2>&1 || true
}
trap cleanup EXIT
rm -rf "$WORK"; mkdir -p "$WORK"/{data,v2data,degraded,secrets}; umask 077
head -c 32 /dev/urandom > "$WORK/secrets/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/secrets/admin.token"
head -c 10 /dev/urandom > "$WORK/secrets/badkey"
chmod 600 "$WORK/secrets/master.key" "$WORK/secrets/admin.token" "$WORK/secrets/badkey"
chmod 777 "$WORK/data" "$WORK/v2data" "$WORK/degraded" # test-only bind-mount write for UID 10001
TOKEN="$(cat "$WORK/secrets/admin.token")"
BASE="http://127.0.0.1:$PORT"
AUTH=(-H "Authorization: Bearer $TOKEN")
JSON=(-H "Content-Type: application/json")
ENVS=(-e ONBOARD_LISTEN=0.0.0.0:8788 -e ONBOARD_ALLOW_PUBLIC_LISTEN=1
      -e ONBOARD_DB_PATH=/app/data/state.sqlite -e ONBOARD_FAKE_PROVIDERS=true
      -e ONBOARD_DOMAINS=moonbyte.test -e ONBOARD_SENDER_ALLOWLIST=no-reply,idp.fake.test,moonbyte.test
      -e ONBOARD_MASTER_KEY_FILE=/run/secrets/onboarding_master_key
      -e ONBOARD_ADMIN_TOKEN_FILE=/run/secrets/onboard_admin_token
      -e ONBOARD_LOG_LEVEL=warn -e ONBOARD_SESSION_TTL=1h)
# Self-check: a public listen without an admin token is rejected by config
# validation, so the token-file env is mandatory for every container below.
case " ${ENVS[*]} " in
  *" ONBOARD_ADMIN_TOKEN_FILE=/run/secrets/onboard_admin_token "*) ;;
  *) echo "internal error: ENVS is missing ONBOARD_ADMIN_TOKEN_FILE"; exit 1 ;;
esac
# Secrets are copied into a dedicated Docker volume by a one-shot root init
# container, then chowned 10001:10001/0400. This gives the same on-disk
# ownership on Linux and Docker Desktop (bind-mount ownership differs by host)
# and lets us prove that an unauthorized UID cannot read them.
SECRET_VOL="$PROJECT-secrets"
MOUNTS=()

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
db_copy_out() { # $1 container, $2 destination directory
  local container="$1" dest="$2"
  mkdir -p "$dest"
  for f in state.sqlite state.sqlite-wal state.sqlite-shm; do
    docker cp "$container:/app/data/$f" "$dest/$f" >/dev/null 2>&1 || true
  done
}
meta_schema() { curl -sS -m 5 "${AUTH[@]}" "http://127.0.0.1:$1/v1/meta" | python3 -c 'import json,sys;print(json.load(sys.stdin)["schema_version"])'; }

RESP_FILE=""
HTTP_CODE=""
request_json() { # $1 method, $2 path, $3 idempotency key (may be empty), $4 body (may be empty)
  local method="$1" path="$2" idem="$3" body="${4:-}"
  RESP_FILE="$WORK/last-response.json"
  local -a args=(-sS -m 10 -o "$RESP_FILE" -w '%{http_code}' "${AUTH[@]}")
  [ -n "$idem" ] && args+=(-H "Idempotency-Key: $idem")
  if [ -n "$body" ]; then
    args+=(-H "Content-Type: application/json" -X "$method" --data-binary "$body")
  else
    args+=(-X "$method")
  fi
  HTTP_CODE="$(curl "${args[@]}" "$BASE$path" 2>/dev/null || echo 000)"
}
json_field() { # $1 file, $2 field
  python3 - "$1" "$2" <<'PY'
import json,sys
try:
    data=json.load(open(sys.argv[1]))
except Exception:
    print(""); raise SystemExit(0)
v=data
for part in sys.argv[2].split("."):
    if not isinstance(v,dict) or part not in v:
        print(""); raise SystemExit(0)
    v=v[part]
print(v if not isinstance(v,(dict,list)) else json.dumps(v))
PY
}
assert_http() { # $1 expected, $2 label
  if [ "$HTTP_CODE" = "$1" ]; then
    ok "$2 (HTTP $HTTP_CODE)"
  else
    bad "$2: HTTP $HTTP_CODE body=$(head -c 300 "$RESP_FILE" 2>/dev/null | tr '\n' ' ')"
  fi
}
CREATED_ID=""
create_run() { # $1 idem key, $2 label; sets CREATED_ID (no subshell)
  CREATED_ID=""
  request_json POST /v1/runs "$1" "$BODY"
  if [ "$HTTP_CODE" != "202" ]; then
    bad "$2: HTTP $HTTP_CODE body=$(head -c 300 "$RESP_FILE" 2>/dev/null | tr '\n' ' ')"
    return 1
  fi
  local id
  id="$(json_field "$RESP_FILE" id)"
  if [ -z "$id" ]; then
    bad "$2: HTTP 202 without JSON id"
    return 1
  fi
  CREATED_ID="$id"
  ok "$2: HTTP 202 id=$id"
}


if [ "$SELF_TEST" = "1" ]; then
  echo "=== script self-test: API contract (no Docker required) ==="
  BODY='{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox"}'
  MOCK_PORT_FILE="$WORK/mock.port"
  python3 - "$MOCK_PORT_FILE" <<'PY' &
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
state = {}

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def _send(self, code, payload):
        raw = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)
    def do_POST(self):
        if self.path != "/v1/runs":
            return self._send(404, {"error": {"class": "NOT_FOUND"}})
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length).decode() if length else ""
        key = self.headers.get("Idempotency-Key", "")
        if not key:
            return self._send(400, {"error": {"class": "VALIDATION", "message": "Idempotency-Key header is required"}})
        if not 8 <= len(key) <= 128:
            return self._send(400, {"error": {"class": "VALIDATION", "message": "Idempotency-Key must be 8..128 characters"}})
        if key in state:
            old_body, run_id = state[key]
            if old_body != body:
                return self._send(409, {"error": {"class": "CONFLICT"}})
            return self._send(202, {"id": run_id})
        run_id = "run-%d" % (len(state) + 1)
        state[key] = (body, run_id)
        return self._send(202, {"id": run_id})
    def do_GET(self):
        if self.path.startswith("/v1/runs/"):
            rid = self.path.rsplit("/", 1)[-1]
            if any(v[1] == rid for v in state.values()):
                return self._send(200, {"id": rid, "status": "CREATED"})
        return self._send(404, {"error": {"class": "NOT_FOUND"}})

srv = HTTPServer(("127.0.0.1", 0), Handler)
open(sys.argv[1], "w").write(str(srv.server_address[1]))
srv.serve_forever()
PY
  MOCK_PID=$!
  for _ in $(seq 1 100); do [ -s "$MOCK_PORT_FILE" ] && break; sleep 0.05; done
  MOCK_PORT="$(cat "$MOCK_PORT_FILE" 2>/dev/null || true)"
  [ -n "$MOCK_PORT" ] || { echo "FAIL mock server did not start"; exit 1; }
  BASE="http://127.0.0.1:$MOCK_PORT"
  AUTH=(-H "Authorization: Bearer self-test")
  request_json POST /v1/runs "" "$BODY"
  assert_http 400 "self-test: missing Idempotency-Key rejected"
  [ "$(json_field "$RESP_FILE" error.class)" = "VALIDATION" ] && ok "self-test: VALIDATION class" || bad "self-test: VALIDATION class"
  create_run self-test-key-1 "self-test: create run-1" || true
  R1="$CREATED_ID"
  create_run self-test-key-2 "self-test: create run-2" || true
  R2="$CREATED_ID"
  [ -n "$R1" ] && [ -n "$R2" ] && [ "$R1" != "$R2" ] && ok "self-test: distinct keys -> distinct ids" || bad "self-test: distinct ids"
  create_run self-test-key-1 "self-test: replay run-1" || true
  [ -n "$R1" ] && [ "$CREATED_ID" = "$R1" ] && ok "self-test: replay -> same id" || bad "self-test: replay id"
  request_json POST /v1/runs self-test-key-1 '{"template":"different"}'
  assert_http 409 "self-test: reused key + different body -> 409"
  [ "$(json_field "$RESP_FILE" error.class)" = "CONFLICT" ] && ok "self-test: CONFLICT class" || bad "self-test: CONFLICT class"
  request_json GET "/v1/runs/$R1" "" ""
  assert_http 200 "self-test: GET run"
  [ "$(json_field "$RESP_FILE" id)" = "$R1" ] && ok "self-test: GET id" || bad "self-test: GET id"
  echo "SUMMARY pass=$PASS fail=$FAIL mode=self-test"
  [ "$FAIL" = "0" ]
  exit $?
fi

echo "=== build / image metadata ==="
GIT_SHA="$(git -C "$ROOT" rev-parse HEAD)"
docker build --build-arg VERSION=0.1.0 --build-arg COMMIT="$GIT_SHA" -t "$IMAGE" "$ROOT"
IMG_ID="$(docker image inspect --format '{{.Id}}' "$IMAGE")"
ARCH="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$IMAGE")"
IMG_USER="$(docker image inspect --format '{{.Config.User}}' "$IMAGE")"
IMG_SIZE="$(docker image inspect --format '{{.Size}}' "$IMAGE")"
ok "image built: id=$IMG_ID platform=$ARCH user=${IMG_USER:-<unset>} size=$IMG_SIZE commit=$GIT_SHA"
BUILT_VERSION="$(docker run --rm "$IMAGE" version 2>&1 || true)"
case "$BUILT_VERSION" in
  *"commit $GIT_SHA"*) ok "image binary reports injected commit ($GIT_SHA)" ;;
  *) bad "image binary commit mismatch: $(echo "$BUILT_VERSION" | tail -1)" ;;
esac

docker volume create "$SECRET_VOL" >/dev/null
if ! docker run --rm --user 0:0 --entrypoint sh \
     -v "$WORK/secrets:/src:ro" -v "$SECRET_VOL:/dst" "$IMAGE" -c \
     'set -e; cp /src/master.key /dst/onboarding_master_key; cp /src/admin.token /dst/onboard_admin_token; chown 10001:10001 /dst/onboarding_master_key /dst/onboard_admin_token; chmod 400 /dst/onboarding_master_key /dst/onboard_admin_token'; then
  bad "secret volume initialization"
fi
MOUNTS=(--mount type=volume,src="$SECRET_VOL",dst=/run/secrets,readonly)

echo "=== run + health + hardening ==="
run_app "$PROJECT-app" "$PORT" "$WORK/data"
curl -fsS -m 3 "$BASE/healthz" >/dev/null 2>&1 || true
wait_http "$PORT" /healthz 200 60 && ok "/healthz 200" || bad "/healthz"
wait_http "$PORT" /readyz 200 60 && ok "/readyz 200 (schema/revocation-table/master-key ready)" || bad "/readyz"
wait_health "$PROJECT-app" 90 && ok "Docker HEALTHCHECK healthy (readyz-based)" || bad "Docker HEALTHCHECK"
docker exec "$PROJECT-app" /app/onboardd healthcheck >/dev/null 2>&1 \
  && ok "in-container 'onboardd healthcheck' exit 0" || bad "in-container healthcheck"
[ "$(meta_schema "$PORT")" = "5" ] && ok "fresh database migrated to schema 5" || bad "fresh schema"
[ "$(docker exec "$PROJECT-app" id -u)" = "10001" ] && ok "runs as UID 10001 (non-root)" || bad "non-root"
[ "$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$PROJECT-app")" = "true" ] \
  && ok "read-only root filesystem (HostConfig)" || bad "ReadonlyRootfs"
docker exec "$PROJECT-app" sh -c 'touch /app/forbidden 2>/dev/null' >/dev/null 2>&1 \
  && bad "rootfs is writable" || ok "write outside /app/data refused"
docker inspect --format '{{json .HostConfig.CapDrop}}' "$PROJECT-app" | grep -q 'ALL' && ok "cap_drop ALL" || bad "cap_drop ALL"
docker inspect --format '{{json .HostConfig.SecurityOpt}}' "$PROJECT-app" | grep -q 'no-new-privileges' \
  && ok "no-new-privileges" || bad "no-new-privileges"
MODES="$(docker exec "$PROJECT-app" stat -c '%a %u' /run/secrets/onboarding_master_key /run/secrets/onboard_admin_token 2>/dev/null || true)"
SECRET_OK=1
[ -n "$MODES" ] || { SECRET_OK=0; MODES="<empty>"; }
while IFS=' ' read -r m owner; do
  [ -n "$m" ] || continue
  [ "$m" = "400" ] && [ "$owner" = "10001" ] || SECRET_OK=0
done <<< "$MODES"
docker exec "$PROJECT-app" test -r /run/secrets/onboarding_master_key \
  && docker exec "$PROJECT-app" test -r /run/secrets/onboard_admin_token \
  && SECRET_READ=1 || SECRET_READ=0
# An unauthorized UID must not be able to read the same secret volume. The
# image has no shell? It is Debian slim, so `sh` is available.
if docker run --rm --user 10002:10002 --entrypoint sh -v "$SECRET_VOL:/run/secrets:ro" "$IMAGE" \
     -c 'test ! -r /run/secrets/onboarding_master_key && test ! -r /run/secrets/onboard_admin_token' >/dev/null 2>&1; then
  SECRET_DENIED=1
else
  SECRET_DENIED=0
fi
[ "$SECRET_OK" = "1" ] && [ "$SECRET_READ" = "1" ] && [ "$SECRET_DENIED" = "1" ] \
  && ok "secrets 0400 uid10001 readable, unauthorized UID 10002 denied (modes: $(echo "$MODES" | tr '\n' ' '))" \
  || bad "secret permissions/readability (modes: $MODES readable=$SECRET_READ denied=$SECRET_DENIED)"
test -f "$WORK/data/state.sqlite" && ok "SQLite persisted on host bind mount" || bad "no SQLite file"
[ "$(docker exec "$PROJECT-app" stat -c '%u' /app/data/state.sqlite)" = "10001" ] \
  && ok "SQLite owned by UID 10001 inside the container" || bad "SQLite ownership"
docker exec "$PROJECT-app" sh -c 'ls /app/data' | grep -q 'state.sqlite-wal' \
  && ok "WAL file present in writable data volume" || bad "WAL file missing"
DB_MODES="$(docker exec "$PROJECT-app" stat -c '%a' /app/data/state.sqlite /app/data/state.sqlite-wal /app/data/state.sqlite-shm 2>/dev/null || true)"
if [ "$(echo "$DB_MODES" | sort -u | tr '\n' ' ')" = "600 " ]; then
  ok "SQLite DB/WAL/SHM are 0600 inside the container"
else
  bad "SQLite DB/WAL/SHM modes: $(echo "$DB_MODES" | tr '\n' ' ')"
fi
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
# The v2 DB was created by host python (host UID). Give it the production
# ownership (10001:10001/0600) via a one-shot root container; a world-writable
# file would not let the service enforce its 0600 invariant (and would test the
# wrong contract).
docker run --rm --user 0:0 -v "$WORK/v2data:/data" --entrypoint sh "$IMAGE" -c \
  'chown 10001:10001 /data/state.sqlite && chmod 600 /data/state.sqlite'
run_app "$PROJECT-upgrade" "$PORT_UPGRADE" "$WORK/v2data"
if wait_http "$PORT_UPGRADE" /readyz 200 60; then
  ok "v2 database upgraded and /readyz 200"
  [ "$(meta_schema "$PORT_UPGRADE")" = "5" ] && ok "v2 -> schema 5 after container start" || bad "v2 upgrade schema"
else
  bad "v2 upgrade readyz"
  echo "--- v2 upgrade diagnostics (state/logs/permissions) ---"
  docker inspect --format 'state={{.State.Status}} exit={{.State.ExitCode}} error={{.State.Error}}' "$PROJECT-upgrade" 2>&1 || true
  docker logs "$PROJECT-upgrade" 2>&1 | tail -30 || true
  ls -la "$WORK/v2data" 2>&1 || true
  docker exec "$PROJECT-upgrade" ls -la /app/data 2>&1 || true
fi
docker rm -f "$PROJECT-upgrade" >/dev/null 2>&1 || true

echo "=== runs + idempotency contract ==="
BODY='{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox"}'
BODY_ALT='{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox","credential_provider":"fake-credential"}'
# Missing Idempotency-Key must be rejected by the real API contract (no id read).
request_json POST /v1/runs "" "$BODY"
assert_http 400 "POST /v1/runs without Idempotency-Key rejected"
[ "$(json_field "$RESP_FILE" error.class)" = "VALIDATION" ] && ok "missing-key error class VALIDATION" || bad "missing-key error class"
# Distinct keys -> distinct runs.
create_run r51b-run-1 'create run 1 (distinct key)' || true
RUN1="$CREATED_ID"
create_run r51b-run-2 'create run 2 (distinct key)' || true
RUN2="$CREATED_ID"
[ -n "$RUN1" ] && [ -n "$RUN2" ] && [ "$RUN1" != "$RUN2" ] && ok "distinct Idempotency-Key -> distinct runs" || bad "distinct run creation"
# Same key + same body -> same run (replay).
create_run r51b-idem 'create replay run' || true
IDEM="$CREATED_ID"
create_run r51b-idem 'replay same key/body' || true
IDEM2="$CREATED_ID"
[ -n "$IDEM" ] && [ "$IDEM" = "$IDEM2" ] && ok "same Idempotency-Key -> same run" || bad "idempotent replay mismatch"
# Same key + different body -> conflict, not a new run.
request_json POST /v1/runs r51b-idem "$BODY_ALT"
assert_http 409 "same Idempotency-Key with different body rejected"
[ "$(json_field "$RESP_FILE" error.class)" = "CONFLICT" ] && ok "reused-key error class CONFLICT" || bad "reused-key error class"
# RUN1 must be the one created above, and must still be serially retrievable.
request_json GET "/v1/runs/$RUN1" "" ""
assert_http 200 "GET created run"
[ "$(json_field "$RESP_FILE" id)" = "$RUN1" ] && ok "GET returned the created run id" || bad "GET run id mismatch"
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
# Stop the app so the WAL is checkpointed before copying the DB out for host
# inspection; then restart it for the replay check.
docker stop --time 10 "$PROJECT-app" >/dev/null
db_copy_out "$PROJECT-app" "$WORK/inspect"
ROWS="$(python3 - "$WORK/inspect/state.sqlite" <<'PY'
import sqlite3,sys
print(sqlite3.connect(sys.argv[1]).execute("select count(*) from session_revocations").fetchone()[0])
PY
)"
[ "$ROWS" -ge 1 ] && ok "revocation row persisted in SQLite ($ROWS)" || bad "no revocation row"
docker start "$PROJECT-app" >/dev/null
wait_http "$PORT" /readyz 200 60 || bad "restart after revocation inspection"
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
run_app "$PROJECT-app2" "$PORT2" "$WORK/data" >/dev/null 2>&1 || true
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
docker stop --time 10 "$PROJECT-app" >/dev/null
db_copy_out "$PROJECT-app" "$WORK/backup-src"
ONBOARD_BACKUP_ENCRYPT=1 ONBOARD_KEYRING="$WORK/secrets/master.key" \
  bash "$ROOT/scripts/backup.sh" --db "$WORK/backup-src/state.sqlite" --out-dir "$WORK/backup" \
  --encrypt --keyring "$WORK/secrets/master.key" >/dev/null 2>&1 \
  && ok "encrypted snapshot of container DB" || bad "encrypted backup"
ENC="$(ls "$WORK"/backup/*.sqlite.enc 2>/dev/null | head -1 || true)"
if [ -n "$ENC" ] && bash "$ROOT/scripts/verify_backup.sh" "$ENC" --keyring "$WORK/secrets/master.key" >/dev/null 2>&1; then
  ok "verify encrypted snapshot"
else
  bad "verify snapshot"
fi
mkdir -p "$WORK/restored"; chmod 777 "$WORK/restored"
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
  ENC_MODE="$(stat -c '%a' "$ENC" 2>/dev/null || stat -f '%Lp' "$ENC" 2>/dev/null || echo '?')"
  [ "$ENC_MODE" = "600" ] && ok "encrypted snapshot file is 0600" || bad "snapshot mode=$ENC_MODE"
  # Start a real instance on the restored database and verify readiness + data.
  docker run --rm --user 0:0 -v "$WORK/restored:/data" --entrypoint sh "$IMAGE" -c \
    'chown 10001:10001 /data/state.sqlite && chmod 600 /data/state.sqlite'
  run_app "$PROJECT-restored" "$PORT_RESTORED" "$WORK/restored"
  wait_http "$PORT_RESTORED" /readyz 200 60 && ok "restored DB instance /readyz 200" || bad "restored DB instance readyz"
  RESTORED_BODY="$WORK/restored-run.json"
  RESTORED_CODE="$(curl -sS -m 10 -o "$RESTORED_BODY" -w '%{http_code}' "${AUTH[@]}" \
    "http://127.0.0.1:$PORT_RESTORED/v1/runs/$RUN1" 2>/dev/null || echo 000)"
  if [ "$RESTORED_CODE" = "200" ] && [ "$(json_field "$RESTORED_BODY" id)" = "$RUN1" ]; then
    ok "restored DB instance serves the original run"
  else
    bad "restored DB run lookup HTTP $RESTORED_CODE"
  fi
  docker rm -f "$PROJECT-restored" >/dev/null 2>&1 || true
  # Wrong key must fail closed and must not write the target.
  mkdir -p "$WORK/restored-bad"
  if CONFIRM_RESTORE=1 bash "$ROOT/scripts/restore.sh" --snapshot "$ENC" \
       --target "$WORK/restored-bad/state.sqlite" --keyring "$WORK/secrets/badkey" --force >/dev/null 2>&1; then
    bad "restore with wrong key unexpectedly succeeded"
  else
    ok "restore with wrong key fails closed"
  fi
  [ ! -e "$WORK/restored-bad/state.sqlite" ] && ok "wrong-key restore left the target untouched" \
    || bad "wrong-key restore created a target file"
else
  bad "restore snapshot"
fi

echo "=== degraded DB: /readyz 503 + healthcheck red ==="
cp "$WORK/backup-src/state.sqlite" "$WORK/degraded/state.sqlite"
chmod 666 "$WORK/degraded/state.sqlite"
python3 - "$WORK/degraded/state.sqlite" <<'PY'
import sqlite3,sys
db=sqlite3.connect(sys.argv[1]); db.execute("DROP TABLE session_revocations"); db.commit(); db.close()
PY
docker run --rm --user 0:0 -v "$WORK/degraded:/data" --entrypoint sh "$IMAGE" -c \
  'chown 10001:10001 /data/state.sqlite && chmod 600 /data/state.sqlite'
run_app "$PROJECT-degraded" "$PORT_DEGRADED" "$WORK/degraded"
wait_http "$PORT_DEGRADED" /healthz 200 30 || bad "degraded healthz"
wait_http "$PORT_DEGRADED" /readyz 503 30 && ok "degraded DB -> /readyz 503" || bad "degraded readyz"
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
  --tmpfs /tmp:size=8m -p "127.0.0.1:$PORT_RO:8788" -v "$WORK/rodata:/app/data" "${MOUNTS[@]}" "${ENVS[@]}" "$IMAGE" >/dev/null 2>&1 || true
sleep 4
RO_STATE="$(docker inspect --format '{{.State.Status}}' "$PROJECT-ro" 2>/dev/null || echo missing)"
if [ "$RO_STATE" = "exited" ]; then
  ok "unwritable data directory fails closed (container exited)"
else
  RO_CODE="$(curl -sS -m 2 -o /dev/null -w '%{http_code}' "${AUTH[@]}" "http://127.0.0.1:$PORT_RO/readyz" 2>/dev/null || true)"
  [ "$RO_CODE" != "200" ] && ok "unwritable data directory not ready (probe=$RO_CODE)" || bad "unwritable data dir reported ready"
fi
docker rm -f "$PROJECT-ro" >/dev/null 2>&1 || true

echo "=== compose parity (isolated project) ==="
docker rm -f "$PROJECT-app" >/dev/null 2>&1 || true
COMPOSE_SECRETS="$WORK/compose-secrets"
mkdir -p "$COMPOSE_SECRETS"
cp "$WORK/secrets/master.key" "$COMPOSE_SECRETS/master.key"
cp "$WORK/secrets/admin.token" "$COMPOSE_SECRETS/admin.token"
COMPOSE_STRICT=0
if [ "$(id -u)" = "0" ]; then
  chown 10001:10001 "$COMPOSE_SECRETS"/* && chmod 400 "$COMPOSE_SECRETS"/* && COMPOSE_STRICT=1
elif sudo -n true >/dev/null 2>&1; then
  if sudo -n chown 10001:10001 "$COMPOSE_SECRETS"/* && sudo -n chmod 400 "$COMPOSE_SECRETS"/*; then
    COMPOSE_STRICT=1
  else
    chmod 0444 "$COMPOSE_SECRETS"/* 2>/dev/null || true
  fi
else
  chmod 0444 "$COMPOSE_SECRETS"/* 2>/dev/null || true
fi
compose_isolated() {
  R51_IMAGE="$IMAGE" R51_DATA="$WORK/data" R51_MASTER_KEY="$COMPOSE_SECRETS/master.key" \
    R51_ADMIN_TOKEN="$COMPOSE_SECRETS/admin.token" R51_PORT="$PORT" \
    docker compose -p "$PROJECT" -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" "$@"
}
compose_isolated config >/dev/null && ok "compose config (isolated override)" || bad "compose config"
compose_isolated up -d >/dev/null && ok "compose up (isolated project)" || bad "compose up"
C=""
for _ in $(seq 1 90); do
  C="$(compose_isolated ps --format '{{.Status}}' 2>/dev/null | head -1 || true)"
  echo "$C" | grep -qi healthy && break; sleep 1
done
echo "$C" | grep -qi healthy && ok "compose service healthy: $C" || bad "compose health: $C"
COMPOSE_CID="$(compose_isolated ps -q onboarding 2>/dev/null | head -1 || true)"
if [ -n "$COMPOSE_CID" ] && docker exec "$COMPOSE_CID" test -r /run/secrets/onboarding_master_key \
     && docker exec "$COMPOSE_CID" test -r /run/secrets/onboard_admin_token; then
  ok "compose secrets readable by UID 10001"
else
  bad "compose secrets not readable by UID 10001 (check host ownership / compose secret mapping)"
fi
COMPOSE_MODES="$(docker exec "$COMPOSE_CID" stat -c '%a %u' /run/secrets/onboarding_master_key /run/secrets/onboard_admin_token 2>/dev/null || true)"
if [ "$COMPOSE_STRICT" = "1" ]; then
  if [ -n "$COMPOSE_MODES" ] && ! echo "$COMPOSE_MODES" | grep -qvE '^(400|600) 10001$' \
     && docker exec --user 10002 "$COMPOSE_CID" sh -c 'test ! -r /run/secrets/onboarding_master_key' >/dev/null 2>&1; then
    ok "compose secrets 0400 uid10001, unauthorized UID 10002 denied"
  else
    bad "compose secret strictness (modes=$(echo "$COMPOSE_MODES" | tr '\n' ' '))"
  fi
else
  echo "WARN compose host-secret strictness not testable without root/sudo; readable-by-10001 PASS, non-authorized denial NOT_RUN"
fi
COUNT="$(docker ps --filter "label=com.docker.compose.project=$PROJECT" --format '{{.ID}}' | wc -l | tr -d ' ')"
[ "$COUNT" = "1" ] && ok "exactly one writer container in the compose project" || bad "writer count=$COUNT"
COMPOSE_PORT="$(compose_isolated port onboarding 8788 2>/dev/null || true)"
case "$COMPOSE_PORT" in 127.0.0.1:*) ok "compose published port loopback-only ($COMPOSE_PORT)";; *) bad "compose port: $COMPOSE_PORT";; esac
compose_isolated down -v >/dev/null 2>&1 || true

if [ -n "${R51_EVIDENCE_DIR:-}" ]; then
  mkdir -p "$R51_EVIDENCE_DIR"
  docker image inspect "$IMAGE" > "$R51_EVIDENCE_DIR/image-inspect.json"
  {
    echo "image_id=$IMG_ID"
    echo "platform=$ARCH"
    echo "user=$IMG_USER"
    echo "size=$IMG_SIZE"
    echo "commit=$GIT_SHA"
  } > "$R51_EVIDENCE_DIR/image.txt"
fi
echo "SUMMARY pass=$PASS fail=$FAIL image=$IMG_ID platform=$ARCH"
[ "$FAIL" = "0" ]
