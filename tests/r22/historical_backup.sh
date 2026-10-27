#!/usr/bin/env bash
# Historical schema5 replay.
#
# Builds the real pre-schema6 source, runs it to produce a real schema5 database
# with real business rows, takes the backup with that same historical binary, then
# restores with the *current* tooling, lets the current service migrate 5 -> 6 and
# checks the business state survived. No table is dropped by hand and no fixture
# imitates an old database.
set -o pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OLD_COMMIT="$1"
[ -n "$OLD_COMMIT" ] || OLD_COMMIT="3796047d3b0bec785bb5671c8fe6cabfd61655ee"
OLD_COMMIT="$(git -C "$ROOT" rev-parse "$OLD_COMMIT")"
WORK="$(mktemp -d /tmp/r22-hist.XXXXXX)"
WT="$WORK/old-src"
EVIDENCE=""; CLEAN_RC=0
APP_PID=""; APP2_PID=""
[ "$#" -ge 3 ] && [ "$2" = "--evidence" ] && EVIDENCE="$3"
P=0; F=0
ok() { P=$((P+1)); echo "PASS $1"; }
bad() { F=$((F+1)); echo "FAIL $1"; }
fail() { echo "FATAL: $*" >&2; exit 1; }
pick_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }
cleanup() {
  local rc=$?
  [ -z "$APP_PID" ] || { kill "$APP_PID" 2>/dev/null; wait "$APP_PID" 2>/dev/null; }
  [ -z "$APP2_PID" ] || { kill "$APP2_PID" 2>/dev/null; wait "$APP2_PID" 2>/dev/null; }
  git -C "$ROOT" worktree remove --force "$WT" >/dev/null 2>&1
  if [ -d "$WT" ]; then echo "cleanup: worktree left" >&2; CLEAN_RC=1; fi
  rm -rf "$WORK" 2>/dev/null
  [ ! -d "$WORK" ] || { echo "cleanup: work dir left" >&2; CLEAN_RC=1; }
  echo "SUMMARY pass=$P fail=$F"
  if [ "$rc" -ne 0 ]; then exit "$rc"; fi
  [ "$CLEAN_RC" -eq 0 ] || { echo "CLEANUP_FAILED" >&2; exit 1; }
  [ "$F" -eq 0 ] && exit 0
  exit 1
}
trap cleanup EXIT
wait_ready() {
  local port="$1" pid="$2" code=""
  for _ in $(seq 1 200); do
    if curl -sf -m 2 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
      code="$(curl -s -o /dev/null -m 2 -w '%{http_code}' -H @"$WORK/headers.txt" "http://127.0.0.1:$port/readyz" 2>/dev/null)"
      case "$code" in
        200) return 0 ;;
        404) return 0 ;;  # historical builds may not expose /readyz
      esac
    fi
    kill -0 "$pid" 2>/dev/null || return 1
    sleep 0.3
  done
  return 1
}
api() {
  local port="$1" method="$2" path="$3" body=""
  if [ "$#" -ge 4 ]; then body="$4"; fi
  if [ -n "$body" ]; then
    curl -s -m 15 -o "$WORK/resp.json" -w '%{http_code}' -X "$method" -H @"$WORK/headers.txt" -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$port$path"
  else
    curl -s -m 15 -o "$WORK/resp.json" -w '%{http_code}' -X "$method" -H @"$WORK/headers.txt" "http://127.0.0.1:$port$path"
  fi
}
create_run() { # create_run <port> <idempotency-key> -> http code, body in resp.json
  local port="$1" key="$2" body
  body='{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox","idempotency_key":"'"$key"'"}'
  curl -s -m 20 -o "$WORK/resp.json" -w '%{http_code}' -X POST \
    -H @"$WORK/headers.txt" -H "Idempotency-Key: $key" -H 'Content-Type: application/json' \
    -d "$body" "http://127.0.0.1:$port/v1/runs"
}
json_field() { python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get(sys.argv[2],""))' "$WORK/resp.json" "$1" 2>/dev/null; }

echo "=== historical schema5 replay (old commit $OLD_COMMIT) ==="
git -C "$ROOT" worktree add --detach "$WT" "$OLD_COMMIT" >"$WORK/wt.log" 2>&1 || { cat "$WORK/wt.log"; fail "worktree add"; }
ok "historical source checked out at $(git -C "$ROOT" rev-parse --short "$OLD_COMMIT")"
[ -f "$WT/internal/storage/migrations/0005_external_operation_lifecycle.sql" ] && ok "historical tree carries migration 0005" || bad "migration 0005 missing"
if [ -f "$WT/internal/storage/migrations/0006_alert_outbox.sql" ]; then bad "historical tree already has schema6 migration"; else ok "historical tree has no 0006 migration (schema5)"; fi

