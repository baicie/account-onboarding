#!/usr/bin/env bash
# R8 isolated staging acceptance:
#   test CA TLS ingress -> private Docker network -> app (UID 10001, read-only)
#   -> SQLite bind mount. Never uses a real domain or real provider credentials.
#
# Exit codes: 0 = PASS, 1 = FAIL, 3 = NOT_RUN (no Docker/CI prerequisites).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if ! docker info >/dev/null 2>&1; then
  echo "NOT_RUN: Docker daemon unavailable"
  exit 3
fi
if [ "$(id -u)" != "0" ] && ! sudo -n true >/dev/null 2>&1; then
  echo "NOT_RUN: staging secret ownership requires root or passwordless sudo (GitHub runner)"
  exit 3
fi
ID="r8-$(date +%s)-$$"
PROJECT="onboarding-r8-$ID"
APP_IMAGE="account-onboarding:$ID"
INGRESS_IMAGE="account-onboarding-ingress:$ID"
WORK="$ROOT/.sandbox/r8/staging-$ID"
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad(){ FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
free_port(){ python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }
HTTPS_PORT="$(free_port)"; HTTP_PORT="$(free_port)"
while [ "$HTTP_PORT" = "$HTTPS_PORT" ]; do HTTP_PORT="$(free_port)"; done

compose(){ R8_IMAGE="$APP_IMAGE" R8_INGRESS_IMAGE="$INGRESS_IMAGE" R8_DATA="$WORK/data" \
  R8_CERTS="$WORK/certs" R8_MASTER_KEY="$WORK/secrets/master.key" \
  R8_ADMIN_TOKEN="$WORK/secrets/admin.token" R8_HTTPS_PORT="$HTTPS_PORT" R8_HTTP_PORT="$HTTP_PORT" \
  docker compose -p "$PROJECT" -f "$ROOT/deploy/staging/compose.yaml" "$@"; }

cleanup(){
  compose down -v >/dev/null 2>&1 || true
  docker image rm "$APP_IMAGE" "$INGRESS_IMAGE" >/dev/null 2>&1 || true
  if [ -d "$WORK" ]; then
    if [ "$(id -u)" = "0" ]; then rm -rf "$WORK"; else sudo -n rm -rf "$WORK" >/dev/null 2>&1 || rm -rf "$WORK" || true; fi
  fi
}
trap cleanup EXIT

rm -rf "$WORK"; mkdir -p "$WORK"/{data,certs,secrets,evidence}
umask 077
head -c 32 /dev/urandom > "$WORK/secrets/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/secrets/admin.token"
chmod 600 "$WORK/secrets/"*
TOKEN="$(cat "$WORK/secrets/admin.token")"
( cd "$ROOT" && go run ./tests/r51/tlsgen --out "$WORK/certs" --host localhost >/dev/null )
GIT_SHA="$(git -C "$ROOT" rev-parse HEAD)"
echo "=== build app + staging ingress ==="
docker build --build-arg VERSION=0.1.0 --build-arg COMMIT="$GIT_SHA" -t "$APP_IMAGE" "$ROOT" >/dev/null
docker build -f "$ROOT/deploy/staging/Dockerfile.ingress" -t "$INGRESS_IMAGE" "$ROOT" >/dev/null
APP_ID="$(docker image inspect --format '{{.Id}}' "$APP_IMAGE")"
ING_ID="$(docker image inspect --format '{{.Id}}' "$INGRESS_IMAGE")"
ok "images built: app=$APP_ID ingress=$ING_ID commit=$GIT_SHA"

# Host ownership contract for bind mounts/secrets.
if [ "$(id -u)" = "0" ]; then OWN=""; else OWN="sudo -n"; fi
$OWN chown 10001:10001 "$WORK/data" "$WORK/secrets/master.key" "$WORK/secrets/admin.token"
$OWN chmod 750 "$WORK/data"
$OWN chmod 400 "$WORK/secrets/master.key" "$WORK/secrets/admin.token"
# The ingress needs the server cert/key; the host test client needs the CA.
# Do not chown the CA to 10001 or the runner can no longer read it for curl.
chmod 755 "$WORK/certs"
$OWN chown 10001:10001 "$WORK/certs/server.pem" "$WORK/certs/server-key.pem"
$OWN chmod 400 "$WORK/certs/server-key.pem"
$OWN chmod 644 "$WORK/certs/server.pem"
chmod 644 "$WORK/certs/ca.pem"

echo "=== compose up ==="
compose config >/dev/null && ok "staging compose config valid" || bad "compose config"
compose up -d >/dev/null && ok "staging compose up" || { bad "compose up"; compose logs --tail 30; exit 1; }
C=""
for _ in $(seq 1 120); do
  C="$(compose ps --format '{{.Service}} {{.Status}}' 2>/dev/null | grep '^app ' | head -1 || true)"
  echo "$C" | grep -qi healthy && break
  sleep 1
done
echo "$C" | grep -qi healthy && ok "app healthcheck healthy ($C)" || { bad "app health: $C"; compose logs --tail 40; }

CURL=(curl -sS --cacert "$WORK/certs/ca.pem" --resolve "localhost:$HTTPS_PORT:127.0.0.1")
BASE="https://localhost:$HTTPS_PORT"
for _ in $(seq 1 60); do
  code="$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/healthz" 2>/dev/null || true)"
  [ "$code" = "200" ] && break; sleep 1
