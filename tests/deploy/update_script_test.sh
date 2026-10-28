#!/usr/bin/env bash
# Failure-scenario suite for scripts/update_account_onboarding.sh.
# Deterministic: isolated ROOT, isolated env, isolated log per case, no docker, no network.
set -uo pipefail
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="$REPO/scripts/update_account_onboarding.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
STUB="$TMP/bin"; mkdir -p "$STUB"
PASS=0; FAIL=0; N=0; RC=0; LOG=""
ok() { PASS=$((PASS+1)); printf "PASS %s\n" "$1"; }
bad() { FAIL=$((FAIL+1)); printf "FAIL %s\n" "$1"; }

# ---- docker stub: parses the compose flag list properly --------------------------
cat > "$STUB/docker" <<'STUB'
#!/usr/bin/env bash
cmd="$1"; shift || true
if [ "$cmd" = "info" ]; then exit "${STUB_INFO_RC:-0}"; fi
if [ "$cmd" = "ps" ]; then [ -n "${STUB_CONTAINER:-}" ] && echo "$STUB_CONTAINER"; exit 0; fi
if [ "$cmd" = "inspect" ]; then
  args="$*"
  case "$args" in
    *"/app/data"*) echo "${STUB_MOUNT_DATA:-}"; exit 0 ;;
    *"/run/secrets"*) echo "${STUB_MOUNT_SECRETS:-}"; exit 0 ;;
    *"{{.Image}}"*) echo "sha256:stubimage"; exit 0 ;;
    *"{{.Id}}"*) echo "sha256:stubimage"; exit 0 ;;
    *"{{.Image}}"*) echo "${STUB_CONTAINER_IMAGE:-sha256:stubimage}"; exit 0 ;;
    *ExitCode*) echo "exit=${STUB_EXIT_CODE:-0} restarts=${STUB_RESTARTS:-0}"; exit 0 ;;
  esac
  echo "exit=0 restarts=0"; exit 0
fi
if [ "$cmd" = "image" ]; then echo "sha256:stubimage"; exit 0; fi
if [ "$cmd" = "build" ]; then sleep "${STUB_BUILD_SLEEP:-0}"; exit "${STUB_BUILD_RC:-0}"; fi
if [ "$cmd" = "run" ]; then echo "DOCKER_RUN"; exit "${STUB_RUN_RC:-0}"; fi
if [ "$cmd" != "compose" ]; then echo "stub: unimplemented docker command: $cmd" >&2; exit 127; fi
# compose: walk the arguments to find the subcommand, ignoring flag values
sub=""
while [ $# -gt 0 ]; do
  case "$1" in
    -p|-f|--env-file|--project-name|--file) shift 2; continue ;;
    -*) shift; continue ;;
    *) sub="$1"; break ;;
  esac
done
case "$sub" in
  version) exit 0 ;;
  config) [ "${2:-}" = "--quiet" ] && exit "${STUB_CONFIG_RC:-0}"; echo "services: {onboarding: {}}"; exit "${STUB_CONFIG_RC:-0}" ;;
  up) echo "COMPOSE_UP"; exit "${STUB_UP_RC:-0}" ;;
  ps) echo "account-onboarding running"; exit 0 ;;
  logs) echo "stub logs"; exit 0 ;;
  *) echo "stub: unimplemented compose subcommand: $sub" >&2; exit 127 ;;
esac
STUB
cat > "$STUB/git" <<'STUB'
#!/usr/bin/env bash
[ "${1:-}" = "-C" ] && shift 2
sub="${1:-}"; shift || true
case "$sub" in
  status) [ "${STUB_DIRTY:-0}" = "1" ] && echo " M changed"; exit 0 ;;
  fetch) exit "${STUB_FETCH_RC:-0}" ;;
  rev-parse) case "${1:-}" in origin/main) echo "${STUB_TARGET:-1111111111111111111111111111111111111111}" ;; *) echo "${STUB_HEAD:-1111111111111111111111111111111111111111}" ;; esac; exit 0 ;;
  cat-file) exit 0 ;; merge-base) exit 0 ;; merge) exit 0 ;;
  *) echo "stub: unimplemented git command: $sub" >&2; exit 127 ;;
