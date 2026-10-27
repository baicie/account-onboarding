#!/usr/bin/env bash
# Failure-scenario tests for scripts/update_account_onboarding.sh.
# Every case runs against a throwaway root with stubbed docker and git, so no
# container, daemon or network is required.
set -uo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="$REPO/scripts/update_account_onboarding.sh"
PASS=0; FAIL=0; TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ok()   { PASS=$((PASS+1)); printf "PASS %s\n" "$1"; }
bad()  { FAIL=$((FAIL+1)); printf "FAIL %s\n" "$1"; }
check() { # check <name> <expected-rc> <actual-rc> [must-contain] <log>
  local name="$1" want="$2" got="$3" needle="${4:-}" log="${5:-}"
  if [ "$got" != "$want" ]; then bad "$name (rc=$got want=$want)"; tail -3 "$log" 2>/dev/null | sed "s/^/    /"; return; fi
  if [ -n "$needle" ] && ! grep -qF "$needle" "$log" 2>/dev/null; then bad "$name (missing: $needle)"; return; fi
  ok "$name"
}

# ---- stub environment ------------------------------------------------------------
new_env() { # new_env -> sets ROOT, STUB, LOG, GITDIR
  ROOT="$TMP/root-$RANDOM"; mkdir -p "$ROOT/app/.git" "$ROOT/runtime/data" "$ROOT/runtime/secrets-src"
  printf "openssl rand 32" > "$ROOT/runtime/secrets-src/onboarding_master_key"
  printf "0123456789abcdef0123456789abcdef" > "$ROOT/runtime/secrets-src/onboard_admin_token"
  STUB="$TMP/bin-$RANDOM"; mkdir -p "$STUB"
  LOG="$ROOT/deploy.log"
  cat > "$STUB/docker" <<'STUBDOCKER'
#!/usr/bin/env bash
case "$1 $2" in
  "info "*) exit "${STUB_DOCKER_INFO_RC:-0}" ;;
  "compose version") exit 0 ;;
  "compose -p")
    case "$*" in *"config --quiet"*) exit "${STUB_COMPOSE_RC:-0}" ;; *"up -d"*) echo "STUB_COMPOSE_UP"; exit 0 ;; esac
    exit 0 ;;
  "ps -a") [ -n "${STUB_CONTAINER:-}" ] && echo "$STUB_CONTAINER"; exit 0 ;;
  "inspect "*) 
    case "$*" in
      *"{{range .Mounts}}"*"/app/data"*) echo "${STUB_MOUNT_DATA:-}" ;;
      *"{{range .Mounts}}"*) echo "${STUB_MOUNT_SECRETS:-}" ;;
      *RestartCount*) echo "${STUB_RESTARTS:-0}" ;;
      *"{{.Id}}"*) echo "sha256:stubimage" ;;
      *) echo "running health=healthy restarts=0 image=stub id=sha256:stubimage" ;;
    esac; exit 0 ;;
  "image inspect") echo "sha256:stubimage"; exit 0 ;;
  "build "*) exit "${STUB_BUILD_RC:-0}" ;;
  "run --rm") exit "${STUB_RUN_RC:-0}" ;;
esac
case "$1" in
  info) exit "${STUB_DOCKER_INFO_RC:-0}" ;;
  ps) [ -n "${STUB_CONTAINER:-}" ] && echo "$STUB_CONTAINER"; exit 0 ;;
  inspect) echo "sha256:stubimage"; exit 0 ;;
esac
exit 0
STUBDOCKER
  cat > "$STUB/git" <<'STUBGIT'
#!/usr/bin/env bash
shift 2  # drop -C <dir>
case "$1" in
  status) [ "${STUB_DIRTY:-0}" = "1" ] && echo " M file" ; exit 0 ;;
  fetch) exit "${STUB_FETCH_RC:-0}" ;;
  rev-parse)
    case "$2" in
      HEAD) echo "${STUB_HEAD:-1111111111111111111111111111111111111111}" ;;
      origin/main) echo "${STUB_TARGET:-1111111111111111111111111111111111111111}" ;;
      *) echo "${STUB_HEAD:-1111111111111111111111111111111111111111}" ;;
    esac; exit 0 ;;
  cat-file) exit "${STUB_CATFILE_RC:-0}" ;;
  merge-base) exit "${STUB_MERGEBASE_RC:-0}" ;;
  merge) exit 0 ;;
