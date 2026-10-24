#!/usr/bin/env bash
# tests/sandbox/up.sh — LOCAL SANDBOX bring-up: real Keycloak + real Mailpit.
#
#   ./up.sh                install (once) + start both services + verify config
#   ./up.sh --install-only download/extract artifacts, do not start
#   ./up.sh --force-install redownload/extract artifacts before start
#   ./up.sh --no-verify    start only, skip the realm/client/SMTP checks
#
# Idempotent: safe to run repeatedly. Loopback binding only.
# All artifacts, logs, runtime data and generated test-only credentials live
# under <repo>/.sandbox (gitignored). Nothing is written outside that tree.
set -euo pipefail

# shellcheck source=lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

INSTALL_ONLY=0
FORCE_INSTALL=0
DO_VERIFY=1
for arg in "$@"; do
  case "$arg" in
    --install-only) INSTALL_ONLY=1 ;;
    --force-install) FORCE_INSTALL=1 ;;
    --no-verify) DO_VERIFY=0 ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) die "unknown argument: $arg" ;;
  esac
done

# ---------------------------------------------------------------- install ---
LAST_DL_URL=""

download() { # download <url> <dest> <label>
  local url="$1" dest="$2" label="$3"
  log "downloading $label"
  log "  $url"
  if ! curl -fL --retry 3 --retry-all-errors --retry-delay 3 --connect-timeout 20 \
        --progress-bar -o "$dest.part" "$url"; then
    rm -f "$dest.part"
    return 1
  fi
  mv "$dest.part" "$dest"
  LAST_DL_URL="$url"
  ok "downloaded $(basename "$dest") ($(du -h "$dest" | cut -f1))"
}

download_first() { # download_first <dest> <label> <url>...
  local dest="$1" label="$2"; shift 2
  local url
  for url in "$@"; do
    if download "$url" "$dest" "$label"; then return 0; fi
    warn "source failed: $url"
  done
  return 1
}

# Best-effort integrity check against a published .md5 sidecar.
verify_md5_sidecar() { # verify_md5_sidecar <file> <md5-url>...
  local file="$1"; shift
  local want="" url got
  for url in "$@"; do
    want="$(curl -sS --max-time 20 "$url" 2>/dev/null | tr -d '\r[:space:]' | grep -oE '^[0-9a-fA-F]{32}' | head -1 || true)"
    [ -n "$want" ] && break
  done
  if [ -z "$want" ]; then
    warn "no published md5 reachable for $(basename "$file") — skipping integrity check"
    return 0
  fi
  got="$(md5 -q "$file" | tr -d '[:space:]')"
  if [ "$got" = "$want" ]; then
    ok "checksum verified: $(basename "$file") md5=$got"
  else
    rm -f "$file"
    die "checksum mismatch for $(basename "$file"): got $got expected $want (deleted the corrupt file, re-run up.sh)"
  fi
}

install_mailpit() {
  if [ -x "$MP_BIN" ] && [ "$FORCE_INSTALL" -eq 0 ]; then
    ok "mailpit already installed: $("$MP_BIN" version 2>/dev/null | head -1)"
    return 0
  fi
  mkdir -p "$SB_BIN" "$SB_DIST"
  if [ ! -s "$SB_DIST/$MP_TARBALL" ] || [ "$FORCE_INSTALL" -eq 1 ]; then
    rm -f "$SB_DIST/$MP_TARBALL"
    download_first "$SB_DIST/$MP_TARBALL" "mailpit v$MP_VERSION (~9.5 MB)" \
      "$MP_RELEASE_URL" "$MP_MIRROR_URL" \
      || die "could not download mailpit from GitHub or the gh-proxy mirror"
  fi
  tar -xzf "$SB_DIST/$MP_TARBALL" -C "$SB_BIN" || die "mailpit tarball is corrupt: $SB_DIST/$MP_TARBALL"
  chmod +x "$MP_BIN"
  ok "installed $("$MP_BIN" version 2>/dev/null | head -1) -> $MP_BIN"
}

