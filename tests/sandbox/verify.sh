#!/usr/bin/env bash
# tests/sandbox/verify.sh — end-to-end verification of the LOCAL SANDBOX.
#
# Drives the exact flow the onboarding service has to implement, against the
# real Keycloak + real Mailpit running on loopback:
#
#   [0] preflight (Keycloak realm + Mailpit API reachable)
#   [1] admin token            master/admin-cli, password grant
#   [2] create user            POST /admin/realms/<realm>/users  (emailVerified=false)
#   [3] send verify email      PUT  .../users/<id>/execute-actions-email  ["VERIFY_EMAIL"]
#   [4] poll Mailpit           GET  /api/v1/messages  -> extract verification link
#   [5] follow the link        -> Admin API shows emailVerified=true
#   [6] OIDC auth code + PKCE  GET auth, POST login form (cookie jar), read code
#   [7] token + userinfo       POST token with code_verifier, GET userinfo, sub == user id
#   [8] negative checks        wrong code_verifier rejected; auth request without
#                             code_challenge rejected (S256 enforced)
#   [9] teardown               DELETE the test user
#
# Usage:
#   bash tests/sandbox/verify.sh [--keep-user] [--email addr@moonbyte.test] [--no-log]
#   bash tests/sandbox/verify.sh --count 5
#
#   --keep-user   do not delete the created user at the end (debugging)
#   --email       override the generated test user email
#   --no-log      do not write a transcript under .sandbox/logs
#   --count N     provision + fully verify N distinct identities, print a compact
#                 PASS/FAIL table, delete them, exit non-zero if any identity fails
#                 (this is the R2 five-identity gate; detailed single-identity
#                 evidence is produced when --count is omitted)
#
# Exit code 0 only if every step passed. Prints the transcript path at the end.
set -euo pipefail

# shellcheck source=lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

KEEP_USER=0
WANT_EMAIL=""
WRITE_LOG=1
COUNT=0
while [ $# -gt 0 ]; do
  case "$1" in
    --keep-user) KEEP_USER=1; shift ;;
    --email) WANT_EMAIL="${2:?--email needs a value}"; shift 2 ;;
    --count) COUNT="${2:?--count needs a number}"; shift 2 ;;
    --no-log) WRITE_LOG=0; shift ;;
    -h|--help) sed -n '2,31p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done
case "$COUNT" in
  ''|*[!0-9]*) die "--count must be a positive integer" ;;
esac
[ "$COUNT" -le 50 ] || die "--count is limited to 50 identities"

require_cmds curl lsof jq openssl base64
load_secrets || die "no generated test credentials at $SB_SECRETS/sandbox.env — run tests/sandbox/up.sh first"

mkdirs
TS="$(date +%Y%m%d-%H%M%S)"
LOG="$SB_LOGS/verify-$TS.log"
if [ "$WRITE_LOG" -eq 1 ]; then
  # mirror everything to a transcript file (still visible on stdout)
  exec > >(tee "$LOG") 2>&1
fi

WORK="$SB_TMP/verify-$TS"
mkdir -p "$WORK"

# ---------------------------------------------------------------- helpers ---
PASS=0
FAIL=0
START_TS="$(date +%s)"

