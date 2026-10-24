#!/usr/bin/env bash
# R5.1-C: isolated local HTTPS ingress acceptance.
#
# Topology (everything loopback):
#   curl --cacert ca.pem -> 127.0.0.1:18443 (TLS test ingress) -> 127.0.0.1:18788 (onboardd)
#   http://127.0.0.1:18080 -> 308 https://localhost:18443/...
# No public DNS, no host trust store, no existing reverse proxy is touched.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$ROOT/.sandbox/r51/tls"
APPPORT=18788
HTTPSPORT=18443
HTTPPORT=18080
UNTRUSTEDPORT=18789
BASE_HTTPS="https://localhost:$HTTPSPORT"
BASE_HTTP="http://localhost:$HTTPPORT"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do
    [ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null || true
  done
  sleep 0.3
  for pid in "${pids[@]:-}"; do
    [ -n "$pid" ] && kill -KILL "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT

rm -rf "$WORK"; mkdir -p "$WORK"; umask 077
head -c 32 /dev/urandom > "$WORK/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/admin.token"
(cd "$ROOT" && go build -trimpath -o bin/onboardd ./cmd/onboardd \
  && go build -trimpath -o bin/r51-ingress ./tests/r51/ingress \
  && go build -trimpath -o bin/r51-tlsgen ./tests/r51/tlsgen)
"$ROOT/bin/r51-tlsgen" -out "$WORK/tls" >/dev/null
CA="$WORK/tls/ca.pem"
TOKEN="$(cat "$WORK/admin.token")"
ENV_APP=(env ONBOARD_LISTEN="127.0.0.1:$APPPORT" ONBOARD_DB_PATH="$WORK/state.sqlite"
  ONBOARD_MASTER_KEY_FILE="$WORK/master.key" ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token"
  ONBOARD_FAKE_PROVIDERS=true ONBOARD_TRUST_PROXY_HEADERS=true ONBOARD_DOMAINS=moonbyte.test
  ONBOARD_SENDER_ALLOWLIST=moonbyte.test ONBOARD_LOG_LEVEL=warn)

start_app() {
  ( cd "$ROOT" && exec "$ROOT/bin/onboardd" ) > "$WORK/onboardd-$1.log" 2>&1 &
  APP_PID=$!; pids+=("$APP_PID")
  for _ in $(seq 1 150); do curl -fsS -m 1 "http://127.0.0.1:$APPPORT/healthz" >/dev/null 2>&1 && return 0; sleep 0.05; done
  echo "app failed to start"; tail -20 "$WORK/onboardd-$1.log"; return 1
}
start_ingress() {
  "$ROOT/bin/r51-ingress" -listen "127.0.0.1:$HTTPSPORT" -http-listen "127.0.0.1:$HTTPPORT" \
    -upstream "http://127.0.0.1:$APPPORT" -cert "$WORK/tls/server.pem" -key "$WORK/tls/server-key.pem" \
    > "$WORK/ingress.log" 2>&1 &
  INGRESS_PID=$!; pids+=("$INGRESS_PID")
  for _ in $(seq 1 150); do curl -fsS -m 1 --cacert "$CA" "$BASE_HTTPS/healthz" >/dev/null 2>&1 && return 0; sleep 0.05; done
  echo "ingress failed to start"; cat "$WORK/ingress.log"; return 1
}

# --- boot ---
"${ENV_APP[@]}" true # sanity: env array is well-formed
( cd "$ROOT" && exec env ONBOARD_LISTEN="127.0.0.1:$APPPORT" ONBOARD_DB_PATH="$WORK/state.sqlite" \
  ONBOARD_MASTER_KEY_FILE="$WORK/master.key" ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token" \
  ONBOARD_FAKE_PROVIDERS=true ONBOARD_TRUST_PROXY_HEADERS=true ONBOARD_DOMAINS=moonbyte.test \
  ONBOARD_SENDER_ALLOWLIST=moonbyte.test ONBOARD_LOG_LEVEL=warn ./bin/onboardd ) > "$WORK/onboardd-1.log" 2>&1 &
APP_PID=$!; pids+=("$APP_PID")
for _ in $(seq 1 150); do curl -fsS -m 1 "http://127.0.0.1:$APPPORT/healthz" >/dev/null 2>&1 && break; sleep 0.05; done
start_ingress || { bad "ingress start"; exit 1; }
ok "TLS ingress and app started on loopback"

# 1. Certificate/hostname verification with the test CA.
CODE="$(curl -sS -o /dev/null -w '%{http_code}' --cacert "$CA" "$BASE_HTTPS/healthz")"
[ "$CODE" = "200" ] && ok "TLS health 200 with test CA (localhost SAN)" || bad "TLS health code=$CODE"
openssl verify -CAfile "$CA" "$WORK/tls/server.pem" >/dev/null 2>&1 && ok "openssl verify server cert against test CA" || bad "openssl verify"
set +e
curl -sS -o /dev/null --cacert "$CA" --resolve wrong.invalid:$HTTPSPORT:127.0.0.1 "https://wrong.invalid:$HTTPSPORT/healthz" 2>"$WORK/hostname.err"
RC=$?
set -e
[ "$RC" -ne 0 ] && grep -qiE "certificate|hostname|SSL" "$WORK/hostname.err" && ok "wrong hostname rejected (curl exit $RC)" || bad "wrong hostname accepted (exit $RC)"
PROTO="$(python3 - "$HTTPSPORT" "$CA" <<'PYTLS'
import socket, ssl, sys
port = int(sys.argv[1]); cafile = sys.argv[2]
ctx = ssl.create_default_context(cafile=cafile)
with socket.create_connection(("127.0.0.1", port), timeout=3) as raw:
    with ctx.wrap_socket(raw, server_hostname="localhost") as tls:
        print(tls.version())
PYTLS
)"
case "$PROTO" in TLSv1.2|TLSv1.3) ok "negotiated $PROTO";; *) bad "unexpected TLS protocol: $PROTO";; esac
set +e
openssl s_client -connect "127.0.0.1:$HTTPSPORT" -servername localhost -tls1_1 -CAfile "$CA" </dev/null >/dev/null 2>&1
RC=$?
set -e
[ "$RC" -ne 0 ] && ok "TLS 1.1 rejected (openssl exit $RC)" || bad "TLS 1.1 accepted"

