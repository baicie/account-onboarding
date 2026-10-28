#!/usr/bin/env bash
# Single-host Docker Compose deployment tool for Account Onboarding.
#
# Default action is --build-only: it never recreates a container. Only an explicit
# --deploy (or --rollback) changes the running service, and every state change is
# preceded by a verified encrypted SQLite snapshot.
#
# Layout under --root (default /opt/account-onboarding):
#   app/      git checkout          runtime/data/    SQLite + WAL
#   runtime/secrets/  materialized 0400 files
#   backups/  encrypted snapshots   deploy/          generated override + state
#   logs/     deployment logs
set -euo pipefail

PROG="$(basename "$0")"
ROOT="/opt/account-onboarding"
ACTION="build"
OFFLINE=0
DRY_RUN=0
NO_FETCH=0
EXPECTED_SHA=""
HOST_PORT="8788"
GO_PROXY="${ONBOARD_GO_PROXY:-https://mirrors.tencent.com/go/,https://goproxy.cn,direct}"
APT_MIRROR="${ONBOARD_APT_MIRROR:-}"
ALLOW_SCHEMA_FORWARD=0
GIT_REMOTE="origin"
GIT_BRANCH="main"
FETCH_TIMEOUT=60
FETCH_RETRIES=3

usage() {
  cat <<USAGE
Usage: $PROG [--preflight|--build-only|--deploy|--status|--rollback|--backup-only|--reconcile]
                    [--offline] [--expected-sha=SHA] [--dry-run] [--no-fetch]
                    [--root=DIR] [--host-port=PORT] [--go-proxy=URL] [--apt-mirror=HOST]
                    [--allow-schema-forward]

  --preflight    check git state, docker, compose, paths, mounts, secrets, disk
  --build-only   resolve SHA, verify it, build account-onboarding:local-<short-sha> (default)
  --deploy       preflight + snapshot + verify + compose up --force-recreate + health
  --status       print the recorded deployment state and the live service state
  --rollback     restore the previous image (binary rollback only; schema checked)
  --backup-only  take and verify an encrypted snapshot, change nothing else
  --reconcile    read a persisted mutation intent and commit state only if the live image matches
  --offline      no network: requires --expected-sha with a full 40-char commit
  --expected-sha full commit SHA that build and deploy must both use
  --dry-run      print every command instead of running it

Exit codes: 0 ok, 1 usage/runtime error, 2 preflight refusal, 3 refused for data safety.
USAGE
}

log()  { printf "%s %s\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
warn() { printf "%s WARN %s\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die()  { local code="${2:-1}"; printf "%s ERROR %s\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" >&2; exit "$code"; }
run()  { if [ "$DRY_RUN" = "1" ]; then log "DRY-RUN would run: $*"; else "$@"; fi; }

while [ $# -gt 0 ]; do
  case "$1" in
    --preflight) ACTION="preflight" ;;
    --build-only) ACTION="build" ;;
    --deploy) ACTION="deploy" ;;
    --status) ACTION="status" ;;
    --rollback) ACTION="rollback" ;;
    --backup-only) ACTION="backup" ;;
    --reconcile) ACTION="reconcile" ;;
    --offline) OFFLINE=1 ;;
    --dry-run) DRY_RUN=1 ;;
    --no-fetch) NO_FETCH=1 ;;
    --allow-schema-forward) ALLOW_SCHEMA_FORWARD=1 ;;
    --expected-sha=*) EXPECTED_SHA="${1#*=}" ;;
    --root=*) ROOT="${1#*=}" ;;
    --host-port=*) HOST_PORT="${1#*=}" ;;
    --go-proxy=*) GO_PROXY="${1#*=}" ;;
    --apt-mirror=*) APT_MIRROR="${1#*=}" ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" 1 ;;
  esac
  shift
done

case "$HOST_PORT" in
  ""|*[!0-9]*) die "--host-port must be a TCP port number" 1 ;;
esac
if [ "$HOST_PORT" -lt 1 ] || [ "$HOST_PORT" -gt 65535 ]; then die "--host-port must be between 1 and 65535" 1; fi