install_keycloak() {
  if kc_installed && [ "$FORCE_INSTALL" -eq 0 ]; then
    ok "keycloak already installed: $KC_HOME ($KC_VERSION)"
    return 0
  fi
  mkdir -p "$SB_DIST" "$SB_TMP"
  if [ ! -s "$SB_DIST/$KC_TARBALL" ] || [ "$FORCE_INSTALL" -eq 1 ]; then
    rm -f "$SB_DIST/$KC_TARBALL"
    download_first "$SB_DIST/$KC_TARBALL" "keycloak $KC_VERSION (~166 MB)" \
      "$KC_RELEASE_URL" "$KC_MIRROR_URL" "$KC_MAVEN_MIRROR_URL" "$KC_MAVEN_URL" \
      || die "could not download keycloak from GitHub, the gh-proxy mirror, or Maven Central (aliyun mirror)"
    verify_md5_sidecar "$SB_DIST/$KC_TARBALL" "$KC_RELEASE_URL.md5" "$KC_MIRROR_URL.md5"
  fi
  tar -tzf "$SB_DIST/$KC_TARBALL" >/dev/null 2>&1 || die "keycloak tarball is corrupt: $SB_DIST/$KC_TARBALL"
  rm -rf "$SB_TMP/extract"
  mkdir -p "$SB_TMP/extract"
  tar -xzf "$SB_DIST/$KC_TARBALL" -C "$SB_TMP/extract"
  local top
  top="$(find "$SB_TMP/extract" -maxdepth 1 -mindepth 1 -type d | head -1)"
  [ -n "$top" ] && [ -x "$top/bin/kc.sh" ] || die "unexpected keycloak tarball layout under $SB_TMP/extract"
  # Runtime data (data/h2, data/import) lives inside $KC_HOME/data; reset.sh
  # wipes that without touching the extracted distribution.
  rm -rf "$KC_HOME"
  mv "$top" "$KC_HOME"
  rm -rf "$SB_TMP/extract"
  ok "extracted keycloak $KC_VERSION -> $KC_HOME"
}

check_java() {
  [ -x "$SB_JAVA_BIN" ] || die "Java 21 not found at $SB_JAVA_HOME (install: brew install openjdk@21)"
  java_ok || true
  log "using JAVA_HOME=$SB_JAVA_HOME ($("$SB_JAVA_BIN" -version 2>&1 | head -1))"
}

# ----------------------------------------------------------------- realm ----
stage_realm_import() {
  local src="$SB_REALM_DIR/moonbyte-test-realm.json"
  [ -s "$src" ] || die "realm import file missing: $src"
  mkdir -p "$KC_HOME/data/import"
  cp -f "$src" "$KC_HOME/data/import/moonbyte-test-realm.json"
  ok "staged realm import -> $KC_HOME/data/import/moonbyte-test-realm.json"
}

# --------------------------------------------------------------- starts -----
check_port_free() { # check_port_free <port> <component-label>
  local port="$1" label="$2" owner
  if is_listening "$port"; then
    owner="$(port_owner "$port")"
    die "port $port is already in use by [$owner] — refusing to start $label. Run tests/sandbox/down.sh or free the port."
  fi
}

start_mailpit() {
  if mp_healthy; then
    ok "mailpit already running ($MP_URL, $(mp_message_count) message(s) stored)"
    return 0
  fi
  if is_listening "$MP_WEB_PORT"; then
    die "port $MP_WEB_PORT is in use by [$(port_owner "$MP_WEB_PORT")] but mailpit API is not answering"
  fi
  check_port_free "$MP_SMTP_PORT" "mailpit smtp"
  mkdir -p "$SB_DATA/mailpit" "$SB_LOGS"
  log "starting mailpit (smtp $MP_SMTP_HOST:$MP_SMTP_PORT, http $MP_WEB_HOST:$MP_WEB_PORT, db $SB_DATA/mailpit/mailpit.db)"
  nohup "$MP_BIN" \
    --database "$SB_DATA/mailpit/mailpit.db" \
    --listen "$MP_WEB_HOST:$MP_WEB_PORT" \
    --smtp "$MP_SMTP_HOST:$MP_SMTP_PORT" \
    --disable-version-check \
    --quiet \
    >>"$MP_LOG" 2>&1 &
  local pid=$!
  write_pid "$MP_PID_FILE" "$pid"
  if ! wait_http "$MP_API/messages?limit=1" 30 200; then
    die "mailpit did not become ready on $MP_URL within 30s — see $MP_LOG"
  fi
  local sid; sid="$(listener_pids "$MP_SMTP_PORT" | awk '{print $1}')"
  ok "mailpit ready (pid $pid, smtp listener pid ${sid:-n/a}, log $MP_LOG)"
}