step() {
  hr
  printf '%s[ step %s ]%s %s\n' "$C_BLUE" "$1" "$C_RESET" "$2"
}
show() { printf '%s$ %s%s\n' "$C_DIM" "$*" "$C_RESET"; }
pass() { PASS=$((PASS + 1)); printf '%s[ PASS ]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
fail() { FAIL=$((FAIL + 1)); printf '%s[ FAIL ]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }
die_step() { fail "$*"; hr; printf '%sVERIFICATION FAILED%s — transcript: %s\n' "$C_RED" "$C_RESET" "${LOG:-<not written>}"; exit 1; }

# urlenc <string> -> percent-encoded (for readable evidence output only)
urlenc() { jq -rn --arg s "$1" '$s|@uri'; }

b64url_decode() {
  local s="$1"
  s="${s//-/+}"; s="${s//_/\/}"
  case $(( ${#s} % 4 )) in
    2) s="$s==" ;;
    3) s="$s=" ;;
  esac
  printf '%s' "$s" | base64 -D 2>/dev/null
}

jwt_payload() { # jwt_payload <compact-jwt> -> pretty JSON payload
  local p
  p="$(printf '%s' "$1" | cut -d. -f2)"
  b64url_decode "$p" | jq . 2>/dev/null || true
}

# obtain_auth_code <jar> <code_challenge> <state> <nonce> <workdir> <prefix>
# Drives the full browser dance (auth request -> login form -> POST credentials)
# and prints the authorization code from the callback redirect.
obtain_auth_code() {
  local jar="$1" challenge="$2" state="$3" nonce="$4" dir="$5" prefix="$6"
  local http action location
  http="$(curl -sS -m 30 -c "$jar" -b "$jar" -o "$dir/$prefix-login.html" -w '%{http_code}' -G "$KC_URL/realms/$REALM/protocol/openid-connect/auth" \
    --data-urlencode "client_id=$CLIENT_ID" \
    --data-urlencode 'response_type=code' \
    --data-urlencode 'scope=openid profile email' \
    --data-urlencode "redirect_uri=$APP_CALLBACK" \
    --data-urlencode "state=$state" \
    --data-urlencode "nonce=$nonce" \
    --data-urlencode "code_challenge=$challenge" \
    --data-urlencode 'code_challenge_method=S256')"
  [ "$http" = "200" ] || { echo "AUTH_HTTP_$http" >&2; return 1; }
  action="$(tr '\n' ' ' < "$dir/$prefix-login.html" | grep -oE '<form[^>]*id="kc-form-login"[^>]*>' | grep -oE 'action="[^"]+"' | head -1 | sed -E 's/^action="//; s/"$//; s/&amp;/\&/g' || true)"
  [ -n "$action" ] || { echo "NO_LOGIN_FORM" >&2; return 1; }
  case "$action" in /*) action="$KC_URL$action" ;; esac
  http="$(curl -sS -m 30 -c "$jar" -b "$jar" -o "$dir/$prefix-post.html" -D "$dir/$prefix-post.headers" -w '%{http_code}' -X POST "$action" \
    --data-urlencode "username=$USER_EMAIL" \
    --data-urlencode "password=$USER_PW" \
    --data-urlencode 'credentialId=')"
  [ "$http" = "302" ] || { echo "LOGIN_HTTP_$http" >&2; return 1; }
  location="$(grep -i '^location:' "$dir/$prefix-post.headers" | tr -d '\r' | sed -E 's/^[Ll]ocation: //')"
  printf '%s' "$location" | sed -nE 's/.*[?&]code=([^&]+).*/\1/p'
}

# --------------------------------------------------- multi-identity mode -----
# Used by --count N. Each helper returns 0/1 and sets MM_LAST_ERR on failure.
MM_WORK=""
MM_LAST_ERR=""
MM_UI_SUB=""
MM_UI_EMAIL=""
MM_UI_VERIFIED=""
TOKEN_EP=""
USERINFO_EP=""

mm_admin_ensure_token() { # refresh the 60s master token when it is about to expire
  local exp now
  now="$(date +%s)"
  exp="$(jwt_payload "$ADMIN_TOKEN" 2>/dev/null | jq -r '.exp // 0' 2>/dev/null || echo 0)"
  case "$exp" in ''|*[!0-9]*) exp=0 ;; esac
  if [ "$exp" -lt $((now + 10)) ]; then
    ADMIN_TOKEN="$(curl -sS -m 30 -X POST "$KC_URL/realms/master/protocol/openid-connect/token" \
      -H 'Content-Type: application/x-www-form-urlencoded' \
      -d 'grant_type=password' -d 'client_id=admin-cli' \
      --data-urlencode "username=$SANDBOX_ADMIN_USER" \
      --data-urlencode "password=$SANDBOX_ADMIN_PASSWORD" | jq -r '.access_token // empty')" || true
  fi
  [ -n "$ADMIN_TOKEN" ] || { MM_LAST_ERR="admin token refresh failed"; return 1; }
}

mm_create_user() { # <username> <email> -> echoes user id
  local u="$1" e="$2" payload code
  payload="$(jq -nc --arg u "$u" --arg e "$e" --arg p "$SANDBOX_TEST_USER_PASSWORD" '{
      username: $u, email: $e, firstName: "E2E", lastName: "Multi",
      enabled: true, emailVerified: false,
      attributes: { sandbox: ["local-e2e-multi"] },
      credentials: [ { type: "password", value: $p, temporary: false } ] }')"
  code="$(curl -sS -m 30 -o "$MM_WORK/create-$u.json" -w '%{http_code}' -X POST \
    "$KC_URL/admin/realms/$REALM/users" -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H 'Content-Type: application/json' -d "$payload")"
  if [ "$code" != "201" ]; then
    MM_LAST_ERR="create HTTP $code: $(head -c 160 "$MM_WORK/create-$u.json")"; return 1
  fi
  curl -sS -m 30 -H "Authorization: Bearer $ADMIN_TOKEN" \
    "$KC_URL/admin/realms/$REALM/users?username=$u&exact=true" | jq -r '.[0].id // empty'
}

mm_send_verify() { # <user-id>
  local code
  code="$(curl -sS -m 30 -o "$MM_WORK/send-$1.json" -w '%{http_code}' -X PUT \
    "$KC_URL/admin/realms/$REALM/users/$1/execute-actions-email?client_id=$CLIENT_ID&lifespan=3600" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
    -d '["VERIFY_EMAIL"]')"
  [ "$code" = "204" ] || { MM_LAST_ERR="execute-actions-email HTTP $code"; return 1; }
}

mm_wait_message() { # <email> [timeout] -> echoes Mailpit message id
  local email="$1" timeout="${2:-45}" id deadline
  deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    id="$(curl -sS -m 10 "$MP_API/messages?limit=200" 2>/dev/null \
      | jq -r --arg e "$email" '[.messages[]? | select(any(.To[]?; ((.Address // "") == $e)))] | sort_by(.Created) | last | .ID // empty' 2>/dev/null || true)"
    if [ -n "$id" ]; then printf '%s' "$id"; return 0; fi
    sleep 1
  done
  MM_LAST_ERR="no email for $email within ${timeout}s"
  return 1
}

mm_message_link() { # <message-id> -> echoes the verification link
  local id="$1" link
  link="$(curl -sS -m 10 "$MP_API/message/$id" | jq -r '.Text // ""' | tr -d '\r' \
    | grep -oE 'https?://127\.0\.0\.1:8180/[^ "<>]+' | head -1 || true)"
  if [ -z "$link" ]; then
    link="$(curl -sS -m 10 "$MP_API/message/$id" | jq -r '.HTML // ""' \
      | sed 's/&amp;/\&/g' | tr -d '\r' \
      | grep -oE 'https?://127\.0\.0\.1:8180/[^ "<>]+' | head -1 || true)"
  fi
  printf '%s' "$link" | sed -E 's/[[:space:]]+$//'
}

mm_follow_link() { # <link> <prefix> -> handles the two-step action-token page
  local link="$1" pfx="$2" jar="$MM_WORK/$2.jar" proceed code
  code="$(curl -sS -m 30 -c "$jar" -b "$jar" -o "$MM_WORK/$pfx-step1.html" -w '%{http_code}' -L "$link")"
  [ "$code" = "200" ] || { MM_LAST_ERR="action-token GET HTTP $code"; return 1; }
  proceed="$(tr '\n' ' ' < "$MM_WORK/$pfx-step1.html" \
    | grep -oE 'href="[^"]*login-actions/action-token[^"]*"' | head -1 \
    | sed -E 's/^href="//; s/"$//; s/&amp;/\&/g' || true)"
  if [ -n "$proceed" ]; then
    code="$(curl -sS -m 30 -c "$jar" -b "$jar" -o "$MM_WORK/$pfx-step2.html" -w '%{http_code}' -L "$proceed")"
    [ "$code" = "200" ] || { MM_LAST_ERR="click-to-proceed GET HTTP $code"; return 1; }
  fi
}

mm_email_verified() { # <user-id> -> true/false
  curl -sS -m 30 -H "Authorization: Bearer $ADMIN_TOKEN" \
    "$KC_URL/admin/realms/$REALM/users/$1" | jq -r '.emailVerified // false' 2>/dev/null
}

