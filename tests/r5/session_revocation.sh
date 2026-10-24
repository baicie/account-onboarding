#!/usr/bin/env bash
# R5-A process-level gate: explicit logout must stay revoked across a real
# onboardd restart (SQLite-backed), not just across an in-process Server object.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$ROOT/.sandbox/r5/session-revocation"
BIN="${R5_BIN:-$ROOT/bin/onboardd}"
PORT="${R5_PORT:-18796}"
BASE="http://127.0.0.1:$PORT"
PASS=0; FAIL=0
ok() { PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
cleanup() {
  if [ -n "${PID:-}" ] && kill -0 "$PID" 2>/dev/null; then kill -TERM "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi
}
trap cleanup EXIT
rm -rf "$WORK"; mkdir -p "$WORK"
# Build in-script so a concurrently running `make verify` cannot leave a stale
# binary behind (REVIEW-R5 P2-3). Set R5_SKIP_BUILD=1 to use an existing binary.
if [ "${R5_SKIP_BUILD:-0}" != "1" ]; then
  ( cd "$ROOT" && go build -trimpath -o "$BIN" ./cmd/onboardd )
fi
umask 077
head -c 32 /dev/urandom > "$WORK/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/admin.token"

export ONBOARD_LISTEN="127.0.0.1:$PORT"
export ONBOARD_DB_PATH="$WORK/state.sqlite"
export ONBOARD_MASTER_KEY_FILE="$WORK/master.key"
export ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token"
export ONBOARD_FAKE_PROVIDERS=true
export ONBOARD_DOMAINS="moonbyte.test"
export ONBOARD_SENDER_ALLOWLIST="moonbyte.test"
export ONBOARD_LOG_LEVEL=warn

start_server() {
  # exec replaces the subshell, so $! is the real onboardd PID (a plain
  # background subshell would leave the server orphaned on cleanup).
  ( cd "$ROOT" && exec "$BIN" ) > "$WORK/onboardd-$1.log" 2>&1 &
  PID=$!
  for _ in $(seq 1 100); do
    if curl -fsS -m 1 "$BASE/healthz" >/dev/null 2>&1; then return 0; fi
    sleep 0.05
  done
  echo "server did not become healthy; log:"; tail -20 "$WORK/onboardd-$1.log"; return 1
}

stop_server() {
  kill -TERM "$PID" 2>/dev/null || true
  for _ in $(seq 1 100); do kill -0 "$PID" 2>/dev/null || { PID=""; return 0; }; sleep 0.05; done
  kill -KILL "$PID" 2>/dev/null || true; PID=""
}

TOKEN="$(cat "$WORK/admin.token")"
start_server 1 || { bad "first start"; exit 1; }
ok "first onboardd start + /healthz 200"

# Login through the real UI route (Origin required for form login).
curl -sS -c "$WORK/jar" -o /dev/null -w '%{http_code}' -X POST "$BASE/ui/login" \
  -H "Origin: $BASE" --data-urlencode "token=$TOKEN" > "$WORK/login.code"
[ "$(cat "$WORK/login.code")" = "303" ] && ok "UI login 303" || bad "UI login $(cat "$WORK/login.code")"
COOKIE="$(awk '$6=="onboard_session"{print $7}' "$WORK/jar")"
[ -n "$COOKIE" ] && ok "session cookie issued" || { bad "no session cookie"; exit 1; }

DASH="$(curl -sS -b "$WORK/jar" -o "$WORK/dashboard.html" -w '%{http_code}' "$BASE/ui/")"
[ "$DASH" = "200" ] && ok "authenticated dashboard 200" || bad "dashboard $DASH"
CSRF="$(grep -o 'name="csrf" value="[^"]*"' "$WORK/dashboard.html" | head -1 | sed 's/.*value="//;s/"//')"
[ -n "$CSRF" ] && ok "CSRF token present" || { bad "no CSRF token"; exit 1; }

LOGOUT="$(curl -sS -b "$WORK/jar" -o /dev/null -w '%{http_code}' -X POST "$BASE/ui/logout" \
  -H "Origin: $BASE" --data-urlencode "csrf=$CSRF")"
[ "$LOGOUT" = "303" ] && ok "logout 303" || bad "logout $LOGOUT"

# Confirm the revocation row is actually in SQLite (not memory).
ROWS="$(python3 - "$WORK/state.sqlite" <<'PY'
import sqlite3,sys
db=sqlite3.connect(sys.argv[1])
print(db.execute("select count(*) from session_revocations").fetchone()[0])
PY
)"
[ "$ROWS" = "1" ] && ok "revocation row persisted (rows=1)" || bad "revocation rows=$ROWS"

# Real process restart: new PID, same data dir and master key.
stop_server
ok "first onboardd stopped"
start_server 2 || { bad "restart"; exit 1; }
ok "second onboardd start + /healthz 200"

REPLAY="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' -H "Cookie: onboard_session=$COOKIE" "$BASE/ui/")"
case "$REPLAY" in
  303*"/ui/login") ok "old cookie rejected after restart (303 /ui/login)" ;;
  *) bad "old cookie after restart: $REPLAY" ;;
esac

# A fresh login on the restarted process still works (revocation is per-session).
curl -sS -c "$WORK/jar2" -o /dev/null -w '%{http_code}' -X POST "$BASE/ui/login" \
  -H "Origin: $BASE" --data-urlencode "token=$TOKEN" > "$WORK/login2.code"
[ "$(cat "$WORK/login2.code")" = "303" ] && ok "fresh login after restart 303" || bad "fresh login $(cat "$WORK/login2.code")"

echo "SUMMARY pass=$PASS fail=$FAIL"
[ "$FAIL" = "0" ]