case "$ROOT" in
  /*) ;;
  *) die "--root must be an absolute path" 1 ;;
esac
case "$ROOT" in
  *..*) die "--root must not contain .." 1 ;;
esac
if [ -L "$ROOT" ]; then die "$ROOT is a symlink; refusing to deploy through it" 3; fi
APP="$ROOT/app"; RUNTIME="$ROOT/runtime"; DATA="$RUNTIME/data"; SECRETS_SRC="$RUNTIME/secrets-src"
SECRETS="$RUNTIME/secrets"; BACKUPS="$ROOT/backups"; DEPLOY="$ROOT/deploy"; LOGS="$ROOT/logs"
STATE="$DEPLOY/deployment-state.json"; ATTEMPT="$DEPLOY/deployment-attempt.json"; OVERRIDE="$DEPLOY/compose.override.yml"
BASE_COMPOSE="$DEPLOY/compose.base.yml"
MARKER="# managed-by: scripts/update_account_onboarding.sh -- generated file, edits are overwritten"

mkdir -p "$BACKUPS" "$DEPLOY" "$LOGS" "$DATA" "$SECRETS" "$SECRETS_SRC" 2>/dev/null || true

# Host-wide operation lock. The Compose project name is fixed, so two different
# ROOT values must still serialize against the same host resource. Use flock
# (released by the kernel on SIGKILL); macOS development falls back to an atomic
# PID directory with stale-owner detection.
LOCK_FILE="${ONBOARD_LOCK_FILE:-/var/tmp/account-onboarding-operation.lock}"
LOCK_DIR_FALLBACK="$LOCK_FILE.lock.d"
LOCK_OWNED=0

release_operation_lock() {
  [ "$LOCK_OWNED" = "1" ] || return 0
  if [ -n "${LOCK_FD_OPEN:-}" ]; then
    exec 9<&-
  else
    rm -rf -- "$LOCK_DIR_FALLBACK" 2>/dev/null || true
  fi
  LOCK_OWNED=0
}

acquire_operation_lock() {
  [ "$ACTION" = "status" ] && return 0
  [ "$ACTION" = "preflight" ] && return 0
  if [ -L "$LOCK_FILE" ] || [ -L "$LOCK_DIR_FALLBACK" ]; then
    die "operation lock path is a symlink; refusing to continue" 3
  fi
  if command -v flock >/dev/null 2>&1; then
    if [ ! -e "$LOCK_FILE" ]; then
      ( umask 022; : > "$LOCK_FILE" ) || die "cannot create operation lock $LOCK_FILE" 3
    fi
    exec 9<"$LOCK_FILE" || die "cannot open operation lock $LOCK_FILE" 3
    if ! flock -n 9; then
      exec 9<&-
      die "another deploy, rollback, backup, or build operation is running (lock $LOCK_FILE)" 3
    fi
    LOCK_FD_OPEN=1
  else
    if ! mkdir "$LOCK_DIR_FALLBACK" 2>/dev/null; then
      local holder=""
      [ -f "$LOCK_DIR_FALLBACK/pid" ] && holder="$(cat "$LOCK_DIR_FALLBACK/pid" 2>/dev/null || true)"
      if [ -n "$holder" ] && kill -0 "$holder" 2>/dev/null; then
        die "another deploy, rollback, backup, or build operation is running (pid $holder)" 3
      fi
      rm -rf -- "$LOCK_DIR_FALLBACK" 2>/dev/null || die "stale operation lock cannot be removed: $LOCK_DIR_FALLBACK" 3
      mkdir "$LOCK_DIR_FALLBACK" 2>/dev/null || die "another operation took the lock" 3
    fi
    printf "%s\n" "$$" > "$LOCK_DIR_FALLBACK/pid" || { rm -rf -- "$LOCK_DIR_FALLBACK"; die "cannot write operation lock pid" 3; }
  fi
  LOCK_OWNED=1
  trap release_operation_lock EXIT INT TERM
}

acquire_operation_lock
if [ "$DRY_RUN" != "1" ]; then
  exec > >(tee -a "$LOGS/deploy-$(date -u +%Y%m%dT%H%M%SZ).log") 2>&1
fi

# ---- docker access ---------------------------------------------------------------
detect_docker() {
  command -v docker >/dev/null 2>&1 || die "docker not found" 2
  if docker info >/dev/null 2>&1; then DOCKER=(docker)
  elif sudo -n true >/dev/null 2>&1 && sudo -n docker info >/dev/null 2>&1; then DOCKER=(sudo -n docker)
  else die "no docker access (add the user to the docker group or provide passwordless sudo)" 2; fi
  docker compose version >/dev/null 2>&1 || die "docker compose v2 plugin missing" 2
}
dk() { "${DOCKER[@]}" "$@"; }

# ---- git -------------------------------------------------------------------------
git_in_app() { git -C "$APP" "$@"; }

git_fetch() {
  if [ "$NO_FETCH" = "1" ]; then log "skipping fetch (--no-fetch)"; return 0; fi
  if [ "$OFFLINE" = "1" ]; then log "offline: using the local checkout only"; return 0; fi
  export GIT_TERMINAL_PROMPT=0 GIT_HTTP_LOW_SPEED_LIMIT=1000 GIT_HTTP_LOW_SPEED_TIME="$FETCH_TIMEOUT"
  local attempt
  for attempt in $(seq 1 "$FETCH_RETRIES"); do
    if git_in_app fetch --prune "$GIT_REMOTE" 2>&1; then return 0; fi
    warn "git fetch attempt $attempt/$FETCH_RETRIES failed"
    sleep "${ONBOARD_RETRY_SLEEP:-5}"
  done
  die "cannot reach the git remote; refusing to claim anything is up to date (use --offline --expected-sha=<sha> to build a pinned commit)" 2
}

resolve_sha() {
  local head target
  head="$(git_in_app rev-parse HEAD)"
  if [ "$OFFLINE" = "1" ]; then
    case "$EXPECTED_SHA" in
      "" ) die "--offline requires --expected-sha=<full 40-char commit>" 1 ;;
      *[!0-9a-f]* ) die "--expected-sha must be lowercase hex" 1 ;;
    esac
    [ "${#EXPECTED_SHA}" -eq 40 ] || die "--offline requires a full 40-char SHA, got ${#EXPECTED_SHA} chars" 1
    git_in_app cat-file -e "${EXPECTED_SHA}^{commit}" 2>/dev/null || die "commit $EXPECTED_SHA not present locally (offline build needs it in the checkout)" 2
    [ "$head" = "$EXPECTED_SHA" ] || die "checkout is at $head but $EXPECTED_SHA was required" 2
    TARGET_SHA="$EXPECTED_SHA"
    return 0
  fi
  target="$(git_in_app rev-parse "$GIT_REMOTE/$GIT_BRANCH")"
  if [ -n "$EXPECTED_SHA" ]; then
    [ "${#EXPECTED_SHA}" -eq 40 ] || die "--expected-sha must be a full 40-char commit" 1
    [ "$target" = "$EXPECTED_SHA" ] || die "$GIT_REMOTE/$GIT_BRANCH is $target, not the required $EXPECTED_SHA" 2
  fi
  TARGET_SHA="$target"
  if [ "$head" != "$target" ]; then
    git_in_app merge-base --is-ancestor "$head" "$target" 2>/dev/null || die "checkout $head has diverged from $GIT_REMOTE/$GIT_BRANCH $target; refusing to reset (reconcile manually)" 2
    git_in_app merge-base --is-ancestor "$target" "$head" 2>/dev/null || {
      [ -z "$(git_in_app status --porcelain)" ] || die "dirty worktree: commit or stash before updating" 2
      run git_in_app merge --ff-only "$target"
    }
    [ "$DRY_RUN" = "1" ] || TARGET_SHA="$(git_in_app rev-parse HEAD)"
  fi
  [ "$TARGET_SHA" = "$(git_in_app rev-parse HEAD)" ] || [ "$DRY_RUN" = "1" ] || die "checkout did not move to $TARGET_SHA" 2
}

check_worktree() {
  local dirty
  dirty="$(git_in_app status --porcelain)"
  if [ -n "$dirty" ]; then die "worktree is dirty; commit or stash first: $(printf "%s" "$dirty" | head -3 | tr \"\\n\" \" \")" 2; fi
}

# ---- compose ---------------------------------------------------------------------
write_overrides() {
  if [ -f "$OVERRIDE" ] && ! head -1 "$OVERRIDE" | grep -qF "managed-by: scripts/update_account_onboarding.sh"; then
    die "$OVERRIDE exists and was not generated by this script; move it away first (refusing to overwrite a hand-written file)" 3
  fi
  cp "$APP/deploy/compose.yaml" "$BASE_COMPOSE"
  if [ "$DRY_RUN" = "1" ]; then log "DRY-RUN would write $OVERRIDE"; return 0; fi
  {
    printf "%s\n" "$MARKER"
    printf "services:\n"
    printf "  onboarding:\n"
    printf "    image: %s\n" "$1"
    printf "    pull_policy: never\n"
    printf "    volumes:\n"
    printf "      - %s:/app/data\n" "$DATA"
    printf "    secrets:\n"
    printf "      - onboarding_master_key\n"
    printf "      - onboard_admin_token\n"
    printf "secrets:\n"
    printf "  onboarding_master_key:\n    file: %s/onboarding_master_key\n" "$SECRETS"
    printf "  onboard_admin_token:\n    file: %s/onboard_admin_token\n" "$SECRETS"
  } > "$OVERRIDE.tmp"
  mv -f "$OVERRIDE.tmp" "$OVERRIDE"
  sync 2>/dev/null || true
}

compose() { export ONBOARD_HOST_PORT="$HOST_PORT"; dk compose -p account-onboarding -f "$BASE_COMPOSE" -f "$OVERRIDE" --env-file /dev/null "$@"; }

compose_valid() {
  compose config --quiet 2>&1 || die "compose configuration is invalid (see docker compose config output above)" 2
}

# ---- sqlite helpers ---------------------------------------------------------------
as_root() { # run a read-only host check when the runtime directory is 0700:10001
  if [ "$(id -u)" = "0" ]; then "$@"
  elif sudo -n true >/dev/null 2>&1; then sudo -n "$@"
  else return 1; fi
}

db_file_exists() { [ -f "$1" ] || as_root test -f "$1"; }
db_file_nonempty() { [ -s "$1" ] || as_root test -s "$1"; }
db_file_owner() { stat -c %u "$1" 2>/dev/null || stat -f %u "$1" 2>/dev/null || as_root stat -c %u "$1" 2>/dev/null || as_root stat -f %u "$1"; }
db_file_mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1" 2>/dev/null || as_root stat -c %a "$1" 2>/dev/null || as_root stat -f %Lp "$1"; }

sqlite_user_version() { # MAX(schema_migrations.version); non-zero when unreadable/empty
  local f="$1"
  db_file_exists "$f" || return 1
  command -v python3 >/dev/null 2>&1 || { warn "python3 is required to read the schema version"; return 1; }
  if [ -r "$f" ]; then
    python3 - "$f" <<'PYEOF'
import sqlite3, sys
try:
    db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
    row = db.execute("SELECT MAX(version) FROM schema_migrations").fetchone()
except Exception:
    sys.exit(1)
if not row or row[0] is None:
    sys.exit(1)
print(int(row[0]))
sys.exit(0)
PYEOF
  else
    as_root python3 - "$f" <<'PYEOF'
import sqlite3, sys
try:
    db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
    row = db.execute("SELECT MAX(version) FROM schema_migrations").fetchone()
except Exception:
    sys.exit(1)
if not row or row[0] is None:
    sys.exit(1)
print(int(row[0]))
sys.exit(0)
PYEOF
  fi
}

detect_existing_container() {
  local name
  name="$(dk ps -a --filter "label=com.docker.compose.project=account-onboarding" --format "{{.Names}}" 2>/dev/null | head -1)"
  [ -z "$name" ] && name="$(dk ps -a --filter "name=^account-onboarding$" --format "{{.Names}}" 2>/dev/null | head -1)"
  printf "%s" "$name"
}

inspect_mount() { # inspect_mount <container> <path>
  dk inspect "$1" --format "{{range .Mounts}}{{if eq .Destination \"$2\"}}{{.Source}}{{end}}{{end}}" 2>/dev/null
}

check_data_safety() {
  local c current_data current_secrets dir
  # Unconditional, first: the marker is written next to the target database path before
  # the publish step, so it matters for a first install, for a database without a
  # container and for a running container alike.
  if [ -L "$DATA" ]; then
    die "$DATA is a symlink; refusing to deploy through it" 3
  fi
  if [ -L "$DATA/state.sqlite" ]; then
    die "$DATA/state.sqlite is a symlink; refusing to deploy through it" 3
  fi
  if db_file_exists "$DATA/state.sqlite.restore-in-progress"; then
    die "a restore marker is present at $DATA/state.sqlite.restore-in-progress; finish or clear the interrupted restore before deploying" 3
  fi
  if db_file_exists "$DATA/state.sqlite" && ! db_file_nonempty "$DATA/state.sqlite"; then
    die "$DATA/state.sqlite exists but is empty; refusing to deploy over a truncated database" 3
  fi
  c="$(detect_existing_container)"
  if [ -z "$c" ]; then
    log "no existing container: first install path"
    if db_file_exists "$DATA/state.sqlite"; then
      log "found a database with no container; it will be reused (not recreated)"
    fi
    return 0
  fi
  log "existing container: $c"
  current_data="$(inspect_mount "$c" /app/data)"
  current_secrets="$(inspect_mount "$c" /run/secrets/onboarding_master_key)"
  if [ -n "$current_data" ] && [ "$current_data" != "$DATA" ]; then
    die "the running container keeps its database at $current_data but this deployment points at $DATA. Refusing to start on an empty directory and call it a first install; move the data or pass --root matching the existing layout" 3
  fi
  if [ -n "$current_secrets" ]; then
    dir="$(dirname "$current_secrets")"
    [ "$dir" = "$SECRETS" ] || warn "existing container mounts secrets from $dir, this run uses $SECRETS"
  fi
}

check_schema_compat() {
  local db="$DATA/state.sqlite" v
  db_file_exists "$db" || { log "no database yet: first install"; return 0; }
  if v="$(sqlite_user_version "$db")"; then
    log "current database schema version: $v"
    if [ -f "$STATE" ] && grep -q "\"schema_version\"" "$STATE" 2>/dev/null; then
      local recorded; recorded="$(sed -n 's/.*"schema_version": *\([0-9][0-9]*\).*/\1/p' "$STATE" | head -1)"
      if [ -n "$recorded" ] && [ "$v" -gt "$recorded" ] && [ "$ALLOW_SCHEMA_FORWARD" != "1" ]; then
        warn "database schema $v is newer than the recorded $recorded (an upgrade ran before)"
      fi
    fi
  else
    warn "cannot read the schema version from $db (continuing; the backup and health checks still guard the deploy)"
  fi
}