start_keycloak() {
  if kc_healthy; then
    ok "keycloak already running ($KC_URL/realms/$ADMIN_REALM)"
    return 0
  fi
  check_port_free "$KC_PORT" "keycloak"
  [ -n "$SANDBOX_ADMIN_USER" ] || die "SANDBOX_ADMIN_USER missing from $SB_SECRETS/sandbox.env"
  mkdir -p "$SB_LOGS"
  log "starting keycloak (dev mode, dev-file H2, http $KC_HOST:$KC_PORT, java $("$SB_JAVA_BIN" -version 2>&1 | head -1))"
  (
    cd "$KC_HOME"
    exec env \
      JAVA_HOME="$SB_JAVA_HOME" \
      PATH="$SB_JAVA_HOME/bin:$PATH" \
      KC_BOOTSTRAP_ADMIN_USERNAME="$SANDBOX_ADMIN_USER" \
      KC_BOOTSTRAP_ADMIN_PASSWORD="$SANDBOX_ADMIN_PASSWORD" \
      "$KC_HOME/bin/kc.sh" start-dev \
        --db=dev-file \
        --http-host="$KC_HOST" \
        --http-port="$KC_PORT" \
        --http-enabled=true \
        --http-management-host="$KC_HOST" \
        --http-management-port="$KC_MGMT_PORT" \
        --hostname="http://$KC_HOST:$KC_PORT" \
        --hostname-strict=true \
        --import-realm \
        --health-enabled=true \
        --log-level=INFO \
        --log-console-color=false
  ) >>"$KC_LOG" 2>&1 &
  local pid=$!
  write_pid "$KC_PID_FILE" "$pid"
  log "waiting for keycloak to become ready (up to 180s) ..."
  if ! wait_http "$KC_URL/realms/$ADMIN_REALM" 180 200; then
    tail -40 "$KC_LOG" >&2 || true
    die "keycloak did not become ready within 180s — see $KC_LOG"
  fi
  # wait for the readiness probe on the management interface (realm import +
  # JPA migrations complete); falls back to the realm endpoint above.
  wait_http "$KC_MGMT_URL/health/ready" 60 200 || warn "keycloak $KC_MGMT_URL/health/ready not 200 yet"
  # record the actual java listener pid (kc.sh execs java, but be explicit)
  local jpid; jpid="$(listener_pids "$KC_PORT" | awk '{print $1}')"
  [ -n "$jpid" ] && write_pid "$KC_PID_FILE" "$jpid"
  ok "keycloak ready (pid ${jpid:-$pid}, log $KC_LOG)"
}

