#!/usr/bin/env bash
# tests/sandbox/status.sh — health/config report for the LOCAL SANDBOX.
#
#   ./status.sh            full report; exit 0 only if everything is healthy
#   ./status.sh --no-token skip the admin-token/API checks (no credentials used)
#   ./status.sh --quiet    one line per check
#
# Read-only: never starts, stops or mutates anything.
set -uo pipefail

# shellcheck source=lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

USE_TOKEN=1
QUIET=0
for arg in "$@"; do
  case "$arg" in
    --no-token) USE_TOKEN=0 ;;
    --quiet) QUIET=1 ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) die "unknown argument: $arg" ;;
  esac
done

FAILS=0
fails() { FAILS=$((FAILS + 1)); }

c_ok()   { printf '%s[ ok ]%s %s\n'   "$C_GREEN"  "$C_RESET" "$*"; }
c_bad()  { printf '%s[FAIL]%s %s\n'   "$C_RED"    "$C_RESET" "$*"; fails; }
c_warn() { printf '%s[WARN]%s %s\n'   "$C_YELLOW" "$C_RESET" "$*"; }

have() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- versions --
section() { [ "$QUIET" -eq 1 ] || { hr; printf '%s\n' "$*"; }; }

section "versions / paths"
if have jq; then c_ok "jq $(jq --version 2>/dev/null | tr -d '\n')"; else c_bad "jq missing (required by sandbox scripts)"; fi
if [ -x "$MP_BIN" ]; then c_ok "mailpit $("$MP_BIN" version 2>/dev/null | head -1) -> $MP_BIN"; else c_bad "mailpit not installed at $MP_BIN (run up.sh)"; fi
if kc_installed; then c_ok "keycloak distribution $(kc_version) -> $KC_HOME"; else c_bad "keycloak not installed at $KC_HOME (run up.sh)"; fi
if [ -x "$SB_JAVA_BIN" ]; then c_ok "java $("$SB_JAVA_BIN" -version 2>&1 | head -1) ($SB_JAVA_HOME)"; else c_bad "java 21 missing at $SB_JAVA_HOME"; fi

# ------------------------------------------------------------------ procs ---
section "processes / listeners (loopback only)"
for spec in "keycloak:$KC_PORT" "keycloak-mgmt:$KC_MGMT_PORT" "mailpit-web:$MP_WEB_PORT" "mailpit-smtp:$MP_SMTP_PORT"; do
  name="${spec%%:*}"; port="${spec##*:}"
  if is_listening "$port"; then
    addr="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $9}')"
    pid="$(listener_pids "$port" | awk '{print $1}')"
    case "$addr" in
      127.0.0.1:*) c_ok "$name listening on $addr (pid $pid)" ;;
      *) c_bad "$name listening on $addr — NOT loopback-only (pid $pid)";;
    esac
  elif [ "$name" = "keycloak-mgmt" ]; then
    c_warn "$name is not listening on 127.0.0.1:$port (health probes unavailable)"
  else
    c_bad "$name is NOT listening on 127.0.0.1:$port"
  fi
done

# ------------------------------------------------------------------- http ---
section "HTTP health"
code="$(http_code "$MP_API/messages?limit=1")"
if [ "$code" = "200" ]; then
  c_ok "mailpit API $MP_API 200 (messages stored: $(mp_message_count))"
else
  c_bad "mailpit API $MP_API -> $code"
fi

code="$(http_code "$KC_URL/realms/$ADMIN_REALM")"
[ "$code" = "200" ] && c_ok "keycloak master realm $KC_URL/realms/$ADMIN_REALM 200" || c_bad "keycloak master realm -> $code"

code="$(http_code "$KC_MGMT_URL/health/ready")"
if [ "$code" = "200" ]; then
  c_ok "keycloak /health/ready 200 on management interface $KC_MGMT_URL"
else
  c_warn "keycloak $KC_MGMT_URL/health/ready -> $code (readiness probe unavailable; realm check above is authoritative)"
fi

code="$(http_code "$KC_URL/realms/$REALM/.well-known/openid-configuration")"
if [ "$code" = "200" ]; then
  issuer="$(curl -sS -m 10 "$KC_URL/realms/$REALM/.well-known/openid-configuration" | jq -r '.issuer')"
  [ "$issuer" = "$KC_URL/realms/$REALM" ] \
    && c_ok "realm $REALM OIDC discovery 200 (issuer $issuer)" \
    || c_bad "realm $REALM issuer mismatch: $issuer"
else
  c_bad "realm $REALM OIDC discovery -> $code"
fi

