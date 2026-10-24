#!/usr/bin/env bash
# tests/sandbox/env.sh — export LOCAL SANDBOX endpoints and test-only credentials.
#
#   source tests/sandbox/env.sh     # exports into your shell (Go tests, curl, ...)
#   bash tests/sandbox/env.sh       # prints what it would export (secrets masked)
#
# Safe to source repeatedly. Everything points at 127.0.0.1 only.
set -uo pipefail

SB_ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$SB_ENV_DIR/lib.sh"

# Load generated test-only credentials if up.sh has run, otherwise fall back to
# unset variables (scripts that need them fail loudly on their own).
load_secrets || true

# ------------------------------------------------------------- endpoints ----
export SANDBOX_ROOT="$SB_ROOT"
export SANDBOX_DIR="$SB_DIR"
export SANDBOX_LOG_DIR="$SB_LOGS"
export SANDBOX_RUN_DIR="$SB_RUN"
export SANDBOX_JAVA_HOME="$SB_JAVA_HOME"

export KEYCLOAK_URL="$KC_URL"
export KEYCLOAK_REALM="$REALM"
export KEYCLOAK_ADMIN_REALM="$ADMIN_REALM"
export KEYCLOAK_CLIENT_ID="$CLIENT_ID"
export KEYCLOAK_OIDC_ISSUER="$KC_URL/realms/$REALM"
export KEYCLOAK_OIDC_DISCOVERY="$KC_URL/realms/$REALM/.well-known/openid-configuration"
export KEYCLOAK_AUTHORIZATION_ENDPOINT="$KC_URL/realms/$REALM/protocol/openid-connect/auth"
export KEYCLOAK_TOKEN_ENDPOINT="$KC_URL/realms/$REALM/protocol/openid-connect/token"
export KEYCLOAK_USERINFO_ENDPOINT="$KC_URL/realms/$REALM/protocol/openid-connect/userinfo"
export KEYCLOAK_JWKS_URI="$KC_URL/realms/$REALM/protocol/openid-connect/certs"
export KEYCLOAK_ADMIN_API="$KC_URL/admin/realms/$REALM"

# test-only admin (master realm); password lives in .sandbox/secrets/sandbox.env
export SANDBOX_ADMIN_USER="${SANDBOX_ADMIN_USER:-}"
export SANDBOX_ADMIN_PASSWORD="${SANDBOX_ADMIN_PASSWORD:-}"
export SANDBOX_TEST_USER_PASSWORD="${SANDBOX_TEST_USER_PASSWORD:-}"
export SANDBOX_TEST_EMAIL_DOMAIN="$TEST_EMAIL_DOMAIN"
# confidential service-account client (client_credentials) for Admin API automation
export SANDBOX_ADMIN_CLIENT_ID="${SANDBOX_ADMIN_CLIENT_ID:-$ADMIN_CLIENT_ID}"
export SANDBOX_ADMIN_CLIENT_SECRET="${SANDBOX_ADMIN_CLIENT_SECRET:-}"
export KEYCLOAK_ADMIN_CLIENT_ID="$ADMIN_CLIENT_ID"

export MAILPIT_HTTP_URL="$MP_URL"
export MAILPIT_API_URL="$MP_API"
export MAILPIT_SMTP_ADDR="$MP_SMTP_HOST:$MP_SMTP_PORT"
export MAILPIT_SMTP_HOST="$MP_SMTP_HOST"
export MAILPIT_SMTP_PORT="$MP_SMTP_PORT"
export MAILPIT_UI_URL="$MP_URL"

# the app under test (not started by this sandbox) — used as the OIDC redirect
export ONBOARDING_BASE_URL="http://127.0.0.1:8788"
export ONBOARDING_OIDC_REDIRECT_URI="$APP_CALLBACK"
export ONBOARDING_UI_OIDC_REDIRECT_URI="$APP_UI_CALLBACK"
export ONBOARDING_SMTP_FROM="$SMTP_FROM"

# Convenience aliases matching the ONBOARD_* env naming used by the service.
export ONBOARD_OIDC_ISSUER="$KEYCLOAK_OIDC_ISSUER"
export ONBOARD_OIDC_CLIENT_ID="$KEYCLOAK_CLIENT_ID"
export ONBOARD_OIDC_REDIRECT_URI="$APP_CALLBACK"
export ONBOARD_SMTP_HOST="$MP_SMTP_HOST"
export ONBOARD_SMTP_PORT="$MP_SMTP_PORT"
export ONBOARD_SMTP_FROM="$SMTP_FROM"

# Executed (not sourced): print a masked summary.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  mask() { if [ -n "${1:-}" ]; then printf '******** (%d chars)' "${#1}"; else printf '<unset>'; fi; }
  hr
  echo "LOCAL SANDBOX environment (loopback only) — source this file, don't run it"
  hr
  printf '  KEYCLOAK_URL            %s\n' "$KEYCLOAK_URL"
  printf '  KEYCLOAK_REALM          %s\n' "$KEYCLOAK_REALM"
  printf '  KEYCLOAK_CLIENT_ID      %s\n' "$KEYCLOAK_CLIENT_ID"
  printf '  KEYCLOAK_OIDC_ISSUER    %s\n' "$KEYCLOAK_OIDC_ISSUER"
  printf '  MAILPIT_HTTP_URL        %s\n' "$MAILPIT_HTTP_URL"
  printf '  MAILPIT_SMTP_ADDR       %s\n' "$MAILPIT_SMTP_ADDR"
  printf '  ONBOARDING_BASE_URL     %s\n' "$ONBOARDING_BASE_URL"
  printf '  ONBOARDING_OIDC_REDIRECT_URI %s\n' "$ONBOARDING_OIDC_REDIRECT_URI"
  printf '  SANDBOX_ADMIN_USER      %s (password %s)\n' "${SANDBOX_ADMIN_USER:-<unset>}" "$(mask "${SANDBOX_ADMIN_PASSWORD:-}")"
  printf '  SANDBOX_ADMIN_CLIENT_ID %s (secret %s)\n' "${SANDBOX_ADMIN_CLIENT_ID:-<unset>}" "$(mask "${SANDBOX_ADMIN_CLIENT_SECRET:-}")"
  printf '  SANDBOX_TEST_USER_PW    %s\n' "$(mask "${SANDBOX_TEST_USER_PASSWORD:-}")"
  printf '  credentials file        %s\n' "$SB_SECRETS/sandbox.env"
  hr
  echo "  usage: source tests/sandbox/env.sh"
  hr
fi