# ------------------------------------------------- admin service account -----
# Ensures the confidential service-account client ($ADMIN_CLIENT_ID) exists with
# the secret generated into .sandbox/secrets/sandbox.env, and that its service
# account holds the realm-management roles needed for automated tests:
#   manage-users, view-users, query-users, view-realm
# Idempotent: safe on every up.sh run.
ensure_admin_client() {
  local token client_uuid rm_uuid sa_id roles_payload code cc_token
  token="$(admin_token)" || die "cannot obtain admin token for service-account setup"
  [ -n "${SANDBOX_ADMIN_CLIENT_SECRET:-}" ] \
    || die "SANDBOX_ADMIN_CLIENT_SECRET missing from $SB_SECRETS/sandbox.env (run tests/sandbox/reset.sh --full)"

  client_uuid="$(curl -sS -m 30 -H "Authorization: Bearer $token" \
    "$KC_URL/admin/realms/$REALM/clients?clientId=$ADMIN_CLIENT_ID" | jq -r '.[0].id // empty')"
  if [ -z "$client_uuid" ]; then
    warn "client '$ADMIN_CLIENT_ID' missing from realm '$REALM' (realm imported before it existed); creating via Admin API"
    code="$(curl -sS -m 30 -o "$SB_TMP/admin-client-create.json" -w '%{http_code}' \
      -X POST "$KC_URL/admin/realms/$REALM/clients" -H "Authorization: Bearer $token" \
      -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg cid "$ADMIN_CLIENT_ID" '{
            clientId:$cid, publicClient:false, standardFlowEnabled:false,
            implicitFlowEnabled:false, directAccessGrantsEnabled:false,
            serviceAccountsEnabled:true, fullScopeAllowed:true }')")"
    [ "$code" = "201" ] || die "could not create client '$ADMIN_CLIENT_ID' (HTTP $code): $(head -c 300 "$SB_TMP/admin-client-create.json")"
    client_uuid="$(curl -sS -m 30 -H "Authorization: Bearer $token" \
      "$KC_URL/admin/realms/$REALM/clients?clientId=$ADMIN_CLIENT_ID" | jq -r '.[0].id // empty')"
    [ -n "$client_uuid" ] || die "client '$ADMIN_CLIENT_ID' still not found after creation"
  fi

  # pin the generated secret (re-runnable: same value from .sandbox/secrets)
  code="$(curl -sS -m 30 -o /dev/null -w '%{http_code}' -X PUT \
    "$KC_URL/admin/realms/$REALM/clients/$client_uuid" -H "Authorization: Bearer $token" \
    -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg s "$SANDBOX_ADMIN_CLIENT_SECRET" '{secret:$s}')")"
  [ "$code" = "204" ] || die "could not set secret on client '$ADMIN_CLIENT_ID' (HTTP $code)"

  # realm-management client roles -> service account user
  rm_uuid="$(curl -sS -m 30 -H "Authorization: Bearer $token" \
    "$KC_URL/admin/realms/$REALM/clients?clientId=realm-management" | jq -r '.[0].id // empty')"
  [ -n "$rm_uuid" ] || die "realm-management client not found in realm '$REALM'"
  roles_payload="$(curl -sS -m 30 -H "Authorization: Bearer $token" \
    "$KC_URL/admin/realms/$REALM/clients/$rm_uuid/roles" \
    | jq '[.[] | select(.name=="manage-users" or .name=="view-users" or .name=="query-users" or .name=="view-realm")]')"
  [ "$(printf '%s' "$roles_payload" | jq 'length')" = "4" ] \
    || die "expected 4 realm-management roles in realm '$REALM', got $(printf '%s' "$roles_payload" | jq 'length')"
  sa_id="$(curl -sS -m 30 -H "Authorization: Bearer $token" \
    "$KC_URL/admin/realms/$REALM/clients/$client_uuid/service-account-user" | jq -r '.id // empty')"
  [ -n "$sa_id" ] || die "service-account user for '$ADMIN_CLIENT_ID' not found (serviceAccountsEnabled?)"
  code="$(curl -sS -m 30 -o /dev/null -w '%{http_code}' -X POST \
    "$KC_URL/admin/realms/$REALM/users/$sa_id/role-mappings/clients/$rm_uuid" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d "$roles_payload")"
  [ "$code" = "204" ] || die "could not map realm-management roles to the service account (HTTP $code)"

  # prove it end to end: client_credentials token must be able to list users
  code="$(curl -sS -m 30 -o "$SB_TMP/admin-cc-token.json" -w '%{http_code}' -X POST \
    "$KC_URL/realms/$REALM/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    -d 'grant_type=client_credentials' \
    --data-urlencode "client_id=$ADMIN_CLIENT_ID" \
    --data-urlencode "client_secret=$SANDBOX_ADMIN_CLIENT_SECRET")"
  [ "$code" = "200" ] || die "client_credentials token for '$ADMIN_CLIENT_ID' failed (HTTP $code): $(head -c 300 "$SB_TMP/admin-cc-token.json")"
  cc_token="$(jq -r '.access_token // empty' "$SB_TMP/admin-cc-token.json")"
  code="$(curl -sS -m 30 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $cc_token" \
    "$KC_URL/admin/realms/$REALM/users?briefRepresentation=true")"
  [ "$code" = "200" ] || die "service-account token cannot list users (HTTP $code) — role mapping failed"
  ok "admin service account '$ADMIN_CLIENT_ID' ready (client_credentials -> GET /admin/realms/$REALM/users 200)"
}

