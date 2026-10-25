#!/usr/bin/env bash
# R11 first-install / failure / recovery drill in isolated Docker.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if ! docker info >/dev/null 2>&1; then echo "NOT_RUN: Docker unavailable"; exit 3; fi
ID="r12-deploy-$(date +%s)-$$"; WORK="$ROOT/.sandbox/r12/deploy-$ID"
PASS=0; FAIL=0
SECVOL=""; EMPTYVOL=""; BADVOL=""; IMAGE=""
ok(){ PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad(){ FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
free_port(){ python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }
cleanup(){
  docker rm -f "$ID-app" "$ID-app2" "$ID-nosecret" "$ID-badsecret" "$ID-ro" "$ID-corrupt" "$ID-partial" "$ID-toonew" >/dev/null 2>&1 || true
  [ -z "$SECVOL" ] || docker volume rm "$SECVOL" >/dev/null 2>&1 || true
  [ -z "$EMPTYVOL" ] || docker volume rm "$EMPTYVOL" >/dev/null 2>&1 || true
  [ -z "$BADVOL" ] || docker volume rm "$BADVOL" >/dev/null 2>&1 || true
  [ -z "$IMAGE" ] || docker image rm "$IMAGE" >/dev/null 2>&1 || true
  if ! rm -rf "$WORK" 2>/dev/null; then
    sudo -n rm -rf "$WORK" 2>/dev/null || true
  fi
  return 0
}
trap cleanup EXIT
umask 077
mkdir -p "$WORK"/{data,data2,empty,nosecret,secrets}
chgrp "$(id -g)" "$WORK/data" "$WORK/data2" "$WORK/empty" "$WORK/nosecret"
chmod 770 "$WORK/data" "$WORK/data2" "$WORK/empty" "$WORK/nosecret"
head -c 32 /dev/urandom > "$WORK/secrets/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/secrets/admin.token"
chmod 600 "$WORK/secrets/"*
GIT_SHA="$(git -C "$ROOT" rev-parse HEAD)"
IMAGE="onboarding-r12-deploy:$ID"
docker build --build-arg VERSION=0.1.0 --build-arg COMMIT="$GIT_SHA" -t "$IMAGE" "$ROOT" >/dev/null
ok "image built"
SECVOL="$ID-secrets"; EMPTYVOL="$ID-empty-secrets"; BADVOL="$ID-bad-secrets"
docker volume create "$SECVOL" >/dev/null
docker volume create "$EMPTYVOL" >/dev/null
docker volume create "$BADVOL" >/dev/null
docker run --rm --user 0:0 --entrypoint sh -v "$WORK/secrets:/src:ro" -v "$SECVOL:/dst" "$IMAGE" -c \
  'cp /src/master.key /dst/onboarding_master_key; cp /src/admin.token /dst/onboard_admin_token; chown 10001:10001 /dst/*; chmod 400 /dst/*'
docker run --rm --user 0:0 --entrypoint sh -v "$WORK/secrets:/src:ro" -v "$BADVOL:/dst" "$IMAGE" -c \
  'cp /src/master.key /dst/onboarding_master_key; cp /src/admin.token /dst/onboard_admin_token; chown 10001:10001 /dst/*; chmod 644 /dst/*'
PORT="$(free_port)"
start(){ docker run -d --name "$1" --user 10001:10001 --group-add "$(id -g)" --read-only --cap-drop ALL \
  --security-opt no-new-privileges --tmpfs /tmp:rw,noexec,nosuid,size=16m -p "127.0.0.1:$2:8788" \
  -v "$3:/app/data" --mount type=volume,src="${4:-$SECVOL}",dst=/run/secrets,readonly \
  -e ONBOARD_LISTEN=0.0.0.0:8788 -e ONBOARD_ALLOW_PUBLIC_LISTEN=1 -e ONBOARD_DB_PATH=/app/data/state.sqlite \
  -e ONBOARD_FAKE_PROVIDERS=true -e ONBOARD_DOMAINS=moonbyte.test \
  -e ONBOARD_MASTER_KEY_FILE=/run/secrets/onboarding_master_key \
  -e ONBOARD_ADMIN_TOKEN_FILE=/run/secrets/onboard_admin_token "$IMAGE" >/dev/null; }
assert_expected_failure(){
  local name="$1" port="$2" pattern="$3" label="$4" running exitcode code logs
  if ! docker inspect "$name" >/dev/null 2>&1; then bad "$label container was not created"; return; fi
  running="$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null || echo unknown)"
  code="$(curl -sS -m 2 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(cat "$WORK/secrets/admin.token")" "http://127.0.0.1:$port/readyz" 2>/dev/null || true)"
  logs="$(docker logs "$name" 2>&1 || true)"
  if [ "$running" = "false" ]; then
    exitcode="$(docker inspect -f '{{.State.ExitCode}}' "$name" 2>/dev/null || echo -1)"
    if [ "$exitcode" -ne 0 ] && printf '%s' "$logs" | grep -Eiq "$pattern"; then ok "$label fail-closed"; else bad "$label exited without matching expected failure evidence"; printf '%s\n' "$logs" | tail -20; fi
  elif [ "$code" != "200" ] && printf '%s' "$logs" | grep -Eiq "$pattern"; then
    ok "$label not ready with expected failure evidence"
  else
    bad "$label did not prove the expected fail-closed behavior"
    printf '%s\n' "$logs" | tail -20
  fi
}
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
RUN="$(curl -sS -H "Authorization: Bearer $(cat "$WORK/secrets/admin.token")" -H 'Content-Type: application/json' -H "Idempotency-Key: r12-deploy-1" -X POST "http://127.0.0.1:$PORT/v1/runs" -d "$BODY" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
[ -n "$RUN" ] && ok "run created" || bad "run create"
docker kill -s KILL "$ID-app" >/dev/null; docker rm -f "$ID-app" >/dev/null
start "$ID-app2" "$PORT" "$WORK/data"
if wait_ready "$PORT"; then ok "SIGKILL + container recreate recovers"; else bad "recreate not ready"; fi
curl -sS -H "Authorization: Bearer $(cat "$WORK/secrets/admin.token")" "http://127.0.0.1:$PORT/v1/runs/$RUN" | grep -q "$RUN" && ok "data survives recreate" || bad "data lost"
docker restart "$ID-app2" >/dev/null
if wait_ready "$PORT"; then ok "docker restart recovers"; else bad "docker restart not ready"; fi
curl -sS -H "Authorization: Bearer $(cat "$WORK/secrets/admin.token")" "http://127.0.0.1:$PORT/v1/runs/$RUN" | grep -q "$RUN" && ok "data survives restart" || bad "data lost after restart"
docker rm -f "$ID-app2" >/dev/null
echo "=== failure modes ==="
start "$ID-nosecret" "$PORT" "$WORK/nosecret" "$EMPTYVOL"
sleep 4
assert_expected_failure "$ID-nosecret" "$PORT" 'secret|master key|admin token|credential' "missing secret"
docker rm -f "$ID-nosecret" >/dev/null 2>&1 || true
PORT_BAD="$(free_port)"
start "$ID-badsecret" "$PORT_BAD" "$WORK/nosecret" "$BADVOL"
sleep 4
assert_expected_failure "$ID-badsecret" "$PORT_BAD" 'permission|mode|secret|master key|credential' "bad secret permissions"
docker rm -f "$ID-badsecret" >/dev/null 2>&1 || true

chmod 550 "$WORK/data"
# Existing SQLite files remain writable by their owner even when the directory
# lacks write permission. Make the data files read-only too, so the app's write
# probe genuinely fails instead of serving with an unchanged WAL.
for f in "$WORK/data"/*; do
  [ -e "$f" ] || continue
  chmod a-w "$f" 2>/dev/null || sudo -n chmod a-w "$f"
done
if docker run --rm --user 10001:10001 --group-add "$(id -g)" -v "$WORK/data:/probe" --entrypoint sh "$IMAGE" -c 'touch /probe/.r12-write-probe' >/dev/null 2>&1; then
  bad "read-only fault injection failed: UID 10001 could write"
else
  ok "read-only fault injection verified for UID 10001"
fi
PORT_RO="$(free_port)"
start "$ID-ro" "$PORT_RO" "$WORK/data"
sleep 4
assert_expected_failure "$ID-ro" "$PORT_RO" 'permission denied|read-only|readonly|unable to open database|attempt to write|database is locked' "read-only DB directory"
chmod 770 "$WORK/data"
for f in "$WORK/data"/*; do
  [ -e "$f" ] || continue
  chmod u+w "$f" 2>/dev/null || sudo -n chmod u+w "$f"
done
docker rm -f "$ID-ro" >/dev/null 2>&1 || true

if ! cp -a "$WORK/data/." "$WORK/data2/" 2>/dev/null; then
  sudo -n cp -a "$WORK/data/." "$WORK/data2/"
  sudo -n chown -R "$(id -u):$(id -g)" "$WORK/data2"
fi
printf 'not-a-sqlite-db' > "$WORK/data2/state.sqlite"
PORT_CORRUPT="$(free_port)"
start "$ID-corrupt" "$PORT_CORRUPT" "$WORK/data2"
sleep 4
assert_expected_failure "$ID-corrupt" "$PORT_CORRUPT" 'malformed|not a database|corrupt|database disk image|sqlite' "corrupt DB"
docker rm -f "$ID-corrupt" >/dev/null 2>&1 || true
echo "=== partial schema upgrade ==="
mkdir -p "$WORK/partial"; python3 - "$ROOT/internal/storage/migrations" "$WORK/partial/state.sqlite" <<'PY'
import sqlite3,sys,glob,os
db=sqlite3.connect(sys.argv[2])
for f in sorted(glob.glob(sys.argv[1]+'/000[1-3]_*.sql')):
    db.executescript(open(f).read())
db.commit(); db.close()
PY
chgrp "$(id -g)" "$WORK/partial"; chmod 770 "$WORK/partial"
if ! chown 10001:10001 "$WORK/partial/state.sqlite" 2>/dev/null; then
  sudo -n chown 10001:10001 "$WORK/partial/state.sqlite"
  sudo -n chmod 600 "$WORK/partial/state.sqlite"
else
  chmod 600 "$WORK/partial/state.sqlite"
fi
PORT_PARTIAL="$(free_port)"
start "$ID-partial" "$PORT_PARTIAL" "$WORK/partial"
if wait_ready "$PORT_PARTIAL"; then ok "partial schema 3 upgrades to 5"; else bad "partial schema upgrade"; docker logs "$ID-partial" 2>&1 | tail -20; fi
SCHEMA="$(curl -sS -H "Authorization: Bearer $(cat "$WORK/secrets/admin.token")" "http://127.0.0.1:$PORT_PARTIAL/v1/meta" 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin).get("schema_version",0))' 2>/dev/null || echo 0)"
[ "$SCHEMA" = "5" ] && ok "partial schema reports 5" || bad "partial schema $SCHEMA"
docker rm -f "$ID-partial" >/dev/null 2>&1 || true
echo "=== schema too-new rejection ==="
mkdir -p "$WORK/toonew"; cp -a "$WORK/partial/." "$WORK/toonew/" 2>/dev/null || { sudo -n cp -a "$WORK/partial/." "$WORK/toonew/"; sudo -n chown -R "$(id -u):$(id -g)" "$WORK/toonew"; }
python3 - "$WORK/toonew/state.sqlite" <<'DBPY'
import sqlite3,sys
db=sqlite3.connect(sys.argv[1]); db.execute("INSERT OR REPLACE INTO schema_migrations(version,applied_at_ms) VALUES(6,strftime('%s','now')*1000)"); db.commit(); db.close()
DBPY
chgrp "$(id -g)" "$WORK/toonew"; chmod 770 "$WORK/toonew"
if ! chown -R 10001:10001 "$WORK/toonew" 2>/dev/null; then sudo -n chown -R 10001:10001 "$WORK/toonew"; fi
chmod 600 "$WORK/toonew/state.sqlite" 2>/dev/null || sudo -n chmod 600 "$WORK/toonew/state.sqlite"
PORT_NEW="$(free_port)"; start "$ID-toonew" "$PORT_NEW" "$WORK/toonew"
sleep 4
assert_expected_failure "$ID-toonew" "$PORT_NEW" 'schema.*new|newer than|unsupported.*schema|database version' "schema newer than binary"
docker rm -f "$ID-toonew" >/dev/null 2>&1 || true
if [ -n "${R12_DEPLOY_REPORT:-}" ]; then
  mkdir -p "$(dirname "$R12_DEPLOY_REPORT")"
  python3 - "$R12_DEPLOY_REPORT" "$PASS" "$FAIL" "$GIT_SHA" <<'RPT'
import json,sys,datetime
out,passed,failed,sha=sys.argv[1:]
json.dump({"steps":{"pass":int(passed),"fail":int(failed)},"commit":sha,
           "cleanup_status":"attempted","recorded_at_utc":datetime.datetime.now(datetime.timezone.utc).isoformat()},
          open(out,"w",encoding="utf-8"),indent=2)
RPT
fi
echo "SUMMARY pass=$PASS fail=$FAIL"
trap - EXIT
cleanup || true
if [ "$FAIL" -eq 0 ]; then exit 0; else exit 1; fi
