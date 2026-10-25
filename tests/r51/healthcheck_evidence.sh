#!/usr/bin/env bash
# R5.1-B readiness audit evidence (no Docker required):
#  - documented JSON keyring envelope is accepted by the service;
#  - `onboardd healthcheck` probes authenticated /readyz (DB + schema + table);
#  - a degraded database (missing revocation table) turns healthcheck red.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$ROOT/.sandbox/r51/healthcheck"
BIN="$ROOT/bin/onboardd"
PORT=18991
BASE="http://127.0.0.1:$PORT"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
PID=""
cleanup() { [ -n "$PID" ] && kill -TERM "$PID" 2>/dev/null || true; [ -n "$PID" ] && wait "$PID" 2>/dev/null || true; }
trap cleanup EXIT
rm -rf "$WORK"; mkdir -p "$WORK"; umask 077
(cd "$ROOT" && go build -trimpath -o bin/onboardd ./cmd/onboardd)
python3 - "$WORK/keyring.json" <<'PY'
import base64,json,sys
key=b'\x11'*32
json.dump({"current":1,"keys":{"1":base64.b64encode(key).decode()}}, open(sys.argv[1],"w"))
PY
head -c 32 /dev/urandom | base64 > "$WORK/admin.token"
export ONBOARD_LISTEN="127.0.0.1:$PORT"
export ONBOARD_DB_PATH="$WORK/state.sqlite"
export ONBOARD_MASTER_KEY_FILE="$WORK/keyring.json"
export ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token"
export ONBOARD_FAKE_PROVIDERS=true
export ONBOARD_DOMAINS=moonbyte.test
export ONBOARD_SENDER_ALLOWLIST=moonbyte.test
export ONBOARD_LOG_LEVEL=warn

start_app() {
  ( cd "$ROOT" && exec "$BIN" ) > "$WORK/onboardd-$1.log" 2>&1 &
  PID=$!
  for _ in $(seq 1 150); do curl -fsS -m 1 "$BASE/healthz" >/dev/null 2>&1 && return 0; sleep 0.05; done
  echo "app failed"; tail -10 "$WORK/onboardd-$1.log"; return 1
}
stop_app() { kill -TERM "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; PID=""; }

start_app 1 || { bad "JSON keyring startup"; exit 1; }
ok "service starts with documented JSON keyring envelope (schema 5)"
CODE="$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(cat "$WORK/admin.token")" "$BASE/readyz")"
[ "$CODE" = "200" ] && ok "/readyz 200 with JSON keyring" || bad "/readyz $CODE"
"$BIN" healthcheck >"$WORK/healthcheck-ok.log" 2>&1 && ok "onboardd healthcheck exit 0 when ready" || bad "healthcheck should be 0: $(cat "$WORK/healthcheck-ok.log")"
stop_app
python3 - "$WORK/state.sqlite" <<'PY'
import sqlite3,sys
db=sqlite3.connect(sys.argv[1]); db.execute("DROP TABLE session_revocations"); db.commit(); db.close()
PY
start_app 2 || { bad "degraded startup"; exit 1; }
CODE="$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(cat "$WORK/admin.token")" "$BASE/readyz")"
[ "$CODE" = "503" ] && ok "/readyz 503 when revocation table is missing" || bad "/readyz degraded=$CODE"
set +e
"$BIN" healthcheck >"$WORK/healthcheck-bad.log" 2>&1
RC=$?
set -e
[ "$RC" -ne 0 ] && grep -qi "not ready" "$WORK/healthcheck-bad.log" \
  && ok "onboardd healthcheck exit $RC when database is degraded" || bad "degraded healthcheck exit=$RC: $(cat "$WORK/healthcheck-bad.log")"
stop_app

# Raw 32-byte key must keep working after the JSON support was added.
head -c 32 /dev/urandom > "$WORK/raw.key"
ONBOARD_MASTER_KEY_FILE="$WORK/raw.key" ONBOARD_DB_PATH="$WORK/raw.sqlite" start_app 3 || { bad "raw key startup"; exit 1; }
ok "raw 32-byte master key still accepted"
stop_app
echo "SUMMARY pass=$PASS fail=$FAIL"
[ "$FAIL" = "0" ]