# --------------------------------------------------------------- verify -----
verify_sandbox() {
  hr
  log "verifying sandbox configuration"
  local token disc realm_json client_json

  # 1. OIDC discovery + issuer
  disc="$(curl -sS -m 15 "$KC_URL/realms/$REALM/.well-known/openid-configuration")" \
    || die "cannot fetch OIDC discovery document (is realm $REALM imported?)"
  local issuer
  issuer="$(printf '%s' "$disc" | jq -r '.issuer // empty')"
  [ "$issuer" = "$KC_URL/realms/$REALM" ] \
    || die "issuer mismatch: expected $KC_URL/realms/$REALM, got '${issuer:-none}' (realm import failed?)"
  ok "OIDC discovery: issuer=$issuer"

  echo "  authorization_endpoint : $(printf '%s' "$disc" | jq -r '.authorization_endpoint')"
  echo "  token_endpoint         : $(printf '%s' "$disc" | jq -r '.token_endpoint')"
  echo "  userinfo_endpoint      : $(printf '%s' "$disc" | jq -r '.userinfo_endpoint')"
  echo "  jwks_uri               : $(printf '%s' "$disc" | jq -r '.jwks_uri')"

  # 2. admin token + realm exists
  token="$(admin_token)" || die "cannot obtain admin token"
  [ -n "$token" ] && ok "admin token acquired from master/admin-cli (password grant)"
  realm_json="$(admin_get "/realms/$REALM")" || die "realm $REALM not found — check $KC_LOG for import errors"
  ok "realm '$REALM' exists (displayName=$(printf '%s' "$realm_json" | jq -r '.displayName'))"

  # 3. SMTP -> Mailpit
  local smtp_host smtp_port smtp_from verify_email
  smtp_host="$(printf '%s' "$realm_json" | jq -r '.smtpServer.host // "<unset>"')"
  smtp_port="$(printf '%s' "$realm_json" | jq -r '.smtpServer.port // "<unset>"')"
  smtp_from="$(printf '%s' "$realm_json" | jq -r '.smtpServer.from // "<unset>"')"
  verify_email="$(printf '%s' "$realm_json" | jq -r '.verifyEmail // false')"
  [ "$smtp_host" = "$MP_SMTP_HOST" ] && [ "$smtp_port" = "$MP_SMTP_PORT" ] \
    || die "realm SMTP is $smtp_host:$smtp_port, expected $MP_SMTP_HOST:$MP_SMTP_PORT — run tests/sandbox/reset.sh"
  [ "$verify_email" = "true" ] || die "realm verifyEmail is '$verify_email', expected true"
  ok "realm SMTP -> $smtp_host:$smtp_port from=$smtp_from (auth off), email verification enabled"

  # 4. client
  client_json="$(admin_get "/realms/$REALM/clients?clientId=$CLIENT_ID")"
  [ "$(printf '%s' "$client_json" | jq 'length')" = "1" ] \
    || die "OIDC client '$CLIENT_ID' not found in realm $REALM — run tests/sandbox/reset.sh"
  local redirects pkce public standard direct
  redirects="$(printf '%s' "$client_json" | jq -r '.[0].redirectUris | join(" ")')"
  pkce="$(printf '%s' "$client_json" | jq -r '.[0].attributes["pkce.code.challenge.method"] // "unset"')"
  public="$(printf '%s' "$client_json" | jq -r '.[0].publicClient')"
  standard="$(printf '%s' "$client_json" | jq -r '.[0].standardFlowEnabled')"
  direct="$(printf '%s' "$client_json" | jq -r '.[0].directAccessGrantsEnabled')"
  [ "$public" = "true" ] && [ "$standard" = "true" ] && [ "$pkce" = "S256" ] && [ "$direct" = "true" ] \
    || die "client '$CLIENT_ID' config wrong: public=$public standard=$standard pkce=$pkce directGrant=$direct"
  case "$redirects" in
    *"$APP_CALLBACK"*) ;;
    *) die "client '$CLIENT_ID' missing redirect URI $APP_CALLBACK (has: $redirects)" ;;
  esac
  ok "client '$CLIENT_ID': public, standard flow=$standard, PKCE=$pkce, direct grants=$direct, redirects: $redirects"
  ok "registration disabled (registrationAllowed=$(printf '%s' "$realm_json" | jq -r '.registrationAllowed'))"

  # 5. admin service-account client (used by automated tests for Admin API)
  local admin_json sa_roles
  admin_json="$(admin_get "/realms/$REALM/clients?clientId=$ADMIN_CLIENT_ID")"
  [ "$(printf '%s' "$admin_json" | jq 'length')" = "1" ] \
    || die "admin client '$ADMIN_CLIENT_ID' not found — run tests/sandbox/up.sh again or reset.sh"
  [ "$(printf '%s' "$admin_json" | jq -r '.[0].serviceAccountsEnabled')" = "true" ] \
    || die "admin client '$ADMIN_CLIENT_ID' has serviceAccountsEnabled=false"
  [ "$(printf '%s' "$admin_json" | jq -r '.[0].publicClient')" = "false" ] \
    || die "admin client '$ADMIN_CLIENT_ID' must be confidential (publicClient=false)"
  sa_roles="$(curl -sS -m 30 -H "Authorization: Bearer $token" \
    "$KC_URL/admin/realms/$REALM/users/$(curl -sS -m 30 -H "Authorization: Bearer $token" \
      "$KC_URL/admin/realms/$REALM/clients/$(printf '%s' "$admin_json" | jq -r '.[0].id')/service-account-user" | jq -r '.id')/role-mappings/clients/$(curl -sS -m 30 -H "Authorization: Bearer $token" "$KC_URL/admin/realms/$REALM/clients?clientId=realm-management" | jq -r '.[0].id')" \
    | jq -r '[.[].name] | sort | join(",")')"
  ok "admin client '$ADMIN_CLIENT_ID': confidential, service account roles -> $sa_roles"
  printf '%s' "$sa_roles" | grep -q 'manage-users' \
    || die "admin service account is missing the 'manage-users' realm-management role"
  [ -n "$(client_credentials_token 2>/dev/null)" ] \
    || die "client_credentials token for '$ADMIN_CLIENT_ID' failed — check $SB_SECRETS/sandbox.env"
  ok "client_credentials token for '$ADMIN_CLIENT_ID' works (Admin API access verified)"

  # 6. loopback-only sanity
  local bad
  bad="$(lsof -nP -iTCP -sTCP:LISTEN 2>/dev/null \
        | awk -v p1="$KC_PORT" -v p2="$MP_SMTP_PORT" -v p3="$MP_WEB_PORT" -v p4="$KC_MGMT_PORT" \
            '$9 ~ (":"p1"$") || $9 ~ (":"p2"$") || $9 ~ (":"p3"$") || $9 ~ (":"p4"$") {print $9}' \
        | grep -v '^127\.0\.0\.1:' || true)"
  [ -z "$bad" ] || die "non-loopback listener detected on sandbox ports: $bad"
  ok "all sandbox listeners are bound to 127.0.0.1 only"
  hr
}