mm_oidc_userinfo() { # <email> <prefix> -> token + userinfo; sets MM_UI_*
  local email="$1" pfx="$2" jar verifier challenge state nonce code http tokens access
  MM_UI_SUB=""; MM_UI_EMAIL=""; MM_UI_VERIFIED=""
  jar="$MM_WORK/$pfx-oidc.jar"
  verifier="$(openssl rand -base64 60 | tr -d '\n=' | tr '+/' '-_')"
  challenge="$(printf '%s' "$verifier" | openssl dgst -binary -sha256 | openssl base64 -A | tr '+/' '-_' | tr -d '=')"
  state="$(openssl rand -hex 12)"
  nonce="$(openssl rand -hex 12)"
  USER_EMAIL="$email"
  USER_PW="$SANDBOX_TEST_USER_PASSWORD"
  code="$(obtain_auth_code "$jar" "$challenge" "$state" "$nonce" "$MM_WORK" "$pfx")" || true
  [ -n "$code" ] || { MM_LAST_ERR="no authorization code for $email"; return 1; }
  tokens="$MM_WORK/$pfx-tokens.json"
  http="$(curl -sS -m 30 -o "$tokens" -w '%{http_code}' -X POST "$TOKEN_EP" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    -d 'grant_type=authorization_code' \
    --data-urlencode "code=$code" \
    --data-urlencode "redirect_uri=$APP_CALLBACK" \
    --data-urlencode "client_id=$CLIENT_ID" \
    --data-urlencode "code_verifier=$verifier")"
  if [ "$http" != "200" ]; then
    MM_LAST_ERR="token exchange HTTP $http: $(jq -c '{error,error_description}' "$tokens" 2>/dev/null || head -c 120 "$tokens")"
    return 1
  fi
  access="$(jq -r '.access_token // empty' "$tokens")"
  http="$(curl -sS -m 30 -o "$MM_WORK/$pfx-userinfo.json" -w '%{http_code}' \
    -H "Authorization: Bearer $access" "$USERINFO_EP")"
  if [ "$http" != "200" ]; then MM_LAST_ERR="userinfo HTTP $http"; return 1; fi
  MM_UI_SUB="$(jq -r '.sub // empty' "$MM_WORK/$pfx-userinfo.json")"
  MM_UI_EMAIL="$(jq -r '.email // empty' "$MM_WORK/$pfx-userinfo.json")"
  MM_UI_VERIFIED="$(jq -r '.email_verified // false' "$MM_WORK/$pfx-userinfo.json")"
}

mm_delete_user() { # <user-id>
  local code
  code="$(curl -sS -m 30 -o /dev/null -w '%{http_code}' -X DELETE \
    "$KC_URL/admin/realms/$REALM/users/$1" -H "Authorization: Bearer $ADMIN_TOKEN")"
  [ "$code" = "204" ] || { MM_LAST_ERR="delete HTTP $code"; return 1; }
}

run_multi() { # run_multi <n>
  local n="$1" i=1 pass_n=0 fail_n=0
  local uname email uid msg link rc ok c_create c_mail c_verified c_token c_ui c_del
  TOKEN_EP="$KC_URL/realms/$REALM/protocol/openid-connect/token"
  USERINFO_EP="$KC_URL/realms/$REALM/protocol/openid-connect/userinfo"
  MM_WORK="$WORK"
  mkdir -p "$MM_WORK"

  hr
  printf 'MULTI-IDENTITY E2E — %s identities (LOCAL SANDBOX, realm %s)\n' "$n" "$REALM"
  printf '%-3s %-41s %-7s %-6s %-9s %-6s %-9s %-7s %s\n' \
    "#" "email" "create" "mail" "verified" "token" "userinfo" "delete" "result"
  while [ "$i" -le "$n" ]; do
    ok=1
    MM_LAST_ERR=""
    c_create="-"; c_mail="-"; c_verified="-"; c_token="-"; c_ui="-"; c_del="-"
    mm_admin_ensure_token || ok=0
    uname="e2e-m${i}-$(date +%s)"
    email="e2e-m${i}.$(date +%s)@$TEST_EMAIL_DOMAIN"

    # create
    uid="$(mm_create_user "$uname" "$email")"; rc=$?
    if [ "$rc" -eq 0 ] && [ -n "$uid" ]; then c_create="201"; else c_create="FAIL"; ok=0; fi

    # send verify email + wait for it in Mailpit
    if [ "$ok" -eq 1 ]; then
      if mm_send_verify "$uid"; then
        msg="$(mm_wait_message "$email" 45)"; rc=$?
        if [ "$rc" -eq 0 ] && [ -n "$msg" ]; then
          link="$(mm_message_link "$msg")"
          if [ -n "$link" ]; then c_mail="ok"; else c_mail="FAIL"; ok=0; fi
        else
          c_mail="FAIL"; ok=0
        fi
      else
        c_mail="FAIL"; ok=0
      fi
    else
      c_mail="FAIL"
    fi

    # follow the verification link (two steps) + confirm emailVerified
    if [ "$ok" -eq 1 ]; then
      if mm_follow_link "$link" "m$i"; then
        if [ "$(mm_email_verified "$uid")" = "true" ]; then c_verified="true"; else c_verified="FAIL"; ok=0; fi
      else
        c_verified="FAIL"; ok=0
      fi
    else
      c_verified="FAIL"
    fi

    # OIDC authorization code + PKCE + userinfo
    if [ -n "$uid" ]; then
      if mm_oidc_userinfo "$email" "m$i"; then
        c_token="200"
        if [ "$MM_UI_SUB" = "$uid" ] && [ "$MM_UI_EMAIL" = "$email" ] && [ "$MM_UI_VERIFIED" = "true" ]; then
          c_ui="ok"
        else
          c_ui="FAIL"; ok=0
          MM_LAST_ERR="userinfo mismatch (sub=$MM_UI_SUB email=$MM_UI_EMAIL verified=$MM_UI_VERIFIED)"
        fi
      else
        c_token="FAIL"; c_ui="FAIL"; ok=0
      fi
    else
      c_token="FAIL"; c_ui="FAIL"
    fi

    # teardown
    if [ -n "$uid" ]; then
      if mm_delete_user "$uid"; then c_del="204"; else c_del="FAIL"; ok=0; fi
    else
      c_del="FAIL"
    fi

    if [ "$ok" -eq 1 ]; then
      pass_n=$((pass_n + 1))
      printf '%-3s %-41s %-7s %-6s %-9s %-6s %-9s %-7s %s\n' \
        "$i" "$email" "$c_create" "$c_mail" "$c_verified" "$c_token" "$c_ui" "$c_del" "PASS"
    else
      fail_n=$((fail_n + 1))
      printf '%-3s %-41s %-7s %-6s %-9s %-6s %-9s %-7s %s\n' \
        "$i" "$email" "$c_create" "$c_mail" "$c_verified" "$c_token" "$c_ui" "$c_del" "FAIL"
      printf '    ↳ id=%s detail: %s\n' "${uid:-n/a}" "${MM_LAST_ERR:-unknown}"
    fi
    i=$((i + 1))
  done

  hr
  printf 'mailpit messages stored: %s   transcript: %s\n' "$(mp_message_count)" "${LOG:-<not written>}"
  if [ "$fail_n" -eq 0 ]; then
    printf '%sMULTI-IDENTITY RESULT: PASS%s — %s/%s identities created, email-verified, OIDC+PKCE token obtained, userinfo matched, deleted\n' \
      "$C_GREEN" "$C_RESET" "$pass_n" "$n"
    hr
    exit 0
  fi
  printf '%sMULTI-IDENTITY RESULT: FAIL%s — %s/%s passed, %s failed\n' \
    "$C_RED" "$C_RESET" "$pass_n" "$n" "$fail_n"
  hr
  exit 1
}

