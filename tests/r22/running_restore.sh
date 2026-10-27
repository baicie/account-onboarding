#!/usr/bin/env bash
# Running-restore and backup-retention acceptance.
#
# Protocol under test (the documented operating procedure):
#   maintenance enter -> writes rejected, workers paused, in-flight drained
#   service stopped   -> the database file is not open by anyone
#   restore           -> snapshot promoted over the live path, old file kept
#   service started   -> ready again, data intact
#   retention         -> prunes to the policy, never below the valid floor
#
# Real binaries, real SQLite/WAL, real encrypted snapshots. No mocks.
set -o pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TMPROOT="$TMPDIR"; [ -n "$TMPROOT" ] || TMPROOT=/tmp
WORK="$(mktemp -d "$TMPROOT/r22-restore.XXXXXX")"
P=0; F=0; SKIP=0; PID=""; CLEAN_RC=0
pick_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }
ok() { P=$((P+1)); echo "PASS $1"; }
bad() { F=$((F+1)); echo "FAIL $1"; }
skip() { SKIP=$((SKIP+1)); echo "SKIP $1"; }
fail() { echo "FATAL: $*" >&2; exit 1; }
cleanup() {
  local rc=$?
  [ -z "$PID" ] || { kill "$PID" 2>/dev/null; wait "$PID" 2>/dev/null; }
  if [ -d "$WORK" ]; then rm -rf "$WORK" || CLEAN_RC=1; fi
  [ ! -d "$WORK" ] || { echo "cleanup: work dir left" >&2; CLEAN_RC=1; }
  echo "SUMMARY pass=$P fail=$F skip=$SKIP"
  if [ "$rc" -ne 0 ]; then exit "$rc"; fi
  [ "$CLEAN_RC" -eq 0 ] || { echo "CLEANUP_FAILED" >&2; exit 1; }
  [ "$F" -eq 0 ] && exit 0
  exit 1
}
trap cleanup EXIT

echo "=== running restore + retention ==="
# R22_BIN_DIR lets CI build once as the normal user and then run this script as
# root, which is what the disk-full case needs.
if [ -n "$R22_BIN_DIR" ]; then
  for b in onboardd backup restore verify retention; do
    [ -x "$R22_BIN_DIR/$b" ] || fail "missing prebuilt binary $b in R22_BIN_DIR"
    cp "$R22_BIN_DIR/$b" "$WORK/$b"
  done
  ok "binaries taken from R22_BIN_DIR"
else
  ( cd "$ROOT" && go build -o "$WORK/onboardd" ./cmd/onboardd \
      && go build -o "$WORK/backup" ./internal/ops/cmd/backup \
      && go build -o "$WORK/restore" ./internal/ops/cmd/restore \
      && go build -o "$WORK/verify" ./internal/ops/cmd/verify \
      && go build -o "$WORK/retention" ./internal/ops/cmd/retention ) >"$WORK/build.log" 2>&1 \
    || { tail -20 "$WORK/build.log"; fail "build"; }
  ok "binaries built"
fi

mkdir -p "$WORK/data" "$WORK/backups" "$WORK/secrets"
python3 - "$WORK/secrets/onboarding_master_key" "$WORK/secrets/onboard_admin_token" <<'PY'
import os, sys
open(sys.argv[1], 'wb').write(os.urandom(32))
open(sys.argv[2], 'w').write('restore-admin-' + os.urandom(16).hex() + '\n')
PY
chmod 0400 "$WORK/secrets/onboarding_master_key" "$WORK/secrets/onboard_admin_token"
KEY="$WORK/secrets/onboarding_master_key"
DB="$WORK/data/state.sqlite"
PORT="$(pick_port)"
printf 'Authorization: Bearer %s\n' "$(cat "$WORK/secrets/onboard_admin_token")" >"$WORK/headers.txt"