done
if [ "$code" != "200" ]; then
  bad "HTTPS healthz=$code"
  echo "--- staging diagnostics ---" >&2
  compose ps -a 2>&1 >&2 || true
  compose logs --no-color --tail 80 app ingress 2>&1 >&2 || true
  ING_CID="$(compose ps -a -q ingress 2>/dev/null || true)"
  if [ -n "$ING_CID" ]; then docker inspect --format 'ingress state={{.State.Status}} exit={{.State.ExitCode}} error={{.State.Error}}' "$ING_CID" 2>&1 >&2 || true; fi
else
  ok "HTTPS /healthz 200 (test CA)"
fi

echo "=== TLS boundary ==="
"${CURL[@]}" -o /dev/null -w '' "$BASE/healthz" 2>/dev/null && ok "CA-verified TLS handshake" || bad "CA-verified TLS"
"${CURL[@]}" -o /dev/null -w '' "$BASE/readyz" 2>/dev/null || true
UNAUTH="$(curl -sS --cacert "$WORK/certs/ca.pem" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -o /dev/null -w '%{http_code}' "$BASE/readyz" 2>/dev/null || echo 000)"
[ "$UNAUTH" = "401" ] && ok "/readyz requires auth over staging ingress (401)" || bad "unauthenticated /readyz=$UNAUTH"
AUTHED="$(curl -sS --cacert "$WORK/certs/ca.pem" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -H "Authorization: Bearer $TOKEN" -o /dev/null -w '%{http_code}' "$BASE/readyz" 2>/dev/null || echo 000)"
[ "$AUTHED" = "200" ] && ok "authenticated /readyz 200" || bad "authenticated /readyz=$AUTHED"
REDIR="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' -H 'Host: localhost' "http://127.0.0.1:$HTTP_PORT/healthz" 2>/dev/null || true)"
case "$REDIR" in "308 https://localhost:$HTTPS_PORT/healthz") ok "HTTP 308 redirect to public HTTPS port" ;; *) bad "HTTP redirect: $REDIR" ;; esac
HOSTCODE="$(curl -sS --cacert "$WORK/certs/ca.pem" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -H 'Host: evil.example' -o /dev/null -w '%{http_code}' "$BASE/healthz" 2>/dev/null || echo 000)"
[ "$HOSTCODE" = "421" ] && ok "evil Host rejected with 421" || bad "evil Host response=$HOSTCODE"
if command -v openssl >/dev/null 2>&1; then
  if openssl s_client -connect "127.0.0.1:$HTTPS_PORT" -servername localhost -tls1_1 -CAfile "$WORK/certs/ca.pem" </dev/null >/dev/null 2>&1; then
    bad "TLS1.1 unexpectedly accepted"
  else
    ok "TLS1.1 rejected"
  fi
  if openssl s_client -connect "127.0.0.1:$HTTPS_PORT" -servername localhost -tls1_3 -CAfile "$WORK/certs/ca.pem" </dev/null >/dev/null 2>&1; then
    ok "TLS1.3 accepted"
  else
    bad "TLS1.3 rejected"
  fi
else
  echo "WARN openssl not installed; TLS version checks NOT_RUN"
fi

echo "=== session / CSRF / proxy trust ==="
curl -sS --cacert "$WORK/certs/ca.pem" --resolve "localhost:$HTTPS_PORT:127.0.0.1" \
  -c "$WORK/jar" -D "$WORK/login.hdr" -o /dev/null -X POST "$BASE/ui/login" \
  -H "Origin: $BASE" -H 'X-Forwarded-Proto: http' -H 'X-Forwarded-For: 6.6.6.6' \
  --data-urlencode "token=$TOKEN"
COOKIE="$(awk '$6=="onboard_session"{print $7}' "$WORK/jar")"
[ -n "$COOKIE" ] && ok "session cookie issued" || bad "no session cookie"
if grep -qi '^set-cookie: onboard_session=' "$WORK/login.hdr" \
   && grep -i '^set-cookie: onboard_session=' "$WORK/login.hdr" | grep -q 'Secure' \
   && grep -i '^set-cookie: onboard_session=' "$WORK/login.hdr" | grep -qi 'HttpOnly' \
   && grep -i '^set-cookie: onboard_session=' "$WORK/login.hdr" | grep -qi 'SameSite=Strict'; then
  ok "cookie flags Secure/HttpOnly/SameSite=Strict (forged X-Forwarded-Proto did not downgrade)"
else
  bad "cookie flags: $(grep -i '^set-cookie:' "$WORK/login.hdr" | head -1)"