# ---- secrets ---------------------------------------------------------------------
materialize_secrets() {
  local img="$1"
  local f
  for f in onboarding_master_key onboard_admin_token; do
    [ -f "$SECRETS_SRC/$f" ] || die "missing $SECRETS_SRC/$f (create it with: openssl rand 32 > $SECRETS_SRC/$f)" 2
  done
  run dk run --rm --user 0:0 --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
    --security-opt no-new-privileges:true \
    -v "$SECRETS_SRC:/src:ro" -v "$SECRETS:/run/secrets" \
    --entrypoint /bin/sh "$img" \
    -c "exec /app/scripts/materialize_secrets.sh --source-dir /src --target-dir /run/secrets --uid 10001 --gid 10001"
  for f in onboarding_master_key onboard_admin_token; do
    dk run --rm --user 0:0 -v "$SECRETS:/run/secrets:ro" --entrypoint /bin/ls "$img" -l "/run/secrets/$f" >/dev/null \
      || die "materialized secret $f is missing from the service mount" 2
    mode="$(dk run --rm --user 0:0 -v "$SECRETS:/run/secrets:ro" --entrypoint /usr/bin/stat "$img" -c %a "/run/secrets/$f" 2>/dev/null | tr -d '[:space:]')"
    case "$mode" in 400|600) ;; *) die "materialized secret $f has mode $mode, expected 0400" 2 ;; esac
  done
}