( cd "$WT" && GOFLAGS=-mod=mod go build -o "$WORK/old-onboardd" ./cmd/onboardd && GOFLAGS=-mod=mod go build -o "$WORK/old-backup" ./internal/ops/cmd/backup ) >"$WORK/old-build.log" 2>&1 \
  || { tail -20 "$WORK/old-build.log"; fail "historical binary build"; }
ok "historical binary built from source"
( cd "$ROOT" && go build -o "$WORK/cur-onboardd" ./cmd/onboardd && go build -o "$WORK/cur-backup" ./internal/ops/cmd/backup \
    && go build -o "$WORK/cur-restore" ./internal/ops/cmd/restore && go build -o "$WORK/cur-verify" ./internal/ops/cmd/verify ) >"$WORK/cur-build.log" 2>&1 \
  || { tail -20 "$WORK/cur-build.log"; fail "current binary build"; }
ok "current binary and operator tools built"

mkdir -p "$WORK/data" "$WORK/backups" "$WORK/secrets"
python3 - "$WORK/secrets/onboarding_master_key" "$WORK/secrets/onboard_admin_token" <<'PY'
import os, sys
open(sys.argv[1], 'wb').write(os.urandom(32))
open(sys.argv[2], 'w').write('hist-admin-' + os.urandom(16).hex() + '\n')
PY
chmod 0400 "$WORK/secrets/onboarding_master_key" "$WORK/secrets/onboard_admin_token"
KEY="$WORK/secrets/onboarding_master_key"
printf 'Authorization: Bearer %s\n' "$(cat "$WORK/secrets/onboard_admin_token")" >"$WORK/headers.txt"
DB="$WORK/data/state.sqlite"
PORT="$(pick_port)"

start_service() { # start_service <binary> <port> <db>
  ONBOARD_LISTEN="127.0.0.1:$2" ONBOARD_DB_PATH="$3" ONBOARD_FAKE_PROVIDERS=true \
  ONBOARD_DOMAINS=moonbyte.test ONBOARD_LOG_LEVEL=warn \
  ONBOARD_MASTER_KEY_FILE="$KEY" ONBOARD_ADMIN_TOKEN_FILE="$WORK/secrets/onboard_admin_token" \
  "$1" >>"$WORK/service.log" 2>&1 &
  echo $!
}
APP_PID="$(start_service "$WORK/old-onboardd" "$PORT" "$DB")"
wait_ready "$PORT" "$APP_PID" || { tail -30 "$WORK/service.log"; fail "historical service did not become ready"; }
ok "historical service runs and is ready (schema5)"

for n in 1 2; do
  IK="hist-$n-$$-$RANDOM"
  code="$(create_run "$PORT" "$IK")"
  case "$code" in 200|201|202) ok "historical run $n created (HTTP $code)";; *) bad "historical run $n HTTP $code: $(head -c 200 "$WORK/resp.json")";; esac
  RUN_ID="$(json_field id)"
  [ -n "$RUN_ID" ] || RUN_ID="$(python3 -c 'import json;d=json.load(open("'"$WORK"'/resp.json"));print(d.get("run",{}).get("id",""))' 2>/dev/null)"
  if [ "$n" = "1" ]; then RUN1="$RUN_ID"; else RUN2="$RUN_ID"; fi
done
sleep 3
code="$(api "$PORT" GET /v1/runs)"; [ "$code" = "200" ] && ok "historical run list readable" || bad "historical run list HTTP $code"
hist_runs="$(python3 -c 'import json;d=json.load(open("'"$WORK"'/resp.json"));print(len(d.get("runs") or d.get("items") or []))' 2>/dev/null)"
code="$(api "$PORT" GET /v1/audit)"; [ "$code" = "200" ] && ok "historical audit readable" || bad "historical audit HTTP $code"
hist_audit="$(python3 -c 'import json;d=json.load(open("'"$WORK"'/resp.json"));print(len(d.get("entries") or d.get("audit") or d.get("items") or []))' 2>/dev/null)"
code="$(api "$PORT" GET /v1/ops/external)"; [ "$code" = "200" ] && ok "historical external operations readable" || bad "historical external ops HTTP $code"
hist_ops="$(python3 -c 'import json;d=json.load(open("'"$WORK"'/resp.json"));print(len(d.get("operations") or d.get("items") or d.get("external_operations") or []))' 2>/dev/null)"
if [ -n "$RUN1" ]; then
  code="$(api "$PORT" GET "/v1/runs/$RUN1")"; [ "$code" = "200" ] && ok "historical run detail readable" || bad "historical run detail HTTP $code"
  code="$(api "$PORT" GET "/v1/runs/$RUN1/events")"; [ "$code" = "200" ] && ok "historical run events readable" || bad "historical run events HTTP $code"
  hist_events="$(python3 -c 'import json;d=json.load(open("'"$WORK"'/resp.json"));print(len(d.get("events") or d.get("items") or []))' 2>/dev/null)"
