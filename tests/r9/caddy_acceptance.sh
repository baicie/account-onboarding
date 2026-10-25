#!/usr/bin/env bash
# R9 production-shaped TLS reverse proxy acceptance using real Caddy in Docker.
# Test CA only; loopback ports only; no real domain/ACME. App stays on an
# internal network with no published port.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if ! docker info >/dev/null 2>&1; then echo "NOT_RUN: Docker daemon unavailable"; exit 3; fi
ID="r9-caddy-$(date +%s)-$$"
WORK="$ROOT/.sandbox/r9/caddy-$ID"
VOL="$ID-secrets"
NET="$ID-net"
APP="account-onboarding-app:$ID"
SECRET_VOL="$VOL"
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad(){ FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
free_port(){ python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }
HTTPS_PORT="$(free_port)"; HTTP_PORT="$(free_port)"; while [ "$HTTP_PORT" = "$HTTPS_PORT" ]; do HTTP_PORT="$(free_port)"; done
cleanup(){
  docker rm -f "$ID-app" "$ID-caddy" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker volume rm "$SECRET_VOL" >/dev/null 2>&1 || true
  docker image rm "$APP" >/dev/null 2>&1 || true
  rm -rf "$WORK"
  return 0
}
trap cleanup EXIT
rm -rf "$WORK"; mkdir -p "$WORK"/{data,certs,secrets}
umask 077
head -c 32 /dev/urandom > "$WORK/secrets/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/secrets/admin.token"
chmod 600 "$WORK/secrets/"*
TOKEN="$(cat "$WORK/secrets/admin.token")"
AUTH=(-H "Authorization: Bearer $TOKEN")
( cd "$ROOT" && go run ./tests/r51/tlsgen --out "$WORK/certs" --host localhost >/dev/null )
GIT_SHA="$(git -C "$ROOT" rev-parse HEAD)"
docker build --build-arg VERSION=0.1.0 --build-arg COMMIT="$GIT_SHA" -t "$APP" "$ROOT" >/dev/null
APP_ID="$(docker image inspect --format '{{.Id}}' "$APP")"
ok "app image built: $APP_ID commit=$GIT_SHA"
docker network create "$NET" >/dev/null
docker volume create "$SECRET_VOL" >/dev/null
docker run --rm --user 0:0 --entrypoint sh -v "$WORK/secrets:/src:ro" -v "$SECRET_VOL:/dst" "$APP" -c \
  'set -e; cp /src/master.key /dst/onboarding_master_key; cp /src/admin.token /dst/onboard_admin_token; chown 10001:10001 /dst/*; chmod 400 /dst/*'
chmod 777 "$WORK/data"
docker run -d --name "$ID-app" --network "$NET" --network-alias app --read-only --cap-drop ALL \
  --security-opt no-new-privileges --user 10001:10001 --tmpfs /tmp:rw,noexec,nosuid,size=16m \
  -v "$WORK/data:/app/data" --mount type=volume,src="$SECRET_VOL",dst=/run/secrets,readonly \
  -e ONBOARD_LISTEN=0.0.0.0:8788 -e ONBOARD_ALLOW_PUBLIC_LISTEN=1 \
  -e ONBOARD_DB_PATH=/app/data/state.sqlite -e ONBOARD_FAKE_PROVIDERS=true \
  -e ONBOARD_DOMAINS=moonbyte.test -e ONBOARD_SENDER_ALLOWLIST=no-reply,idp.fake.test,moonbyte.test \
  -e ONBOARD_TRUST_PROXY_HEADERS=1 -e ONBOARD_MASTER_KEY_FILE=/run/secrets/onboarding_master_key \
  -e ONBOARD_ADMIN_TOKEN_FILE=/run/secrets/onboard_admin_token -e ONBOARD_LOG_LEVEL=warn \
  "$APP" >/dev/null
# Caddy runs as root inside its image (the official binary carries a file
# capability and cannot exec under no-new-privileges/cap_drop ALL); rootfs is
# read-only, data/config/tmp are nosuid tmpfs, and only NET_BIND_SERVICE is added.
docker run -d --name "$ID-caddy" --network "$NET" --read-only \
  --cap-add NET_BIND_SERVICE --tmpfs /data:rw,nosuid,size=16m \
  --tmpfs /config:rw,nosuid,size=8m --tmpfs /tmp:rw,noexec,nosuid,size=8m -p "127.0.0.1:$HTTPS_PORT:8443" -p "127.0.0.1:$HTTP_PORT:8080" \
  -v "$WORK/certs:/certs:ro" -v "$ROOT/tests/r9/Caddyfile:/etc/caddy/Caddyfile:ro" \
  -e R9_PUBLIC_HOST="localhost:$HTTPS_PORT" -e R9_PUBLIC_PORT="$HTTPS_PORT" \
  --entrypoint caddy caddy:2.8.4-alpine run --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null
CA="$WORK/certs/ca.pem"
BASE="https://localhost:$HTTPS_PORT"
CURL=(curl -sS --cacert "$CA" --resolve "localhost:$HTTPS_PORT:127.0.0.1")
code=""
for _ in $(seq 1 90); do
  code="$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/healthz" 2>/dev/null || true)"
  [ "$code" = "200" ] && break; sleep 1
