#!/usr/bin/env bash
# tests/sandbox/reset.sh — clean re-bootstrap of the LOCAL SANDBOX.
#
#   ./reset.sh           stop services, wipe runtime data (Keycloak H2 + Mailpit
#                        mail store + logs), start again, re-import realm
#   ./reset.sh --full    also delete downloaded artifacts and generated
#                        credentials, then download + install from scratch
#   ./reset.sh --keep-creds  wipe runtime data only (default behaviour, kept for
#                        symmetry / older call sites)
#
# Everything it deletes lives under <repo>/.sandbox. Nothing outside it is touched.
set -euo pipefail

# shellcheck source=lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

FULL=0
for arg in "$@"; do
  case "$arg" in
    --full) FULL=1 ;;
    --keep-creds) ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) die "unknown argument: $arg" ;;
  esac
done

main() {
  require_cmds curl tar lsof jq openssl
  hr
  log "LOCAL SANDBOX reset (full=$FULL)"

  bash "$SB_LIB_DIR/down.sh" --purge

  if [ "$FULL" -eq 1 ]; then
    log "removing downloaded artifacts, Keycloak distribution and generated credentials"
    rm -rf "$SB_DIST" "$SB_BIN" "$SB_DATA" "$KC_HOME" "$SB_SECRETS" "$SB_TMP"
    ok "artifacts + credentials removed"
  fi

  mkdirs
  log "starting fresh sandbox"
  exec bash "$SB_LIB_DIR/up.sh"
}

main