# -------------------------------------------------------------- admin API ---
if [ "$USE_TOKEN" -eq 1 ]; then
  section "admin API (test-only credentials from $SB_SECRETS/sandbox.env)"
  if ! load_secrets; then
    c_bad "no generated credentials at $SB_SECRETS/sandbox.env — run up.sh"
  elif ! kc_healthy; then
    c_bad "keycloak not reachable; skipping admin API checks"
  else
    token="$(admin_token 2>/dev/null || true)"
    if [ -z "$token" ]; then
      c_bad "could not obtain admin token (master/admin-cli password grant)"
    else
      c_ok "admin token acquired for user '${SANDBOX_ADMIN_USER}' (token redacted, $((${#token} - 200))-ish chars) "
      H=(-H "Authorization: Bearer $token")
      realm_json="$(curl -sS -m 20 "${H[@]}" "$KC_URL/admin/realms/$REALM" 2>/dev/null)"
      if printf '%s' "$realm_json" | jq -e '.realm' >/dev/null 2>&1; then
        c_ok "realm $REALM present (registrationAllowed=$(printf '%s' "$realm_json" | jq -r '.registrationAllowed'), verifyEmail=$(printf '%s' "$realm_json" | jq -r '.verifyEmail'))"
        c_ok "realm SMTP -> $(printf '%s' "$realm_json" | jq -r '.smtpServer.host'):$(printf '%s' "$realm_json" | jq -r '.smtpServer.port') from=$(printf '%s' "$realm_json" | jq -r '.smtpServer.from')"
        users="$(curl -sS -m 20 "${H[@]}" "$KC_URL/admin/realms/$REALM/users?briefRepresentation=true" 2>/dev/null | jq 'length' 2>/dev/null)"
        c_ok "realm users: ${users:-unknown}"
      else
        c_bad "realm $REALM not readable via admin API: $(printf '%s' "$realm_json" | head -c 200)"
      fi
      client_json="$(curl -sS -m 20 "${H[@]}" "$KC_URL/admin/realms/$REALM/clients?clientId=$CLIENT_ID" 2>/dev/null)"
      if [ "$(printf '%s' "$client_json" | jq 'length' 2>/dev/null)" = "1" ]; then
        c_ok "client $CLIENT_ID present (public=$(printf '%s' "$client_json" | jq -r '.[0].publicClient'), pkce=$(printf '%s' "$client_json" | jq -r '.[0].attributes["pkce.code.challenge.method"] // "unset"'), directGrant=$(printf '%s' "$client_json" | jq -r '.[0].directAccessGrantsEnabled'))"
      else
        c_bad "client $CLIENT_ID not found in realm $REALM"
      fi

      # service-account client + client_credentials token + Admin API access
      admin_json="$(curl -sS -m 20 "${H[@]}" "$KC_URL/admin/realms/$REALM/clients?clientId=$ADMIN_CLIENT_ID" 2>/dev/null)"
      if [ "$(printf '%s' "$admin_json" | jq 'length' 2>/dev/null)" = "1" ]; then
        c_ok "admin client $ADMIN_CLIENT_ID present (confidential=$(printf '%s' "$admin_json" | jq -r '.[0].publicClient | not'), serviceAccount=$(printf '%s' "$admin_json" | jq -r '.[0].serviceAccountsEnabled'))"
      else
        c_bad "admin client $ADMIN_CLIENT_ID not found (run tests/sandbox/up.sh)"
      fi
      cc_token="$(client_credentials_token 2>/dev/null || true)"
      if [ -z "$cc_token" ]; then
        c_bad "client_credentials token for '$ADMIN_CLIENT_ID' failed (secret in $SB_SECRETS/sandbox.env)"
      else
        cc_code="$(curl -sS -m 20 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $cc_token" "$KC_URL/admin/realms/$REALM/users?briefRepresentation=true")"
        if [ "$cc_code" = "200" ]; then
          c_ok "client_credentials token works: GET /admin/realms/$REALM/users 200 (no 60s TTL churn)"
        else
          c_bad "client_credentials token present but Admin API users -> $cc_code (role mapping broken?)"
        fi
      fi
    fi
  fi
fi

# ---------------------------------------------------------------- summary ---
section "summary"
if [ "$FAILS" -eq 0 ]; then
  printf '%s[ ok ]%s LOCAL SANDBOX healthy: keycloak %s (realm %s) + mailpit %s\n' "$C_GREEN" "$C_RESET" "$KC_URL" "$REALM" "$MP_URL"
  [ "$QUIET" -eq 1 ] || hr
  exit 0
fi
printf '%s[FAIL]%s %d check(s) failed — see above. Logs: %s , %s\n' "$C_RED" "$C_RESET" "$FAILS" "$KC_LOG" "$MP_LOG"
[ "$QUIET" -eq 1 ] || hr
exit 1
