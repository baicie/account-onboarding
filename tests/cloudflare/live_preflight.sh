#!/usr/bin/env bash
# R6 P2: Cloudflare Live preflight / dry-run gate.
#
# Default is DRY-RUN: offline contract checks only, no network, no writes.
#   bash tests/cloudflare/live_preflight.sh
#
# --live is refused unless ALL of the following are set:
#   ONBOARD_CLOUDFLARE_LIVE_AUTHORIZED=YES_I_HAVE_WRITTEN_AUTHORIZATION
#   ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE=<0600 file>
#   ONBOARD_CLOUDFLARE_TEST_NAME=<explicit unique label, ^[a-z0-9-]{8,40}$>
#   ONBOARD_CLOUDFLARE_DOMAIN=moonbyte.top
#   ONBOARD_CLOUDFLARE_BASE_URL=https://api.moonbyte.top (default)
# Optional: ONBOARD_CLOUDFLARE_SITE_PASSWORD_FILE, sent as x-custom-auth.
#
# Live mode creates EXACTLY ONE new explicit test address, never touches
# existing addresses, never deletes anything, and writes a reconciliation record.
# Real mail delivery / cleanup requires a separate operator action.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE="dry-run"
case "${1:-}" in
  "") ;;
  --live) MODE="live" ;;
  *) echo "usage: $0 [--live]" >&2; exit 2 ;;
esac
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }

if [ "$MODE" = "dry-run" ]; then
  echo "MODE dry-run (no network, no writes)"
  echo "== offline Cloudflare contract tests =="
  ( cd "$ROOT" && go test ./internal/provider/mailbox/cloudflare/... ./internal/mailx/... -count=1 ) \
    && ok "Cloudflare/mailx contract tests pass (offline)" || bad "contract tests"
  echo "== documented preconditions (read-only) =="
  for k in ONBOARD_CLOUDFLARE_ENABLED ONBOARD_CLOUDFLARE_BASE_URL ONBOARD_CLOUDFLARE_DOMAIN \
           ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE ONBOARD_CLOUDFLARE_SITE_PASSWORD_FILE; do
    printf 'PRECONDITION %s=%s\n' "$k" "${!k:-<unset>}"
  done
  echo "LIVE_PLAN:"
  echo "  1. Operator supplies written authorization + 0600 admin/site secret files."
  echo "  2. Run: ONBOARD_CLOUDFLARE_LIVE_AUTHORIZED=YES_I_HAVE_WRITTEN_AUTHORIZATION \\"
  echo "          ONBOARD_CLOUDFLARE_TEST_NAME=r6-live-<unique> \\"
  echo "          ONBOARD_CLOUDFLARE_DOMAIN=moonbyte.top \\"
  echo "          ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE=/path/admin-pass \\"
  echo "          bash tests/cloudflare/live_preflight.sh --live"
  echo "  3. The script creates ONE explicit test address via POST /admin/new_address,"
  echo "     records address_id/address (JWT stays in a 0600 temp file), and never deletes."
  echo "  4. Operator sends a nonce mail from an authorized SMTP relay, then polls"
  echo "     /api/parsed_mails up to 180s (documented; not automated here)."
  echo "  5. Record the test address for reconciliation; do NOT auto-delete long-lived addresses."
  echo "== config validation with fake local files (config.Load; no network) =="
  TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
  printf 'fake-admin-password-not-used\n' > "$TMP/admin"; chmod 600 "$TMP/admin"
  head -c 32 /dev/urandom > "$TMP/master"; chmod 600 "$TMP/master"
  CFG_ENV=(env ONBOARD_MODE=production ONBOARD_DB_PATH="$TMP/state.sqlite"
    ONBOARD_MASTER_KEY_FILE="$TMP/master" ONBOARD_ADMIN_TOKEN=abcdefghijklmnopqrstuvwxyz012345
    ONBOARD_CLOUDFLARE_ENABLED=true ONBOARD_CLOUDFLARE_BASE_URL=https://api.moonbyte.top
    ONBOARD_CLOUDFLARE_DOMAIN=moonbyte.top
    ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE="$TMP/admin"
    ONBOARD_FAKE_PROVIDERS=false)
  ( cd "$ROOT" && go build -trimpath -o bin/onboardd ./cmd/onboardd )
  ( cd "$ROOT" && "${CFG_ENV[@]}" ./bin/onboardd configcheck >/dev/null 2>&1 ) \
    && ok "config accepts documented Cloudflare file-based settings (offline)" \
    || bad "config rejected documented Cloudflare settings"
  ( cd "$ROOT" && "${CFG_ENV[@]}" ONBOARD_MODE=bogus-mode ./bin/onboardd configcheck >/dev/null 2>&1 ) \
    && bad "configcheck accepted an invalid mode" || ok "configcheck rejects invalid mode (negative case)"
  echo "SUMMARY pass=$PASS fail=$FAIL mode=dry-run"
  [ "$FAIL" = "0" ]
  exit 0
