#!/usr/bin/env bash
# R11 first-install / failure / recovery drill in isolated Docker.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if ! docker info >/dev/null 2>&1; then echo "NOT_RUN: Docker unavailable"; exit 3; fi
ID="r11-deploy-$(date +%s)-$$"; WORK="$ROOT/.sandbox/r11/deploy-$ID"
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad(){ FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
free_port(){ python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }
cleanup(){ docker rm -f "$ID-app" "$ID-app2" "$ID-nosecret" "$ID-ro" "$ID-corrupt" "$ID-partial" >/dev/null 2>&1 || true; rm -rf "$WORK"; return 0; }
trap cleanup EXIT
rm -rf "$WORK"; mkdir -p "$WORK"/{data,data2,empty,nosecret,secrets}
umask 077
head -c 32 /dev/urandom > "$WORK/secrets/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/secrets/admin.token"
chmod 600 "$WORK/secrets/"*
chmod 777 "$WORK/data" "$WORK/data2" "$WORK/empty" "$WORK/nosecret"
GIT_SHA="$(git -C "$ROOT" rev-parse HEAD)"
IMAGE="onboarding-r11-deploy:$ID"
docker build --build-arg VERSION=0.1.0 --build-arg COMMIT="$GIT_SHA" -t "$IMAGE" "$ROOT" >/dev/null
ok "image built"
SECVOL="$ID-secrets"; docker volume create "$SECVOL" >/dev/null
docker run --rm --user 0:0 --entrypoint sh -v "$WORK/secrets:/src:ro" -v "$SECVOL:/dst" "$IMAGE" -c \
  'cp /src/master.key /dst/onboarding_master_key; cp /src/admin.token /dst/onboard_admin_token; chown 10001:10001 /dst/*; chmod 400 /dst/*'
PORT="$(free_port)"
start(){ docker run -d --name "$1" --user 10001:10001 --read-only --cap-drop ALL \
  --security-opt no-new-privileges --tmpfs /tmp:rw,noexec,nosuid,size=16m -p "127.0.0.1:$2:8788" \
  -v "$3:/app/data" --mount type=volume,src="${4:-$SECVOL}",dst=/run/secrets,readonly \
  -e ONBOARD_LISTEN=0.0.0.0:8788 -e ONBOARD_ALLOW_PUBLIC_LISTEN=1 -e ONBOARD_DB_PATH=/app/data/state.sqlite \
  -e ONBOARD_FAKE_PROVIDERS=true -e ONBOARD_DOMAINS=moonbyte.test \
  -e ONBOARD_MASTER_KEY_FILE=/run/secrets/onboarding_master_key \
  -e ONBOARD_ADMIN_TOKEN_FILE=/run/secrets/onboard_admin_token "$IMAGE" >/dev/null; }
wait_ready(){ for _ in $(seq 1 60); do
  c="$(curl -sS -m 1 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(cat "$WORK/secrets/admin.token")" "http://127.0.0.1:$1/readyz" 2>/dev/null || true)"
  [ "$c" = "200" ] && return 0; sleep 0.5; done; return 1; }