# redact secrets that must never land in logs/docs
redact_link() { sed -E 's/([?&]key=)[^&"]+/\1<JWT-REDACTED>/g; s/([?&]code=)[^&"]+/\1<CODE-REDACTED>/g; s/([?&]session_code=)[^&"]+/\1<REDACTED>/g; s/(execution=)[^&"]+/\1<REDACTED>/g'; }
redact_token() { printf '%s' "$1" | head -c 12; printf '…(%d chars, redacted)' "${#1}"; }

ADMIN_TOKEN=""
admin_get_code() { # admin_get_code <path> <outfile>
  curl -sS -m 30 -o "$2" -w '%{http_code}' -H "Authorization: Bearer $ADMIN_TOKEN" "$KC_URL/admin$1"
}

# URL-encoded redirect URI used in the authorization request
REDIRECT_URI_ENC="$(urlenc "$APP_CALLBACK")"

printf '%s\n' "LOCAL SANDBOX E2E VERIFICATION — $(date '+%Y-%m-%d %H:%M:%S%z')"
echo "host: $(sw_vers -productName 2>/dev/null) $(sw_vers -productVersion 2>/dev/null) $(uname -m)   shell: bash $BASH_VERSION"
echo "keycloak: $KC_URL  realm: $REALM  client: $CLIENT_ID"
echo "mailpit : $MP_URL  (smtp $MP_SMTP_HOST:$MP_SMTP_PORT)"
echo "java    : $("$SB_JAVA_BIN" -version 2>&1 | head -1)"

# --------------------------------------------------------------- 0 preflight
step 0 "preflight: services reachable"
CODE="$(http_code "$KC_URL/realms/$REALM/.well-known/openid-configuration")"
show "curl -s -o /dev/null -w '%{http_code}' $KC_URL/realms/$REALM/.well-known/openid-configuration"
echo "-> HTTP $CODE"
[ "$CODE" = "200" ] || die_step "Keycloak realm $REALM is not serving OIDC discovery (HTTP $CODE) — run tests/sandbox/up.sh"
pass "Keycloak OIDC discovery: HTTP 200"

CODE="$(http_code "$MP_API/messages?limit=1")"
show "curl -s -o /dev/null -w '%{http_code}' '$MP_API/messages?limit=1'"
echo "-> HTTP $CODE"
[ "$CODE" = "200" ] || die_step "Mailpit API not reachable (HTTP $CODE) — run tests/sandbox/up.sh"
pass "Mailpit API: HTTP 200 ($(mp_message_count) message(s) stored before run)"

# ------------------------------------------------------------- 1 admin auth
step 1 "admin token: password grant on master/admin-cli (test-only admin)"
show "curl -sS -X POST $KC_URL/realms/master/protocol/openid-connect/token -d grant_type=password -d client_id=admin-cli -d username=\$SANDBOX_ADMIN_USER --data-urlencode password=\$SANDBOX_ADMIN_PASSWORD"
CODE="$(curl -sS -m 30 -o "$WORK/admin-token.json" -w '%{http_code}' \
  -X POST "$KC_URL/realms/master/protocol/openid-connect/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=password' -d 'client_id=admin-cli' \
  --data-urlencode "username=$SANDBOX_ADMIN_USER" \
  --data-urlencode "password=$SANDBOX_ADMIN_PASSWORD")"
echo "-> HTTP $CODE"
[ "$CODE" = "200" ] || die_step "admin token request failed (HTTP $CODE): $(head -c 300 "$WORK/admin-token.json")"
ADMIN_TOKEN="$(jq -r '.access_token // empty' "$WORK/admin-token.json")"
[ -n "$ADMIN_TOKEN" ] || die_step "admin token response has no access_token: $(jq -c '{error,error_description}' "$WORK/admin-token.json")"
jq -r '"token_type=\(.token_type) expires_in=\(.expires_in)s scope=\(.scope // "-") refresh_expires_in=\(.refresh_expires_in)s"' "$WORK/admin-token.json"
echo "access_token: $(redact_token "$ADMIN_TOKEN")"
echo "admin JWT payload (redacted):"
jwt_payload "$ADMIN_TOKEN" | jq '{iss, azp, typ, sub_len: (.sub|length), exp}' 2>/dev/null || true
pass "admin token acquired for '$SANDBOX_ADMIN_USER' (master realm, realm-management rights)"

# --count N: switch to the compact multi-identity gate (R2 five-identity check).
# run_multi prints its own table/summary and exits.
if [ "$COUNT" -gt 0 ]; then
  run_multi "$COUNT"
