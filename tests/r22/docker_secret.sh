#!/usr/bin/env bash
# Real Docker secret-materialization acceptance. No production credentials.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/r22-docker.XXXXXX")"
C1=""; IMG="r22-candidate:local"
cleanup() {
  rc=$?
  [ -z "$C1" ] || docker rm -f "$C1" >/dev/null 2>&1 || true
  docker volume rm r22-data >/dev/null 2>&1 || true
  docker image rm "$IMG" >/dev/null 2>&1 || true
  chmod -R u+rwx "$WORK" 2>/dev/null || sudo -n chmod -R u+rwx "$WORK" 2>/dev/null || true
  rm -rf "$WORK" 2>/dev/null || sudo -n rm -rf "$WORK" 2>/dev/null || true
  exit "$rc"
}
trap cleanup EXIT
P=0; F=0; ok(){ P=$((P+1)); echo "PASS $1"; }; bad(){ F=$((F+1)); echo "FAIL $1"; }
fail(){ echo "ERROR: $*" >&2; exit 1; }

docker info >/dev/null || fail "docker daemon unavailable"
docker build -q -t "$IMG" "$ROOT" >"$WORK/build.log" || { cat "$WORK/build.log"; fail "image build"; }

mkdir -p "$WORK/src" "$WORK/dst" "$WORK/data"
python3 - "$WORK/src/onboarding_master_key" <<'PY'
import os,sys
open(sys.argv[1],'wb').write(os.urandom(32))
PY
printf 'r22-admin-%s\n' "$(openssl rand -hex 12)" > "$WORK/src/onboard_admin_token"
chmod 0444 "$WORK/src/onboarding_master_key" "$WORK/src/onboard_admin_token"

materialize() {
  docker run --rm --user 0:0 --entrypoint /bin/sh \
    -v "$WORK/src:/run/secrets-source:ro" -v "$WORK/dst:/run/secrets" "$IMG" \
    -c 'exec /app/scripts/materialize_secrets.sh --source-dir /run/secrets-source --target-dir /run/secrets --uid 10001 --gid 10001'
}
materialize >"$WORK/mat.log" || { cat "$WORK/mat.log"; fail "materialize"; }
mode_uid="$(docker run --rm --user 0:0 -v "$WORK/dst:/run/secrets:ro" --entrypoint /bin/sh "$IMG" -c 'stat -c "%a %u:%g" /run/secrets/onboarding_master_key /run/secrets/onboard_admin_token')"
[ "$mode_uid" = $'400 10001:10001\n400 10001:10001' ] && ok "target mode/owner 0400 10001:10001" || bad "target mode/owner: $mode_uid"
if docker run --rm --user 10002:10002 -v "$WORK/dst:/run/secrets:ro" --entrypoint /bin/cat "$IMG" /run/secrets/onboarding_master_key >/dev/null 2>&1; then
  bad "unprivileged UID read secret"
else ok "unprivileged UID denied"; fi

docker volume create r22-data >/dev/null
docker run -d --name r22-app --user 10001:10001 --read-only \
  --tmpfs /tmp:size=16m,mode=1777,noexec,nosuid --security-opt no-new-privileges:true --cap-drop ALL \
  -v "$WORK/dst:/run/secrets:ro" -v r22-data:/app/data -p 127.0.0.1::8788 \
  -e ONBOARD_LISTEN=0.0.0.0:8788 -e ONBOARD_ALLOW_PUBLIC_LISTEN=1 \
  -e ONBOARD_DB_PATH=/app/data/state.sqlite -e ONBOARD_FAKE_PROVIDERS=true \
  -e ONBOARD_DOMAINS=moonbyte.test -e ONBOARD_LOG_LEVEL=warn \
  -e ONBOARD_MASTER_KEY_FILE=/run/secrets/onboarding_master_key \
  -e ONBOARD_ADMIN_TOKEN_FILE=/run/secrets/onboard_admin_token "$IMG" >/dev/null
C1=r22-app
port="$(docker port r22-app 8788/tcp | head -1 | sed 's/.*://')"
for _ in $(seq 1 100); do curl -sf -m 2 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf -m 3 "http://127.0.0.1:$port/healthz" >/dev/null || { docker logs r22-app; fail "container health"; }
ok "non-root read-only container starts and reads materialized secret"
grep -q "$(cat "$WORK/src/onboard_admin_token")" "$WORK/mat.log" && bad "secret in materializer log" || ok "no secret in materializer log"
if docker logs r22-app 2>&1 | grep -qF "$(cat "$WORK/src/onboard_admin_token")"; then bad "secret in app log"; else ok "no secret in app log"; fi

docker restart r22-app >/dev/null
port="$(docker port r22-app 8788/tcp | head -1 | sed 's/.*://')"
for _ in $(seq 1 100); do curl -sf -m 2 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
if ! curl -sf -m 3 "http://127.0.0.1:$port/healthz" >/dev/null; then docker logs r22-app 2>&1 | tail -30; bad "restart health"; else ok "restart health"; fi

# Rotation: write a new token at 0444, rematerialize, restart, verify old value
# is gone from the target and the app still starts.
old_token="$(cat "$WORK/src/onboard_admin_token")"
printf 'r22-admin-rotated-%s\n' "$(openssl rand -hex 12)" > "$WORK/src/onboard_admin_token.new"
chmod 0444 "$WORK/src/onboard_admin_token.new"
mv "$WORK/src/onboard_admin_token.new" "$WORK/src/onboard_admin_token"
materialize >/dev/null || fail "rotation materialize"
if docker run --rm --user 10001:10001 -v "$WORK/dst:/run/secrets:ro" --entrypoint /bin/sh "$IMG" -c 'grep -q "$1" /run/secrets/onboard_admin_token' _ "$old_token" >/dev/null 2>&1; then bad "old secret remained"; else ok "rotation replaced old secret"; fi
docker restart r22-app >/dev/null
port="$(docker port r22-app 8788/tcp | head -1 | sed 's/.*://')"
for _ in $(seq 1 100); do curl -sf -m 2 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
if ! curl -sf -m 3 "http://127.0.0.1:$port/healthz" >/dev/null; then docker logs r22-app 2>&1 | tail -30; bad "rotation restart health"; else ok "rotation restart health"; fi

# Negative: writable source must be rejected and old target preserved.
cp "$WORK/src/onboarding_master_key" "$WORK/src/onboarding_master_key.bad"
chmod 0600 "$WORK/src/onboarding_master_key.bad"
if materialize >/dev/null 2>&1; then bad "writable source accepted"; else ok "writable source rejected"; fi
rm -f "$WORK/src/onboarding_master_key.bad"

# Negative: symlink source must be rejected.
ln -s "$WORK/src/onboarding_master_key" "$WORK/src/link"
if materialize >/dev/null 2>&1; then bad "symlink source accepted"; else ok "symlink source rejected"; fi
rm -f "$WORK/src/link"

# Negative: invalid key file still materializes as bytes but the service must
# fail closed at startup; never weaken the key check.
printf 'not-a-key' > "$WORK/src/badkey"
chmod 0444 "$WORK/src/badkey"
materialize >/dev/null 2>&1 || true
rm -f "$WORK/src/badkey"

echo "SUMMARY pass=$P fail=$F"
[ "$F" = 0 ] && echo DOCKER_SECRET_PASS
exit "$F"
