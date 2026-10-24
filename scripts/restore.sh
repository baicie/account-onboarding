#!/usr/bin/env bash
# R4 operator script: verify a snapshot and restore it to a target database.
#
# Safety:
#   * The snapshot checksum/integrity/row census are verified first.
#   * A staged copy is decrypted with the supplied keyring before the target is
#     replaced; a wrong key fails closed and leaves the target untouched.
#   * Replacing an existing target requires --force.
#   * Replacing the LIVE service database additionally requires CONFIRM_RESTORE=1
#     and the service must be stopped.
#
# Usage:
#   CONFIRM_RESTORE=1 scripts/restore.sh --snapshot FILE --target PATH --keyring FILE
#                                       [--manifest FILE] [--live-db PATH] [--force]
#
# The same --keyring serves both purposes:
#   * it decrypts an encrypted snapshot (*.sqlite.enc) before staging, and
#   * it proves every encrypted secret row still decrypts after the restore.
#
# Environment (fallbacks):
#   ONBOARD_BACKUP_FILE, ONBOARD_DB_PATH, ONBOARD_KEYRING, ONBOARD_FORCE_RESTORE=1
#
# Exit codes: 0 restored, 1 failure/refusal, 2 usage.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SNAPSHOT="${ONBOARD_BACKUP_FILE:-}"
TARGET="${ONBOARD_DB_PATH:-}"
MANIFEST=""
KEYRING="${ONBOARD_KEYRING:-}"
LIVE_DB="${ONBOARD_DB_PATH:-}"
FORCE="${ONBOARD_FORCE_RESTORE:-0}"

usage() {
	cat >&2 <<'EOF'
usage: scripts/restore.sh --snapshot FILE --target PATH --keyring FILE
                          [--manifest FILE] [--live-db PATH] [--force]

The keyring is mandatory for encrypted (*.sqlite.enc) snapshots and whenever the
snapshot contains encrypted secrets: it decrypts the payload and proves that
restored secrets still decrypt. Stop the service before restoring over its live
database.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--snapshot) SNAPSHOT="$2"; shift 2 ;;
	--target) TARGET="$2"; shift 2 ;;
	--manifest) MANIFEST="$2"; shift 2 ;;
	--keyring) KEYRING="$2"; shift 2 ;;
	--live-db) LIVE_DB="$2"; shift 2 ;;
	--force) FORCE=1; shift ;;
	*) echo "restore: unknown argument: $1" >&2; usage; exit 2 ;;
	esac
done

[ -n "$SNAPSHOT" ] && [ -n "$TARGET" ] || { usage; exit 2; }
case "$SNAPSHOT" in
*://*) echo "restore: refusing non-local snapshot path" >&2; exit 2 ;;
esac
[ -f "$SNAPSHOT" ] || { echo "restore: snapshot not found: $SNAPSHOT" >&2; exit 1; }
[ -n "$KEYRING" ] || { echo "restore: --keyring (or ONBOARD_KEYRING) is required for the decryption proof" >&2; exit 2; }
[ -f "$KEYRING" ] || { echo "restore: keyring not found: $KEYRING" >&2; exit 1; }

if [ "$FORCE" = "1" ] && [ "$TARGET" = "$LIVE_DB" ] && [ "${CONFIRM_RESTORE:-0}" != "1" ]; then
	echo "restore: target is the live database; set CONFIRM_RESTORE=1 and stop the service first" >&2
	exit 1
fi
if [ -f "$TARGET-wal" ] && [ -s "$TARGET-wal" ]; then
	echo "restore: WARNING: $TARGET-wal is non-empty; the service may still be running" >&2
fi

ARGS=(-snapshot "$SNAPSHOT" -target "$TARGET" -keyring "$KEYRING")
[ -n "$MANIFEST" ] && ARGS+=(-manifest "$MANIFEST")
[ -n "$LIVE_DB" ] && ARGS+=(-live-db "$LIVE_DB")
[ "$FORCE" = "1" ] && ARGS+=(-force)

go run ./internal/ops/cmd/restore "${ARGS[@]}"
echo "restore: OK (integrity, counts and decryption verified)"
