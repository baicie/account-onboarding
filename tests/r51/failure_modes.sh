#!/usr/bin/env bash
# R5.1-D (process-level supporting evidence): single-writer enforcement and
# failure modes. Container-level fault injection remains NOT_RUN when the Docker
# daemon is unavailable; these checks exercise the same binary and storage layer.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$ROOT/.sandbox/r51/failure-modes"
BIN="$ROOT/bin/onboardd"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill -KILL "$p" 2>/dev/null || true; done; }
trap cleanup EXIT
rm -rf "$WORK"; mkdir -p "$WORK"; umask 077
head -c 32 /dev/urandom > "$WORK/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/admin.token"
(cd "$ROOT" && go build -trimpath -o bin/onboardd ./cmd/onboardd)
TOKEN="$(cat "$WORK/admin.token")"

start() { # $1=port $2=dbpath $3=log
  ( cd "$ROOT" && exec env ONBOARD_LISTEN="127.0.0.1:$1" ONBOARD_DB_PATH="$2" \
      ONBOARD_MASTER_KEY_FILE="$WORK/master.key" ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token" \
      ONBOARD_FAKE_PROVIDERS=true ONBOARD_DOMAINS=moonbyte.test ONBOARD_SENDER_ALLOWLIST=moonbyte.test \
      ONBOARD_LOG_LEVEL=warn ./bin/onboardd ) > "$3" 2>&1 &
  echo $!
}
wait_health() { for _ in $(seq 1 150); do curl -fsS -m 1 "http://127.0.0.1:$1/healthz" >/dev/null 2>&1 && return 0; sleep 0.05; done; return 1; }

DB="$WORK/state.sqlite"
P1=$(start 18791 "$DB" "$WORK/app1.log"); PIDS+=("$P1")
wait_health 18791 && ok "first instance healthy" || { bad "first instance"; exit 1; }

# 1) Second instance on the same database must be refused (single writer).
P2=$(start 18792 "$DB" "$WORK/app2.log"); PIDS+=("$P2")
REFUSED=0
for _ in $(seq 1 100); do
  if ! kill -0 "$P2" 2>/dev/null; then REFUSED=1; break; fi
  sleep 0.05
done
if [ "$REFUSED" = "1" ] && grep -qi "single-writer" "$WORK/app2.log"; then
  ok "second instance refused while first holds the DB ($(grep -o 'single-writer' "$WORK/app2.log" | head -1))"
else
  bad "second instance was not refused"
fi

# 2) SIGKILL releases the instance lock; the replacement starts on the same DB.
kill -KILL "$P1" 2>/dev/null || true
for _ in $(seq 1 100); do kill -0 "$P1" 2>/dev/null || break; sleep 0.05; done
P3=$(start 18793 "$DB" "$WORK/app3.log"); PIDS+=("$P3")
busy=1; for _ in $(seq 1 150); do curl -fsS -m 1 "http://127.0.0.1:18793/healthz" >/dev/null 2>&1 && { busy=0; break; }; sleep 0.05; done
[ "$busy" = "0" ] && ok "lock released after SIGKILL; fresh instance healthy" || bad "lock not released after SIGKILL"
curl -sS -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:18793/readyz" | grep -q '"ready":true' && ok "readyz true after crash restart" || bad "readyz after crash"

# 3) Missing master key file fails closed (no silent ephemeral key).
set +e
( cd "$ROOT" && env ONBOARD_LISTEN=127.0.0.1:18794 ONBOARD_DB_PATH="$WORK/badkey.sqlite" \
  ONBOARD_MASTER_KEY_FILE="$WORK/does-not-exist" ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token" ./bin/onboardd ) > "$WORK/badkey.log" 2>&1
RC=$?
set -e
[ "$RC" -ne 0 ] && grep -qi "master key" "$WORK/badkey.log" && ok "missing master key fails closed (exit $RC)" || bad "missing master key (exit $RC)"

# 4) Missing revocation table -> readyz 503 (health probes the table).
kill -TERM "$P3" 2>/dev/null || true
sleep 0.5
python3 - "$DB" <<'PY'
import sqlite3,sys
db=sqlite3.connect(sys.argv[1]); db.execute("DROP TABLE IF EXISTS session_revocations"); db.commit(); db.close()
PY
P4=$(start 18795 "$DB" "$WORK/app4.log"); PIDS+=("$P4")
wait_health 18795 || bad "degraded instance failed to boot"
CODE=$(curl -sS -o "$WORK/readyz.json" -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:18795/readyz")
if [ "$CODE" = "503" ] && grep -q '"ready":false' "$WORK/readyz.json"; then
  ok "missing revocation table -> /readyz 503 (ready:false)"
else
  bad "missing revocation table readyz=$CODE body=$(cat "$WORK/readyz.json" 2>/dev/null)"
fi
# Without the revocation table, cookie sessions must fail closed (the login
# form may still be served, but the issued session cannot be authorised).
curl -sS -c "$WORK/jar" -o /dev/null -X POST "http://127.0.0.1:18795/ui/login" -H "Origin: http://127.0.0.1:18795" --data-urlencode "token=$TOKEN" >/dev/null
DASH_CODE=$(curl -sS -b "$WORK/jar" -o /dev/null -w '%{http_code}' "http://127.0.0.1:18795/ui/")
if [ "$DASH_CODE" = "303" ]; then
  ok "cookie session rejected (303 login) when revocation table is missing"
else
  bad "degraded dashboard = $DASH_CODE (expected fail-closed redirect)"
fi
kill -TERM "$P4" 2>/dev/null || true
sleep 0.5

echo "SUMMARY pass=$PASS fail=$FAIL"
[ "$FAIL" = "0" ]
