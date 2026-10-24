#!/usr/bin/env bash
# R4 operator script: rotate the AES-256-GCM master key.
#
# Workflow:
#   1. scripts/rotate_key.sh                 -> DRY RUN (default, read-only)
#   2. stop the service
#   3. CONFIRM_ROTATE=1 scripts/rotate_key.sh --apply
#
# Step 3 generates <KEYRING>.next-<UTC> containing the old versions plus a new
# current key, re-encrypts every secrets row in one transaction, then atomically
# swaps the keyring file. A missing key version rolls the transaction back.
#
# Usage:
#   scripts/rotate_key.sh [--db PATH] [--keyring PATH] [--apply]
#
# Environment:
#   ONBOARD_DB_PATH   database (default ./data/state.sqlite)
#   ONBOARD_KEYRING   keyring file (JSON envelope; required)
#   CONFIRM_ROTATE=1  required for --apply
#
# Exit codes: 0 success/dry-run OK, 1 failure/refusal, 2 usage.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DB="${ONBOARD_DB_PATH:-./data/state.sqlite}"
KEYRING="${ONBOARD_KEYRING:-}"
APPLY=0

usage() {
	cat >&2 <<'EOF'
usage: scripts/rotate_key.sh [--db PATH] [--keyring PATH] [--apply]

Default is a read-only dry run. --apply requires the service to be stopped and
CONFIRM_ROTATE=1. The keyring must be a JSON envelope; a raw 32-byte key is
treated as version 1 by the keyring tool.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--db) DB="$2"; shift 2 ;;
	--keyring) KEYRING="$2"; shift 2 ;;
	--apply) APPLY=1; shift ;;
	*) echo "rotate_key: unknown argument: $1" >&2; usage; exit 2 ;;
	esac
done

[ -n "$KEYRING" ] || { echo "rotate_key: --keyring (or ONBOARD_KEYRING) is required" >&2; exit 2; }
[ -f "$KEYRING" ] || { echo "rotate_key: keyring not found: $KEYRING" >&2; exit 1; }
[ -f "$DB" ] || { echo "rotate_key: database not found: $DB" >&2; exit 1; }

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
NEXT="$KEYRING.next-$STAMP"
echo "rotate_key: current keyring: $KEYRING"
go run ./internal/ops/cmd/keyring show -in "$KEYRING"
echo "rotate_key: writing next keyring (old versions retained): $NEXT"
go run ./internal/ops/cmd/keyring rotate -in "$KEYRING" -out "$NEXT"
chmod 600 "$NEXT"

echo "rotate_key: dry run against $DB"
go run ./internal/ops/cmd/rotate -db "$DB" -keyring "$NEXT" -dry-run

if [ "$APPLY" != "1" ]; then
	echo "rotate_key: DRY RUN complete. Stop the service, then re-run with --apply CONFIRM_ROTATE=1"
	exit 0
fi
if [ "${CONFIRM_ROTATE:-0}" != "1" ]; then
	echo "rotate_key: refusing --apply without CONFIRM_ROTATE=1 (service must be stopped)" >&2
	exit 1
fi
if [ -s "$DB-wal" ]; then
	echo "rotate_key: $DB-wal is non-empty; stop the service before applying the rotation" >&2
	exit 1
fi

echo "rotate_key: applying rotation"
go run ./internal/ops/cmd/rotate -db "$DB" -keyring "$NEXT"

PREV="$KEYRING.prev-$STAMP"
mv "$KEYRING" "$PREV"
mv "$NEXT" "$KEYRING"
chmod 600 "$KEYRING"
echo "rotate_key: keyring swapped; previous keyring kept at $PREV (0600)."
echo "rotate_key: restart the service so it loads the new current version."