fi

# ------------------------------------------------------------ 2 create user
step 2 "create test user in realm '$REALM' via Admin API (emailVerified=false)"
USER_EMAIL="${WANT_EMAIL:-e2e.$(date +%s)@$TEST_EMAIL_DOMAIN}"
USERNAME="e2e-$(date +%s)"
USER_PW="$SANDBOX_TEST_USER_PASSWORD"
jq -n --arg u "$USERNAME" --arg e "$USER_EMAIL" --arg p "$USER_PW" '{
  username: $u, email: $e, firstName: "E2E", lastName: "Sandbox",
  enabled: true, emailVerified: false,
  attributes: { sandbox: ["local-e2e"] },
  credentials: [ { type: "password", value: $p, temporary: false } ]
}' > "$WORK/create-user.json"
show "curl -sS -X POST $KC_URL/admin/realms/$REALM/users -H 'Authorization: Bearer <admin>' -H 'Content-Type: application/json' -d @<user payload: username/email/enabled/credentials>"
echo "payload (password redacted):"
jq '.credentials[0].value="<redacted>"' "$WORK/create-user.json"
CODE="$(curl -sS -m 30 -o "$WORK/create-user.out" -w '%{http_code}' \
  -X POST "$KC_URL/admin/realms/$REALM/users" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d @"$WORK/create-user.json")"
echo "-> HTTP $CODE (201 = created)"
[ "$CODE" = "201" ] || die_step "user creation failed (HTTP $CODE): $(head -c 400 "$WORK/create-user.out")"
pass "user created: $USER_EMAIL"

CODE="$(admin_get_code "/realms/$REALM/users?username=$(urlenc "$USERNAME")&exact=true" "$WORK/user.json")"
[ "$CODE" = "200" ] || die_step "user lookup failed (HTTP $CODE)"
USER_ID="$(jq -r '.[0].id // empty' "$WORK/user.json")"
[ -n "$USER_ID" ] || die_step "created user not found by username '$USERNAME'"
jq -r '.[0] | "id=\(.id)\nusername=\(.username)\nemail=\(.email)\nenabled=\(.enabled)\nemailVerified=\(.emailVerified)\nrequiredActions=\(.requiredActions|join(",") // "-")"' "$WORK/user.json"
[ "$(jq -r '.[0].emailVerified' "$WORK/user.json")" = "false" ] || die_step "expected emailVerified=false right after creation"
pass "user id=$USER_ID, emailVerified=false (as expected before verification)"

# ---------------------------------------------------- 3 trigger verify email
step 3 "trigger VERIFY_EMAIL email (Admin API execute-actions-email)"
show "curl -sS -X PUT '$KC_URL/admin/realms/$REALM/users/$USER_ID/execute-actions-email?client_id=$CLIENT_ID&lifespan=3600' -d '[\"VERIFY_EMAIL\"]'"
echo "-> sending action-token email through the realm SMTP ($MP_SMTP_HOST:$MP_SMTP_PORT)"
CODE="$(curl -sS -m 30 -o "$WORK/act.out" -w '%{http_code}' \
  -X PUT "$KC_URL/admin/realms/$REALM/users/$USER_ID/execute-actions-email?client_id=$CLIENT_ID&lifespan=3600" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '["VERIFY_EMAIL"]')"
echo "-> HTTP $CODE (204 = email accepted for delivery)"
[ "$CODE" = "204" ] || die_step "execute-actions-email failed (HTTP $CODE): $(head -c 400 "$WORK/act.out")"
pass "verification email requested (HTTP 204)"

# --------------------------------------------------------- 4 poll mailpit
step 4 "poll Mailpit HTTP API until the email arrives, extract the verification link"
MSG_ID=""
DEADLINE=$(( $(date +%s) + 60 ))
TRIES=0
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  TRIES=$((TRIES + 1))
  curl -sS -m 10 "$MP_API/messages?limit=100" > "$WORK/messages.json" 2>/dev/null || true
  MSG_ID="$(jq -r --arg e "$USER_EMAIL" '
      [.messages[]? | select(any(.To[]?; (.Address // "") == $e))] | sort_by(.Created) | last | .ID // empty
    ' "$WORK/messages.json" 2>/dev/null || true)"
  [ -n "$MSG_ID" ] && break
  sleep 2
done
show "curl -sS '$MP_API/messages?limit=100' | jq '[.messages[] | select(.To[].Address == \"$USER_EMAIL\")]'"
echo "-> polled $TRIES time(s), matched message id: ${MSG_ID:-<none>}"
[ -n "$MSG_ID" ] || die_step "no email for $USER_EMAIL arrived in Mailpit within 60s (is the realm SMTP pointing at $MP_SMTP_ADDR?)"
curl -sS -m 10 "$MP_API/message/$MSG_ID" > "$WORK/message.json"
jq -r '"From: \(.From.Name // "") <\(.From.Address // "")>\nTo: \([.To[] | .Address] | join(", "))\nSubject: \(.Subject)\nCreated: \(.Created)\nSize: \(.Size) bytes"' "$WORK/message.json"
SUBJECT="$(jq -r '.Subject' "$WORK/message.json")"
case "$SUBJECT" in
  *[Vv]erify*|*[Vv]erif*|*[Cc]onfirm*|*[Aa]ctivat*|*[Uu]pdate*) pass "email received: \"$SUBJECT\" -> $USER_EMAIL" ;;
  *) fail "email received but subject looks unexpected: $SUBJECT"; die_step "unexpected verification email subject" ;;
esac

LINK="$(jq -r '.Text // ""' "$WORK/message.json" | tr -d '\r' | grep -oE "https?://127\.0\.0\.1:8180/[^ \"<>]+" | head -1 || true)"
if [ -z "$LINK" ]; then
  # fall back to the HTML part (HTML-escaped ampersands)
  LINK="$(jq -r '.HTML // ""' "$WORK/message.json" | sed 's/&amp;/\&/g' | tr -d '\r' | grep -oE "https?://127\.0\.0\.1:8180/[^ \"<>]+" | head -1 || true)"
fi
LINK="$(printf '%s' "$LINK" | sed -E 's/[[:space:]]+$//')"
[ -n "$LINK" ] || die_step "could not find a 127.0.0.1:8180 link in the email body (parts: $(jq -r 'keys|join(",")' "$WORK/message.json"))"
echo "verification link (JWT redacted): $(printf '%s' "$LINK" | redact_link)"
case "$LINK" in
  *"/login-actions/action-token"*) pass "email contains a Keycloak action-token link (VERIFY_EMAIL)" ;;
  *) fail "link is not an action-token link: $(printf '%s' "$LINK" | sed -E 's/[?].*//')"; die_step "unexpected verification link shape" ;;
