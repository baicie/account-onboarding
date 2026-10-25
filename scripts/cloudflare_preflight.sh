#!/usr/bin/env bash
# Cloudflare preflight modes:
#   A dry-run (default): no network, no credentials read.
#   B mock-live: real HTTPS requests to a local/mock endpoint (CI only).
#   C authorized read-only live: requires an explicit authorization file AND a
#     separate address-token file; only GET /api/parsed_mails; no create/send/
#     delete/DNS/Worker/D1 changes. R12 has NOT run mode C.
set -euo pipefail
umask 077
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
[ -n "$TEST_ADDRESS" ] || { echo "ERROR: --test-address is required" >&2; exit 2; }
if ! python3 - "$TEST_ADDRESS" "$ADDR_FILE" <<'PY'
import os, re, sys
address, allow_file = sys.argv[1:]
if not re.fullmatch("[^@\\s]+@[^@\\s]+", address):
    sys.exit(2)
allowed = os.environ.get("ONBOARD_CF_TEST_ADDRESSES", "").split()
if allow_file:
    try:
        with open(allow_file, encoding="utf-8") as f:
            allowed.extend(line.strip() for line in f if line.strip())
    except OSError:
        sys.exit(2)
sys.exit(0 if address in allowed else 2)
PY
then
  echo "ERROR: test address is invalid or not allowlisted" >&2
  exit 2
fi
case "$MAX_REQUESTS" in ''|*[!0-9]*) echo "ERROR: request budget must be an integer" >&2; exit 2 ;; esac
case "$MAX_SECONDS" in ''|*[!0-9]*) echo "ERROR: time budget must be an integer" >&2; exit 2 ;; esac
[ "$MAX_REQUESTS" -ge 1 ] && [ "$MAX_REQUESTS" -le 50 ] || { echo "ERROR: request budget 1..50" >&2; exit 2; }
[ "$MAX_SECONDS" -ge 10 ] && [ "$MAX_SECONDS" -le 900 ] || { echo "ERROR: time budget 10..900" >&2; exit 2; }
readonly_paths=("/api/parsed_mails")
write_report(){ [ -n "$REPORT" ] || return 0; python3 - "$REPORT" "$@" <<'PY'
import json, os, sys
out = os.path.abspath(sys.argv[1])
data = dict(x.split("=", 1) for x in sys.argv[2:])
os.makedirs(os.path.dirname(out), exist_ok=True)
flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0)
fd = os.open(out, flags, 0o600)
os.fchmod(fd, 0o600)
with os.fdopen(fd, "w", encoding="utf-8") as stream:
    json.dump(data, stream, indent=2, sort_keys=True)
    stream.write(chr(10))
PY
}
if [ "$MODE" = "dry" ]; then
  write_report authorized_scope=DRY_RUN request_method=GET request_path=/api/parsed_mails \
    request_budget="$MAX_REQUESTS" requests_used=0 target_host="" target_port="" \
    tls_result=NOT_RUN auth_result=NOT_RUN readonly_contract_result=NOT_RUN \
    response_schema_valid=NOT_RUN redaction_result=NOT_RUN stop_reason=DRY_RUN \
    cleanup_result=NOT_RUN verification_status=NOT_RUN
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
if ! python3 - "$TOKEN_FILE" <<'PY'
import os, stat, sys
path = sys.argv[1]
try:
    st = os.stat(path, follow_symlinks=False)
except OSError:
    sys.exit(2)
if not stat.S_ISREG(st.st_mode) or (st.st_mode & 0o077) or st.st_size < 1 or st.st_size > 8192:
    sys.exit(2)
PY
then echo "ERROR: token file must be a regular file with mode 0600 and bounded size" >&2; exit 2; fi
TOKEN="$(python3 - "$TOKEN_FILE" <<'PY'
import sys
try:
    data = open(sys.argv[1], "rb").read()
except OSError:
    sys.exit(2)
