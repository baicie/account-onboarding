#!/usr/bin/env bash
# R4 operator script: scan logs and/or persisted event rows for leaked secrets.
# Exit 2 means findings (fail the release gate); exit 1 is a scanner error.
#
# Usage:
#   scripts/redact_scan.sh --file /var/log/onboardd.log \
#                          [--event-db /app/data/state.sqlite] \
#                          [--known-secrets-file FILE]
#
# With no --file/--event-db, scans ./data/state.sqlite if it exists.
# Findings never echo the matched value.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

FILES=()
EVENT_DB=""
KNOWN_FILE="${ONBOARD_KNOWN_SECRETS_FILE:-}"

usage() {
	cat >&2 <<'EOF'
usage: scripts/redact_scan.sh [--file PATH]... [--event-db PATH] [--known-secrets-file PATH]

Detects JWTs, Bearer/Basic credentials, password=/state=/token= assignments,
long base64-like token shapes, PEM private keys, known secret values and event
payload keys outside the allowlist. Exit 2 = findings.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--file) FILES+=("$2"); shift 2 ;;
	--event-db) EVENT_DB="$2"; shift 2 ;;
	--known-secrets-file) KNOWN_FILE="$2"; shift 2 ;;
	*) echo "redact_scan: unknown argument: $1" >&2; usage; exit 2 ;;
	esac
done

if [ ${#FILES[@]} -eq 0 ] && [ -z "$EVENT_DB" ]; then
	if [ -f "./data/state.sqlite" ]; then
		EVENT_DB="./data/state.sqlite"
		echo "redact_scan: no arguments; scanning ./data/state.sqlite"
	else
		usage
		exit 2
	fi
fi

ARGS=()
for f in "${FILES[@]}"; do
	case "$f" in
	*://*) echo "redact_scan: refusing non-local file: $f" >&2; exit 2 ;;
	esac
	[ -f "$f" ] || { echo "redact_scan: file not found: $f" >&2; exit 1; }
	ARGS+=(-file "$f")
done
if [ -n "$EVENT_DB" ]; then
	[ -f "$EVENT_DB" ] || { echo "redact_scan: database not found: $EVENT_DB" >&2; exit 1; }
	ARGS+=(-event-db "$EVENT_DB")
fi
if [ -n "$KNOWN_FILE" ]; then
	ARGS+=(-known-secrets-file "$KNOWN_FILE")
fi

go run ./internal/ops/cmd/redactscan "${ARGS[@]}"
echo "redact_scan: OK (no sensitive-data findings)"