esac

# ------------------------------------------------------- 5 follow the link
step 5 "follow the verification link (curl) and confirm emailVerified=true"
show "curl -sS -c jar -b jar -L '$(printf '%s' "$LINK" | redact_link)'"
META="$(curl -sS -m 30 -c "$WORK/jar-verify" -b "$WORK/jar-verify" -o "$WORK/verify-result.html" -D "$WORK/verify-result.headers" -w '%{http_code}\n%{url_effective}' -L "$LINK")"
CODE="${META%%$'\n'*}"
FINAL_URL="${META#*$'\n'}"
echo "-> final HTTP $CODE"
echo "-> final URL (redacted): $(printf '%s' "$FINAL_URL" | redact_link)"
echo "-> final page title: $(grep -oE '<title>[^<]*</title>' "$WORK/verify-result.html" | head -1 | sed -E 's#</?title>##g')"

# Keycloak renders an interstitial page ("Perform the following action(s) » Click
# here to proceed"): the action only executes when that link is followed too.
# A browser does this with a click; an E2E driver has to do the same GET.
PROCEED="$(tr '\n' ' ' < "$WORK/verify-result.html" | grep -oE 'href="[^"]*login-actions/action-token[^"]*"' | head -1 | sed -E 's/^href="//; s/"$//; s/&amp;/\&/g' || true)"
if [ -n "$PROCEED" ]; then
  echo "-> interstitial confirmation page detected: performing the \"Click here to proceed\" step"
  show "curl -sS -c jar -b jar -L '$(printf '%s' "$PROCEED" | redact_link)'"
  CODE="$(curl -sS -m 30 -c "$WORK/jar-verify" -b "$WORK/jar-verify" -o "$WORK/verify-final.html" -D "$WORK/verify-final.headers" -w '%{http_code}' -L "$PROCEED")"
  echo "-> HTTP $CODE"
  echo "-> page title: $(grep -oE '<title>[^<]*</title>' "$WORK/verify-final.html" | head -1 | sed -E 's#</?title>##g')"
else
  echo "-> no interstitial page: action token executed directly"
fi

CODE="$(admin_get_code "/realms/$REALM/users/$USER_ID" "$WORK/user-after-verify.json")"
[ "$CODE" = "200" ] || die_step "user lookup after verification failed (HTTP $CODE)"
VERIFIED="$(jq -r '.emailVerified' "$WORK/user-after-verify.json")"
echo "admin API: emailVerified=$VERIFIED, requiredActions=$(jq -r '.requiredActions|join(",")' "$WORK/user-after-verify.json")"
[ "$VERIFIED" = "true" ] || die_step "emailVerified is still '$VERIFIED' after following the link"
pass "emailVerified=true confirmed via Admin API"

# --------------------------------------------------- 6 OIDC auth code + PKCE
step 6 "OIDC authorization code + PKCE (S256) via curl cookie jar"
VERIFIER="$(openssl rand -base64 60 | tr -d '\n=' | tr '+/' '-_')"
CHALLENGE="$(printf '%s' "$VERIFIER" | openssl dgst -binary -sha256 | openssl base64 -A | tr '+/' '-_' | tr -d '=')"
STATE="$(openssl rand -hex 12)"
NONCE="$(openssl rand -hex 12)"
echo "code_verifier  : $VERIFIER"
echo "code_challenge : $CHALLENGE  (base64url(sha256(verifier)), method=S256)"
echo "state          : $STATE"
echo "nonce          : $NONCE"
show "curl -sS -c jar -b jar -G '$KC_URL/realms/$REALM/protocol/openid-connect/auth' \\
  --data-urlencode client_id=$CLIENT_ID --data-urlencode response_type=code \\
  --data-urlencode 'scope=openid profile email' --data-urlencode redirect_uri=$APP_CALLBACK \\
  --data-urlencode state=$STATE --data-urlencode nonce=$NONCE \\
  --data-urlencode code_challenge=$CHALLENGE --data-urlencode code_challenge_method=S256"
CODE="$(curl -sS -m 30 -c "$WORK/jar-auth" -b "$WORK/jar-auth" -o "$WORK/login.html" -w '%{http_code}' -G "$KC_URL/realms/$REALM/protocol/openid-connect/auth" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "response_type=code" \
  --data-urlencode "scope=openid profile email" \
  --data-urlencode "redirect_uri=$APP_CALLBACK" \
  --data-urlencode "state=$STATE" \
  --data-urlencode "nonce=$NONCE" \
  --data-urlencode "code_challenge=$CHALLENGE" \
  --data-urlencode "code_challenge_method=S256")"
echo "-> HTTP $CODE"
[ "$CODE" = "200" ] || die_step "authorization endpoint did not return the login form (HTTP $CODE): $(grep -oE '<p[^>]*>[^<]*' "$WORK/login.html" | head -3)"
echo "-> login page title: $(grep -oE '<title>[^<]*</title>' "$WORK/login.html" | head -1 | sed -E 's#</?title>##g')"
echo "-> cookie jar: $(grep -c . "$WORK/jar-auth" 2>/dev/null || echo 0) line(s) ($(grep -oE 'AUTH_SESSION_ID' "$WORK/jar-auth" | head -1 || true) set)"