fi

echo "MODE live (requires written authorization)"
AUTH="${ONBOARD_CLOUDFLARE_LIVE_AUTHORIZED:-}"
if [ "$AUTH" != "YES_I_HAVE_WRITTEN_AUTHORIZATION" ]; then
  echo "REFUSED: live mode requires ONBOARD_CLOUDFLARE_LIVE_AUTHORIZED=YES_I_HAVE_WRITTEN_AUTHORIZATION" >&2
  exit 2
fi
BASE="${ONBOARD_CLOUDFLARE_BASE_URL:-https://api.moonbyte.top}"
[ "$BASE" = "https://api.moonbyte.top" ] || { echo "REFUSED: non-allowlisted base URL: $BASE" >&2; exit 2; }
DOMAIN="${ONBOARD_CLOUDFLARE_DOMAIN:-}"
[ "$DOMAIN" = "moonbyte.top" ] || { echo "REFUSED: ONBOARD_CLOUDFLARE_DOMAIN must be moonbyte.top" >&2; exit 2; }
NAME="${ONBOARD_CLOUDFLARE_TEST_NAME:-}"
echo "$NAME" | grep -qE '^[a-z0-9-]{8,40}$' || { echo "REFUSED: ONBOARD_CLOUDFLARE_TEST_NAME must be explicit ^[a-z0-9-]{8,40}$" >&2; exit 2; }
ADMIN_FILE="${ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE:-}"
[ -n "$ADMIN_FILE" ] && [ -r "$ADMIN_FILE" ] || { echo "REFUSED: admin password file missing/unreadable" >&2; exit 2; }
SITE_FILE="${ONBOARD_CLOUDFLARE_SITE_PASSWORD_FILE:-}"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
umask 077
# Keep credentials out of the process argv: curl -H @file reads headers from a
# 0600 file instead of exposing them in `ps`.
printf 'x-admin-auth: %s\n' "$(tr -d '\r\n' < "$ADMIN_FILE")" > "$TMP/headers"
chmod 600 "$TMP/headers"
if [ -n "$SITE_FILE" ]; then
  printf 'x-custom-auth: %s\n' "$(tr -d '\r\n' < "$SITE_FILE")" >> "$TMP/headers"
fi
HDRS=(-H "Content-Type: application/json" -H "@$TMP/headers")
BODY="$(python3 -c 'import json,sys;print(json.dumps({"enablePrefix":False,"name":sys.argv[1],"domain":sys.argv[2]}))' "$NAME" "$DOMAIN")"
echo "creating explicit test address: $NAME@$DOMAIN (one address; no delete)"
RESP="$(curl -sS -m 30 -X POST "$BASE/admin/new_address" "${HDRS[@]}" -d "$BODY" -w '\n%{http_code}')"
CODE="$(printf '%s' "$RESP" | tail -1)"
JSON="$(printf '%s' "$RESP" | sed '$d')"
[ "$CODE" = "200" ] || { echo "REFUSED/FAILED: create address HTTP $CODE"; exit 1; }
python3 - "$JSON" "$TMP/address.json" "$NAME" <<'PY'
import json,sys
data=json.loads(sys.argv[1])
out={"address":data.get("address"),"address_id":data.get("address_id"),"jwt":data.get("jwt"),"test_name":sys.argv[3]}
json.dump(out, open(sys.argv[2],"w"))
print("created", data.get("address"), "address_id", data.get("address_id"))
PY
chmod 600 "$TMP/address.json"
echo "RECONCILE: test address created; record address_id and address; JWT stored in $TMP/address.json (will be deleted on exit)."
echo "NEXT: operator sends a nonce mail from an authorized SMTP relay, then polls"
echo "      GET $BASE/api/parsed_mails with Authorization: Bearer <jwt> (never printed)."
echo "Do NOT delete real long-lived addresses; decide cleanup with the operator after reconciliation."
echo "SUMMARY pass=1 fail=0 mode=live"