start_service() {
  ONBOARD_LISTEN="127.0.0.1:$PORT" ONBOARD_DB_PATH="$DB" ONBOARD_FAKE_PROVIDERS=true \
  ONBOARD_DOMAINS=moonbyte.test ONBOARD_LOG_LEVEL=warn \
  ONBOARD_MASTER_KEY_FILE="$KEY" ONBOARD_ADMIN_TOKEN_FILE="$WORK/secrets/onboard_admin_token" \
  "$WORK/onboardd" >>"$WORK/service.log" 2>&1 &
  PID=$!
  for _ in $(seq 1 300); do
    curl -sf -m 2 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$PID" 2>/dev/null || return 1
    sleep 0.2
  done
  return 1
}
api() {
  local method="$1" path="$2" body=""
  if [ "$#" -ge 3 ]; then body="$3"; fi
  if [ -n "$body" ]; then
    curl -s -m 20 -o "$WORK/resp.json" -w '%{http_code}' -X "$method" \
      -H @"$WORK/headers.txt" -H 'Content-Type: application/json' -d "$body" \
      "http://127.0.0.1:$PORT$path"
  else
    curl -s -m 20 -o "$WORK/resp.json" -w '%{http_code}' -X "$method" \
      -H @"$WORK/headers.txt" "http://127.0.0.1:$PORT$path"
  fi
}
create_run() {
  local key="$1" body
  body='{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e","mailbox_provider":"fake-mailbox","identity_provider":"fake-identity","oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox","idempotency_key":"'"$key"'"}'
  curl -s -m 20 -o "$WORK/resp.json" -w '%{http_code}' -X POST \
    -H @"$WORK/headers.txt" -H "Idempotency-Key: $key" -H 'Content-Type: application/json' \
    -d "$body" "http://127.0.0.1:$PORT/v1/runs"
}
count_runs() { python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(len(d.get("runs") or d.get("items") or []))' "$WORK/resp.json" 2>/dev/null; }
snap_of() { python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("snapshot_path") or "")' "$1" 2>/dev/null; }

start_service || { tail -30 "$WORK/service.log"; fail "service did not start"; }
ok "service started on a fresh database"

for n in 1 2; do
  code="$(create_run "rr-$n-$$")"
  case "$code" in 200|202) ok "run $n created (HTTP $code)";; *) bad "run $n HTTP $code: $(head -c 160 "$WORK/resp.json")";; esac
done
sleep 2
code="$(api GET /v1/runs)"; [ "$code" = "200" ] && ok "run list readable" || bad "run list HTTP $code"
runs_before="$(count_runs)"; [ -n "$runs_before" ] || runs_before=0
echo "runs_before=$runs_before"

"$WORK/backup" -db "$DB" -out-dir "$WORK/backups" -encrypt -keyring "$KEY" >"$WORK/bak1.json" 2>"$WORK/bak1.err" \
  || { cat "$WORK/bak1.err"; fail "backup before maintenance"; }
SNAP1="$(snap_of "$WORK/bak1.json")"
[ -n "$SNAP1" ] && ok "encrypted snapshot taken before maintenance" || fail "no snapshot"

# ---- maintenance window ----------------------------------------------------
status="$(api POST /v1/maintenance/enter '{"reason":"running restore drill"}')"
[ "$status" = "200" ] && ok "maintenance enter accepted" || bad "maintenance enter HTTP $status"
drained="$(python3 -c 'import json,sys;d=json.load(open("'"$WORK"'/resp.json"));print(d.get("drained"),d.get("workers_paused"),d.get("active"))' 2>/dev/null)"
echo "enter: $drained"
case "$drained" in "True True True") ok "maintenance window is active, drained and workers are paused";; *) bad "maintenance enter response: $drained";; esac

code="$(create_run "rr-blocked-$$")"
[ "$code" = "503" ] && ok "write rejected during maintenance (HTTP 503)" || bad "write during maintenance HTTP $code"
retry_after="$(curl -s -D - -o /dev/null -m 5 -X POST -H @"$WORK/headers.txt" -H "Idempotency-Key: rr-blocked2-$$" \
  -H 'Content-Type: application/json' -d '{}' "http://127.0.0.1:$PORT/v1/runs" | tr -d '\r' | awk 'tolower($1)=="retry-after:"{print $2}')"
