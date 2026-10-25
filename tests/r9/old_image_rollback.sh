#!/usr/bin/env bash
# R9 old-image upgrade/rollback drill.
#
#   old image (R8 commit, schema 3, future-schema guard)
#     -> create run/event/session, encrypted backup
#     -> new image migrates to schema 4 and serves the data
#     -> old image MUST refuse the schema-4 DB (rollback safety)
#     -> restore the pre-upgrade backup to a new volume; old image serves schema 3
#     -> wrong key / failed restore must not overwrite valid data
#
# Exit: 0 PASS, 1 FAIL, 3 NOT_RUN (no Docker/old source).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OLD_SRC="${1:-${R9_OLD_SRC:-}}"
if ! docker info >/dev/null 2>&1; then echo "NOT_RUN: Docker daemon unavailable"; exit 3; fi
[ -n "$OLD_SRC" ] && [ -f "$OLD_SRC/Dockerfile" ] || { echo "NOT_RUN: old source context missing (pass path)"; exit 3; }
ID="r9-rollback-$(date +%s)-$$"
WORK="$ROOT/.sandbox/r9/rollback-$ID"
SECRET_VOL="$ID-secrets"
OLD_IMAGE="account-onboarding-old:$ID"
NEW_IMAGE="account-onboarding-new:$ID"
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad(){ FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
free_port(){ python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }
PORT="$(free_port)"; PORT2="$(free_port)"; while [ "$PORT2" = "$PORT" ]; do PORT2="$(free_port)"; done
BASE="http://127.0.0.1:$PORT"; BASE2="http://127.0.0.1:$PORT2"
cleanup(){
  for n in "$ID-old1" "$ID-new" "$ID-old2" "$ID-oldrefuse" "$ID-oldfuture"; do docker rm -f "$n" >/dev/null 2>&1 || true; done
  docker volume rm "$SECRET_VOL" >/dev/null 2>&1 || true
  docker image rm "$OLD_IMAGE" "$NEW_IMAGE" >/dev/null 2>&1 || true
  rm -rf "$WORK"
  return 0
}
trap cleanup EXIT
rm -rf "$WORK"; mkdir -p "$WORK"/{data,upgrade,restored,futures,secrets,backup,inspect}
umask 077
head -c 32 /dev/urandom > "$WORK/secrets/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/secrets/admin.token"
head -c 10 /dev/urandom > "$WORK/secrets/badkey"
chmod 600 "$WORK/secrets/"*
TOKEN="$(cat "$WORK/secrets/admin.token")"
AUTH=(-H "Authorization: Bearer $TOKEN")
JSON=(-H "Content-Type: application/json")
OLD_SHA="${R9_OLD_SHA:-$(git -C "$OLD_SRC" rev-parse HEAD 2>/dev/null || echo old-unknown)}"
NEW_SHA="$(git -C "$ROOT" rev-parse HEAD)"
echo "=== build old/new images (old $OLD_SHA, new $NEW_SHA) ==="
docker build --build-arg VERSION=0.1.0 --build-arg COMMIT="$OLD_SHA" -t "$OLD_IMAGE" "$OLD_SRC" >/dev/null
docker build --build-arg VERSION=0.1.0 --build-arg COMMIT="$NEW_SHA" -t "$NEW_IMAGE" "$ROOT" >/dev/null
OLD_ID="$(docker image inspect --format '{{.Id}}' "$OLD_IMAGE")"; NEW_ID="$(docker image inspect --format '{{.Id}}' "$NEW_IMAGE")"
ok "images built: old=$OLD_ID new=$NEW_ID"
docker volume create "$SECRET_VOL" >/dev/null
docker run --rm --user 0:0 --entrypoint sh -v "$WORK/secrets:/src:ro" -v "$SECRET_VOL:/dst" "$NEW_IMAGE" -c \
  'set -e; cp /src/master.key /dst/onboarding_master_key; cp /src/admin.token /dst/onboard_admin_token; chown 10001:10001 /dst/*; chmod 400 /dst/*'
ENVS=(-e ONBOARD_LISTEN=0.0.0.0:8788 -e ONBOARD_ALLOW_PUBLIC_LISTEN=1
  -e ONBOARD_DB_PATH=/app/data/state.sqlite -e ONBOARD_FAKE_PROVIDERS=true
  -e ONBOARD_DOMAINS=moonbyte.test -e ONBOARD_SENDER_ALLOWLIST=no-reply,idp.fake.test,moonbyte.test
  -e ONBOARD_MASTER_KEY_FILE=/run/secrets/onboarding_master_key
  -e ONBOARD_ADMIN_TOKEN_FILE=/run/secrets/onboard_admin_token
  -e ONBOARD_LOG_LEVEL=warn -e ONBOARD_SESSION_TTL=1h)
run_app(){ # $1 name $2 image $3 port $4 data
  docker run -d --name "$1" --read-only --cap-drop ALL --security-opt no-new-privileges \
    --user 10001:10001 --tmpfs /tmp:rw,noexec,nosuid,size=16m -p "127.0.0.1:$3:8788" \
    -v "$4:/app/data" --mount type=volume,src="$SECRET_VOL",dst=/run/secrets,readonly \
    "${ENVS[@]}" "$2" >/dev/null
}
wait_http(){ for _ in $(seq 1 160); do
  c="$(curl -sS -m 1 -o /dev/null -w '%{http_code}' "${AUTH[@]}" "http://127.0.0.1:$1$2" 2>/dev/null || true)"
  [ "$c" = "$3" ] && return 0; sleep 0.25; done; return 1; }
meta_schema(){ curl -sS -m 5 "${AUTH[@]}" "http://127.0.0.1:$1/v1/meta" | python3 -c 'import json,sys;print(json.load(sys.stdin)["schema_version"])'; }
db_copy_out(){ mkdir -p "$2"; for f in state.sqlite state.sqlite-wal state.sqlite-shm; do docker cp "$1:/app/data/$f" "$2/$f" >/dev/null 2>&1 || true; done; }
chmod 777 "$WORK/data" "$WORK/upgrade" "$WORK/restored" "$WORK/futures"

echo "=== old version: schema 3, run, session ==="
run_app "$ID-old1" "$OLD_IMAGE" "$PORT" "$WORK/data"
wait_http "$PORT" /readyz 200 90 && ok "old image ready (schema 3)" || bad "old image ready"
[ "$(meta_schema "$PORT")" = "3" ] && ok "old schema is 3" || bad "old schema=$(meta_schema "$PORT")"
BODY='{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox"}'
RUN="$(curl -sS "${AUTH[@]}" "${JSON[@]}" -X POST "$BASE/v1/runs" -H 'Idempotency-Key: r9-rollback-run-1' -d "$BODY" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
[ -n "$RUN" ] && ok "old image created run $RUN" || bad "old run create"
for _ in $(seq 1 80); do S="$(curl -sS "${AUTH[@]}" "$BASE/v1/runs/$RUN" | python3 -c 'import json,sys;print(json.load(sys.stdin)["status"])')"; case "$S" in COMPLETED|FAILED_TERMINAL|CANCELLED) break;; esac; sleep 0.25; done
[ "$S" = "COMPLETED" ] && ok "old run completed" || bad "old run status=$S"
curl -sS -c "$WORK/jar" -o /dev/null -X POST "$BASE/ui/login" -H "Origin: $BASE" --data-urlencode "token=$TOKEN"
COOKIE="$(awk '$6=="onboard_session"{print $7}' "$WORK/jar")"
CSRF="$(curl -sS -b "$WORK/jar" "$BASE/ui/" | grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"//')"
curl -sS -b "$WORK/jar" -o /dev/null -X POST "$BASE/ui/logout" -H "Origin: $BASE" --data-urlencode "csrf=$CSRF"
[ -n "$COOKIE" ] && ok "old session + logout issued cookie" || bad "old login"
docker stop --time 10 "$ID-old1" >/dev/null
db_copy_out "$ID-old1" "$WORK/inspect"
REV="$(python3 - "$WORK/inspect/state.sqlite" <<'PY'
import sqlite3,sys
print(sqlite3.connect(sys.argv[1]).execute("select count(*) from session_revocations").fetchone()[0])
PY
)"
[ "$REV" -ge 1 ] && ok "pre-upgrade revocation row present ($REV)" || bad "no revocation row"
ONBOARD_BACKUP_ENCRYPT=1 ONBOARD_KEYRING="$WORK/secrets/master.key" \
  bash "$ROOT/scripts/backup.sh" --db "$WORK/inspect/state.sqlite" --out-dir "$WORK/backup" \
  --encrypt --keyring "$WORK/secrets/master.key" >/dev/null 2>&1 && ok "pre-upgrade encrypted backup" || bad "backup"
