#!/usr/bin/env bash
# Admin CLI for external-operation reconciliation. Uses the admin bearer token
# from a file; it never prints the token. No secrets are accepted or echoed.
#
#   ONBOARD_ADMIN_TOKEN_FILE=/secure/admin.token \
#   ONBOARD_BASE_URL=https://onboarding.example.com \
#   scripts/ops_external.sh list [--status OUTCOME_UNKNOWN] [--provider cloudflare] \
#       [--run-id RUN] [--limit 50] [--cursor CURSOR]
#
#   ONBOARD_ADMIN_TOKEN_FILE=... scripts/ops_external.sh resolve \
#       --id OP_ID --version 3 --resolution RESOURCE_CONFIRMED \
#       --reason "verified mailbox exists in provider console" \
#       [--evidence ticket-123]
#
# `resolve` does NOT delete remote resources and does NOT trigger automatic
# retries; it only records an audited operator decision.
set -euo pipefail
BASE="${ONBOARD_BASE_URL:-http://127.0.0.1:8788}"
TOKEN_FILE="${ONBOARD_ADMIN_TOKEN_FILE:-}"
[ -n "$TOKEN_FILE" ] && [ -r "$TOKEN_FILE" ] || {
  echo "ERROR: ONBOARD_ADMIN_TOKEN_FILE must point to a readable admin token file" >&2
  exit 2
}
TOKEN="$(tr -d '\r\n' < "$TOKEN_FILE")"
[ -n "$TOKEN" ] || { echo "ERROR: admin token file is empty" >&2; exit 2; }
AUTH=(-H "Authorization: Bearer $TOKEN")
case "${1:-}" in
  list)
    shift
    Q=()
    while [ $# -gt 0 ]; do
      case "$1" in
        --status) Q+=("status=$2"); shift 2 ;;
        --provider) Q+=("provider=$2"); shift 2 ;;
        --run-id) Q+=("run_id=$2"); shift 2 ;;
        --limit) Q+=("limit=$2"); shift 2 ;;
        --cursor) Q+=("cursor=$2"); shift 2 ;;
        *) echo "unknown list option: $1" >&2; exit 2 ;;
      esac
    done
    URL="$BASE/v1/ops/external"
    if [ "${#Q[@]}" -gt 0 ]; then
      JOINED="$(printf '%s&' "${Q[@]}")"
      URL="$URL?${JOINED%&}"
    fi
    OUT="$(curl -sS -m 15 -w '\n%{http_code}' "${AUTH[@]}" "$URL")"
    CODE="$(printf '%s' "$OUT" | tail -1)"
    BODY="$(printf '%s' "$OUT" | sed '$d')"
    [ "$CODE" = "200" ] || { echo "ERROR: HTTP $CODE: $BODY" >&2; exit 1; }
    printf '%s' "$BODY" | python3 -m json.tool
    ;;
  resolve)
    shift
    ID="" VERSION="" RES="" REASON="" EVIDENCE=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --id) ID="$2"; shift 2 ;;
        --version) VERSION="$2"; shift 2 ;;
        --resolution) RES="$2"; shift 2 ;;
        --reason) REASON="$2"; shift 2 ;;
        --evidence) EVIDENCE="$2"; shift 2 ;;
        *) echo "unknown resolve option: $1" >&2; exit 2 ;;
      esac
    done
    [ -n "$ID" ] && [ -n "$VERSION" ] && [ -n "$RES" ] && [ -n "$REASON" ] || {
      echo "ERROR: --id --version --resolution --reason are required" >&2; exit 2
    }
    BODY="$(python3 - "$RES" "$REASON" "$VERSION" "$EVIDENCE" <<'PY'
import json,sys
print(json.dumps({"resolution":sys.argv[1],"reason":sys.argv[2],
                  "expected_version":int(sys.argv[3]),"evidence_reference":sys.argv[4]}))
PY
)"
    OUT="$(curl -sS -m 15 -w '\n%{http_code}' "${AUTH[@]}" -H 'Content-Type: application/json' \
      -X POST "$BASE/v1/ops/external/$ID/resolve" -d "$BODY")"
    CODE="$(printf '%s' "$OUT" | tail -1)"
    RBODY="$(printf '%s' "$OUT" | sed '$d')"
    [ "$CODE" = "200" ] || { echo "ERROR: HTTP $CODE: $RBODY" >&2; exit 1; }
    printf '%s' "$RBODY" | python3 -m json.tool
    ;;
  *)
    echo "usage: $0 list [options] | resolve --id ... --version ... --resolution ... --reason ..." >&2
    exit 2
    ;;
esac