# ---- backup ----------------------------------------------------------------------
ops_image() { sed -n "s/^ARG GOLANG_IMAGE=//p" "$APP/Dockerfile" | head -1; }

ops_run() { # ops_run <backup|verify|restore> <args...>
  local tool="$1"; shift
  local img; img="$(ops_image)"
  [ -n "$img" ] || die "cannot read ARG GOLANG_IMAGE from the Dockerfile" 2
  run dk run --rm -e CGO_ENABLED=1 -e "GOPROXY=$GO_PROXY" \
    -v "$APP:/src:ro" -v "$BACKUPS:/backups" -v "$DATA:/data:ro" -v "$SECRETS:/secrets:ro" -v "$RUNTIME/ops:/out" \
    -w /src "$img" sh -ec "go build -buildvcs=false -o /out/ops-$tool ./internal/ops/cmd/$tool && /out/ops-$tool $*"
}

take_backup() {
  local ts dir snap man
  ts="$(date -u +%Y%m%dT%H%M%SZ)"; dir="$BACKUPS/$ts"
  if ! db_file_exists "$DATA/state.sqlite"; then log "no database to back up (first install)"; return 0; fi
  if [ "$DRY_RUN" = "1" ]; then log "DRY-RUN would snapshot $DATA/state.sqlite into $dir"; return 0; fi
  mkdir -p "$RUNTIME/ops" "$dir"
  log "taking an encrypted snapshot in $dir"
  ops_run backup -db /data/state.sqlite -out-dir "/backups/$ts" -encrypt -keyring /secrets/onboarding_master_key \
    || die "backup failed; refusing to continue" 3
  snap="$(ls -1 "$dir"/*.enc 2>/dev/null | head -1)"
  [ -n "$snap" ] || die "backup produced no encrypted snapshot in $dir" 3
  man="$snap.manifest.json"
  log "verifying $(basename "$snap")"
  ops_run verify -snapshot "/backups/$ts/$(basename "$snap")" -manifest "/backups/$ts/$(basename "$man")" \
    -keyring /secrets/onboarding_master_key || die "snapshot verification failed; refusing to continue" 3
  printf "%s" "$ts" > "$BACKUPS/LATEST"
  log "backup ok: $snap"
}

