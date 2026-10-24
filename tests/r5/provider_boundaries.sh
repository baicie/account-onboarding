#!/usr/bin/env bash
# R5-D gate: platform-boundary defaults are fail-closed.
#  A) no live credentials -> every real provider is registered disabled, no fakes
#  B) WorkBuddy enabled without written-permission ack -> startup refused
#  C) GitHub enabled without a token file -> startup refused
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$ROOT/.sandbox/r5/provider-boundaries"
BIN="${R5_BIN:-$ROOT/bin/onboardd}"
PORT="${R5_PROVIDER_PORT:-18798}"
BASE="http://127.0.0.1:$PORT"
PASS=0; FAIL=0
ok() { PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
cleanup() { if [ -n "${PID:-}" ] && kill -0 "$PID" 2>/dev/null; then kill -TERM "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi; }
trap cleanup EXIT
rm -rf "$WORK"; mkdir -p "$WORK"; umask 077
if [ "${R5_SKIP_BUILD:-0}" != "1" ]; then
  ( cd "$ROOT" && go build -trimpath -o "$BIN" ./cmd/onboardd )
fi
head -c 32 /dev/urandom > "$WORK/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/admin.token"
BASE_ENV=(env ONBOARD_LISTEN="127.0.0.1:$PORT" ONBOARD_DB_PATH="$WORK/state.sqlite"
  ONBOARD_MASTER_KEY_FILE="$WORK/master.key" ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token"
  ONBOARD_DOMAINS="moonbyte.test" ONBOARD_SENDER_ALLOWLIST="moonbyte.test" ONBOARD_LOG_LEVEL=warn)

# A) default: no live providers enabled, no fake providers.
"${BASE_ENV[@]}" "$BIN" > "$WORK/server-default.log" 2>&1 &
PID=$!
for _ in $(seq 1 100); do curl -fsS -m 1 "$BASE/healthz" >/dev/null 2>&1 && break; sleep 0.05; done
TOKEN="$(cat "$WORK/admin.token")"
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/v1/providers" > "$WORK/providers.json"
python3 - "$WORK/providers.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
items=d.get("items",[])
enabled=[(p.get("kind"),p.get("name")) for p in items if p.get("enabled")]
assert items, "empty provider catalog"
assert not enabled, f"live providers enabled without credentials: {enabled}"
kinds={p.get("kind") for p in items}
assert {"email","oauth"} <= kinds, f"missing disabled gates: {sorted(kinds)}"
print("providers:", [(p.get("kind"),p.get("name"),p.get("enabled")) for p in items])
PY
[ $? -eq 0 ] && ok "A: no live provider enabled; disabled gates registered" || bad "A: provider catalog"
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/v1/meta" | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d.get("mode")=="sandbox",d'
grep -q "fake providers enabled" "$WORK/server-default.log" && bad "A: fake providers unexpectedly enabled" || ok "A: fake providers off"
cleanup; PID=""

# B) WorkBuddy enabled without the written-permission acknowledgement.
set +e
"${BASE_ENV[@]}" ONBOARD_WORKBUDDY_ENABLED=true ONBOARD_WORKBUDDY_BASE_URL=http://127.0.0.1:9999 "$BIN" > "$WORK/server-workbuddy.log" 2>&1
RC=$?
set -e
[ "$RC" -ne 0 ] && grep -qi "permission" "$WORK/server-workbuddy.log" \
  && ok "B: WorkBuddy without permission ack refused (exit $RC)" || bad "B: WorkBuddy guard (exit $RC)"

# C) GitHub enabled without a token file.
set +e
"${BASE_ENV[@]}" ONBOARD_GITHUB_ENABLED=true "$BIN" > "$WORK/server-github.log" 2>&1
RC=$?
set -e
[ "$RC" -ne 0 ] && ok "C: GitHub without token file refused (exit $RC)" || bad "C: GitHub without token file started (exit $RC)"

# D) Non-loopback listen is refused by default (deployment/TLS precondition).
set +e
"${BASE_ENV[@]}" ONBOARD_LISTEN=0.0.0.0:18799 "$BIN" > "$WORK/server-nonloopback.log" 2>&1
RC=$?
set -e
[ "$RC" -ne 0 ] && grep -qi "is not loopback" "$WORK/server-nonloopback.log" \
  && ok "D: non-loopback listen refused (exit $RC)" || bad "D: non-loopback guard (exit $RC)"

echo "SUMMARY pass=$PASS fail=$FAIL"
[ "$FAIL" = "0" ]