esac
STUB
printf "#!/usr/bin/env bash\nshift 2\nexec \"\$@\"\n" > "$STUB/timeout"
cat > "$STUB/df" <<'STUB'
#!/usr/bin/env bash
echo "Filesystem     1024-blocks    Used Available Capacity Mounted on"
echo "/dev/root         100000000 1000000  99000000       2% /"
STUB
cat > "$STUB/curl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  *readyz*)
    if [ "${STUB_REQUIRE_AUTH:-0}" = "1" ]; then
      case "$*" in *"-H @"*) echo "${STUB_READYZ:-200}" ;; *) echo 401 ;; esac
    else
      echo "${STUB_READYZ:-200}"
    fi ;;
  *healthz*) echo "${STUB_HEALTHZ:-200}" ;;
  *) echo 200 ;;
esac
STUB
chmod +x "$STUB"/*

mkroot() { # fresh root + fixture; clears every per-case env override
  N=$((N+1)); ROOT="$TMP/r$N"; LOG="$TMP/log$N.txt"
  mkdir -p "$ROOT/app/.git" "$ROOT/app/deploy" "$ROOT/runtime/data" "$ROOT/runtime/secrets-src" "$ROOT/backups" "$ROOT/deploy" "$ROOT/logs"
  printf k > "$ROOT/runtime/secrets-src/onboarding_master_key"
  printf t > "$ROOT/runtime/secrets-src/onboard_admin_token"
  printf "ARG GOLANG_IMAGE=golang:stub@sha256:stub\nFROM scratch\n" > "$ROOT/app/Dockerfile"
  cp "$REPO/deploy/compose.yaml" "$ROOT/app/deploy/compose.yaml"
  unset STUB_DIRTY STUB_FETCH_RC STUB_HEAD STUB_TARGET STUB_CONTAINER STUB_MOUNT_DATA STUB_MOUNT_SECRETS
  unset STUB_CONFIG_RC STUB_BUILD_RC STUB_RUN_RC STUB_INFO_RC STUB_UP_RC STUB_EXIT_CODE STUB_RESTARTS STUB_REQUIRE_AUTH
}
run() { ( PATH="$STUB:/usr/bin:/bin" ONBOARD_RETRY_SLEEP=0 "$SCRIPT" "$@" ) >"$LOG" 2>&1; RC=$?; }
want() { # want <name> <rc> [needle]
  if [ "$RC" != "$2" ]; then
    bad "$1 (rc=$RC want=$2)"; sed "s/^/    | /" "$LOG" | tail -12; return
  fi
  if [ -n "${3:-}" ] && ! grep -qF -- "$3" "$LOG"; then bad "$1 (missing: $3)"; sed "s/^/    | /" "$LOG" | tail -12; return; fi
  ok "$1"
}
mkroot; run --nonsense; want "unknown argument rejected" 1 "unknown argument"
mkroot; run --offline --build-only --root="$ROOT"; want "offline without sha rejected" 1 "requires --expected-sha"
mkroot; STUB_DIRTY=1 run --preflight --root="$ROOT"; want "dirty worktree refused" 2 "worktree is dirty"
mkroot; STUB_FETCH_RC=1 run --preflight --root="$ROOT"; want "fetch failure not silently ignored" 2 "cannot reach the git remote"
mkroot; STUB_TARGET=2222222222222222222222222222222222222222 run --preflight --expected-sha=3333333333333333333333333333333333333333 --root="$ROOT"; want "expected-sha mismatch refused" 2 "not the required"
mkroot; : > "$ROOT/runtime/data/state.sqlite.restore-in-progress"; run --preflight --root="$ROOT"; want "marker blocks a first install" 3 "restore marker is present"
mkroot; mv "$ROOT/runtime/data" "$ROOT/runtime/data-real"; ln -s "$ROOT/runtime/data-real" "$ROOT/runtime/data"; run --preflight --root="$ROOT"; want "symlinked data directory is refused" 3 "is a symlink"
mkroot; : > "$ROOT/runtime/data/state.sqlite"; run --preflight --root="$ROOT"; want "empty database without a container is refused" 3 "exists but is empty"
mkroot; python3 - "$ROOT/runtime/data/state.sqlite" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)")
db.execute("INSERT INTO schema_migrations(version) VALUES (6)")
db.commit(); db.close()
PY
run --preflight --root="$ROOT"; want "valid database without a container is safely reused" 0 "found a database with no container"
mkroot; : > "$ROOT/runtime/data/state.sqlite"; : > "$ROOT/runtime/data/state.sqlite.restore-in-progress"; run --preflight --root="$ROOT"; want "marker blocks a database without a container" 3 "restore marker is present"
mkroot; : > "$ROOT/runtime/data/state.sqlite.restore-in-progress"; STUB_CONTAINER=account-onboarding STUB_MOUNT_DATA="$ROOT/runtime/data" run --preflight --root="$ROOT"; want "marker blocks a running instance" 3 "restore marker is present"
mkroot; STUB_CONTAINER=account-onboarding STUB_MOUNT_DATA=/elsewhere/data run --preflight --root="$ROOT"; want "data mount mismatch refused" 3 "Refusing to start on an empty directory"
mkroot; printf "services: {}\n" > "$ROOT/deploy/compose.override.yml"; run --build-only --root="$ROOT"; want "hand-written override preserved" 3 "not generated by this script"
mkroot; STUB_CONFIG_RC=1 run --build-only --root="$ROOT"; want "invalid compose refused" 2 "compose configuration is invalid"
mkroot; STUB_BUILD_RC=1 run --build-only --root="$ROOT"; want "build failure aborts" 1 "image build failed"
mkroot; run --root="$ROOT"; want "default action is build-only" 0 "nothing was deployed"
if grep -q COMPOSE_UP "$LOG"; then bad "default action touched the service"; else ok "default action left the service alone"; fi
if grep -q DOCKER_RUN "$LOG"; then bad "build-only materialized secrets"; else ok "build-only did not touch the secret set"; fi
mkroot; run --deploy --dry-run --root="$ROOT"; want "dry-run deploy ok" 0 "DRY-RUN"
if grep -q COMPOSE_UP "$LOG"; then bad "dry-run started a container"; else ok "dry-run started nothing"; fi
mkroot; run --status --root="$ROOT"; want "status without state ok" 0 "no deployment state"
mkroot
python3 - "$ROOT/runtime/data/state.sqlite" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)")
db.execute("INSERT INTO schema_migrations(version) VALUES (6)")
db.commit(); db.close()
PY
printf '{\n  "image": "account-onboarding:local-abc",\n  "image_id": "sha256:stubimage",\n  "commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",\n  "schema_version": 6,\n  "previous_image": "",\n  "previous_image_id": "",\n  "previous_commit": "",\n  "previous_schema_version": 0\n}\n' > "$ROOT/deploy/deployment-state.json"
printf '{\n  "attempt_id": "deploy-stub",\n  "status": "mutation_started",\n  "operation": "deploy",\n  "requested_commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",\n  "image_tag": "account-onboarding:local-abc",\n  "expected_image_id": "sha256:stubimage",\n  "previous_image": "",\n  "previous_image_id": "",\n  "previous_commit": "",\n  "previous_schema_version": 0,\n  "snapshot": "snapshot-1"\n}\n' > "$ROOT/deploy/deployment-attempt.json"
STUB_CONTAINER=account-onboarding run --reconcile --root="$ROOT"; want "reconcile commits a matching healthy attempt" 0 "reconciled persisted attempt"
grep -q '"status": "committed"' "$ROOT/deploy/deployment-attempt.json" || bad "reconcile did not mark the attempt committed"
mkroot; STUB_CONTAINER=account-onboarding STUB_REQUIRE_AUTH=1 run --status --root="$ROOT"; want "status authenticates readiness with the source token" 0 "readyz: 200"
mkroot; run --rollback --root="$ROOT"; want "rollback without state refused" 1 "nothing to roll back"
mkroot; run --backup-only --root="$ROOT"; want "backup-only without a database is a no-op" 0 "no database to back up"
mkroot
(
  PATH="$STUB:/usr/bin:/bin" ONBOARD_LOCK_FILE="$TMP/race.lock" STUB_BUILD_SLEEP=2 ONBOARD_RETRY_SLEEP=0 "$SCRIPT" --build-only --root="$ROOT" >"$TMP/race1.log" 2>&1
  echo $? >"$TMP/race1.rc"
) &
RACE_PID=$!
sleep 0.2
PATH="$STUB:/usr/bin:/bin" ONBOARD_LOCK_FILE="$TMP/race.lock" ONBOARD_RETRY_SLEEP=0 "$SCRIPT" --build-only --root="$ROOT" >"$TMP/race2.log" 2>&1
RACE2_RC=$?
wait "$RACE_PID"
RACE1_RC="$(cat "$TMP/race1.rc")"
if [ "$RACE1_RC" = "0" ] && [ "$RACE2_RC" -ne 0 ] && grep -q "operation is running" "$TMP/race2.log"; then
  ok "concurrent build-only is excluded by the operation lock"
else
  bad "concurrent build-only is excluded by the operation lock (first=$RACE1_RC second=$RACE2_RC)"
fi
mkroot; : > "$ROOT/runtime/data/state.sqlite"; STUB_RUN_RC=1 run --backup-only --root="$ROOT"; want "backup failure aborts" 3 "backup failed"
mkroot; printf "not a database" > "$ROOT/runtime/data/state.sqlite"; printf '{\n  "previous_image": "account-onboarding:local-old",\n  "previous_commit": "abc",\n  "previous_schema_version": 5,\n  "current_image_id": "sha256:stubimage"\n}\n' > "$ROOT/deploy/deployment-state.json"; run --rollback --root="$ROOT"; want "corrupt database fails the schema read" 3 "cannot read the schema version"
mkroot; python3 - "$ROOT/runtime/data/state.sqlite" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)")
db.executemany("INSERT INTO schema_migrations(version) VALUES (?)", [(1,), (5,), (6,)])
db.commit(); db.close()
PY
printf '{\n  "previous_image": "account-onboarding:local-old",\n  "previous_commit": "abc",\n  "previous_schema_version": 5,\n  "current_image_id": "sha256:stubimage"\n}\n' > "$ROOT/deploy/deployment-state.json"
run --rollback --root="$ROOT"; want "schema 6 refuses rollback to a schema 5 release" 3 "not a safe rollback"
# --- deployment state protocol ---
mkroot; printf '{\n  "image": "account-onboarding:local-new",\n  "image_id": "sha256:stubimage",\n  "commit": "abc",\n  "schema_version": 6,\n  "host_port": 8788,\n  "current_image_id": "sha256:stubimage",\n  "last_good_image": "account-onboarding:local-old"\n}\n' > "$ROOT/deploy/deployment-state.json"
printf '{\n  "tag": "account-onboarding:local-new",\n  "image_id": "sha256:stubimage",\n  "commit": "abc",\n  "started_at": "2026-01-01T00:00:00Z"\n}\n' > "$ROOT/deploy/deployment-state.json.pending"
run --status --root="$ROOT"; want "--status reports a pending deployment" 0 "pending deployment"
mkroot; printf '{\n  "image": "account-onboarding:local-new",\n  "current_image_id": "sha256:stubimage",\n  "host_port": 8788,\n  "last_good_image": "account-onboarding:local-old",\n  "last_good_commit": "abc",\n  "last_good_schema_version": 6\n}\n' > "$ROOT/deploy/deployment-state.json"
python3 - "$ROOT/runtime/data/state.sqlite" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)")
db.executemany("INSERT INTO schema_migrations(version) VALUES (?)", [(1,), (6,)])
db.commit(); db.close()
PY
run --rollback --root="$ROOT"; grep -q "rolling back to account-onboarding:local-old" "$LOG" && ok "--rollback targets last_good_image" || bad "--rollback did not target last_good_image"
mkroot; printf '{\n  "image": "account-onboarding:local-new",\n  "current_image_id": "sha256:stubimage",\n  "host_port": 8788,\n  "last_good_image": "account-onboarding:local-old",\n  "last_good_commit": "abc",\n  "last_good_schema_version": 6,\n  "previous_image_id": "sha256:wrongimage",\n  "previous_schema_version": 6\n}\n' > "$ROOT/deploy/deployment-state.json"; python3 - "$ROOT/runtime/data/state.sqlite" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1]); db.execute("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)"); db.execute("INSERT INTO schema_migrations(version) VALUES (6)"); db.commit(); db.close()
PY
STUB_CONTAINER=account-onboarding run --rollback --root="$ROOT"; want "rollback refuses a replaced image tag" 3 "recorded previous image id"
mkroot; printf '{\n  "image": "account-onboarding:local-new",\n  "current_image_id": "sha256:stubimage",\n  "host_port": 8788\n}\n' > "$ROOT/deploy/deployment-state.json"
run --rollback --root="$ROOT"; want "--rollback refuses without a last known good" 1 "no previous image recorded"

# --- failed deploy: the container runs the candidate, rollback must recover ---
mkroot; printf '{\n  "current_image_id": "sha256:lastgood",\n  "last_good_image": "account-onboarding:local-old",\n  "last_good_commit": "abc",\n  "last_good_schema_version": 6,\n  "host_port": 8788\n}\n' > "$ROOT/deploy/deployment-state.json"
printf '{\n  "attempt_id": "deploy-1",\n  "status": "failed",\n  "image_tag": "account-onboarding:local-new",\n  "expected_image_id": "sha256:stubimage",\n  "previous_image": "account-onboarding:local-old"\n}\n' > "$ROOT/deploy/deployment-attempt.json"
python3 - "$ROOT/runtime/data/state.sqlite" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1]); db.execute("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)")
db.executemany("INSERT INTO schema_migrations(version) VALUES (?)", [(1,), (6,)]); db.commit(); db.close()
PY
STUB_CONTAINER=account-onboarding run --rollback --root="$ROOT"; grep -q "unfinished deployment attempt" "$LOG" && ok "a failed switch is recoverable by rollback" || bad "failed switch recovery missing"
mkroot; printf '{\n  "current_image_id": "sha256:lastgood",\n  "last_good_image": "account-onboarding:local-old",\n  "last_good_schema_version": 6,\n  "host_port": 8788\n}\n' > "$ROOT/deploy/deployment-state.json"
printf '{\n  "image_id": "sha256:stubimage",\n  "started_at": "x"\n}\n' > "$ROOT/deploy/deployment-state.json.pending"
run --status --root="$ROOT"; grep -q "recovery: run --rollback" "$LOG" && ok "--status gives a recovery instruction" || bad "--status recovery instruction missing"


# mismatch with no attempt file must be an explicit refusal, never a silent abort
mkroot; printf '{\n  "current_image_id": "sha256:lastgood",\n  "last_good_image": "account-onboarding:local-old",\n  "last_good_schema_version": 6,\n  "host_port": 8788\n}\n' > "$ROOT/deploy/deployment-state.json"
run --rollback --root="$ROOT"; want "a mismatch without an attempt is refused with 3" 3 "does not match the recorded current image"
printf "\nSUMMARY pass=%d fail=%d\n" "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] && echo DEPLOY_TOOLING_TESTS_PASS || echo DEPLOY_TOOLING_TESTS_FAIL
[ "$FAIL" -eq 0 ]