if data.endswith(b"\n"):
    data = data[:-1]
if not data or b"\n" in data or b"\r" in data or len(data) > 8192:
    sys.exit(2)
try:
    token = data.decode("utf-8")
except UnicodeDecodeError:
    sys.exit(2)
sys.stdout.write(token)
PY
)" || { echo "ERROR: token file is empty or contains invalid line breaks" >&2; exit 2; }
[ -n "$TOKEN" ] || { echo "ERROR: token file empty" >&2; exit 2; }
TARGET="$MOCK_URL"
if [ "$MODE" = "live" ]; then TARGET="$BASE_URL"; fi
if [ "$MODE" != "mock" ] && [ "$MODE" != "live" ]; then echo "ERROR: unsupported mode" >&2; exit 2; fi
TARGET_META="$(python3 - "$TARGET" "$MODE" <<'PY'
import sys
from urllib.parse import urlsplit
raw, mode = sys.argv[1:]
try:
    p = urlsplit(raw)
    host = (p.hostname or "").lower().rstrip(".")
    port = p.port or 443
except ValueError:
    sys.exit(2)
if p.scheme.lower() != "https" or not host or p.username or p.password or p.path not in ("", "/") or p.query or p.fragment:
    sys.exit(2)
if mode == "live" and port != 443:
    sys.exit(2)
if mode == "mock" and host not in ("127.0.0.1", "localhost", "::1"):
    sys.exit(2)
print(host)
print(port)
PY
)" || { echo "ERROR: target must be a valid HTTPS origin within the mode's host/port policy" >&2; exit 2; }
HOST="$(printf '%s\n' "$TARGET_META" | sed -n '1p')"
PORT="$(printf '%s\n' "$TARGET_META" | sed -n '2p')"
if [ "$MODE" = "live" ]; then
  AUTHZ="${ONBOARD_CF_LIVE_AUTHORIZATION:-}"
  [ -n "$AUTHZ" ] && [ -r "$AUTHZ" ] || { echo "ERROR: live requires ONBOARD_CF_LIVE_AUTHORIZATION file" >&2; exit 2; }
  if ! python3 - "$AUTHZ" "$HOST" "$TEST_ADDRESS" "$MAX_REQUESTS" "$MAX_SECONDS" <<'PY'
import datetime as dt, os, stat, sys
path, host, address, requested_requests, requested_seconds = sys.argv[1:]
try:
    st = os.stat(path, follow_symlinks=False)
    if not stat.S_ISREG(st.st_mode) or (st.st_mode & 0o077) or st.st_size > 16384:
        sys.exit(2)
    fields = {}
    with open(path, encoding="utf-8") as stream:
        for raw in stream:
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            key, sep, value = line.partition("=")
            if not sep or not key or key in fields:
                sys.exit(2)
            fields[key] = value
    required = {"AUTHORIZED", "allowed_host", "allowed_address", "allowed_operation",
                "window_start_utc", "window_end_utc", "max_requests", "max_seconds"}
    if set(fields) != required or fields["AUTHORIZED"] != "YES":
        sys.exit(2)
    if fields["allowed_host"].lower().rstrip(".") != host:
        sys.exit(2)
    if fields["allowed_address"] != address or fields["allowed_operation"] != "READ_ONLY_LIST":
        sys.exit(2)
    start = dt.datetime.fromisoformat(fields["window_start_utc"].replace("Z", "+00:00"))
    end = dt.datetime.fromisoformat(fields["window_end_utc"].replace("Z", "+00:00"))
    now = dt.datetime.now(dt.timezone.utc)
    if start.tzinfo is None or end.tzinfo is None or start.utcoffset() != dt.timedelta(0) or end.utcoffset() != dt.timedelta(0):
        sys.exit(2)
    if end <= start or now < start or now > end:
        sys.exit(2)
    if int(requested_requests) < 1 or int(requested_seconds) < 1:
        sys.exit(2)
    if int(requested_requests) > int(fields["max_requests"]) or int(requested_seconds) > int(fields["max_seconds"]):
        sys.exit(2)
    if now + dt.timedelta(seconds=int(requested_seconds)) > end:
        sys.exit(2)