# 2. HTTP -> HTTPS policy.
REDIR="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' "$BASE_HTTP/healthz")"
case "$REDIR" in "308 https://localhost:$HTTPSPORT/healthz") ok "HTTP 308 to HTTPS ($REDIR)";; *) bad "HTTP redirect: $REDIR";; esac

# 3. Login over TLS + cookie flags.
curl -sS --cacert "$CA" -c "$WORK/jar" -D "$WORK/login.headers" -o /dev/null -X POST "$BASE_HTTPS/ui/login" \
  -H "Origin: $BASE_HTTPS" --data-urlencode "token=$TOKEN" -w '%{http_code}' > "$WORK/login.code"
[ "$(cat "$WORK/login.code")" = "303" ] && ok "login over HTTPS 303" || bad "login $(cat "$WORK/login.code")"
COOKIE_LINE="$(grep -i '^set-cookie: onboard_session=' "$WORK/login.headers" | head -1)"
case "$COOKIE_LINE" in
  *"Secure"*|*"secure"*) ok "session cookie has Secure";;
  *) bad "session cookie missing Secure: $COOKIE_LINE";;
esac
case "$COOKIE_LINE" in *"HttpOnly"*|*"httponly"*) ok "session cookie has HttpOnly";; *) bad "missing HttpOnly";; esac
case "$COOKIE_LINE" in *"SameSite=Strict"*|*"samesite=strict"*) ok "session cookie has SameSite=Strict";; *) bad "missing SameSite=Strict";; esac
COOKIE="$(awk '$6=="onboard_session"{print $7}' "$WORK/jar")"
[ -n "$COOKIE" ] || { bad "no cookie value"; exit 1; }
DASH="$(curl -sS --cacert "$CA" -b "$WORK/jar" -o "$WORK/dashboard.html" -w '%{http_code}' "$BASE_HTTPS/ui/")"
[ "$DASH" = "200" ] && ok "authenticated dashboard over HTTPS 200" || bad "dashboard $DASH"
CSRF="$(grep -o 'name="csrf" value="[^"]*"' "$WORK/dashboard.html" | head -1 | sed 's/.*value="//;s/"//')"
[ -n "$CSRF" ] && ok "CSRF token present" || { bad "no CSRF"; exit 1; }

# 4. CSRF / Origin enforcement.
C="$(curl -sS --cacert "$CA" -b "$WORK/jar" -o /dev/null -w '%{http_code}' -X POST "$BASE_HTTPS/ui/logout" -H "Origin: $BASE_HTTPS")"
[ "$C" = "403" ] && ok "logout without CSRF rejected 403" || bad "CSRF-less logout = $C"
C="$(curl -sS --cacert "$CA" -b "$WORK/jar" -o /dev/null -w '%{http_code}' -X POST "$BASE_HTTPS/ui/logout" \
  -H "Origin: https://evil.example" --data-urlencode "csrf=$CSRF")"
[ "$C" = "403" ] && ok "cross-origin logout rejected 403" || bad "cross-origin logout = $C"

# 5. Valid logout + durability across SIGKILL.
C="$(curl -sS --cacert "$CA" -b "$WORK/jar" -o /dev/null -w '%{http_code}' -X POST "$BASE_HTTPS/ui/logout" \
  -H "Origin: $BASE_HTTPS" --data-urlencode "csrf=$CSRF")"
[ "$C" = "303" ] && ok "valid logout 303" || bad "logout = $C"
C="$(curl -sS --cacert "$CA" -o /dev/null -w '%{http_code} %{redirect_url}' -H "Cookie: onboard_session=$COOKIE" "$BASE_HTTPS/ui/")"
case "$C" in "303 "*"/ui/login") ok "old cookie rejected immediately";; *) bad "post-logout replay: $C";; esac
kill -KILL "$APP_PID" 2>/dev/null || true
sleep 0.3
( cd "$ROOT" && exec env ONBOARD_LISTEN="127.0.0.1:$APPPORT" ONBOARD_DB_PATH="$WORK/state.sqlite" \
  ONBOARD_MASTER_KEY_FILE="$WORK/master.key" ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token" \
  ONBOARD_FAKE_PROVIDERS=true ONBOARD_TRUST_PROXY_HEADERS=true ONBOARD_DOMAINS=moonbyte.test \
  ONBOARD_SENDER_ALLOWLIST=moonbyte.test ONBOARD_LOG_LEVEL=warn ./bin/onboardd ) > "$WORK/onboardd-2.log" 2>&1 &
APP_PID=$!; pids+=("$APP_PID")
for _ in $(seq 1 150); do curl -fsS -m 1 "http://127.0.0.1:$APPPORT/healthz" >/dev/null 2>&1 && break; sleep 0.05; done
C="$(curl -sS --cacert "$CA" -o /dev/null -w '%{http_code} %{redirect_url}' -H "Cookie: onboard_session=$COOKIE" "$BASE_HTTPS/ui/")"
case "$C" in "303 "*"/ui/login") ok "revocation survives SIGKILL + restart over TLS";; *) bad "post-crash replay: $C";; esac
C="$(curl -sS --cacert "$CA" -c "$WORK/jar2" -o /dev/null -w '%{http_code}' -X POST "$BASE_HTTPS/ui/login" -H "Origin: $BASE_HTTPS" --data-urlencode "token=$TOKEN")"
[ "$C" = "303" ] && ok "fresh login after crash 303" || bad "fresh login = $C"