ACTION="$(tr '\n' ' ' < "$WORK/login.html" | grep -oE '<form[^>]*id="kc-form-login"[^>]*>' | grep -oE 'action="[^"]+"' | head -1 | sed -E 's/^action="//; s/"$//; s/&amp;/\&/g' || true)"
[ -n "$ACTION" ] || die_step "could not parse the Keycloak login form action from the HTML"
case "$ACTION" in
  /*) ACTION="$KC_URL$ACTION" ;;
esac
echo "login form action: $(printf '%s' "$ACTION" | redact_link)"
show "curl -sS -c jar -b jar -X POST '$ACTION' --data-urlencode username=$USER_EMAIL --data-urlencode password=<redacted>"
CODE="$(curl -sS -m 30 -c "$WORK/jar-auth" -b "$WORK/jar-auth" -o "$WORK/post-login.html" -D "$WORK/post-login.headers" -w '%{http_code}' \
  -X POST "$ACTION" \
  --data-urlencode "username=$USER_EMAIL" \
  --data-urlencode "password=$USER_PW" \
  --data-urlencode 'credentialId=')"
echo "-> HTTP $CODE (302 = credentials accepted, redirect to the callback)"
[ "$CODE" = "302" ] || die_step "login POST did not redirect (HTTP $CODE) — check the user is enabled and password is correct: $(grep -oE 'id="input-error"[^>]*>[^<]*|kc-feedback-text[^>]*>[^<]*' "$WORK/post-login.html" | head -3)"
LOCATION="$(grep -i '^location:' "$WORK/post-login.headers" | tr -d '\r' | sed -E 's/^[Ll]ocation: //')"
echo "redirect: $(printf '%s' "$LOCATION" | sed -E 's/^(.*[?&]code=)[^&]+/\1<CODE-REDACTED>/; s/&state=.*/\&state='"$STATE"'/')"
case "$LOCATION" in
  "$APP_CALLBACK"*) ;;
  *) die_step "redirect target '$LOCATION' does not start with the registered redirect URI '$APP_CALLBACK'" ;;
esac
AUTH_CODE="$(printf '%s' "$LOCATION" | sed -nE 's/.*[?&]code=([^&]+).*/\1/p')"
STATE_BACK="$(printf '%s' "$LOCATION" | sed -nE 's/.*[?&]state=([^&]+).*/\1/p')"
[ -n "$AUTH_CODE" ] || die_step "no code= in the callback redirect: $LOCATION"
[ "$STATE_BACK" = "$STATE" ] || die_step "state mismatch: sent '$STATE', got '$STATE_BACK' (CSRF protection would fail)"
echo "authorization code: $(redact_token "$AUTH_CODE")"
pass "authorization code issued for $USER_EMAIL, state echoed back correctly"

# -------------------------------------- 7 token exchange (PKCE) + userinfo
step 7 "exchange code for tokens with code_verifier, then verify via userinfo"
WRONG_VERIFIER="$(openssl rand -base64 60 | tr -d '\n=' | tr '+/' '-_')"
show "curl -sS -X POST $KC_URL/realms/$REALM/protocol/openid-connect/token -d grant_type=authorization_code -d code=<code> -d redirect_uri=$APP_CALLBACK -d client_id=$CLIENT_ID -d code_verifier=<verifier>"
CODE="$(curl -sS -m 30 -o "$WORK/tokens.json" -w '%{http_code}' \
  -X POST "$KC_URL/realms/$REALM/protocol/openid-connect/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=authorization_code' \
  --data-urlencode "code=$AUTH_CODE" \
  --data-urlencode "redirect_uri=$APP_CALLBACK" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "code_verifier=$VERIFIER")"
echo "-> HTTP $CODE"
[ "$CODE" = "200" ] || die_step "token exchange failed (HTTP $CODE): $(jq -c '{error,error_description}' "$WORK/tokens.json" 2>/dev/null || head -c 300 "$WORK/tokens.json")"
ACCESS_TOKEN="$(jq -r '.access_token // empty' "$WORK/tokens.json")"
ID_TOKEN="$(jq -r '.id_token // empty' "$WORK/tokens.json")"
[ -n "$ACCESS_TOKEN" ] || die_step "no access_token in the token response"
jq -r '"token_type=\(.token_type)\nexpires_in=\(.expires_in)s\nrefresh_expires_in=\(.refresh_expires_in // "-")s\nscope=\(.scope)\nsession_state=\(.session_state // "-")"' "$WORK/tokens.json"
echo "access_token: $(redact_token "$ACCESS_TOKEN")"
echo "id_token    : $(redact_token "$ID_TOKEN")"
echo "access token claims (selected):"
jwt_payload "$ACCESS_TOKEN" | jq '{iss, aud, azp, typ, scope, preferred_username, email, email_verified, exp}' 2>/dev/null || true
echo "id_token claims (selected):"
jwt_payload "$ID_TOKEN" | jq '{iss, aud, azp, sub, nonce, preferred_username, email, email_verified, name, exp}' 2>/dev/null || true
ID_NONCE="$(jwt_payload "$ID_TOKEN" | jq -r '.nonce // empty')"
[ "$ID_NONCE" = "$NONCE" ] || die_step "id_token nonce mismatch (sent '$NONCE', got '$ID_NONCE')"
pass "token response 200 (Bearer, $(jq -r '.expires_in' "$WORK/tokens.json")s), id_token nonce matches"

show "curl -sS -H 'Authorization: Bearer <access_token>' $KC_URL/realms/$REALM/protocol/openid-connect/userinfo"
CODE="$(curl -sS -m 30 -o "$WORK/userinfo.json" -w '%{http_code}' \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  "$KC_URL/realms/$REALM/protocol/openid-connect/userinfo")"
echo "-> HTTP $CODE"
[ "$CODE" = "200" ] || die_step "userinfo failed (HTTP $CODE): $(head -c 300 "$WORK/userinfo.json")"
jq '{sub, preferred_username, email, email_verified, given_name, family_name, name}' "$WORK/userinfo.json"
UI_SUB="$(jq -r '.sub' "$WORK/userinfo.json")"
UI_EMAIL="$(jq -r '.email' "$WORK/userinfo.json")"
UI_VERIFIED="$(jq -r '.email_verified' "$WORK/userinfo.json")"
[ "$UI_SUB" = "$USER_ID" ] || die_step "userinfo sub '$UI_SUB' does not match the created user id '$USER_ID'"
[ "$UI_EMAIL" = "$USER_EMAIL" ] || die_step "userinfo email '$UI_EMAIL' does not match '$USER_EMAIL'"
[ "$UI_VERIFIED" = "true" ] || die_step "userinfo email_verified is '$UI_VERIFIED'"
pass "userinfo sub == created Keycloak user id ($USER_ID), email=$UI_EMAIL, email_verified=true"