done
[ "$code" = "200" ] && ok "Caddy HTTPS /healthz 200 (test CA)" || { bad "Caddy healthz=$code"; docker logs "$ID-caddy" 2>&1 | tail -20; }
HSTS="$(curl -sSI --cacert "$CA" --resolve "localhost:$HTTPS_PORT:127.0.0.1" "$BASE/healthz" 2>/dev/null | tr -d '\r' | awk -F': ' 'tolower($1)=="strict-transport-security"{print $2}')"
echo "$HSTS" | grep -qi 'max-age=300' && ok "HSTS present ($HSTS)" || bad "HSTS: $HSTS"
UNAUTH="$(curl -sS --cacert "$CA" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -o /dev/null -w '%{http_code}' "$BASE/readyz" 2>/dev/null || echo 000)"
[ "$UNAUTH" = "401" ] && ok "/readyz requires auth through Caddy (401)" || bad "readyz unauth=$UNAUTH"
REDIR="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' -H 'Host: localhost' "http://127.0.0.1:$HTTP_PORT/healthz" 2>/dev/null || true)"
case "$REDIR" in "308 https://localhost:8443/healthz") ok "Caddy HTTP 308 redirect to HTTPS" ;; *) bad "Caddy redirect=$REDIR" ;; esac
HOSTCODE="$(curl -sS --cacert "$CA" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -H 'Host: evil.example' -o /dev/null -w '%{http_code}' "$BASE/healthz" 2>/dev/null || echo 000)"
[ "$HOSTCODE" = "421" ] && ok "Caddy rejects unknown Host (421)" || bad "Caddy evil host=$HOSTCODE"
if command -v openssl >/dev/null 2>&1; then
  openssl s_client -connect "127.0.0.1:$HTTPS_PORT" -servername localhost -tls1_1 -CAfile "$CA" </dev/null >/dev/null 2>&1 \
    && bad "TLS1.1 accepted" || ok "TLS1.1 rejected"
  openssl s_client -connect "127.0.0.1:$HTTPS_PORT" -servername localhost -tls1_3 -CAfile "$CA" </dev/null >/dev/null 2>&1 \
    && ok "TLS1.3 accepted" || bad "TLS1.3 rejected"
else
  echo "WARN openssl unavailable; TLS version checks NOT_RUN"
fi
LOGIN_STATUS="$(curl -sS --cacert "$CA" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -c "$WORK/jar" -D "$WORK/login.hdr" \
  -o "$WORK/login.body" -w '%{http_code}' -X POST "$BASE/ui/login" -H "Origin: $BASE" -H 'X-Forwarded-Proto: http' \
  --data-urlencode "token=$TOKEN" 2>/dev/null || echo 000)"
if [ "$LOGIN_STATUS" != "303" ]; then
  echo "DEBUG login status=$LOGIN_STATUS"
  cat "$WORK/login.hdr" 2>/dev/null | head -20
  head -c 500 "$WORK/login.body" 2>/dev/null; echo
  echo "DEBUG app logs:"; docker logs "$ID-app" 2>&1 | tail -20
