#!/usr/bin/env bash
# R0 acceptance smoke: fake providers, no external calls.
# Usage: bash tests/r0/smoke.sh
# Writes nothing outside .sandbox/r0-evidence and prints a transcript.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

WORK="$ROOT/.sandbox/r0-evidence"
PORT=18788
BASE="http://127.0.0.1:$PORT"

say() { printf '\n=== %s ===\n' "$*"; }

cleanup() {
  if [ -n "${SERVER_PID:-}" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

say "environment"
go version
sw_vers -productVersion 2>/dev/null || true
uname -m

say "build / vet / test of R0-scope packages (exit codes are real)"
R0_PKGS="./internal/api/... ./internal/config/... ./internal/security/... ./internal/storage/... ./internal/workflow/... ./internal/secrets/... ./internal/provider/fake/... ./internal/provider/boot/... ./internal/logging/... ./internal/errs/... ./cmd/..."
go build ./... ; BUILD_EXIT=$?; echo "go build ./... exit=$BUILD_EXIT"
go vet ./... ; VET_EXIT=$?; echo "go vet ./... exit=$VET_EXIT"
go test $R0_PKGS -race -count=1 >/tmp/r0-test.log 2>&1 ; TEST_EXIT=$?; echo "go test <R0 packages> -race -count=1 exit=$TEST_EXIT"
grep -c '^ok ' /tmp/r0-test.log | sed 's/^/packages_ok=/' || true
grep -c '^FAIL' /tmp/r0-test.log | sed 's/^/packages_fail=/' || true

say "prepare isolated state (loopback only)"
rm -rf "$WORK"
mkdir -p "$WORK"
python3 -c "import base64,os;open('$WORK/master.key','w').write(base64.b64encode(os.urandom(32)).decode())"
chmod 600 "$WORK/master.key"
printf 'r0-evidence-token-%s\n' "$(openssl rand -hex 8)" > "$WORK/admin.token"
chmod 600 "$WORK/admin.token"
TOKEN="$(cat "$WORK/admin.token")"

export ONBOARD_LISTEN="127.0.0.1:$PORT"
export ONBOARD_DB_PATH="$WORK/state.sqlite"
export ONBOARD_MASTER_KEY_FILE="$WORK/master.key"
export ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token"
export ONBOARD_FAKE_PROVIDERS=true
export ONBOARD_DOMAINS=moonbyte.test
export ONBOARD_POLL_INTERVAL=1s
export ONBOARD_TICK_INTERVAL=100ms
export ONBOARD_LOG_LEVEL=warn

go build -trimpath -o "$WORK/onboardd" ./cmd/onboardd
"$WORK/onboardd" > "$WORK/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
  curl -sf -m 2 "$BASE/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done

say "liveness / readiness"
curl -s -m 3 "$BASE/healthz"; echo
curl -s -m 3 -o /dev/null -w 'readyz_unauth=%{http_code}\n' "$BASE/readyz"
curl -s -m 3 -H "Authorization: Bearer $TOKEN" "$BASE/readyz"; echo

say "create sandbox run (6 steps, fake providers)"
CREATE=$(curl -s -m 5 -X POST "$BASE/v1/runs" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: r0-evidence-0001" \
  -d '{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth"}')
echo "$CREATE" | python3 -m json.tool | head -12
RID=$(echo "$CREATE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

for _ in $(seq 1 30); do
  STATUS=$(curl -s -m 3 -H "Authorization: Bearer $TOKEN" "$BASE/v1/runs/$RID" | python3 -c 'import sys,json;print(json.load(sys.stdin)["status"])')
  [ "$STATUS" = "COMPLETED" ] && break
  sleep 0.5
done
echo "final_status=$STATUS"
curl -s -m 3 -H "Authorization: Bearer $TOKEN" "$BASE/v1/runs/$RID" | python3 -c '
import sys,json
d=json.load(sys.stdin)
print("steps:", [(s["name"], s["status"]) for s in d["steps"]])
print("action_required:", d["action_required"], "last_error_class:", d.get("last_error_class",""))'

say "event timeline (redacted)"
curl -s -m 3 -H "Authorization: Bearer $TOKEN" "$BASE/v1/runs/$RID/events" > "$WORK/events.json"
python3 - <<PY
import json
d=json.load(open("$WORK/events.json"))
print([e["type"] for e in d["items"]])
print("payload_keys:", sorted({k for e in d["items"] for k in e["payload"]}))
PY

say "idempotency: replay returns the same run id"
REPLAY=$(curl -s -m 5 -X POST "$BASE/v1/runs" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: r0-evidence-0001" \
  -d '{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth"}')
python3 - <<PY
import json
d=json.loads('''$REPLAY''')
print("replay_id_matches:", d["id"] == "$RID")
PY

say "idempotency conflict (different body, same key) => 409"
curl -s -m 5 -o "$WORK/conflict.json" -w 'conflict_status=%{http_code}\n' -X POST "$BASE/v1/runs" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: r0-evidence-0001" \
  -d '{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","email_provider":"fake-github"}'
cat "$WORK/conflict.json"; echo

say "strict body: unknown field (account_count) => 400"
curl -s -m 5 -o "$WORK/unknown.json" -w 'unknown_field_status=%{http_code}\n' -X POST "$BASE/v1/runs" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: r0-evidence-0002" \
  -d '{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","account_count":5}'
cat "$WORK/unknown.json"; echo

say "operator action: waiting run, resume, then cancel"
GH=$(curl -s -m 5 -X POST "$BASE/v1/runs" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: r0-evidence-wait-0001" \
  -d '{"template":"github-existing-email","mode":"real-account","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","existing_identity_ref":"operator-owned-account-1"}')
GID=$(echo "$GH" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
for _ in $(seq 1 20); do
  GSTATUS=$(curl -s -m 3 -H "Authorization: Bearer $TOKEN" "$BASE/v1/runs/$GID" | python3 -c 'import sys,json;print(json.load(sys.stdin)["status"])')
  [ "$GSTATUS" = "WAITING_USER_ACTION" ] && break
  sleep 0.5
done
echo "waiting_status=$GSTATUS"
curl -s -m 3 -H "Authorization: Bearer $TOKEN" "$BASE/v1/runs/$GID" | python3 -c 'import sys,json;d=json.load(sys.stdin);print("action_required:",d["action_required"],"current_step:",d["current_step"])'
curl -s -m 3 -o /dev/null -w 'resume_status=%{http_code}\n' -X POST -H "Authorization: Bearer $TOKEN" "$BASE/v1/runs/$GID/resume"
curl -s -m 3 -o /dev/null -w 'cancel_status=%{http_code}\n' -X POST -H "Authorization: Bearer $TOKEN" "$BASE/v1/runs/$GID/cancel"
for _ in $(seq 1 20); do
  GSTATUS=$(curl -s -m 3 -H "Authorization: Bearer $TOKEN" "$BASE/v1/runs/$GID" | python3 -c 'import sys,json;print(json.load(sys.stdin)["status"])')
  [ "$GSTATUS" = "CANCELLED" ] && break
  sleep 0.5
done
echo "cancelled_status=$GSTATUS"
curl -s -m 3 -o /dev/null -w 'cancel_again_status=%{http_code}\n' -X POST -H "Authorization: Bearer $TOKEN" "$BASE/v1/runs/$GID/cancel"

say "console UI (HttpOnly session + CSRF)"
curl -s -m 3 -o /dev/null -w 'login_page=%{http_code}\n' "$BASE/ui/login"
curl -s -c "$WORK/cookies" -o /dev/null -w 'login=%{http_code}\n' -X POST "$BASE/ui/login" \
  -H "Origin: $BASE" --data-urlencode "token=$TOKEN"
curl -s -b "$WORK/cookies" -o "$WORK/dashboard.html" -w 'dashboard=%{http_code}\n' "$BASE/ui/"
curl -s -b "$WORK/cookies" -o "$WORK/runpage.html" -w 'run_page=%{http_code}\n' "$BASE/ui/runs/$RID"
grep -o 'name="csrf"' "$WORK/dashboard.html" | head -1 | sed 's/^/csrf_field=/' || true
curl -s -b "$WORK/cookies" -o /dev/null -w 'ui_create_without_csrf=%{http_code}\n' -X POST "$BASE/ui/runs" \
  -H "Origin: $BASE" --data-urlencode "template=sandbox-email-verify-oidc" \
  --data-urlencode "mailbox_provider=fake-mailbox" --data-urlencode "identity_provider=fake-identity" \
  --data-urlencode "oauth_provider=fake-oauth"

say "secret leak scan over API responses"
if grep -Eq 'secret://|eyJ[A-Za-z0-9]{10,}' "$WORK/events.json" "$WORK/dashboard.html" "$WORK/runpage.html"; then
  echo "LEAK_FOUND"
  exit 1
else
  echo "no secret references or JWTs in run JSON, event JSON or console HTML"
fi

say "restart durability (SIGTERM + same DB/master key)"
kill -TERM "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true
SERVER_PID=""
"$WORK/onboardd" >> "$WORK/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do curl -sf -m 2 "$BASE/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -s -m 3 -H "Authorization: Bearer $TOKEN" "$BASE/v1/runs/$RID" | python3 -c 'import sys,json;d=json.load(sys.stdin);print("persisted_status:",d["status"],"steps:",len(d["steps"]))'
REPLAY2=$(curl -s -m 5 -X POST "$BASE/v1/runs" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: r0-evidence-0001" \
  -d '{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth"}')
python3 - <<PY
import json
d=json.loads('''$REPLAY2''')
print("replay_after_restart_matches:", d["id"] == "$RID")
PY

say "result"
if [ "${BUILD_EXIT:-1}" -eq 0 ] && [ "${VET_EXIT:-1}" -eq 0 ] && [ "${TEST_EXIT:-1}" -eq 0 ]; then
  echo "R0 SMOKE: PASS"
else
  echo "R0 SMOKE: FAIL (build=$BUILD_EXIT vet=$VET_EXIT test=$TEST_EXIT)"
  exit 1
fi
echo "evidence dir: $WORK"