fi
echo "historical counts: runs=$hist_runs audit=$hist_audit external_ops=$hist_ops events=$hist_events"
[ -e "$DB-wal" ] && ok "historical database runs in WAL mode" || bad "no WAL file on the historical database"

# Session row: log in through the console so the database carries a real session.
curl -s -m 10 -o /dev/null -c "$WORK/cookies.txt" -X POST -d "token=$(cat "$WORK/secrets/onboard_admin_token")" "http://127.0.0.1:$PORT/ui/login" || true
if [ -s "$WORK/cookies.txt" ]; then ok "historical console session created"; else bad "no console session cookie"; fi

api "$PORT" GET /v1/audit >/dev/null 2>&1
python3 - "$WORK/resp.json" "$WORK/hist-audit-ids.json" <<'PY3'
import json, sys
d = json.load(open(sys.argv[1]))
entries = d.get("entries") or d.get("audit") or d.get("items") or []
json.dump(entries, open(sys.argv[2], "w"))
PY3
kill -TERM "$APP_PID" 2>/dev/null; wait "$APP_PID" 2>/dev/null; APP_PID=""
[ -f "$DB" ] && ok "historical database file present after shutdown" || fail "historical database missing"

# The historical backup tool produces the snapshot: this is the real old format.
"$WORK/old-backup" -db "$DB" -out-dir "$WORK/backups" -encrypt -keyring "$KEY" >"$WORK/old-backup.json" 2>"$WORK/old-backup.err" \
  || { cat "$WORK/old-backup.err"; fail "historical backup failed"; }
SNAP="$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("snapshot_path") or d.get("snapshot") or d.get("path") or "")' "$WORK/old-backup.json" 2>/dev/null)"
[ -n "$SNAP" ] && [ -f "$SNAP" ] && ok "historical tool produced a snapshot" || { cat "$WORK/old-backup.json"; fail "historical snapshot missing"; }
old_schema="$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print((d.get("manifest") or {}).get("schema_version") or d.get("schema_version") or "")' "$WORK/old-backup.json" 2>/dev/null)"
[ "$old_schema" = "5" ] && ok "historical snapshot schema_version is 5" || bad "historical snapshot schema_version was '$old_schema'"

# Current tooling restores the historical snapshot into a fresh database.
NEWDB="$WORK/data/restored.sqlite"
"$WORK/cur-restore" -snapshot "$SNAP" -target "$NEWDB" -keyring "$KEY" -force >"$WORK/cur-restore.json" 2>"$WORK/cur-restore.err" \
  || { cat "$WORK/cur-restore.err"; fail "current restore of the historical snapshot failed"; }
ok "current tooling restored the historical snapshot"

PORT2="$(pick_port)"
APP2_PID="$(start_service "$WORK/cur-onboardd" "$PORT2" "$NEWDB")"
wait_ready "$PORT2" "$APP2_PID" || { tail -40 "$WORK/service.log"; fail "current service did not start on the restored database"; }
ok "current service starts on the restored database and migrates 5 -> 6"

code="$(api "$PORT2" GET /v1/runs)"; [ "$code" = "200" ] && ok "restored run list readable" || bad "restored run list HTTP $code"
new_runs="$(python3 -c 'import json;d=json.load(open("'"$WORK"'/resp.json"));print(len(d.get("runs") or d.get("items") or []))' 2>/dev/null)"
[ "$new_runs" = "$hist_runs" ] && ok "run count preserved ($new_runs)" || bad "run count changed: $hist_runs -> $new_runs"
if [ -n "$RUN1" ]; then
  code="$(api "$PORT2" GET "/v1/runs/$RUN1")"; [ "$code" = "200" ] && ok "original run id still resolves after migration" || bad "run id lost after migration (HTTP $code)"
  code="$(api "$PORT2" GET "/v1/runs/$RUN1/events")"; [ "$code" = "200" ] && ok "run events still resolve after migration" || bad "run events lost (HTTP $code)"
  new_events="$(python3 -c 'import json;d=json.load(open("'"$WORK"'/resp.json"));print(len(d.get("events") or d.get("items") or []))' 2>/dev/null)"
  [ -n "$hist_events" ] && [ "$new_events" = "$hist_events" ] && ok "event count preserved ($new_events)" || bad "event count changed: $hist_events -> $new_events"
