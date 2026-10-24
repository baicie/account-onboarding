#!/usr/bin/env bash
# tests/sandbox/down.sh — stop the LOCAL SANDBOX (Keycloak + Mailpit).
#
#   ./down.sh           stop both services, keep all runtime data (DB, mail)
#   ./down.sh --purge   stop both services and delete runtime data
#
# Idempotent: running it when nothing is up exits 0. Only processes that are
# provably ours (pid file, listening on our loopback ports, command line points
# into <repo>/.sandbox or is the Keycloak/Mailpit runtime) are ever signalled.
set -euo pipefail

# shellcheck source=lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

PURGE=0
for arg in "$@"; do
  case "$arg" in
    --purge) PURGE=1 ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) die "unknown argument: $arg" ;;
  esac
done

# Collect pids that belong to one component.
# $1 = component name, $2 = pid file, $3 = port, $4.. = cmdline substrings
component_pids() {
  local name="$1" pidfile="$2" port="$3"; shift 3
  local pats=("$@")
  local pids=() p
  p="$(read_pid "$pidfile")"
  if pid_alive "$p" && pid_matches "$p" "${pats[@]}"; then pids+=("$p"); fi
  for p in $(listener_pids "$port"); do
    if pid_matches "$p" "${pats[@]}"; then pids+=("$p"); fi
  done
  # stale java/mailpit that no longer listens but still runs from our sandbox dir
  for p in $(pgrep -f "$SB_DIR" 2>/dev/null || true); do
    [ "$p" = "$$" ] && continue
    if pid_matches "$p" "${pats[@]}"; then pids+=("$p"); fi
  done
  printf '%s\n' "${pids[@]:-}" | awk 'NF' | sort -u
}

# stop_component <name> <port> <pidfile> <cmdline-substring>...
stop_component() {
  local name="$1" port="$2" pidfile="$3"; shift 3
  local pats=("$@")
  local pids
  pids="$(component_pids "$name" "$pidfile" "$port" "${pats[@]}")"

  if [ -z "$pids" ] && ! is_listening "$port"; then
    ok "$name is not running"
    rm_pid "$pidfile"
    return 0
  fi
  if [ -z "$pids" ]; then
    warn "$name: port $port is held by [$(port_owner "$port")] which is not a sandbox process — leaving it alone"
    rm_pid "$pidfile"
    return 0
  fi

  log "stopping $name (pids: $(printf '%s ' $pids))"
  # shellcheck disable=SC2086
  kill -TERM $pids 2>/dev/null || true
  local waited=0
  while [ "$waited" -lt 45 ]; do
    local alive=0
    for p in $pids; do pid_alive "$p" && alive=1; done
    if [ "$alive" -eq 0 ] && ! is_listening "$port"; then
      ok "$name stopped"
      rm_pid "$pidfile"
      return 0
    fi
    sleep 1; waited=$((waited + 1))
  done
  warn "$name did not stop within 45s; sending SIGKILL"
  # shellcheck disable=SC2086
  kill -KILL $pids 2>/dev/null || true
  sleep 2
  if is_listening "$port"; then
    die "$name still listening on port $port after SIGKILL — investigate manually (lsof -nP -iTCP:$port -sTCP:LISTEN)"
  fi
  ok "$name killed"
  rm_pid "$pidfile"
}

main() {
  hr
  log "LOCAL SANDBOX shutdown"
  # Keycloak first so in-flight login cookies stop hitting Mailpit.
  stop_component keycloak "$KC_PORT" "$KC_PID_FILE" keycloak quarkus
  stop_component mailpit "$MP_WEB_PORT" "$MP_PID_FILE" mailpit
  stop_component mailpit-smtp "$MP_SMTP_PORT" "$MP_PID_FILE" mailpit

  if [ "$PURGE" -eq 1 ]; then
    log "purging runtime data (realm DB, mail store, logs)"
    rm -rf "$SB_DATA/keycloak" "$SB_DATA/mailpit" "$KC_HOME/data" "$SB_LOGS"
    mkdir -p "$SB_LOGS"
    ok "runtime data purged (artifacts and generated test credentials kept)"
  fi

  hr
  ok "LOCAL SANDBOX IS DOWN"
  if [ "$PURGE" -eq 0 ]; then
    echo "  runtime data kept in $SB_DATA (start again with tests/sandbox/up.sh)"
  fi
  hr
}

main
