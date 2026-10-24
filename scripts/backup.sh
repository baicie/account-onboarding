#!/usr/bin/env bash
# R4 operator script: create a consistent, checksummed SQLite snapshot of a
# live WAL database. Loopback/local files only; no network access, no Docker.
#
# Usage:
#   scripts/backup.sh [--db PATH] [--out-dir DIR] [--encrypt --keyring FILE]
#
# Environment:
#   ONBOARD_DB_PATH        live database (default ./data/state.sqlite; container:
#                          /app/data/state.sqlite)
#   ONBOARD_BACKUP_DIR     snapshot directory (default ./backups, created 0700)
#   ONBOARD_BACKUP_ENCRYPT 1 = encrypt the snapshot at rest (AES-256-GCM)
#   ONBOARD_KEYRING        keyring file used with --encrypt (required); also
#                          used for the post-backup verification of .sqlite.enc
#
# Exit codes: 0 success, 1 failure, 2 usage.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DB="${ONBOARD_DB_PATH:-./data/state.sqlite}"
OUT_DIR="${ONBOARD_BACKUP_DIR:-./backups}"
ENCRYPT="${ONBOARD_BACKUP_ENCRYPT:-0}"
KEYRING="${ONBOARD_KEYRING:-}"

usage() {
	cat >&2 <<'EOF'
usage: scripts/backup.sh [--db PATH] [--out-dir DIR] [--encrypt --keyring FILE]

Creates <out-dir>/state-<UTC>.sqlite (or .sqlite.enc with --encrypt) plus a
.manifest.json sidecar with sha256, schema version and row census. The snapshot
is private (0600) and self-verified. --encrypt seals it with AES-256-GCM using
the keyring current version (key_version and plaintext_sha256 are recorded in
the manifest); a wrong/missing keyring fails before anything is published.
Stop nothing: VACUUM INTO runs against the live WAL database without blocking it.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--db)
		[ $# -ge 2 ] || { echo "backup: --db needs a value" >&2; exit 2; }
		DB="$2"; shift 2 ;;
	--out-dir)
		[ $# -ge 2 ] || { echo "backup: --out-dir needs a value" >&2; exit 2; }
		OUT_DIR="$2"; shift 2 ;;
	--encrypt) ENCRYPT=1; shift ;;
	--keyring)
		[ $# -ge 2 ] || { echo "backup: --keyring needs a value" >&2; exit 2; }
		KEYRING="$2"; shift 2 ;;
	*) echo "backup: unknown argument: $1" >&2; usage; exit 2 ;;
	esac
done

case "$DB" in
*://*) echo "backup: refusing non-local database path: $DB" >&2; exit 2 ;;
esac
[ -f "$DB" ] || { echo "backup: database not found: $DB" >&2; exit 1; }
ARGS=(-db "$DB" -out-dir "$OUT_DIR")
if [ "$ENCRYPT" = "1" ]; then
	if [ -z "$KEYRING" ]; then
		echo "backup: --encrypt requires --keyring (or ONBOARD_KEYRING)" >&2
		exit 2
	fi
	[ -f "$KEYRING" ] || { echo "backup: keyring not found: $KEYRING" >&2; exit 1; }
	ARGS+=(-encrypt -keyring "$KEYRING")
elif [ -n "$KEYRING" ]; then
	echo "backup: NOTE: ONBOARD_KEYRING is set but --encrypt is not; snapshot will be plaintext" >&2
fi

mkdir -p "$OUT_DIR"
chmod 700 "$OUT_DIR" 2>/dev/null || true

echo "backup: source=$DB out_dir=$OUT_DIR encrypt=$ENCRYPT"
go run ./internal/ops/cmd/backup "${ARGS[@]}"

SNAP="$(ls -t "$OUT_DIR"/*.sqlite "$OUT_DIR"/*.sqlite.enc 2>/dev/null | head -n 1 || true)"
if [ -z "$SNAP" ]; then
	echo "backup: no snapshot published in $OUT_DIR" >&2
	exit 1
fi
echo "backup: verifying published snapshot $SNAP"
if [ -n "$KEYRING" ]; then
	go run ./internal/ops/cmd/verify -snapshot "$SNAP" -keyring "$KEYRING"
else
	go run ./internal/ops/cmd/verify -snapshot "$SNAP"
fi
echo "backup: OK (snapshot, manifest and sha256 verified)"