# ------------------------------------- 7b negative: PKCE + single-use code
echo
echo "--- negative checks: PKCE mismatch and code replay must both be rejected ---"
echo "1) a fresh authorization code exchanged with the WRONG code_verifier"
V2="$(openssl rand -base64 60 | tr -d '\n=' | tr '+/' '-_')"
C2="$(printf '%s' "$V2" | openssl dgst -binary -sha256 | openssl base64 -A | tr '+/' '-_' | tr -d '=')"
S2="$(openssl rand -hex 12)"
N2="$(openssl rand -hex 12)"
CODE2="$(obtain_auth_code "$WORK/jar-neg" "$C2" "$S2" "$N2" "$WORK" "neg")" || die_step "could not obtain a second authorization code for the PKCE negative test"
[ -n "$CODE2" ] || die_step "second authorization request returned no code"
show "curl -sS -X POST $KC_URL/realms/$REALM/protocol/openid-connect/token -d grant_type=authorization_code -d code=<fresh code> -d code_verifier=<wrong verifier>"
CODE="$(curl -sS -m 30 -o "$WORK/token-bad.json" -w '%{http_code}' \
  -X POST "$KC_URL/realms/$REALM/protocol/openid-connect/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=authorization_code' \
  --data-urlencode "code=$CODE2" \
  --data-urlencode "redirect_uri=$APP_CALLBACK" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "code_verifier=$WRONG_VERIFIER")"
echo "-> HTTP $CODE $(jq -c '{error,error_description}' "$WORK/token-bad.json" 2>/dev/null || head -c 200 "$WORK/token-bad.json")"
[ "$CODE" = "400" ] || die_step "wrong code_verifier was accepted (HTTP $CODE) — PKCE is NOT enforced"
pass "invalid code_verifier rejected (HTTP 400 invalid_grant, PKCE S256 enforced)"

echo "2) replaying the already-used authorization code"
show "curl -sS -X POST $KC_URL/realms/$REALM/protocol/openid-connect/token -d grant_type=authorization_code -d code=<used code> -d code_verifier=<correct verifier>"
CODE="$(curl -sS -m 30 -o "$WORK/token-replay.json" -w '%{http_code}' \
  -X POST "$KC_URL/realms/$REALM/protocol/openid-connect/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=authorization_code' \
  --data-urlencode "code=$AUTH_CODE" \
  --data-urlencode "redirect_uri=$APP_CALLBACK" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "code_verifier=$VERIFIER")"
echo "-> HTTP $CODE $(jq -c '{error,error_description}' "$WORK/token-replay.json" 2>/dev/null || head -c 200 "$WORK/token-replay.json")"
[ "$CODE" = "400" ] || die_step "a consumed authorization code was accepted again (HTTP $CODE)"
pass "replayed authorization code rejected (single-use, HTTP 400 invalid_grant)"

# ------------------------------------------------- 8 PKCE enforcement probe
step 8 "negative check: PKCE is actually required (S256) for this client"
show "curl -sS -o /dev/null -w '%{http_code}' -G '$KC_URL/realms/$REALM/protocol/openid-connect/auth' ... (no code_challenge param)"
CODE="$(curl -sS -m 30 -o "$WORK/auth-no-pkce.html" -D "$WORK/auth-no-pkce.headers" -w '%{http_code}' -G "$KC_URL/realms/$REALM/protocol/openid-connect/auth" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "response_type=code" \
  --data-urlencode "scope=openid" \
  --data-urlencode "redirect_uri=$APP_CALLBACK" \
  --data-urlencode "state=pkce-probe")"
echo "-> HTTP $CODE"
NO_PKCE_LOC="$(grep -i '^location:' "$WORK/auth-no-pkce.headers" | tr -d '\r' | sed -E 's/^[Ll]ocation: //' || true)"
if [ -n "$NO_PKCE_LOC" ]; then
  echo "-> redirect: $(printf '%s' "$NO_PKCE_LOC" | sed -E 's/^(.{0,120}).*/\1/')"
fi
if printf '%s' "$NO_PKCE_LOC" | grep -q 'error=invalid_request' || [ "$CODE" = "400" ]; then
  pass "authorization request without code_challenge rejected (PKCE required)"
else
  fail "authorization request without code_challenge was NOT rejected (HTTP $CODE, location: ${NO_PKCE_LOC:-none})"
  die_step "PKCE enforcement check failed"
fi

# --------------------------------------------------------------- 9 teardown
step 9 "teardown: delete the test user"
if [ "$KEEP_USER" -eq 1 ]; then
  warn "--keep-user set; leaving $USER_EMAIL (id $USER_ID) in the realm"
else
  show "curl -sS -X DELETE $KC_URL/admin/realms/$REALM/users/$USER_ID"
  CODE="$(curl -sS -m 30 -o "$WORK/delete.out" -w '%{http_code}' \
    -X DELETE "$KC_URL/admin/realms/$REALM/users/$USER_ID" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
  echo "-> HTTP $CODE (204 = deleted)"
  [ "$CODE" = "204" ] || die_step "could not delete test user (HTTP $CODE)"
  CODE="$(admin_get_code "/realms/$REALM/users/$USER_ID" "$WORK/user-deleted.json")"
  echo "-> follow-up GET user: HTTP $CODE (404 = gone)"
  [ "$CODE" = "404" ] || die_step "user $USER_ID still readable after delete (HTTP $CODE)"
  pass "test user deleted (DELETE 204, subsequent GET 404)"
fi

# ---------------------------------------------------------------- summary ---
ELAPSED=$(( $(date +%s) - START_TS ))
hr
printf '%sVERIFICATION PASSED%s — %d checks, %d failed, %ds\n' "$C_GREEN" "$C_RESET" "$PASS" "$FAIL" "$ELAPSED"
echo "realm: $REALM   client: $CLIENT_ID   issuer: $KC_URL/realms/$REALM"
echo "mailpit messages still stored: $(mp_message_count)"
[ "$WRITE_LOG" -eq 1 ] && echo "transcript: $LOG"
hr
exit 0