echo "=== first install ==="
start "$ID-app" "$PORT" "$WORK/data"
if wait_ready "$PORT"; then ok "first install ready"; else bad "first install not ready"; docker logs "$ID-app" | tail -10; fi
SCHEMA="$(curl -sS -H "Authorization: Bearer $(cat "$WORK/secrets/admin.token")" "http://127.0.0.1:$PORT/v1/meta" | python3 -c 'import json,sys;print(json.load(sys.stdin)["schema_version"])')"
[ "$SCHEMA" = "5" ] && ok "first install schema 5" || bad "schema $SCHEMA"
# Create durable data (event/run via API) and SIGKILL.
BODY='{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox"}'
RUN="$(curl -sS -H "Authorization: Bearer $(cat "$WORK/secrets/admin.token")" -H 'Content-Type: application/json' -H "Idempotency-Key: r11-deploy-1" -X POST "http://127.0.0.1:$PORT/v1/runs" -d "$BODY" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
[ -n "$RUN" ] && ok "run created" || bad "run create"
docker kill -s KILL "$ID-app" >/dev/null; docker rm -f "$ID-app" >/dev/null
start "$ID-app2" "$PORT" "$WORK/data"
if wait_ready "$PORT"; then ok "SIGKILL + container recreate recovers"; else bad "recreate not ready"; fi
curl -sS -H "Authorization: Bearer $(cat "$WORK/secrets/admin.token")" "http://127.0.0.1:$PORT/v1/runs/$RUN" | grep -q "$RUN" && ok "data survives recreate" || bad "data lost"
docker rm -f "$ID-app2" >/dev/null
echo "=== failure modes ==="
start "$ID-nosecret" "$PORT" "$WORK/nosecret" "$ID-emptyvol"
sleep 4
if docker inspect -f '{{.State.Running}}' "$ID-nosecret" 2>/dev/null | grep -q false; then ok "missing secret fails closed"; else
  [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/readyz" 2>/dev/null || true)" != "200" ] && ok "missing secret not ready" || bad "missing secret served ready"
fi
docker rm -f "$ID-nosecret" >/dev/null 2>&1 || true
chmod 555 "$WORK/data"; start "$ID-ro" "$PORT" "$WORK/data" || true
sleep 4
if [ "$(curl -sS -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/readyz" 2>/dev/null || true)" != "200" ]; then ok "read-only DB dir fails closed"; else bad "read-only DB dir served"; fi
chmod 777 "$WORK/data"; docker rm -f "$ID-ro" >/dev/null 2>&1 || true
if ! cp -a "$WORK/data/." "$WORK/data2/" 2>/dev/null; then
  sudo -n cp -a "$WORK/data/." "$WORK/data2/"
  sudo -n chown -R "$(id -u):$(id -g)" "$WORK/data2"
fi
printf 'not-a-sqlite-db' > "$WORK/data2/state.sqlite"
start "$ID-corrupt" "$PORT" "$WORK/data2" || true; sleep 4
if docker inspect -f '{{.State.Running}}' "$ID-corrupt" 2>/dev/null | grep -q false; then ok "corrupt DB fails closed"; else
  [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/readyz" 2>/dev/null || true)" != "200" ] && ok "corrupt DB not ready" || bad "corrupt DB served"
fi
docker rm -f "$ID-corrupt" >/dev/null 2>&1 || true
echo "=== partial schema upgrade ==="
mkdir -p "$WORK/partial"; python3 - "$ROOT/internal/storage/migrations" "$WORK/partial/state.sqlite" <<'PY'
import sqlite3,sys,glob,os
db=sqlite3.connect(sys.argv[2])
for f in sorted(glob.glob(sys.argv[1]+'/000[1-3]_*.sql')):
    db.executescript(open(f).read())
db.commit(); db.close()
PY
chmod 777 "$WORK/partial"
if ! chown 10001:10001 "$WORK/partial/state.sqlite" 2>/dev/null; then
  sudo -n chown 10001:10001 "$WORK/partial/state.sqlite"
  sudo -n chmod 600 "$WORK/partial/state.sqlite"
else
  chmod 600 "$WORK/partial/state.sqlite"
fi
start "$ID-partial" "$PORT" "$WORK/partial"
if wait_ready "$PORT"; then ok "partial schema 3 upgrades to 5"; else bad "partial schema upgrade"; docker logs "$ID-partial" 2>&1 | tail -20; fi
SCHEMA="$(curl -sS -H "Authorization: Bearer $(cat "$WORK/secrets/admin.token")" "http://127.0.0.1:$PORT/v1/meta" 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin).get("schema_version",0))' 2>/dev/null || echo 0)"
[ "$SCHEMA" = "5" ] && ok "partial schema reports 5" || bad "partial schema $SCHEMA"
echo "SUMMARY pass=$PASS fail=$FAIL"
trap - EXIT
cleanup || true
[ "$FAIL" = "0" ]
exit $?