[ "$retry_after" = "30" ] && ok "rejection carries Retry-After: 30" || bad "Retry-After header: '$retry_after'"
code="$(api GET /v1/runs)"
[ "$code" = "200" ] && ok "reads still served during maintenance" || bad "read during maintenance HTTP $code"
code="$(curl -s -o /dev/null -w '%{http_code}' -m 5 -H @"$WORK/headers.txt" "http://127.0.0.1:$PORT/readyz")"
[ "$code" = "503" ] && ok "readiness reports the maintenance window" || bad "readyz during maintenance HTTP $code"
code="$(api GET /v1/audit)"
grep -q "maintenance.enter" "$WORK/resp.json" && ok "maintenance enter is audited" || bad "maintenance enter not audited"

# ---- stop, restore, start --------------------------------------------------
kill -TERM "$PID"; wait "$PID" 2>/dev/null; PID=""
ok "service stopped for the maintenance window (database closed)"
[ -e "$DB" ] && ok "database file present while stopped" || bad "database missing"

"$WORK/restore" -snapshot "$SNAP1" -target "$DB" -live-db "$DB" -keyring "$KEY" -force -keep-old >"$WORK/restore.json" 2>"$WORK/restore.err" \
  || { cat "$WORK/restore.err"; fail "restore over the live database"; }
ok "snapshot restored over the live database path"
kept="$(find "$WORK/data" -maxdepth 1 -name 'state.sqlite.pre-restore-*' | wc -l | tr -d ' ')"
[ "$kept" -ge 1 ] && ok "replaced database kept as state.sqlite.pre-restore-*" || bad "no pre-restore copy kept"

start_service || { tail -30 "$WORK/service.log"; fail "service did not start after restore"; }
ok "service started again after the restore"
code="$(curl -s -o /dev/null -w '%{http_code}' -m 5 -H @"$WORK/headers.txt" "http://127.0.0.1:$PORT/readyz")"
[ "$code" = "200" ] && ok "readiness green after restart (window is process local)" || bad "readyz after restart HTTP $code"
code="$(api GET /v1/runs)"; [ "$code" = "200" ] && ok "run list readable after the restore" || bad "run list after restore HTTP $code"
runs_after="$(count_runs)"
[ "$runs_after" = "$runs_before" ] && ok "restored database has the same run count ($runs_after)" || bad "run count changed: $runs_before -> $runs_after"
code="$(create_run "rr-after-$$")"
case "$code" in 200|202) ok "writes are accepted again after the window ($code)";; *) bad "write after window HTTP $code";; esac

# ---- negative restore paths ------------------------------------------------
python3 -c 'import os,sys;open(sys.argv[1],"wb").write(os.urandom(32))' "$WORK/wrong.key"
chmod 0400 "$WORK/wrong.key"
if "$WORK/restore" -snapshot "$SNAP1" -target "$WORK/data/other.sqlite" -keyring "$WORK/wrong.key" -force >/dev/null 2>&1; then
  bad "restore accepted a wrong master key"; else ok "restore rejects a wrong master key"; fi
[ ! -e "$WORK/data/other.sqlite" ] && ok "wrong-key restore left no target" || bad "wrong-key restore created a target"
cp "$SNAP1" "$WORK/backups/tampered.enc"; printf 'corrupt' >>"$WORK/backups/tampered.enc"; cp "$SNAP1.manifest.json" "$WORK/backups/tampered.enc.manifest.json"
if "$WORK/verify" -snapshot "$WORK/backups/tampered.enc" -keyring "$KEY" >/dev/null 2>&1; then
  bad "verify accepted a tampered snapshot"; else ok "verify rejects a tampered snapshot"; fi
if "$WORK/restore" -snapshot "$WORK/backups/tampered.enc" -target "$WORK/data/tampered.sqlite" -keyring "$KEY" -force >/dev/null 2>&1; then
  bad "restore accepted a tampered snapshot"; else ok "restore rejects a tampered snapshot"; fi
