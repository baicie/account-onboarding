#!/usr/bin/env bash
# tests/sandbox/lib.sh — shared config + helpers for the LOCAL SANDBOX.
# shellcheck shell=bash disable=SC2034
#
# Sourced by up.sh / down.sh / reset.sh / status.sh / verify.sh / env.sh.
#
# HARD RULES baked into this file:
#   * everything binds 127.0.0.1 only
#   * runtime data, downloaded binaries and generated test-only credentials live
#     under <repo>/.sandbox (gitignored)
#   * nothing here talks to a real deployment, a real mailbox or the internet
#     (except the one-time artifact download in up.sh)
#
# Do not source this from production code.

# ---------------------------------------------------------------- paths -----
SB_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SB_ROOT="$(cd "$SB_LIB_DIR/../.." && pwd)"
SB_DIR="${SB_DIR:-$SB_ROOT/.sandbox}"
SB_DIST="$SB_DIR/dist"
SB_BIN="$SB_DIR/bin"
SB_DATA="$SB_DIR/data"
SB_LOGS="$SB_DIR/logs"
SB_RUN="$SB_DIR/run"
SB_SECRETS="$SB_DIR/secrets"
SB_TMP="$SB_DIR/tmp"
SB_REALM_DIR="$SB_LIB_DIR/realm"

# ------------------------------------------------------------ versions ------
KC_VERSION="${KC_VERSION:-26.8.1}"
MP_VERSION="${MP_VERSION:-1.32.0}"

# ------------------------------------------------------------- java ---------
# Keycloak 26.x needs Java 17/21. The system java on this machine is 25 and is
# not supported by the Keycloak distribution; prefer Homebrew's JDK 21.
SB_JAVA_HOME="${SB_JAVA_HOME:-/opt/homebrew/opt/openjdk@21/libexec/openjdk.jdk/Contents/Home}"
SB_JAVA_BIN="$SB_JAVA_HOME/bin/java"

# --------------------------------------------------------- components -------
KC_HOME="${KC_HOME:-$SB_DIR/keycloak}"
KC_TARBALL="keycloak-$KC_VERSION.tar.gz"
KC_RELEASE_URL="https://github.com/keycloak/keycloak/releases/download/$KC_VERSION/$KC_TARBALL"
KC_MIRROR_URL="https://gh-proxy.com/$KC_RELEASE_URL"
KC_MD5_URL="$KC_RELEASE_URL.md5"
# Keycloak also publishes the (byte-identical) distribution to Maven Central.
# The Aliyun mirror is what actually works from this network — see README.
KC_MAVEN_URL="https://repo1.maven.org/maven2/org/keycloak/keycloak-quarkus-dist/$KC_VERSION/keycloak-quarkus-dist-$KC_VERSION.tar.gz"
KC_MAVEN_MIRROR_URL="https://maven.aliyun.com/repository/public/org/keycloak/keycloak-quarkus-dist/$KC_VERSION/keycloak-quarkus-dist-$KC_VERSION.tar.gz"

MP_PLATFORM="${MP_PLATFORM:-darwin-arm64}"
MP_TARBALL="mailpit-$MP_PLATFORM.tar.gz"
MP_RELEASE_URL="https://github.com/axllent/mailpit/releases/download/v$MP_VERSION/$MP_TARBALL"
MP_MIRROR_URL="https://gh-proxy.com/$MP_RELEASE_URL"
MP_BIN="$SB_BIN/mailpit"

KC_PID_FILE="$SB_RUN/keycloak.pid"
MP_PID_FILE="$SB_RUN/mailpit.pid"
KC_LOG="$SB_LOGS/keycloak.log"
MP_LOG="$SB_LOGS/mailpit.log"

# ------------------------------------------------------------- network ------
# Loopback ONLY. Never 0.0.0.0.
KC_HOST=127.0.0.1
KC_PORT=8180
KC_URL="http://$KC_HOST:$KC_PORT"
# Keycloak/Quarkus exposes its health probes on the management interface.
KC_MGMT_PORT=9000
KC_MGMT_URL="http://$KC_HOST:$KC_MGMT_PORT"