except (OSError, ValueError, OverflowError):
    sys.exit(2)
sys.exit(0)
PY
  then echo "ERROR: authorization file is invalid, out of window, or does not match the exact live scope/budget" >&2; exit 2; fi
fi
TMP_PARENT="${TMPDIR:-/tmp}"
TMP_DIR="$(mktemp -d "$TMP_PARENT/cf-preflight.XXXXXX")"
cleanup_temp() { if [ -n "${TMP_DIR:-}" ] && [ -d "$TMP_DIR" ]; then rm -rf "$TMP_DIR"; fi; TMP_DIR=""; }
trap cleanup_temp EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
chmod 700 "$TMP_DIR"
HEADER_FILE="$TMP_DIR/headers"
BODY_FILE="$TMP_DIR/body"
printf 'Authorization: Bearer %s\n' "$TOKEN" > "$HEADER_FILE"
chmod 600 "$HEADER_FILE"
unset TOKEN
CURL=(curl -sS --connect-timeout 10 --max-redirs 0 --proto '=https' --max-time "$MAX_SECONDS" --max-filesize 1048576 --header "@$HEADER_FILE" -H 'Accept: application/json')
if [ -n "$CA_FILE" ] && [ "$MODE" = "mock" ]; then CURL+=(--cacert "$CA_FILE"); fi
USED=0 STOP="" AUTH_RESULT=NOT_VERIFIED READ_RESULT=NOT_RUN TLS_RESULT=NOT_VERIFIED SCHEMA_RESULT=NOT_RUN
CODE=000 CONTENT_TYPE="" BYTES=0 STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)" START_EPOCH="$(date +%s)"
REQUEST_PATH="/api/parsed_mails?limit=1&offset=0"
if [ "$USED" -ge "$MAX_REQUESTS" ]; then
  STOP=REQUEST_BUDGET_EXHAUSTED
else
  USED=$((USED+1))
  REQUEST_URL="$TARGET$REQUEST_PATH"
  set +e
  META="$("${CURL[@]}" -o "$BODY_FILE" -w '%{http_code}|%{content_type}' "$REQUEST_URL" 2>/dev/null)"
  CURL_RC=$?
  set -e
  CODE="${META%%|*}"
  CONTENT_TYPE="${META#*|}"
  [ "$CONTENT_TYPE" = "$META" ] && CONTENT_TYPE=""
  [ -n "$CODE" ] || CODE=000
  [ -f "$BODY_FILE" ] && BYTES="$(wc -c < "$BODY_FILE" | tr -d ' ')" || BYTES=0
  if [ "$CURL_RC" -eq 0 ]; then
    TLS_RESULT=PASS
  elif [ "$CURL_RC" -eq 60 ] || [ "$CURL_RC" -eq 77 ]; then
    TLS_RESULT=FAIL
  fi
  if [ "$BYTES" -gt 1048576 ] || [ "$CURL_RC" -eq 63 ]; then
    STOP=RESPONSE_TOO_LARGE
  elif [ "$CURL_RC" -eq 28 ]; then
    STOP=TIMEOUT
  elif [ "$CURL_RC" -ne 0 ]; then
    STOP=NETWORK_OR_TLS_ERROR
  else
    case "$CODE" in
      401|403) AUTH_RESULT=DENIED; STOP=AUTH_DENIED ;;
      429) STOP=RATE_LIMITED ;;
      3*) STOP=REDIRECT_REFUSED ;;
      2[0-9][0-9])
        AUTH_RESULT=ACCEPTED
        if SCHEMA_RESULT="$(python3 - "$BODY_FILE" "$CONTENT_TYPE" <<'PY'
