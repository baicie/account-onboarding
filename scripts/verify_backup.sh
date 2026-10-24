#!/usr/bin/env bash
# R4 operator script: verify a backup snapshot and its manifest before relying
# on either. Local files only.
#
# Usage:
#   scripts/verify_backup.sh SNAPSHOT [MANIFEST] [--keyring FILE]
#
# Environment:
#   ONBOARD_BACKUP_FILE   snapshot path (alternative to the positional argument)
#   ONBOARD_KEYRING       keyring (required when SNAPSHOT is AES-256-GCM encrypted)
#
# Exit codes: 0 verified, 1 verification failure, 2 usage.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SNAPSHOT="${ONBOARD_BACKUP_FILE:-}"
MANIFEST=""
KEYRING="${ONBOARD_KEYRING:-}"

usage() {
	cat >&2 <<'EOF'
usage: scripts/verify_backup.sh SNAPSHOT [MANIFEST] [--keyring FILE]

Checks: manifest envelope, snapshot name, size, sha256, schema version,
PRAGMA integrity_check, PRAGMA foreign_key_check and per-table row census.
MANIFEST defaults to <SNAPSHOT>.manifest.json. For *.sqlite.enc snapshots the
keyring decrypts and verifies the plaintext sha256/size before integrity_check;
a wrong/missing key fails closed.
EOF
}

POSITIONAL=()
while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--keyring)
		[ $# -ge 2 ] || { echo "verify_backup: --keyring needs a value" >&2; exit 2; }
		KEYRING="$2"; shift 2 ;;
	*) POSITIONAL+=("$1"); shift ;;
	esac
done
if [ ${#POSITIONAL[@]} -ge 1 ]; then SNAPSHOT="${POSITIONAL[0]}"; fi
if [ ${#POSITIONAL[@]} -ge 2 ]; then MANIFEST="${POSITIONAL[1]}"; fi
if [ -z "$SNAPSHOT" ]; then
	usage
	exit 2
fi
case "$SNAPSHOT" in
*://*) echo "verify_backup: refusing non-local snapshot path: $SNAPSHOT" >&2; exit 2 ;;
esac
[ -f "$SNAPSHOT" ] || { echo "verify_backup: snapshot not found: $SNAPSHOT" >&2; exit 1; }

ARGS=(-snapshot "$SNAPSHOT")
[ -n "$MANIFEST" ] && ARGS+=(-manifest "$MANIFEST")
[ -n "$KEYRING" ] && ARGS+=(-keyring "$KEYRING")
go run ./internal/ops/cmd/verify "${ARGS[@]}"
echo "verify_backup: OK"