MP_SMTP_HOST=127.0.0.1
MP_SMTP_PORT=1025
MP_WEB_HOST=127.0.0.1
MP_WEB_PORT=8025
MP_URL="http://$MP_WEB_HOST:$MP_WEB_PORT"
MP_API="$MP_URL/api/v1"

# ------------------------------------------------------------- identity -----
REALM="${KC_REALM:-moonbyte-test}"
ADMIN_REALM=master
ADMIN_CLI=admin-cli
CLIENT_ID="${KC_CLIENT_ID:-onboarding-test}"
# Confidential service-account client for automated Admin API access
# (client_credentials; no 60s master-token churn). Secret lives in .sandbox/secrets.
ADMIN_CLIENT_ID="${KC_ADMIN_CLIENT_ID:-onboarding-admin}"
APP_CALLBACK="http://127.0.0.1:8788/oidc/callback"
APP_UI_CALLBACK="http://127.0.0.1:8788/ui/oauth/callback"
SMTP_FROM="no-reply@moonbyte.test"
TEST_EMAIL_DOMAIN="${SANDBOX_EMAIL_DOMAIN:-moonbyte.test}"

# ------------------------------------------------------------- output -------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_RESET=$'\033[0m'; C_RED=$'\033[31m'; C_GREEN=$'\033[32m'
  C_YELLOW=$'\033[33m'; C_BLUE=$'\033[34m'; C_DIM=$'\033[2m'
else
  C_RESET=''; C_RED=''; C_GREEN=''; C_YELLOW=''; C_BLUE=''; C_DIM=''
fi

