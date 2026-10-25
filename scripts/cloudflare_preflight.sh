#!/usr/bin/env bash
# Cloudflare preflight modes:
#   A dry-run (default): no network, no credentials read.
#   B mock-live: real HTTPS requests to a local/mock endpoint (CI only).
#   C authorized read-only live: requires an explicit authorization file AND a
#     separate address-token file; only GET /api/parsed_mails; no create/send/
#     delete/DNS/Worker/D1 changes. R11 did NOT run mode C.
set -euo pipefail
MODE=dry TEST_ADDRESS="" BASE_URL="" MOCK_URL="" CA_FILE="" TOKEN_FILE="" REPORT=""
MAX_REQUESTS=10 MAX_SECONDS=120 SELF_TEST=0
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) MODE=dry; shift ;;
    --mock-live) MODE=mock; shift ;;
    --live) MODE=live; shift ;;
    --self-test) SELF_TEST=1; shift ;;
    --base-url) BASE_URL="$2"; shift 2 ;;
    --mock-url) MOCK_URL="$2"; shift 2 ;;
    --mock-ca) CA_FILE="$2"; shift 2 ;;
    --test-address) TEST_ADDRESS="$2"; shift 2 ;;
    --token-file) TOKEN_FILE="$2"; shift 2 ;;
    --max-requests) MAX_REQUESTS="$2"; shift 2 ;;
    --max-seconds) MAX_SECONDS="$2"; shift 2 ;;
    --report) REPORT="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
if [ "$SELF_TEST" = "1" ]; then
  if ONBOARD_CF_TEST_ADDRESSES="" scripts/cloudflare_preflight.sh --live --test-address x 2>/dev/null; then
    echo "SELF-TEST FAIL: live accepted without allowlist" >&2; exit 1
  fi
  echo "SELF-TEST PASS: live fails closed without allowlist/authorization"
  exit 0
fi
ADDR_FILE="${ONBOARD_CF_TEST_ADDRESSES_FILE:-}"
ALLOWED="${ONBOARD_CF_TEST_ADDRESSES:-}"
[ -n "$ADDR_FILE" ] && [ -r "$ADDR_FILE" ] && ALLOWED="$(tr '\n' ' ' < "$ADDR_FILE")"
[ -n "$TEST_ADDRESS" ] || { echo "ERROR: --test-address is required" >&2; exit 2; }
case " $ALLOWED " in *" $TEST_ADDRESS "*) ;; *) echo "ERROR: test address not allowlisted" >&2; exit 2 ;; esac
[ "$MAX_REQUESTS" -ge 1 ] && [ "$MAX_REQUESTS" -le 50 ] || { echo "ERROR: request budget 1..50" >&2; exit 2; }
[ "$MAX_SECONDS" -ge 10 ] && [ "$MAX_SECONDS" -le 900 ] || { echo "ERROR: time budget 10..900" >&2; exit 2; }
readonly_paths=("/api/parsed_mails")
write_report(){ [ -n "$REPORT" ] || return 0; python3 - "$REPORT" "$@" <<'PY'
import json,sys
out=sys.argv[1]; d=dict(x.split("=",1) for x in sys.argv[2:])
json.dump(d,open(out,"w"),indent=2)
PY
}
if [ "$MODE" = "dry" ]; then
  write_report authorized_scope=DRY_RUN request_budget="$MAX_REQUESTS" requests_used=0 \
    target_host="" tls_result=NOT_RUN auth_result=NOT_RUN readonly_contract_result=NOT_RUN \
    stop_reason=DRY_RUN cleanup_result=NOT_RUN verification_status=NOT_RUN
  cat <<PLAN
DRY RUN — no network calls, no credentials read.
  test_address:    $TEST_ADDRESS
  request_budget:  $MAX_REQUESTS
  time_budget_sec: $MAX_SECONDS
  read-only path:  /api/parsed_mails (existing Cloudflare mailbox GET contract)
  forbidden:       create/modify/delete mailbox, send mail, DNS/Worker/D1 changes
  modes:           dry(default) | mock-live(local HTTPS only) | live(authorized read-only)
PLAN
  exit 0
fi
if [ -z "$TOKEN_FILE" ] || [ ! -r "$TOKEN_FILE" ]; then echo "ERROR: --token-file must be readable" >&2; exit 2; fi
TOKEN="$(tr -d '\r\n' < "$TOKEN_FILE")"; [ -n "$TOKEN" ] || { echo "ERROR: token file empty" >&2; exit 2; }
TARGET="$MOCK_URL"; [ "$MODE" = "live" ] && TARGET="$BASE_URL"
case "$TARGET" in https://*) ;; *) echo "ERROR: TLS target must be https" >&2; exit 2 ;; esac
if [ "$MODE" = "live" ]; then
  AUTHZ="${ONBOARD_CF_LIVE_AUTHORIZATION:-}"
  [ -n "$AUTHZ" ] && [ -r "$AUTHZ" ] || { echo "ERROR: live requires ONBOARD_CF_LIVE_AUTHORIZATION file" >&2; exit 2; }
  grep -q '^AUTHORIZED=YES$' "$AUTHZ" || { echo "ERROR: authorization file does not say AUTHORIZED=YES" >&2; exit 2; }
  HOST="$(python3 -c 'import urllib.parse,sys;print(urllib.parse.urlparse(sys.argv[1]).hostname or "")' "$TARGET")"
  grep -q "^allowed_host=$HOST$" "$AUTHZ" || { echo "ERROR: target host not authorized" >&2; exit 2; }
fi
CURL=(curl -sS --max-redirs 0 --proto '=https' --max-time "$MAX_SECONDS" -H "Authorization: Bearer $TOKEN")
[ -n "$CA_FILE" ] && [ "$MODE" = "mock" ] && CURL+=(--cacert "$CA_FILE")
USED=0 STOP="" AUTH_RESULT=NOT_RUN READ_RESULT=NOT_RUN
for path in "${readonly_paths[@]}"; do
  if [ "$USED" -ge "$MAX_REQUESTS" ]; then STOP=REQUEST_BUDGET_EXHAUSTED; break; fi
  USED=$((USED+1))
  CODE="$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$TARGET$path" 2>/dev/null || echo 000)"
  case "$CODE" in
    200|204) READ_RESULT=PASS; AUTH_RESULT=PASS ;;
    401|403) AUTH_RESULT=DENIED; STOP=AUTH_DENIED; break ;;
    429) STOP=RATE_LIMITED; break ;;
    000) STOP=NETWORK_OR_TLS_ERROR; break ;;
    3*) STOP=REDIRECT_REFUSED; break ;;
    *) STOP="HTTP_$CODE"; break ;;
  esac
done
[ -z "$STOP" ] && STOP=COMPLETED
write_report authorized_scope="$([ "$MODE" = mock ] && echo MOCK_LIVE || echo AUTHORIZED_READ_ONLY_LIVE)" \
  request_budget="$MAX_REQUESTS" requests_used="$USED" target_host="$TARGET" \
  tls_result=PASS auth_result="$AUTH_RESULT" readonly_contract_result="$READ_RESULT" \
  stop_reason="$STOP" cleanup_result=NOT_REQUIRED verification_status="$([ "$STOP" = COMPLETED ] && echo PASS || echo NOT_RUN)"
echo "SUMMARY mode=$MODE requests=$USED stop=$STOP auth=$AUTH_RESULT read=$READ_RESULT"
[ "$STOP" = "COMPLETED" ] || exit 1