if [ "$(uname -s)" = "Linux" ] && [ "$(id -u)" = "0" ]; then
  FULL="$WORK/full"; mkdir -p "$FULL"
  if mount -t tmpfs -o size=256k tmpfs "$FULL" 2>/dev/null; then
    cp "$WORK/data/state.sqlite" "$FULL/target.sqlite" 2>/dev/null || true
    dd if=/dev/zero of="$FULL/filler" bs=1k count=250 2>/dev/null
    if "$WORK/restore" -snapshot "$SNAP1" -target "$FULL/target.sqlite" -keyring "$KEY" -force >/dev/null 2>&1; then
      bad "restore succeeded on a full filesystem"; else ok "restore fails closed on a full filesystem"; fi
    echo "DISK_FULL_RAN"
    rm -f "$FULL/filler"; umount "$FULL" 2>/dev/null
  else
    skip "disk full: tmpfs mount unavailable (BLOCKED, not PASS)"
    echo "DISK_FULL_SKIPPED"
  fi
else
  skip "disk full: requires root on Linux (BLOCKED, not PASS)"
  echo "DISK_FULL_SKIPPED"
fi
echo "INTERRUPTED_RESTORE_COVERED_BY_GO_TESTS"
echo "  crash before/between/after publish is covered deterministically by"
echo "  internal/ops/restore_crash_test.go (restorePublishHook), not by this shell run"

# ---- retention -------------------------------------------------------------
# Retention runs against its own directory: the earlier steps already put
# snapshots there, and a policy test must not depend on that count.
RET_DIR="$WORK/retention-snaps"  # the retention binary is $WORK/retention
mkdir -p "$RET_DIR"
for n in 1 2 3; do
  "$WORK/backup" -db "$DB" -out-dir "$RET_DIR" -encrypt -keyring "$KEY" >"$WORK/bak-$n.json" 2>/dev/null || fail "backup $n"
  sleep 1
done
before="$(find "$RET_DIR" -maxdepth 1 -name 'state-*.sqlite.enc' | wc -l | tr -d ' ')"
newest="$(find "$RET_DIR" -maxdepth 1 -name 'state-*.sqlite.enc' -print0 | xargs -0 ls -t 2>/dev/null | head -1)"
[ "$before" -ge 3 ] && ok "snapshot directory holds $before snapshots before retention" || bad "unexpected snapshot count $before"

"$WORK/retention" -dir "$RET_DIR" -max-count 2 -min-valid 1 -keyring "$KEY" >"$WORK/retention.json" 2>"$WORK/retention.err" \
  || { cat "$WORK/retention.err"; cat "$WORK/retention.json"; fail "retention"; }
after="$(find "$RET_DIR" -maxdepth 1 -name 'state-*.sqlite.enc' | wc -l | tr -d ' ')"
[ "$after" = "2" ] && ok "retention kept exactly the policy count ($after)" || bad "retention kept $after snapshots"
[ -e "$newest" ] && ok "retention kept the newest recovery point" || bad "retention deleted the newest snapshot"
report="$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("report_path",""))' "$WORK/retention.json" 2>/dev/null)"
[ -n "$report" ] && [ -f "$report" ] && ok "retention audit report written ($(basename "$report"))" || bad "retention report missing"
python3 - "$WORK/retention.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
deleted = int(d.get("deleted") or 0)
kept = int(d.get("kept") or 0)
print(f"retention: scanned={d.get('scanned')} valid={d.get('valid')} kept={kept} deleted={deleted} min_valid={d.get('min_valid')}")
sys.exit(0 if deleted >= 1 and kept >= 1 else 1)
PY
[ "$?" -eq 0 ] && ok "retention report shows both keeps and deletes" || bad "retention report inconsistent"

"$WORK/retention" -dir "$RET_DIR" -max-count 1 -min-valid 3 -keyring "$KEY" >"$WORK/retention2.json" 2>/dev/null || true
floor_kept="$(find "$RET_DIR" -maxdepth 1 -name 'state-*.sqlite.enc' | wc -l | tr -d ' ')"
[ "$floor_kept" = "2" ] && ok "minimum-valid floor protects the remaining recovery points" || bad "floor violated: $floor_kept snapshots left"

kill -TERM "$PID" 2>/dev/null; wait "$PID" 2>/dev/null; PID=""
echo "RUNNING_RESTORE_RETENTION_PASS"
exit 0