# 6. Client-supplied forwarding headers cannot downgrade the cookie.
curl -sS --cacert "$CA" -c "$WORK/jar3" -D "$WORK/inject.headers" -o /dev/null -X POST "$BASE_HTTPS/ui/login" \
  -H "Origin: $BASE_HTTPS" -H "X-Forwarded-Proto: http" -H "X-Forwarded-For: 203.0.113.9" \
  -H "Forwarded: for=203.0.113.9;proto=http" --data-urlencode "token=$TOKEN"
grep -qi '^set-cookie: onboard_session=.*secure' "$WORK/inject.headers" \
  && ok "forged X-Forwarded-Proto cannot downgrade Secure" || bad "forged XFP downgraded cookie"

# 7. Untrusted X-Forwarded-* is ignored when trust is disabled (direct plain HTTP).
( cd "$ROOT" && exec env ONBOARD_LISTEN="127.0.0.1:$UNTRUSTEDPORT" ONBOARD_DB_PATH="$WORK/untrusted.sqlite" \
  ONBOARD_MASTER_KEY_FILE="$WORK/master.key" ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token" \
  ONBOARD_FAKE_PROVIDERS=true ONBOARD_TRUST_PROXY_HEADERS=false ONBOARD_DOMAINS=moonbyte.test \
  ONBOARD_SENDER_ALLOWLIST=moonbyte.test ONBOARD_LOG_LEVEL=warn ./bin/onboardd ) > "$WORK/onboardd-untrusted.log" 2>&1 &
UPID=$!; pids+=("$UPID")
for _ in $(seq 1 150); do curl -fsS -m 1 "http://127.0.0.1:$UNTRUSTEDPORT/healthz" >/dev/null 2>&1 && break; sleep 0.05; done
curl -sS -c "$WORK/jar-untrusted" -D "$WORK/untrusted.headers" -o /dev/null -X POST "http://127.0.0.1:$UNTRUSTEDPORT/ui/login" \
  -H "Origin: http://127.0.0.1:$UNTRUSTEDPORT" -H "X-Forwarded-Proto: https" --data-urlencode "token=$TOKEN" >/dev/null
if grep -i '^set-cookie: onboard_session=' "$WORK/untrusted.headers" | grep -qi 'secure'; then
  bad "secure flag set from untrusted X-Forwarded-Proto"
else
  ok "untrusted X-Forwarded-Proto ignored (no Secure on plain HTTP)"
fi
kill -TERM "$UPID" 2>/dev/null || true

# 8. Non-loopback listen refused and loopback-only listeners.
set +e
( cd "$ROOT" && env ONBOARD_LISTEN=0.0.0.0:18790 ONBOARD_DB_PATH="$WORK/nonloop.sqlite" \
  ONBOARD_MASTER_KEY_FILE="$WORK/master.key" ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token" \
  ./bin/onboardd ) > "$WORK/nonloopback.log" 2>&1
RC=$?
set -e
[ "$RC" -ne 0 ] && grep -qi "not loopback" "$WORK/nonloopback.log" \
  && ok "non-loopback listen refused (exit $RC)" || bad "non-loopback listen (exit $RC)"
LISTENERS="$(lsof -nP -iTCP:$APPPORT -iTCP:$HTTPSPORT -iTCP:$HTTPPORT -sTCP:LISTEN 2>/dev/null)"
if echo "$LISTENERS" | grep -qE '(0\.0\.0\.0|\*):'; then
  bad "wildcard listener detected"
else
  ok "all test listeners are loopback-only"
fi

# 9. HTTP -> HTTPS + security headers on a browser-facing page.
HDRS="$(curl -sS -D - -o /dev/null --cacert "$CA" "$BASE_HTTPS/ui/login")"
echo "$HDRS" | grep -qi '^x-frame-options: DENY' && ok "X-Frame-Options DENY present" || bad "missing X-Frame-Options"
echo "$HDRS" | grep -qi '^content-security-policy:' && ok "CSP present on login page" || bad "missing CSP"

echo "SUMMARY pass=$PASS fail=$FAIL"
[ "$FAIL" = "0" ]