ENC="$(ls "$WORK"/backup/*.sqlite.enc | head -1)"
cp "$WORK/inspect/state.sqlite" "$WORK/upgrade/state.sqlite"
docker run --rm --user 0:0 -v "$WORK/upgrade:/data" --entrypoint sh "$NEW_IMAGE" -c 'chown 10001:10001 /data/state.sqlite && chmod 600 /data/state.sqlite'

echo "=== new version: schema 4 upgrade ==="
run_app "$ID-new" "$NEW_IMAGE" "$PORT2" "$WORK/upgrade"
wait_http "$PORT2" /readyz 200 90 && ok "new image upgraded DB and is ready" || bad "new ready"
[ "$(meta_schema "$PORT2")" = "4" ] && ok "new schema is 4" || bad "new schema=$(meta_schema "$PORT2")"
curl -sS "${AUTH[@]}" "$BASE2/v1/runs/$RUN" | grep -q '"id"' && ok "new image serves old run" || bad "new run missing"
# Deliberate crash of the new instance before rollback.
docker kill -s KILL "$ID-new" >/dev/null
docker rm -f "$ID-new" >/dev/null 2>&1 || true

echo "=== old image must refuse schema-4 DB ==="
run_app "$ID-oldrefuse" "$OLD_IMAGE" "$PORT" "$WORK/upgrade" || true
sleep 5
STATE="$(docker inspect --format '{{.State.Status}} exit={{.State.ExitCode}}' "$ID-oldrefuse" 2>/dev/null || echo missing)"
LOGS="$(docker logs "$ID-oldrefuse" 2>&1 || true)"
if echo "$LOGS" | grep -qi "schema is newer\|schema_too_new\|newer than this binary"; then
  ok "old image refused schema 4 ($STATE)"