backup_is_usable() {
  local ts; ts="$(cat "$BACKUPS/LATEST" 2>/dev/null || true)"
  [ -n "$ts" ] || return 1
  ls -1 "$BACKUPS/$ts"/*.enc >/dev/null 2>&1 || return 1
  ls -1 "$BACKUPS/$ts"/*.manifest.json >/dev/null 2>&1 || return 1
}

# ---- build -----------------------------------------------------------------------
short_sha() { printf "%s" "${1:0:12}"; }

build_image() {
  local sha
  local sha="$1" ver tag
  ver="$(printf %s "${ONBOARD_BUILD_VERSION:-0.1.0}" | tr '.' '-')"
  tag="account-onboarding:local-$(short_sha "$1")-$ver"
  log "building $tag from $sha"
  local args=(--build-arg "VERSION=${ONBOARD_BUILD_VERSION:-0.1.0}" --build-arg "COMMIT=$sha" --build-arg "GOPROXY=$GO_PROXY")
  [ -n "$APT_MIRROR" ] && args+=(--build-arg "APT_MIRROR=$APT_MIRROR")
  run dk build "${args[@]}" -t "$tag" -f "$APP/Dockerfile" "$APP" || die "image build failed" 1
  IMAGE_TAG="$tag"
  if [ "$DRY_RUN" = "1" ]; then IMAGE_ID="dry-run"; return 0; fi
  IMAGE_ID="$(dk image inspect "$tag" --format "{{.Id}}")" || die "built image $tag not found" 1
  log "image id: $IMAGE_ID"
}

# ---- state -----------------------------------------------------------------------
write_pending() { # write_pending <tag> <image_id> <sha>
  [ "$DRY_RUN" = "1" ] && { log "DRY-RUN would record a pending deployment"; return 0; }
  {
    printf "{\n"
    printf "  \"tag\": \"%s\",\n" "$1"
    printf "  \"image_id\": \"%s\",\n" "$2"
    printf "  \"commit\": \"%s\",\n" "$3"
    printf "  \"started_at\": \"%s\"\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf "}\n"
  } > "$STATE.pending.tmp"
  sync "$STATE.pending.tmp" 2>/dev/null || true
  mv -f "$STATE.pending.tmp" "$STATE.pending"
}

write_state() { # write_state <image_tag> <image_id> <sha> <schema_version> [previous_image] [previous_sha] [previous_schema] [previous_image_id]
  [ "$DRY_RUN" = "1" ] && { log "DRY-RUN would record $1 $3"; return 0; }
  local ts; ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  local prev_image="${5:-}" prev_sha="${6:-}" prev_schema="${7:-}" prev_id="${8:-}"
  {
    printf "{\n"
    printf "  \"state_version\": 2,\n"
    printf "  \"image\": \"%s\",\n" "$1"
    printf "  \"image_id\": \"%s\",\n" "$2"
    printf "  \"commit\": \"%s\",\n" "$3"
    printf "  \"schema_version\": %s,\n" "${4:-0}"
    printf "  \"deployed_at\": \"%s\",\n" "$ts"
    printf "  \"root\": \"%s\",\n" "$ROOT"
    printf "  \"host_port\": %s,\n" "$HOST_PORT"
    printf "  \"previous_image\": \"%s\",\n" "$prev_image"
    printf "  \"last_good_image\": \"%s\",\n" "$prev_image"
    printf "  \"last_good_commit\": \"%s\",\n" "$prev_sha"
    printf "  \"last_good_schema_version\": %s,\n" "${prev_schema:-0}"
    printf "  \"current_image\": \"%s\",\n" "$1"
    printf "  \"current_image_id\": \"%s\",\n" "$2"
    printf "  \"previous_image_id\": \"%s\",\n" "$prev_id"
    printf "  \"previous_commit\": \"%s\",\n" "$prev_sha"
    printf "  \"previous_schema_version\": %s\n" "${prev_schema:-0}"
    printf "}\n"
  } > "$STATE.tmp"
  sync "$STATE.tmp" 2>/dev/null || true
  mv -f "$STATE.tmp" "$STATE"
  rm -f "$STATE.pending"
  sync 2>/dev/null || true
}

write_attempt() { # write_attempt <status>; detail globals are set by do_deploy
  [ "$DRY_RUN" = "1" ] && return 0
  local status="$1" ts
  ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  {
    printf "{\n"
    printf "  \"attempt_id\": \"%s\",\n" "$ATTEMPT_ID"
    printf "  \"status\": \"%s\",\n" "$status"
    printf "  \"operation\": \"deploy\",\n"
    printf "  \"requested_commit\": \"%s\",\n" "$TARGET_SHA"
    printf "  \"image_tag\": \"%s\",\n" "$IMAGE_TAG"
    printf "  \"expected_image_id\": \"%s\",\n" "$IMAGE_ID"
    printf "  \"previous_image\": \"%s\",\n" "$ATTEMPT_PREV_IMAGE"
    printf "  \"previous_image_id\": \"%s\",\n" "$ATTEMPT_PREV_ID"
    printf "  \"previous_commit\": \"%s\",\n" "$ATTEMPT_PREV_SHA"
    printf "  \"previous_schema_version\": %s,\n" "${ATTEMPT_PREV_SCHEMA:-0}"
    printf "  \"snapshot\": \"%s\",\n" "$ATTEMPT_SNAPSHOT"
    printf "  \"created_at\": \"%s\",\n" "$ts"
    printf "  \"last_transition\": \"%s\"\n" "$ts"
    printf "}\n"
  } > "$ATTEMPT.tmp"
  mv -f "$ATTEMPT.tmp" "$ATTEMPT"
  sync 2>/dev/null || true
}

attempt_field() { sed -n "s/.*\"$1\": *\"\([^\"]*\)\".*/\1/p" "$ATTEMPT" 2>/dev/null | head -1; }


state_field() { sed -n "s/.*\"$1\": *\"\([^\"]*\)\".*/\1/p" "$STATE" 2>/dev/null | head -1; }
state_num()   { sed -n "s/.*\"$1\": *\([0-9][0-9]*\).*/\1/p" "$STATE" 2>/dev/null | head -1; }

# ---- health ----------------------------------------------------------------------
api_probe() { # api_probe <path> -> http code, token taken from a file so it never reaches argv
  local path="$1" token="" candidate code
  # The materialized directory is intentionally 0700 and owned by the runtime
  # UID, so the deploying user may not be able to read it. The source token is
  # the same credential and is readable by the deployer; prefer it, then fall
  # back to the materialized copy.
  for candidate in "$SECRETS_SRC/onboard_admin_token" "$SECRETS/onboard_admin_token"; do
    if [ -r "$candidate" ]; then token="$candidate"; break; fi
  done
  if [ -n "$token" ]; then
    local hdr; hdr="$(mktemp)"
    printf "Authorization: Bearer %s\n" "$(cat "$token")" > "$hdr"; chmod 600 "$hdr"
    code="$(curl -s -o /dev/null -m 5 -w "%{http_code}" -H @"$hdr" "http://127.0.0.1:$HOST_PORT$path" || true)"
    rm -f "$hdr"
  else
    code="$(curl -s -o /dev/null -m 5 -w "%{http_code}" "http://127.0.0.1:$HOST_PORT$path" || true)"
  fi
  printf "%s" "$code"
}

wait_healthy() { # 0 healthy, non-zero otherwise; never exits the process
  local code ready c restarts
  code=""
  for _ in $(seq 1 60); do
    code="$(api_probe /healthz)"
    [ "$code" = "200" ] && break
    sleep 2
  done
  if [ "$code" != "200" ]; then warn "liveness never came up on 127.0.0.1:$HOST_PORT/healthz (last http=$code)"; return 3; fi
  ready="$(api_probe /readyz)"
  if [ "$ready" != "200" ]; then warn "liveness is up but readiness returned $ready"; return 4; fi
  c="$(detect_existing_container)"
  if [ -z "$c" ]; then warn "no container is running for this compose project"; return 5; fi
  restarts="$(dk inspect "$c" --format "{{.RestartCount}}" 2>/dev/null || echo 0)"
  if [ "${restarts:-0}" -ge 5 ]; then warn "container restarted $restarts times (crash loop)"; return 6; fi
  log "healthy: liveness 200, readiness 200, restart count $restarts"
  return 0
}

# ---- actions ---------------------------------------------------------------------
do_preflight() {
  require_readable_data
  detect_docker
  [ -d "$APP/.git" ] || die "$APP is not a git checkout" 2
  check_worktree
  git_fetch
  resolve_sha
  log "target commit: $TARGET_SHA"
  check_data_safety
  check_schema_compat
  [ -f "$SECRETS_SRC/onboarding_master_key" ] || die "missing $SECRETS_SRC/onboarding_master_key" 2
  [ -f "$SECRETS_SRC/onboard_admin_token" ] || die "missing $SECRETS_SRC/onboard_admin_token" 2
  local avail; avail="$(df -Pk "$ROOT" | awk "NR==2{print \$4}")"
  [ "${avail:-0}" -gt 524288 ] || die "less than 512 MB free under $ROOT" 2
  log "preflight ok (free ${avail}K, port 127.0.0.1:$HOST_PORT)"
}

do_build() {
  do_preflight
  build_image "$TARGET_SHA"
  write_overrides "$IMAGE_TAG"
  if [ "$DRY_RUN" != "1" ]; then
    compose_valid
  fi
  log "build-only complete: $IMAGE_TAG ($IMAGE_ID) from $TARGET_SHA"
  log "nothing was deployed; run --deploy to change the running service"
}

do_backup() {
  require_readable_data
  detect_docker
  take_backup
  backup_is_usable || ! db_file_exists "$DATA/state.sqlite" || die "no usable snapshot after the backup step" 3
  log "backup-only complete"
}

do_deploy() {
  do_preflight
  local prev_image prev_sha prev_schema prev_id
  prev_image="$(state_field image || true)"; prev_sha="$(state_field commit || true)"
  prev_schema="$(state_num schema_version || true)"; prev_id="$(state_field image_id || true)"
  take_backup
  build_image "$TARGET_SHA"
  write_overrides "$IMAGE_TAG"
  if [ "$DRY_RUN" != "1" ]; then
    prepare_deploy
    compose_valid
    materialize_secrets "$IMAGE_TAG"
  fi
  [ "$DRY_RUN" = "1" ] && { log "DRY-RUN would start the service"; return 0; }
  ATTEMPT_ID="deploy-$(date -u +%Y%m%dT%H%M%SZ)-$$"
  ATTEMPT_PREV_IMAGE="$prev_image"; ATTEMPT_PREV_ID="$prev_id"; ATTEMPT_PREV_SHA="$prev_sha"
  ATTEMPT_PREV_SCHEMA="${prev_schema:-0}"; ATTEMPT_SNAPSHOT="$(cat "$BACKUPS/LATEST" 2>/dev/null || true)"
  write_attempt mutation_started || die "cannot persist deployment intent; refusing to change the service" 3
  compose up -d --no-build --no-deps --force-recreate onboarding
  if ! wait_healthy; then
    write_attempt failed || true
    dump_diagnostics
    warn "deployment is not healthy; keeping the database and evidence, the container is left as-is for inspection"
    warn "to go back: $PROG --rollback (binary rollback; database restore is a separate, destructive decision)"
    exit 3
  fi
  local schema; schema="$(sqlite_user_version "$DATA/state.sqlite" || echo 0)"
  write_state "$IMAGE_TAG" "$IMAGE_ID" "$TARGET_SHA" "$schema" "$prev_image" "$prev_sha" "$prev_schema" "$prev_id"
  write_attempt committed || warn "deployment is healthy but the attempt commit marker could not be written; run --status"
  log "deployed $IMAGE_TAG from $TARGET_SHA (schema $schema)"
}

do_rollback() {
  local rp
  rp="$(state_num host_port || true)"
  if [ -n "$rp" ]; then HOST_PORT="$rp"; fi
  detect_docker
  [ -f "$STATE" ] || die "no deployment state; nothing to roll back to" 1
  local prev_image prev_sha recorded_schema current_schema
  prev_image="$(state_field last_good_image || true)"
  [ -n "$prev_image" ] || prev_image="$(state_field previous_image || true)"
  prev_sha="$(state_field last_good_commit || true)"
  [ -n "$prev_sha" ] || prev_sha="$(state_field previous_commit || true)"
  recorded_schema="$(state_num previous_schema_version || true)"
  [ -n "$prev_image" ] || die "no previous image recorded; roll back by restoring a snapshot with the documented procedure" 1
  current_schema="$(sqlite_user_version "$DATA/state.sqlite" || true)"
  if [ -z "$current_schema" ]; then
    die "cannot read the schema version from $DATA/state.sqlite; refusing to roll back blindly. Restore the matching snapshot with internal/ops/cmd/restore instead" 3
  fi
  if [ -n "$recorded_schema" ] && [ "$current_schema" -gt "$recorded_schema" ] && [ "$ALLOW_SCHEMA_FORWARD" != "1" ]; then
    die "database schema $current_schema is newer than the recorded $recorded_schema for the previous release; running the old binary on a migrated database is not a safe rollback. Restore the matching snapshot with internal/ops/cmd/restore during a maintenance window (destructive) or pass --allow-schema-forward after verifying compatibility" 3
  fi
  log "rolling back to $prev_image ($prev_sha)"
  if [ "$DRY_RUN" != "1" ]; then
    prepare_deploy
    take_backup
    backup_is_usable || die "no usable snapshot after the rollback protection backup; refusing to change the service" 3
  fi
  write_overrides "$prev_image"
  compose_valid
  materialize_secrets "$prev_image"
  write_pending "$IMAGE_TAG" "$IMAGE_ID" "$TARGET_SHA"
  [ "$DRY_RUN" = "1" ] || compose up -d --no-build --no-deps --force-recreate onboarding
  [ "$DRY_RUN" = "1" ] && { log "DRY-RUN would roll back"; return 0; }
  wait_healthy || die "rollback target $prev_image is not healthy either; the database is untouched, inspect the logs" 3
  write_state "$prev_image" "$(dk image inspect "$prev_image" --format "{{.Id}}")" "$prev_sha" "$current_schema" "" "" ""
  log "rolled back to $prev_image"
}

do_reconcile() {
  detect_docker
  [ -f "$STATE" ] || die "no deployment state; nothing to reconcile" 1
  [ -f "$ATTEMPT" ] || die "no deployment attempt; nothing to reconcile" 1
  local status expected actual c schema
  status="$(attempt_field status)"
  [ "$status" = "mutation_started" ] || { log "attempt is $status; no reconcile needed"; return 0; }
  expected="$(attempt_field expected_image_id)"
  [ -n "$expected" ] || die "attempt has no expected image id; refusing to infer it" 3
  c="$(detect_existing_container)"
  [ -n "$c" ] || die "no running container for the persisted attempt" 3
  actual="$(dk inspect "$c" --format "{{.Image}}" 2>/dev/null || true)"
  if [ "$actual" != "$expected" ]; then
    die "IMAGE_MISMATCH: persisted attempt expected $expected but container is $actual; no state was written" 4
  fi
  wait_healthy || die "container image matches the attempt but health/readiness did not pass; no state was written" 3
  schema="$(sqlite_user_version "$DATA/state.sqlite" || true)"
  [ -n "$schema" ] || die "cannot read schema during reconcile; no state was written" 3
  ATTEMPT_ID="$(attempt_field attempt_id)"; ATTEMPT_PREV_IMAGE="$(attempt_field previous_image)"
  ATTEMPT_PREV_ID="$(attempt_field previous_image_id)"; ATTEMPT_PREV_SHA="$(attempt_field previous_commit)"
  ATTEMPT_PREV_SCHEMA="$(sed -n 's/.*"previous_schema_version": *\([0-9][0-9]*\).*/\1/p' "$ATTEMPT" | head -1)"
  ATTEMPT_SNAPSHOT="$(attempt_field snapshot)"
  TARGET_SHA="$(attempt_field requested_commit)"; IMAGE_TAG="$(attempt_field image_tag)"; IMAGE_ID="$expected"
  write_state "$IMAGE_TAG" "$expected" "$TARGET_SHA" "$schema" \
    "$ATTEMPT_PREV_IMAGE" "$ATTEMPT_PREV_SHA" "${ATTEMPT_PREV_SCHEMA:-0}" "$ATTEMPT_PREV_ID"
  write_attempt committed || die "state written but attempt marker failed" 3
  log "reconciled persisted attempt $ATTEMPT_ID to healthy image $expected"
}

do_status() {
  local rp
  rp="$(state_num host_port || true)"
  if [ -n "$rp" ]; then HOST_PORT="$rp"; fi
  if [ -f "$STATE" ]; then
    log "recorded deployment state:"
    cat "$STATE"
  else
    log "no deployment state recorded under $DEPLOY"
  fi
  detect_docker 2>/dev/null || { warn "docker unavailable: live state unknown"; return 0; }
  local c; c="$(detect_existing_container)"
  if [ -z "$c" ]; then log "no container for the account-onboarding compose project"; return 0; fi
  if [ -f "$STATE.pending" ]; then log "pending deployment (unfinished switch):"; cat "$STATE.pending"; fi
  log "container: $c"
  dk inspect "$c" --format "{{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} restarts={{.RestartCount}} image={{.Config.Image}} id={{.Image}}"
  log "healthz: $(api_probe /healthz)  readyz: $(api_probe /readyz)"
  if [ -f "$ATTEMPT" ]; then
    local astatus aexpected aid
    astatus="$(attempt_field status)"; aexpected="$(attempt_field expected_image_id)"; aid="$(attempt_field attempt_id)"
    log "attempt: id=$aid status=$astatus expected_image=$aexpected"
    if [ "$astatus" = "mutation_started" ]; then
      local actual; actual="$(dk inspect "$c" --format "{{.Image}}" 2>/dev/null || true)"
      if [ -n "$aexpected" ] && [ "$actual" != "$aexpected" ]; then
        warn "RECOVERY_REQUIRED: IMAGE_MISMATCH (actual=$actual expected=$aexpected); run --reconcile only after review"
      else
        warn "INCOMPLETE_ATTEMPT: image matches but LastKnownGood was not committed; run --reconcile"
      fi
    fi
  fi
  if db_file_exists "$DATA/state.sqlite"; then log "database schema: $(sqlite_user_version "$DATA/state.sqlite" || echo unknown)"; fi
}

ensure_data_owner() {
  # install -d is atomic and idempotent; plain chown after chmod proved unreliable on
  # some hosts, so the directory is created/owned in one step and then verified.
  if [ "$(id -u)" = "0" ]; then
    install -d -o 10001 -g 10001 -m 0700 "$DATA" || die "cannot prepare $DATA as root" 2
  elif sudo -n true >/dev/null 2>&1; then
    sudo -n install -d -o 10001 -g 10001 -m 0700 "$DATA" || die "cannot prepare $DATA via sudo" 2
  else
    die "need root or passwordless sudo to give $DATA to uid 10001; refusing to start a container that cannot write its database" 2
  fi
}

require_readable_data() {
  # The runtime dir is intentionally private. A non-root deployer may need the
  # host root/sudo boundary to inspect it; permission failure is not evidence
  # that the database is absent.
  if [ -d "$DATA" ] && [ ! -x "$DATA" ]; then
    as_root test -x "$DATA" 2>/dev/null || die "$DATA is not readable by the current user; run with root or passwordless sudo (refusing to treat the existing database as absent)" 3
  fi
  local f
  for f in state.sqlite state.sqlite-wal state.sqlite-shm state.sqlite.restore-in-progress; do
    if db_file_exists "$DATA/$f" && [ ! -r "$DATA/$f" ]; then
      as_root test -r "$DATA/$f" 2>/dev/null || die "$DATA/$f exists but is not readable by the current user; refusing to treat it as absent" 3
    fi
  done
}

harden_secret_source() {
  local f mode owner
  mkdir -p "$SECRETS_SRC"
  chmod 0700 "$SECRETS_SRC" || die "cannot restrict $SECRETS_SRC to 0700" 2
  for f in onboarding_master_key onboard_admin_token; do
    [ -f "$SECRETS_SRC/$f" ] || continue
    if [ -L "$SECRETS_SRC/$f" ]; then die "$SECRETS_SRC/$f is a symlink; refusing" 2; fi
    mode="$(stat -c %a "$SECRETS_SRC/$f" 2>/dev/null || stat -f %Lp "$SECRETS_SRC/$f")"
    case "$mode" in
      400|600) ;;
      444|644)
        # readable by every local user: tighten it, the materializer runs as root
        chmod 0400 "$SECRETS_SRC/$f" || die "cannot restrict $SECRETS_SRC/$f to 0400 (run as its owner or with sudo)" 2 ;;
      *) die "$SECRETS_SRC/$f has mode $mode; expected 0400 or 0600" 2 ;;
    esac
  done
}