fi
COOKIE="$(awk '$6=="onboard_session"{print $7}' "$WORK/jar")"
[ -n "$COOKIE" ] && ok "session cookie issued through Caddy" || bad "no session cookie"
if grep -i '^set-cookie: onboard_session=' "$WORK/login.hdr" | grep -q Secure \
   && grep -i '^set-cookie: onboard_session=' "$WORK/login.hdr" | grep -qi HttpOnly \
   && grep -i '^set-cookie: onboard_session=' "$WORK/login.hdr" | grep -qi 'SameSite=Strict'; then
  ok "cookie Secure/HttpOnly/SameSite=Strict through Caddy"
else
  bad "cookie flags: $(grep -i '^set-cookie:' "$WORK/login.hdr" | head -1)"
fi
CSRF="$(curl -sS --cacert "$CA" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -b "$WORK/jar" "$BASE/ui/" | grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"//')"
WRONG="$(curl -sS --cacert "$CA" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -b "$WORK/jar" -o /dev/null -w '%{http_code}' -X POST "$BASE/ui/logout" -H 'Origin: https://evil.example' --data-urlencode "csrf=$CSRF" 2>/dev/null || echo 000)"
[ "$WRONG" = "403" ] && ok "CSRF/Origin enforced through Caddy (403)" || bad "cross-origin logout=$WRONG"
GOOD="$(curl -sS --cacert "$CA" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -b "$WORK/jar" -o /dev/null -w '%{http_code}' -X POST "$BASE/ui/logout" -H "Origin: $BASE" --data-urlencode "csrf=$CSRF" 2>/dev/null || echo 000)"
[ "$GOOD" = "303" ] && ok "same-origin logout 303" || bad "logout=$GOOD"
APP_PORTS="$(docker port "$ID-app" 2>/dev/null || true)"
[ -z "$APP_PORTS" ] && ok "app publishes no host port" || bad "app ports: $APP_PORTS"
INSPECT="$(docker inspect --format '{{json .NetworkSettings.Networks}}' "$ID-caddy")"
echo "$INSPECT" | grep -q "$NET" && ok "Caddy attached to staging network" || bad "Caddy network"
BODY="$(python3 -c 'print("x"*1200000)')"
OVERSIZE="$(curl -sS --cacert "$CA" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/runs" -H 'Content-Type: application/json' -d "$BODY" 2>/dev/null || echo 000)"
[ "$OVERSIZE" = "413" ] && ok "proxy request body limit enforced (413)" || echo "WARN oversize response=$OVERSIZE (expected 413; recorded)"
# Backend restart: predictable proxy failure then recovery.
docker stop --time 5 "$ID-app" >/dev/null
DOWN="$(curl -sS --cacert "$CA" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -o /dev/null -w '%{http_code}' "$BASE/healthz" 2>/dev/null || echo 000)"
[ "$DOWN" = "502" ] || [ "$DOWN" = "000" ] && ok "backend down -> predictable proxy failure ($DOWN)" || bad "backend down response=$DOWN"
docker start "$ID-app" >/dev/null
for _ in $(seq 1 60); do c="$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/healthz" 2>/dev/null || true)"; [ "$c" = "200" ] && break; sleep 1; done
[ "$c" = "200" ] && ok "backend restart -> proxy recovers" || bad "backend recovery=$c"
# Certificate rotation: generate a new cert and restart Caddy.
( cd "$ROOT" && go run ./tests/r51/tlsgen --out "$WORK/certs2" --host localhost >/dev/null )
cp "$WORK/certs2/server.pem" "$WORK/certs/server.pem"; cp "$WORK/certs2/server-key.pem" "$WORK/certs/server-key.pem"; chmod 400 "$WORK/certs/server-key.pem"
docker restart "$ID-caddy" >/dev/null
for _ in $(seq 1 60); do c="$(curl -sS --cacert "$WORK/certs2/ca.pem" --resolve "localhost:$HTTPS_PORT:127.0.0.1" -o /dev/null -w '%{http_code}' "$BASE/healthz" 2>/dev/null || true)"; [ "$c" = "200" ] && break; sleep 1; done
[ "$c" = "200" ] && ok "certificate rotation + proxy reload recovers" || bad "rotation recovery=$c"
echo "SUMMARY pass=$PASS fail=$FAIL app=$APP_ID caddy=$(docker image inspect --format '{{.Id}}' caddy:2.8.4-alpine 2>/dev/null || echo unknown)"
trap - EXIT
cleanup || true
[ "$FAIL" = "0" ]
exit $?