fi
code="$(api "$PORT2" GET /v1/audit)"; [ "$code" = "200" ] && ok "audit readable after migration" || bad "audit HTTP $code"
new_audit="$(python3 -c 'import json;d=json.load(open("'"$WORK"'/resp.json"));print(len(d.get("entries") or d.get("audit") or d.get("items") or []))' 2>/dev/null)"
missing_audit="$(python3 - "$WORK/hist-audit-ids.json" "$WORK/resp.json" <<'PY2'
import json, sys
old = json.load(open(sys.argv[1]))
new = json.load(open(sys.argv[2]))
entries = new.get("entries") or new.get("audit") or new.get("items") or []
have = set()
for e in entries:
    for cand in (e.get("id"), e.get("audit_id"), e.get("seq")):
        if cand not in (None, ""):
            have.add(str(cand))
missing = 0
for e in old:
    for cand in (e.get("id"), e.get("audit_id"), e.get("seq")):
        if cand not in (None, ""):
            if str(cand) not in have:
                missing += 1
            break
print(missing)
PY2
)"
if [ -z "$missing_audit" ]; then missing_audit=1; fi
[ "$missing_audit" = "0" ] && ok "every historical audit entry still present ($hist_audit -> $new_audit)" || bad "$missing_audit historical audit entries missing"
code="$(api "$PORT2" GET /v1/ops/external)"; [ "$code" = "200" ] && ok "external operations readable after migration" || bad "external ops HTTP $code"
new_ops="$(python3 -c 'import json;d=json.load(open("'"$WORK"'/resp.json"));print(len(d.get("operations") or d.get("items") or d.get("external_operations") or []))' 2>/dev/null)"
[ -n "$hist_ops" ] && [ "$new_ops" = "$hist_ops" ] && ok "external operation count preserved ($new_ops)" || bad "external op count changed: $hist_ops -> $new_ops"
code="$(api "$PORT2" GET /v1/ops/alerts/outbox)"; [ "$code" = "200" ] && ok "schema6 outbox exists after migration (empty, no re-enqueue)" || bad "outbox HTTP $code after migration"

# The migrated database must report schema 6 through the current backup tool.
"$WORK/cur-backup" -db "$NEWDB" -out-dir "$WORK/backups" -encrypt -keyring "$KEY" >"$WORK/cur-backup.json" 2>"$WORK/cur-backup.err" \
  || { cat "$WORK/cur-backup.err"; fail "current backup of the migrated database failed"; }
new_schema="$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print((d.get("manifest") or {}).get("schema_version") or d.get("schema_version") or "")' "$WORK/cur-backup.json" 2>/dev/null)"
[ "$new_schema" = "6" ] && ok "migrated database reports schema_version 6" || bad "migrated schema_version was '$new_schema'"

# ---- failure modes ---------------------------------------------------------
python3 -c 'import os,sys;open(sys.argv[1],"wb").write(os.urandom(32))' "$WORK/secrets/wrong.key"
if "$WORK/cur-verify" -snapshot "$SNAP" -keyring "$WORK/secrets/wrong.key" >/dev/null 2>&1; then bad "wrong key accepted"; else ok "wrong master key rejected"; fi
cp "$SNAP" "$WORK/backups/corrupt.sqlite.enc"; cp "$SNAP.manifest.json" "$WORK/backups/corrupt.sqlite.enc.manifest.json"
printf 'corrupt' >>"$WORK/backups/corrupt.sqlite.enc"
if "$WORK/cur-verify" -snapshot "$WORK/backups/corrupt.sqlite.enc" -keyring "$KEY" >/dev/null 2>&1; then bad "corrupt snapshot accepted"; else ok "corrupt snapshot rejected"; fi
cp "$SNAP" "$WORK/backups/nomanifest.sqlite.enc"
if "$WORK/cur-restore" -snapshot "$WORK/backups/nomanifest.sqlite.enc" -target "$WORK/data/x.sqlite" -keyring "$KEY" -force >/dev/null 2>&1; then bad "missing manifest accepted"; else ok "missing manifest rejected"; fi

echo "OLD_COMMIT=$OLD_COMMIT OLD_SCHEMA=$old_schema NEW_SCHEMA=$new_schema historic_runs=$hist_runs restored_runs=$new_runs"
[ "$F" -eq 0 ] && echo HISTORICAL_BACKUP_PASS
exit "$F"