esac
exit 0
STUBGIT
  cat > "$STUB/curl" <<'STUBCURL'
#!/usr/bin/env bash
case "$*" in *healthz*) echo "${STUB_HEALTHZ:-200}" ;; *readyz*) echo "${STUB_READYZ:-200}" ;; *) echo 200 ;; esac
STUBCURL
  cat > "$STUB/df" <<'STUBDF'
#!/usr/bin/env bash
echo "Filesystem 1024-blocks Used Available Capacity Mounted"; echo "stub 100 0 ${STUB_FREE:-9000000} 0% /"
STUBDF
  cat > "$STUB/timeout" <<'STUBTIMEOUT'
#!/usr/bin/env bash
shift 1
exec "$@"
STUBTIMEOUT
  chmod +x "$STUB"/*
}

go_case() { # go_case <name> <args...>  -> runs the script with a hard 60s watchdog
  local name="$1"; shift
  ( PATH="$STUB:/usr/bin:/bin:/usr/sbin:/sbin" ONBOARD_TEST=1 ONBOARD_RETRY_SLEEP=0 "$SCRIPT" "$@" ) >"$LOG" 2>&1 &
  local pid=$! i=0
  while kill -0 "$pid" 2>/dev/null; do
    i=$((i+1))
    if [ "$i" -gt 60 ]; then
      kill -9 "$pid" 2>/dev/null
      wait "$pid" 2>/dev/null
      RC=124
      printf "WATCHDOG killed a hanging run
" >> "$LOG"
      return 0
    fi
    sleep 1
  done
  wait "$pid"
  RC=$?
}

# ---- cases -----------------------------------------------------------------------
new_env; go_case "unknown argument" --nonsense
check "unknown argument is rejected" 1 "$RC" "unknown argument" "$LOG"

new_env; go_case "offline without sha" --offline --build-only --root="$ROOT"
check "offline without --expected-sha is rejected" 1 "$RC" "requires --expected-sha" "$LOG"

new_env; go_case "offline short sha" --offline --expected-sha=abc123 --root="$ROOT"
check "offline with a short SHA is rejected" 1 "$RC" "40-char" "$LOG"

new_env; STUB_DIRTY=1 go_case "dirty worktree" --preflight --root="$ROOT"
check "dirty worktree is refused" 2 "$RC" "worktree is dirty" "$LOG"

new_env; STUB_DOCKER_INFO_RC=1 go_case "docker unavailable" --preflight --root="$ROOT"
check "docker unavailable is refused" 2 "$RC" "no docker access" "$LOG"

new_env; STUB_FETCH_RC=1 go_case "fetch failure" --preflight --root="$ROOT"
check "git fetch failure is not silently ignored" 2 "$RC" "cannot reach the git remote" "$LOG"

new_env; STUB_HEAD=1111111111111111111111111111111111111111 STUB_TARGET=2222222222222222222222222222222222222222 STUB_MERGEBASE_RC=1 \
  go_case "diverged checkout" --preflight --root="$ROOT"
check "diverged checkout is refused" 2 "$RC" "diverged" "$LOG"

new_env; go_case "sha mismatch" --preflight --expected-sha=3333333333333333333333333333333333333333 --root="$ROOT"
check "--expected-sha mismatch is refused" 2 "$RC" "not the required" "$LOG"

new_env; mkdir -p "$ROOT/deploy"; printf "services: {}\n" > "$ROOT/deploy/compose.override.yml"
go_case "hand written override" --build-only --root="$ROOT"
check "a hand-written override is never overwritten" 3 "$RC" "not generated by this script" "$LOG"

new_env; STUB_CONTAINER=account-onboarding STUB_MOUNT_DATA=/somewhere/else/data go_case "mount mismatch" --preflight --root="$ROOT"
check "a data mount mismatch is refused" 3 "$RC" "Refusing to start on an empty directory" "$LOG"

new_env; : > "$ROOT/runtime/data/state.sqlite.restore-in-progress"; go_case "restore marker" --preflight --root="$ROOT"
check "a pending restore marker blocks the deploy" 3 "$RC" "restore marker" "$LOG"

new_env; STUB_CONTAINER=account-onboarding STUB_MOUNT_DATA="$ROOT/runtime/data" rm -f "$ROOT/runtime/secrets-src/onboard_admin_token"
go_case "missing admin token" --preflight --root="$ROOT"
check "a missing admin token is refused" 2 "$RC" "onboard_admin_token" "$LOG"

new_env; STUB_COMPOSE_RC=1 go_case "invalid compose" --build-only --root="$ROOT"
check "invalid compose configuration is refused" 2 "$RC" "compose configuration is invalid" "$LOG"

new_env; STUB_RUN_RC=1 go_case "backup failure" --backup-only --root="$ROOT"
check "a failing backup aborts the run" 3 "$RC" "backup failed" "$LOG"

new_env; go_case "status without state" --status --root="$ROOT"
check "--status without state still reports cleanly" 0 "$RC" "no deployment state" "$LOG"

new_env; go_case "rollback without state" --rollback --root="$ROOT"
check "--rollback without state is refused" 1 "$RC" "nothing to roll back" "$LOG"

new_env; STUB_BUILD_RC=1 go_case "build failure" --build-only --root="$ROOT"
check "a failing build aborts before any deploy" 1 "$RC" "image build failed" "$LOG"

new_env; go_case "default action builds only" --root="$ROOT"
check "the default action never deploys" 0 "$RC" "nothing was deployed" "$LOG"
if grep -q "STUB_COMPOSE_UP" "$LOG"; then bad "default action started a container"; else ok "default action did not touch the running service"; fi

new_env; go_case "dry run deploy" --deploy --dry-run --root="$ROOT"
check "--dry-run deploy succeeds without changing state" 0 "$RC" "DRY-RUN" "$LOG"
if grep -q "STUB_COMPOSE_UP" "$LOG"; then bad "--dry-run started a container"; else ok "--dry-run started no container"; fi

new_env; mkdir -p "$ROOT/backups"; printf "{}" > "$ROOT/deploy/deployment-state.json"
go_case "rollback without previous image" --rollback --root="$ROOT"
check "rollback without a recorded previous image is refused" 1 "$RC" "no previous image recorded" "$LOG"

new_env; mkdir -p "$ROOT/backups/20260101T000000Z"; : > "$ROOT/backups/20260101T000000Z/x.enc"
printf "%s" "20260101T000000Z" > "$ROOT/backups/LATEST"
printf "{\n  \"previous_image\": \"account-onboarding:local-old\",\n  \"previous_commit\": \"abc\",\n  \"previous_schema_version\": 5\n}\n" > "$ROOT/deploy/deployment-state.json"
printf "SQLite format 3" > "$ROOT/runtime/data/state.sqlite"; python3 - "$ROOT/runtime/data/state.sqlite" <<'PY'
import sys
p = sys.argv[1]
data = bytearray(open(p, "rb").read())
data.extend(b"\x00" * (64 - len(data)))
data[60:64] = (6).to_bytes(4, "big")
open(p, "wb").write(bytes(data))
PY
go_case "schema forward rollback" --rollback --root="$ROOT"
check "rollback refuses when the database schema is newer" 3 "$RC" "not a safe rollback" "$LOG"

new_env; STUB_CONTAINER=account-onboarding STUB_MOUNT_DATA="$ROOT/runtime/data" go_case "status with live container" --status --root="$ROOT"
check "--status reports the live container" 0 "$RC" "healthz:" "$LOG"

printf "\nSUMMARY pass=%d fail=%d\n" "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] && echo DEPLOY_TOOLING_TESTS_PASS || echo DEPLOY_TOOLING_TESTS_FAIL
[ "$FAIL" -eq 0 ]