fi
curl -sS --cacert "$WORK/certs/ca.pem" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -b "$WORK/jar" -o "$WORK/dash.html" "$BASE/ui/"
CSRF="$(grep -o 'name="csrf" value="[^"]*"' "$WORK/dash.html" | head -1 | sed 's/.*value="//;s/"//')"
[ -n "$CSRF" ] && ok "CSRF token present" || bad "CSRF token missing"
WRONG="$(curl -sS --cacert "$WORK/certs/ca.pem" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -b "$WORK/jar" -o /dev/null -w '%{http_code}' -X POST "$BASE/ui/logout" -H 'Origin: https://evil.example' --data-urlencode "csrf=$CSRF" 2>/dev/null || echo 000)"
[ "$WRONG" = "403" ] && ok "cross-origin logout rejected (403)" || bad "cross-origin logout=$WRONG"
GOOD="$(curl -sS --cacert "$WORK/certs/ca.pem" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -b "$WORK/jar" -o /dev/null -w '%{http_code}' -X POST "$BASE/ui/logout" -H "Origin: $BASE" --data-urlencode "csrf=$CSRF" 2>/dev/null || echo 000)"
[ "$GOOD" = "303" ] && ok "same-origin logout accepted (303)" || bad "same-origin logout=$GOOD"
REPLAY="$(curl -sS --cacert "$WORK/certs/ca.pem" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -o /dev/null -w '%{http_code} %{redirect_url}' -H "Cookie: onboard_session=$COOKIE" "$BASE/ui/" 2>/dev/null || true)"
case "$REPLAY" in "303 "*"/ui/login") ok "revoked cookie rejected after logout" ;; *) bad "revoked cookie replay: $REPLAY" ;; esac

echo "=== isolation / hardening ==="
APP_PORTS="$(compose port app 8788 2>/dev/null || true)"
[ -z "$APP_PORTS" ] && ok "app port not published to host" || bad "app port published: $APP_PORTS"
ING_PORTS="$(compose ps --format json 2>/dev/null | python3 -c 'import json,sys
for line in sys.stdin:
    try: d=json.loads(line)
    except Exception: continue
    if d.get("Service")=="ingress":
        print(d.get("Publishers",[]))
' 2>/dev/null || true)"
echo "$ING_PORTS" | grep -q '127.0.0.1' && ok "ingress published on loopback only" || bad "ingress publish: $ING_PORTS"
NETWORK_INTERNAL="$(docker network inspect "${PROJECT}_staging-internal" --format '{{.Internal}}' 2>/dev/null || true)"
[ "$NETWORK_INTERNAL" = "true" ] && ok "staging network is internal" || bad "network internal=$NETWORK_INTERNAL"
APP_CID="$(compose ps -q app 2>/dev/null || true)"
ING_CID="$(compose ps -q ingress 2>/dev/null || true)"
[ "$(docker inspect --format '{{.Config.User}}' "$APP_CID" 2>/dev/null)" = "10001:10001" ] && ok "app runs as 10001:10001" || bad "app user"
[ "$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$APP_CID" 2>/dev/null)" = "true" ] && ok "app rootfs read-only" || bad "app rootfs"
docker inspect --format '{{json .HostConfig.CapDrop}}' "$APP_CID" 2>/dev/null | grep -q 'ALL' && ok "app cap_drop ALL" || bad "app cap_drop"
[ "$(docker inspect --format '{{.Config.User}}' "$ING_CID" 2>/dev/null)" = "10001:10001" ] && ok "ingress runs as 10001:10001" || bad "ingress user"
PUBLIC_APP="$(curl -sS -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$HTTPS_PORT/healthz" 2>/dev/null || echo 000)"
[ "$PUBLIC_APP" != "200" ] && ok "app/backend not reachable on HTTPS port as plaintext" || bad "plaintext backend exposed"

compose ps > "$WORK/evidence/compose-ps.txt" 2>&1 || true
docker inspect "$APP_CID" > "$WORK/evidence/app-inspect.json" 2>&1 || true
docker inspect "$ING_CID" > "$WORK/evidence/ingress-inspect.json" 2>&1 || true
cp "$WORK/login.hdr" "$WORK/evidence/login-headers.txt" 2>/dev/null || true
if [ -n "${R8_EVIDENCE_DIR:-}" ]; then
  mkdir -p "$R8_EVIDENCE_DIR"
  cp "$WORK"/evidence/* "$R8_EVIDENCE_DIR"/ 2>/dev/null || true
  { echo "commit=$GIT_SHA"; echo "app_image=$APP_ID"; echo "ingress_image=$ING_ID";
    echo "project=$PROJECT"; echo "https_port=$HTTPS_PORT"; echo "http_port=$HTTP_PORT"; }     > "$R8_EVIDENCE_DIR/staging-identity.txt"
fi
echo "SUMMARY pass=$PASS fail=$FAIL project=$PROJECT image=$APP_ID ingress=$ING_ID"
[ "$FAIL" = "0" ]
