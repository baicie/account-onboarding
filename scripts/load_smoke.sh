#!/usr/bin/env bash
# R4 operator script: offline load/concurrency smoke (5 concurrent + 30
# sequential by default). Runs entirely in-process against fake providers and a
# loopback httptest server: no network, no real provider, no Docker.
#
# It leaves evidence under $ONBOARD_LOAD_DIR (default: a new mktemp dir):
#   db/state.sqlite     completed runs, steps, events, encrypted tests secrets
#   db/test-master.key  test-only keyring for restore/rotation drills (0600)
#   service.log         redacted JSON service logs
#
# Usage:
#   scripts/load_smoke.sh
#   ONBOARD_LOAD_CONCURRENCY=5 ONBOARD_LOAD_SEQUENTIAL=30 scripts/load_smoke.sh
#
# Exit codes: 0 pass, 1 failures/duplicates/timeout, 2 usage.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CONCURRENCY="${ONBOARD_LOAD_CONCURRENCY:-5}"
SEQUENTIAL="${ONBOARD_LOAD_SEQUENTIAL:-30}"
if [ -z "${ONBOARD_LOAD_DIR:-}" ]; then
	ART_DIR="$(mktemp -d "${TMPDIR:-/tmp}/onboard-r4-load.XXXXXX")"
else
	ART_DIR="$ONBOARD_LOAD_DIR"
	mkdir -p "$ART_DIR"
fi
chmod 700 "$ART_DIR" 2>/dev/null || true
DB_DIR="$ART_DIR/db"
LOG_FILE="$ART_DIR/service.log"
KEY_FILE="$DB_DIR/test-master.key"
mkdir -p "$DB_DIR"

echo "load_smoke: concurrency=$CONCURRENCY sequential=$SEQUENTIAL artifacts=$ART_DIR"
go run ./internal/ops/cmd/loadsmoke \
	-concurrency "$CONCURRENCY" \
	-sequential "$SEQUENTIAL" \
	-work-dir "$DB_DIR" \
	-log-file "$LOG_FILE" \
	-keyring-out "$KEY_FILE"
echo "load_smoke: PASS (p50/p95 and duplicate checks in the JSON above)"
echo "load_smoke: evidence db=$DB_DIR/state.sqlite log=$LOG_FILE keyring=$KEY_FILE"

echo "load_smoke: scanning generated service log and event rows for secrets"
go run ./internal/ops/cmd/redactscan -file "$LOG_FILE" -event-db "$DB_DIR/state.sqlite"
echo "load_smoke: OK (logs and persisted events are clean)"