log()   { printf '%s[sandbox]%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()    { printf '%s[   ok  ]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn()  { printf '%s[ warn  ]%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
die()   { printf '%s[ FAIL  ]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }
hr()    { printf '%s\n' "------------------------------------------------------------------------"; }

have() { command -v "$1" >/dev/null 2>&1; }

# ------------------------------------------------------------- checks -------
require_cmds() {
  local missing=()
  local c
  for c in "$@"; do have "$c" || missing+=("$c"); done
  if [ "${#missing[@]}" -gt 0 ]; then
    die "missing required command(s): ${missing[*]}"
  fi
}

mkdirs() {
  mkdir -p "$SB_DIST" "$SB_BIN" "$SB_DATA" "$SB_LOGS" "$SB_RUN" "$SB_SECRETS" "$SB_TMP"
}

# ------------------------------------------------------------ secrets -------
# Generated once, stored chmod 600 under .sandbox/secrets (gitignored).
# These are TEST-ONLY credentials for this loopback sandbox.
load_secrets() {
  if [ -s "$SB_SECRETS/sandbox.env" ]; then
    set -a
    # shellcheck disable=SC1091
    . "$SB_SECRETS/sandbox.env"
    set +a
    return 0
  fi
  return 1
}

ensure_secrets() {
  mkdirs
  if [ -s "$SB_SECRETS/sandbox.env" ]; then
    load_secrets
    return 0
  fi
  local admin_pw user_pw client_secret
  admin_pw="$(openssl rand -hex 16)"
  user_pw="$(openssl rand -hex 16)"
  client_secret="$(openssl rand -hex 32)"
  umask 077
  cat > "$SB_SECRETS/sandbox.env" <<EOF
# LOCAL SANDBOX test-only credentials. Generated by tests/sandbox/up.sh.
# NEVER commit this file (.sandbox/ is gitignored) and never reuse these values.
SANDBOX_ADMIN_USER=kcadmin
SANDBOX_ADMIN_PASSWORD=$admin_pw
SANDBOX_TEST_USER_PASSWORD=$user_pw
SANDBOX_ADMIN_CLIENT_ID=$ADMIN_CLIENT_ID
SANDBOX_ADMIN_CLIENT_SECRET=$client_secret
EOF
  chmod 600 "$SB_SECRETS/sandbox.env"
  load_secrets
  ok "generated test-only credentials at $SB_SECRETS/sandbox.env (chmod 600)"
}

# Backfill keys added after the first version of sandbox.env (idempotent).
ensure_admin_client_secret() {
  mkdirs
  [ -s "$SB_SECRETS/sandbox.env" ] || ensure_secrets
  local changed=0
  if ! grep -q '^SANDBOX_ADMIN_CLIENT_ID=' "$SB_SECRETS/sandbox.env" 2>/dev/null; then
    umask 077
    printf 'SANDBOX_ADMIN_CLIENT_ID=%s\n' "$ADMIN_CLIENT_ID" >> "$SB_SECRETS/sandbox.env"
    changed=1
  fi
  if ! grep -q '^SANDBOX_ADMIN_CLIENT_SECRET=' "$SB_SECRETS/sandbox.env" 2>/dev/null; then
    umask 077
    printf 'SANDBOX_ADMIN_CLIENT_SECRET=%s\n' "$(openssl rand -hex 32)" >> "$SB_SECRETS/sandbox.env"
    changed=1
  fi
  if [ "$changed" -eq 1 ]; then
    chmod 600 "$SB_SECRETS/sandbox.env"
    ok "added service-account credentials to $SB_SECRETS/sandbox.env (chmod 600)"
  fi
  load_secrets
}

# ------------------------------------------------------------- http ---------
# http_code <url> [extra curl args...] -> prints status code, exit 0 always
http_code() {
  local url="$1"; shift || true
  local code
  # curl prints '000' via -w even when the connection fails, and exits non-zero;
  # never let the exit status leak into the caller (scripts run with set -e).
  code="$(curl -s -o /dev/null -m 10 -w '%{http_code}' "$@" "$url" 2>/dev/null)" || true
  printf '%s' "${code:-000}"
}

# wait_http <url> <timeout_seconds> <expected_code>
# NOTE: one declaration per line — bash expands all words of a `local` command
# before assigning, so `${4:-$url}` in the same `local` trips `set -u`.
wait_http() {
  local url="$1"
  local timeout="${2:-60}"
  local want="${3:-200}"
  local waited=0 code
  while [ "$waited" -lt "$timeout" ]; do
    code="$(http_code "$url")"
    if [ "$code" = "$want" ]; then return 0; fi
    sleep 1; waited=$((waited + 1))
  done
  return 1
}

is_listening() {
  local port="$1"
  lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1
}

listener_pids() {
  local port="$1"
  lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | tr '\n' ' '
}

port_owner() {
  local port="$1"
  lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $1" pid="$2}'
}

# ------------------------------------------------------------ processes -----
pid_alive() {
  local pid="$1"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

# Match a process against expected command substrings (safety before kill).
pid_matches() {
  local pid="$1"; shift
  local cmd
  cmd="$(ps -p "$pid" -o command= 2>/dev/null || true)"
  [ -n "$cmd" ] || return 1
  local pat
  for pat in "$@"; do
    case "$cmd" in *"$pat"*) return 0 ;; esac
  done
  return 1
}

write_pid() { local f="$1" p="$2"; mkdir -p "$(dirname "$f")"; printf '%s\n' "$p" > "$f"; }
read_pid()  { local f="$1"; [ -s "$f" ] && tr -d '[:space:]' < "$f" || true; }
rm_pid()    { rm -f "$1"; }

# ----------------------------------------------------------- keycloak -------
kc_installed()  { [ -x "$KC_HOME/bin/kc.sh" ]; }
kc_healthy()    { [ "$(http_code "$KC_URL/realms/$ADMIN_REALM")" = "200" ]; }
mp_healthy()    { [ "$(http_code "$MP_API/messages?limit=1")" = "200" ]; }

# Always run the Keycloak CLI with JDK 21 (the system JVM is newer/unsupported).
kc_version() {
  [ -x "$KC_HOME/bin/kc.sh" ] || return 1
  JAVA_HOME="$SB_JAVA_HOME" "$KC_HOME/bin/kc.sh" --version 2>/dev/null | head -1
}

# admin_token: master-realm admin-cli password grant (direct access grants).
# Memoized in _SB_ADMIN_TOKEN for the lifetime of the shell.
admin_token() {
  local force="${1:-}"
  if [ -n "${_SB_ADMIN_TOKEN:-}" ] && [ "$force" != "--refresh" ]; then
    printf '%s' "$_SB_ADMIN_TOKEN"; return 0
  fi
  load_secrets || die "no $SB_SECRETS/sandbox.env — run tests/sandbox/up.sh first"
  local resp
  resp="$(curl -sS -m 20 -X POST \
    "$KC_URL/realms/$ADMIN_REALM/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode 'grant_type=password' \
    --data-urlencode 'client_id=admin-cli' \
    --data-urlencode "username=$SANDBOX_ADMIN_USER" \
    --data-urlencode "password=$SANDBOX_ADMIN_PASSWORD")" || die "admin token request failed (keycloak down?)"
  _SB_ADMIN_TOKEN="$(printf '%s' "$resp" | jq -r '.access_token // empty')"
  [ -n "$_SB_ADMIN_TOKEN" ] || die "admin token request returned no access_token: $(printf '%s' "$resp" | jq -c '{error,error_description}' 2>/dev/null)"
  printf '%s' "$_SB_ADMIN_TOKEN"
}

# admin_api <method> <path-after-/admin> [curl args...] -> body, prints http code to stderr? 
# Use compact helpers instead:
admin_get() { # admin_get <path> -> body (fails on non-2xx)
  local path="$1" body code
  body="$(mktemp "$SB_TMP/admin.XXXXXX")"
  code="$(curl -sS -m 30 -o "$body" -w '%{http_code}' \
    -H "Authorization: Bearer $(admin_token)" "$KC_URL/admin$path")" || { rm -f "$body"; die "admin GET $path failed (curl)"; }
  if [ "${code:0:1}" != "2" ]; then
    local err; err="$(cat "$body" 2>/dev/null | head -c 400)"
    rm -f "$body"; die "admin GET $path -> HTTP $code: $err"
  fi
  cat "$body"; rm -f "$body"
}

# client_credentials_token: Admin API token via the service-account client
# ($ADMIN_CLIENT_ID, secret in .sandbox/secrets/sandbox.env). No password grant,
# no 60s TTL surprises — preferred for automated/CI use.
# Prints the access token; returns non-zero with a message on failure.
client_credentials_token() {
  load_secrets || { warn "no $SB_SECRETS/sandbox.env — run tests/sandbox/up.sh first"; return 1; }
  [ -n "${SANDBOX_ADMIN_CLIENT_SECRET:-}" ] || { warn "SANDBOX_ADMIN_CLIENT_SECRET missing (old sandbox.env? run tests/sandbox/up.sh)"; return 1; }
  local client_id="${SANDBOX_ADMIN_CLIENT_ID:-$ADMIN_CLIENT_ID}"
  local resp token
  resp="$(curl -sS -m 20 -X POST \
    "$KC_URL/realms/$REALM/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    -d 'grant_type=client_credentials' \
    --data-urlencode "client_id=$client_id" \
    --data-urlencode "client_secret=$SANDBOX_ADMIN_CLIENT_SECRET")" || { warn "client_credentials request failed (keycloak down?)"; return 1; }
  token="$(printf '%s' "$resp" | jq -r '.access_token // empty' 2>/dev/null)"
  if [ -z "$token" ]; then
    warn "client_credentials token denied: $(printf '%s' "$resp" | jq -c '{error,error_description}' 2>/dev/null || printf '%s' "$resp" | head -c 200)"
    return 1
  fi
  printf '%s' "$token"
}

java_ok() {
  [ -x "$SB_JAVA_BIN" ] || return 1
  local v
  v="$("$SB_JAVA_BIN" -version 2>&1 | awk -F'"' '/version/{print $2; exit}')"
  case "$v" in
    17*|18*|19*|20*|21*) return 0 ;;
    *) warn "JAVA_HOME points to java $v at $SB_JAVA_HOME — Keycloak $KC_VERSION needs 17/21"; return 1 ;;
  esac
}

# ------------------------------------------------------------- mailpit ------
mp_message_count() {
  curl -sS -m 10 "$MP_API/messages?limit=0" 2>/dev/null | jq -r '.total // 0' 2>/dev/null || echo 0
}