else
  bad "old image did not refuse schema 4 ($STATE) logs=$(echo "$LOGS" | tail -2 | tr '\n' ' ')"
fi
docker rm -f "$ID-oldrefuse" >/dev/null 2>&1 || true

echo "=== restore pre-upgrade backup and old-image rollback ==="
CONFIRM_RESTORE=1 bash "$ROOT/scripts/restore.sh" --snapshot "$ENC" --target "$WORK/restored/state.sqlite" \
  --keyring "$WORK/secrets/master.key" --force >/dev/null 2>&1 && ok "backup restored to isolated volume" || bad "restore"
docker run --rm --user 0:0 -v "$WORK/restored:/data" --entrypoint sh "$OLD_IMAGE" -c 'chown 10001:10001 /data/state.sqlite && chmod 600 /data/state.sqlite'
run_app "$ID-old2" "$OLD_IMAGE" "$PORT" "$WORK/restored"
wait_http "$PORT" /readyz 200 90 && ok "old image serves restored schema-3 DB" || bad "rollback ready"
[ "$(meta_schema "$PORT")" = "3" ] && ok "rollback schema is 3" || bad "rollback schema"
curl -sS "${AUTH[@]}" "$BASE/v1/runs/$RUN" | grep -q '"id"' && ok "rollback preserves run" || bad "rollback lost run"
C="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' -H "Cookie: onboard_session=$COOKIE" "$BASE/ui/")"
case "$C" in "303 "*"/ui/login") ok "rollback preserves revocation state";; *) bad "rollback revocation replay: $C";; esac
docker stop --time 10 "$ID-old2" >/dev/null

echo "=== failed recovery must not overwrite valid data ==="
cp "$WORK/restored/state.sqlite" "$WORK/futures/valid.sqlite"
BEFORE="$(python3 -c 'import hashlib,sys;print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$WORK/futures/valid.sqlite")"
if CONFIRM_RESTORE=1 bash "$ROOT/scripts/restore.sh" --snapshot "$ENC" --target "$WORK/futures/valid.sqlite" \
     --keyring "$WORK/secrets/badkey" --force >/dev/null 2>&1; then
  bad "wrong-key restore unexpectedly succeeded"
else
  ok "wrong-key restore fails closed"
fi
AFTER="$(python3 -c 'import hashlib,sys;print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$WORK/futures/valid.sqlite")"
[ "$BEFORE" = "$AFTER" ] && ok "failed restore did not overwrite valid data" || bad "failed restore modified valid data"
# Future-schema DB: old image must refuse it too.
cp "$WORK/restored/state.sqlite" "$WORK/futures/future.sqlite"
python3 - "$WORK/futures/future.sqlite" <<'PY'
import sqlite3,sys
db=sqlite3.connect(sys.argv[1]); db.execute("INSERT INTO schema_migrations(version,applied_at_ms) VALUES(999,1)"); db.commit(); db.close()
PY
docker run --rm --user 0:0 -v "$WORK/futures:/data" --entrypoint sh "$OLD_IMAGE" -c 'chown 10001:10001 /data/future.sqlite && chmod 600 /data/future.sqlite' >/dev/null 2>&1 || true
docker run -d --name "$ID-oldfuture" --read-only --cap-drop ALL --user 10001:10001 --tmpfs /tmp:size=8m \
  -v "$WORK/futures:/app/data" --mount type=volume,src="$SECRET_VOL",dst=/run/secrets,readonly \
  "${ENVS[@]}" -e ONBOARD_DB_PATH=/app/data/future.sqlite "$OLD_IMAGE" >/dev/null 2>&1 || true
sleep 4
FSTATE="$(docker inspect --format '{{.State.Status}} exit={{.State.ExitCode}}' "$ID-oldfuture" 2>/dev/null || echo missing)"
FLOGS="$(docker logs "$ID-oldfuture" 2>&1 || true)"
if echo "$FLOGS" | grep -qi "schema is newer\|schema_too_new\|newer than this binary"; then
  ok "old image refuses future schema ($FSTATE)"
else
  bad "old image future schema not refused ($FSTATE) logs=$(echo "$FLOGS" | tail -2 | tr '\n' ' ')"
fi
echo "SUMMARY pass=$PASS fail=$FAIL"
trap - EXIT
cleanup || true
[ "$FAIL" = "0" ]
exit $?
