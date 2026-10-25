#!/usr/bin/env bash
# Cloudflare live preflight. DEFAULT IS DRY RUN: it validates configuration and
# prints the exact plan; it performs no network calls. --live is refused unless
# a dedicated test address allowlist and explicit operator confirmation are set.
# R10 does NOT create/delete a real mailbox or send real mail.
set -euo pipefail
LIVE=0 SELF_TEST=0 BASE_URL="" TEST_ADDRESS="" MAX_REQUESTS=10 MAX_SECONDS=120
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) LIVE=0; shift ;;
    --live) LIVE=1; shift ;;
    --self-test) SELF_TEST=1; shift ;;
    --base-url) BASE_URL="$2"; shift 2 ;;
    --test-address) TEST_ADDRESS="$2"; shift 2 ;;
    --max-requests) MAX_REQUESTS="$2"; shift 2 ;;
    --max-seconds) MAX_SECONDS="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
if [ "$SELF_TEST" = "1" ]; then
  # No network, no credentials: prove --live fails closed without allowlist.
  if ONBOARD_CF_TEST_ADDRESSES="" scripts/cloudflare_preflight.sh --live --test-address x 2>/dev/null; then
    echo "SELF-TEST FAIL: --live accepted without allowlist" >&2; exit 1
  fi
  echo "SELF-TEST PASS: live mode fails closed without allowlist"
  exit 0
fi
ADDR_FILE="${ONBOARD_CF_TEST_ADDRESSES_FILE:-}"
ALLOWED="${ONBOARD_CF_TEST_ADDRESSES:-}"
if [ -n "$ADDR_FILE" ] && [ -r "$ADDR_FILE" ]; then
  ALLOWED="$(tr '\n' ' ' < "$ADDR_FILE")"
fi
[ -n "$TEST_ADDRESS" ] || { echo "ERROR: --test-address is required"; exit 2; }
[ "$MAX_REQUESTS" -ge 1 ] && [ "$MAX_REQUESTS" -le 50 ] || { echo "ERROR: request budget 1..50"; exit 2; }
[ "$MAX_SECONDS" -ge 10 ] && [ "$MAX_SECONDS" -le 900 ] || { echo "ERROR: time budget 10..900"; exit 2; }
case " $ALLOWED " in
  *" $TEST_ADDRESS "*) ;;
  *) echo "ERROR: test address is not in the dedicated allowlist"; exit 2 ;;
esac
if [ "$LIVE" = "1" ]; then
  [ -n "$BASE_URL" ] || { echo "ERROR: --base-url required for live"; exit 2; }
  [ -n "${ONBOARD_ADMIN_TOKEN_FILE:-}" ] && [ -r "$ONBOARD_ADMIN_TOKEN_FILE" ] || {
    echo "ERROR: ONBOARD_ADMIN_TOKEN_FILE must be readable for live"; exit 2; }
  case "$BASE_URL" in https://*) ;; *) echo "ERROR: live base URL must be HTTPS"; exit 2 ;; esac
  echo "LIVE PREFLIGHT (read-only only; no create/send/delete in R10)"
  echo "address=$TEST_ADDRESS budget_requests=$MAX_REQUESTS budget_seconds=$MAX_SECONDS"
  echo "REFUSED: mailbox create, mail send, delete, DNS/Worker/D1 changes are not implemented by this preflight."
  exit 3
fi
cat <<PLAN
DRY RUN — no network calls, no credentials read.
  test_address:       $TEST_ADDRESS
  allowlisted:        yes
  plan:               validate config -> health/readiness -> list existing external
                      operations -> STOP (manual authorization required before
                      create/send/delete)
  request_budget:     $MAX_REQUESTS
  time_budget_sec:    $MAX_SECONDS
  fail_closed:        --live requires allowlist, HTTPS base URL and readable
                      admin token file; mailbox create/send/delete are NOT
                      implemented without a separately reviewed change window.
PLAN