prepare_deploy() { # only for a real deploy or rollback, never build-only or preflight
  local owner mode f
  harden_secret_source
  require_readable_data
  ensure_data_owner
  owner="$(db_file_owner "$DATA")"
  [ "$owner" = "10001" ] || die "$DATA is owned by $owner, not 10001; refusing to start" 2
  mode="$(db_file_mode "$DATA")"
  case "$mode" in 700|750) ;; *) die "$DATA mode $mode is too permissive; expected 0700" 2 ;; esac
  for f in state.sqlite state.sqlite-wal state.sqlite-shm; do
    db_file_exists "$DATA/$f" || continue
    owner="$(db_file_owner "$DATA/$f")"
    [ "$owner" = "10001" ] || die "$DATA/$f is owned by $owner, not 10001; fix the ownership explicitly (this script does not rewrite existing data files)" 3
  done
  log "data directory ready for uid 10001 (mode $mode)"
}

dump_diagnostics() {
  local c
  c="$(detect_existing_container || true)"
  log "--- diagnostics ---"
  compose ps -a 2>&1 || true
  if [ -n "$c" ]; then
    dk inspect "$c" --format "exit={{.State.ExitCode}} restarts={{.RestartCount}} status={{.State.Status}} ports={{.HostConfig.PortBindings}}" 2>&1 || true
    dk logs --tail 60 "$c" 2>&1 || true
  fi
  ls -ln "$DATA" 2>&1 || true
  ls -ln "$SECRETS" 2>&1 || true
  compose config 2>&1 | sed -n '1,40p' || true
  log "--- end diagnostics ---"
}

log "$PROG action=$ACTION root=$ROOT offline=$OFFLINE dry_run=$DRY_RUN"
case "$ACTION" in
  preflight) do_preflight ;;
  build) do_build ;;
  deploy) do_deploy ;;
  backup) do_backup ;;
  rollback) do_rollback ;;
  status) do_status ;;
  reconcile) do_reconcile ;;
  *) die "unhandled action $ACTION" 1 ;;
esac