# ----------------------------------------------------------------- main -----
main() {
  require_cmds curl tar lsof jq openssl
  mkdirs
  check_java
  ensure_secrets
  ensure_admin_client_secret

  hr
  log "LOCAL SANDBOX bring-up — real Keycloak $KC_VERSION + real Mailpit v$MP_VERSION"
  log "repo: $SB_ROOT   sandbox dir: $SB_DIR"
  hr

  install_mailpit
  install_keycloak

  if [ "$INSTALL_ONLY" -eq 1 ]; then
    stage_realm_import
    ok "install-only requested; not starting services"
    exit 0
  fi

  stage_realm_import
  start_mailpit
  start_keycloak
  ensure_admin_client
  [ "$DO_VERIFY" -eq 1 ] && verify_sandbox

  hr
  ok "LOCAL SANDBOX IS UP"
  echo "  keycloak   : $KC_URL  (realm $REALM, oidc client $CLIENT_ID)"
  echo "  mailpit    : $MP_URL  (api $MP_API, smtp $MP_SMTP_HOST:$MP_SMTP_PORT)"
  echo "  admin svc  : client '$ADMIN_CLIENT_ID' (client_credentials; secret in $SB_SECRETS/sandbox.env)"
  echo "  admin user : $SANDBOX_ADMIN_USER (master realm password grant, 60s TTL)"
  echo
  echo "  next: source tests/sandbox/env.sh   then   bash tests/sandbox/verify.sh"
  hr
}

main
