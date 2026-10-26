#!/usr/bin/env bash
# R6 P1-A: encrypted backup -> restore -> restart must preserve workflow data and
# durable session revocations (process-level evidence; Docker-level remains
# NOT_RUN while no daemon exists).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$ROOT/.sandbox/r6/restore-revocation"
BIN="$ROOT/bin/onboardd"
PORT1=18994
PORT2=18995
BASE1="http://127.0.0.1:$PORT1"
BASE2="http://127.0.0.1:$PORT2"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
PID=""
cleanup() { [ -n "$PID" ] && kill -TERM "$PID" 2>/dev/null || true; [ -n "$PID" ] && wait "$PID" 2>/dev/null || true; }
trap cleanup EXIT
rm -rf "$WORK"; mkdir -p "$WORK"/{data,restored,backup}; umask 077
head -c 32 /dev/urandom > "$WORK/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/admin.token"
TOKEN="$(cat "$WORK/admin.token")"
(cd "$ROOT" && go build -trimpath -o bin/onboardd ./cmd/onboardd)

start_app() { # $1 port, $2 db, $3 log
  ( cd "$ROOT" && exec env ONBOARD_LISTEN="127.0.0.1:$1" ONBOARD_DB_PATH="$2" \
      ONBOARD_MASTER_KEY_FILE="$WORK/master.key" ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token" \
      ONBOARD_FAKE_PROVIDERS=true ONBOARD_DOMAINS=moonbyte.test ONBOARD_SENDER_ALLOWLIST=no-reply,idp.fake.test,moonbyte.test \
      ONBOARD_LOG_LEVEL=warn ./bin/onboardd ) > "$3" 2>&1 &
  PID=$!
  for _ in $(seq 1 150); do curl -fsS -m 1 "http://127.0.0.1:$1/healthz" >/dev/null 2>&1 && return 0; sleep 0.05; done
  tail -10 "$3"; return 1
}
stop_app() { kill -TERM "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; PID=""; }
api() { curl -sS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" "$@"; }

DB="$WORK/data/state.sqlite"
start_app "$PORT1" "$DB" "$WORK/app1.log" || { bad "first start"; exit 1; }
ok "service started (fresh DB, schema 6)"

# Workflow data: a sandbox run through the real engine with fake providers.
RUN_JSON="$(api -X POST "$BASE1/v1/runs" -H "Idempotency-Key: r6-restore-1" -d '{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox"}')"
RUN="$(echo "$RUN_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
[ -n "$RUN" ] || { echo "create run failed: $RUN_JSON"; bad "create run"; exit 1; }
for _ in $(seq 1 120); do
  STATUS="$(api "$BASE1/v1/runs/$RUN" | python3 -c 'import json,sys;print(json.load(sys.stdin)["status"])')"
  case "$STATUS" in COMPLETED|FAILED_TERMINAL|CANCELLED) break;; esac
  sleep 0.25
done
[ "$STATUS" = "COMPLETED" ] && ok "run completed before backup ($RUN)" || bad "run status=$STATUS"

# Session + durable revocation.
curl -sS -c "$WORK/jar" -o /dev/null -X POST "$BASE1/ui/login" -H "Origin: $BASE1" --data-urlencode "token=$TOKEN"
COOKIE="$(awk '$6=="onboard_session"{print $7}' "$WORK/jar")"
DASH="$(curl -sS -b "$WORK/jar" "$BASE1/ui/")"
CSRF="$(echo "$DASH" | grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"//')"
[ -n "$COOKIE" ] && [ -n "$CSRF" ] && ok "login + CSRF acquired" || { bad "login/CSRF"; exit 1; }
curl -sS -b "$WORK/jar" -o /dev/null -X POST "$BASE1/ui/logout" -H "Origin: $BASE1" --data-urlencode "csrf=$CSRF"
ROWS="$(python3 - "$DB" <<'PY'
import sqlite3,sys
print(sqlite3.connect(sys.argv[1]).execute("select count(*) from session_revocations").fetchone()[0])
PY
)"
[ "$ROWS" -ge 1 ] && ok "revocation row in source DB ($ROWS)" || bad "no revocation row"
stop_app

# Encrypted backup + verify + restore.
ONBOARD_BACKUP_ENCRYPT=1 ONBOARD_KEYRING="$WORK/master.key" \
  bash "$ROOT/scripts/backup.sh" --db "$DB" --out-dir "$WORK/backup" \
  --encrypt --keyring "$WORK/master.key" > "$WORK/backup.log" 2>&1 \
  && ok "encrypted backup exit 0" || { bad "backup failed"; tail -5 "$WORK/backup.log"; exit 1; }
ENC="$(ls "$WORK"/backup/*.sqlite.enc | head -1)"
bash "$ROOT/scripts/verify_backup.sh" "$ENC" --keyring "$WORK/master.key" >/dev/null 2>&1 \
  && ok "verify backup exit 0" || bad "verify backup"
CONFIRM_RESTORE=1 bash "$ROOT/scripts/restore.sh" --snapshot "$ENC" \
  --target "$WORK/restored/state.sqlite" --keyring "$WORK/master.key" --force > "$WORK/restore.log" 2>&1 \
  && ok "restore exit 0" || { bad "restore failed"; tail -5 "$WORK/restore.log"; exit 1; }

# Restart on the restored database.
start_app "$PORT2" "$WORK/restored/state.sqlite" "$WORK/app2.log" || { bad "restart on restored DB"; exit 1; }
ok "service started on restored database"
api "$BASE2/v1/runs/$RUN" | grep -q '"status":"COMPLETED"' && ok "run preserved after restore" || bad "run lost after restore"
api "$BASE2/readyz" | grep -q '"ready":true' && ok "readyz true after restore" || bad "readyz after restore"
C="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' -H "Cookie: onboard_session=$COOKIE" "$BASE2/ui/")"
case "$C" in "303 "*"/ui/login") ok "revoked session still rejected after restore";; *) bad "revoked session after restore: $C";; esac
stop_app
echo "SUMMARY pass=$PASS fail=$FAIL"
[ "$FAIL" = "0" ]