import json, os, sys
path, content_type = sys.argv[1:]
if os.path.getsize(path) > 1048576:
    print("BODY_TOO_LARGE"); sys.exit(2)
media = content_type.split(";", 1)[0].strip().lower()
if not (media == "application/json" or (media.startswith("application/") and media.endswith("+json"))):
    print("CONTENT_TYPE_INVALID"); sys.exit(2)
if os.path.getsize(path) == 0:
    print("EMPTY_BODY"); sys.exit(2)
try:
    with open(path, "r", encoding="utf-8") as stream:
        data = json.load(stream)
except (OSError, UnicodeError, json.JSONDecodeError):
    print("INVALID_JSON"); sys.exit(2)
if isinstance(data, list):
    items = data
elif isinstance(data, dict):
    aliases = {"results", "parsed_mails", "mails", "messages", "data", "items", "list"}
    items = None
    for key, value in data.items():
        if key.lower() in aliases:
            if value is None:
                items = []
                break
            if isinstance(value, list):
                items = value
                break
    if items is None:
        print("SCHEMA_INVALID"); sys.exit(2)
else:
    print("SCHEMA_INVALID"); sys.exit(2)
if any(not (isinstance(item, (dict, str)) or item is None) for item in items):
    print("ITEM_TYPE_INVALID"); sys.exit(2)
print("PASS")
PY
        ); then
          READ_RESULT=PASS
          AUTH_RESULT=ACCEPTED
          STOP=COMPLETED
        else
          STOP="RESPONSE_${SCHEMA_RESULT:-SCHEMA_INVALID}"
          READ_RESULT=FAIL
        fi
        ;;
      000) STOP=NETWORK_OR_TLS_ERROR ;;
      *) STOP="HTTP_$CODE" ;;
    esac
  fi
fi
END_EPOCH="$(date +%s)"
ENDED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
DURATION="$((END_EPOCH-START_EPOCH))"
EVIDENCE_HASH="$(python3 - "$MODE" "$HOST" "$REQUEST_PATH" "$CODE" "$CONTENT_TYPE" "$BYTES" "$TLS_RESULT" "$READ_RESULT" <<'PY'
import hashlib, sys
print(hashlib.sha256("|".join(sys.argv[1:]).encode("utf-8")).hexdigest())
PY
)"
cleanup_temp
[ -z "$STOP" ] && STOP=COMPLETED
if [ "$STOP" = "COMPLETED" ]; then
  if [ "$MODE" = "live" ]; then VERIFICATION=PASS; else VERIFICATION=MOCK_PASS; fi
else
  VERIFICATION=FAIL
fi
write_report authorized_scope="$([ "$MODE" = mock ] && echo MOCK_LIVE || echo AUTHORIZED_READ_ONLY_LIVE)" \
  request_method=GET request_path="$REQUEST_PATH" request_budget="$MAX_REQUESTS" requests_used="$USED" \
  target_host="$HOST" target_port="$PORT" target_address_reference=ALLOWLISTED_TEST_ADDRESS \
  start_utc="$STARTED_AT" end_utc="$ENDED_AT" duration_seconds="$DURATION" http_status="$CODE" \
  content_type="$CONTENT_TYPE" response_bytes="$BYTES" tls_result="$TLS_RESULT" auth_result="$AUTH_RESULT" \
  response_schema_valid="$SCHEMA_RESULT" readonly_contract_result="$READ_RESULT" redaction_result=NOT_RUN \
  stop_reason="$STOP" cleanup_result=TEMP_FILES_REMOVED evidence_hash="$EVIDENCE_HASH" \
  verification_status="$VERIFICATION"
echo "SUMMARY mode=$MODE requests=$USED status=$CODE stop=$STOP tls=$TLS_RESULT auth=$AUTH_RESULT read=$READ_RESULT"
[ "$STOP" = "COMPLETED" ]
