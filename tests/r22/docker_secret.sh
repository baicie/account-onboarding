#!/usr/bin/env bash
# Real Docker secret-materialization acceptance. No production credentials.
#
# Identity is proven with digests (source vs materialized), rotation must change
# the admin token and leave the master key untouched, and an interrupted
# transaction left by a killed materializer must be replayed by the next run.
set -o pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TMPROOT="$TMPDIR"; [ -n "$TMPROOT" ] || TMPROOT=/tmp
WORK="$(mktemp -d "$TMPROOT/r22-docker.XXXXXX")"
SUFFIX="$$-$(date -u +%s)"
C1="r22-app-$SUFFIX"; VOL="r22-data-$SUFFIX"; IMG="r22-candidate:$SUFFIX"
CLEAN_RC=0
cleanup() {
  local rc=$?
  [ -z "$C1" ] || docker rm -f "$C1" >/dev/null 2>&1
  docker volume rm "$VOL" >/dev/null 2>&1
  docker image rm "$IMG" >/dev/null 2>&1
  if docker ps -a --format '{{.Names}}' | grep -qx "$C1"; then echo "cleanup: container $C1 still present" >&2; CLEAN_RC=1; fi
  if docker volume ls --format '{{.Name}}' | grep -qx "$VOL"; then echo "cleanup: volume $VOL still present" >&2; CLEAN_RC=1; fi
  if docker images --format '{{.Repository}}:{{.Tag}}' | grep -qx "$IMG"; then echo "cleanup: image $IMG still present" >&2; CLEAN_RC=1; fi
  # The materialized target is 0700 and owned by the runtime UID, so the runner
  # user cannot remove it without the privilege the materializer itself used.
  chmod -R u+rwX "$WORK" 2>/dev/null
  rm -rf "$WORK" 2>/dev/null
  if [ -d "$WORK" ]; then sudo -n rm -rf "$WORK" 2>/dev/null; fi
  if [ -d "$WORK" ]; then echo "cleanup: work dir still present" >&2; CLEAN_RC=1; fi
  if [ "$rc" -ne 0 ]; then echo "test exit=$rc cleanup_exit=$CLEAN_RC" >&2; exit "$rc"; fi
  if [ "$CLEAN_RC" -ne 0 ]; then echo "CLEANUP_FAILED" >&2; exit 1; fi
  exit 0
}
trap cleanup EXIT
P=0; F=0
ok() { P=$((P+1)); echo "PASS $1"; }
bad() { F=$((F+1)); echo "FAIL $1"; }
fail() { echo "ERROR: $*" >&2; exit 1; }
digest_file() { sha256sum "$1" | cut -d' ' -f1 | cut -c1-16; }
digest_in_image() {
  docker run --rm --user 10001:10001 -v "$WORK/dst:/run/secrets:ro" --entrypoint /bin/sh "$IMG" \
    -c "sha256sum /run/secrets/$1" | cut -d' ' -f1 | cut -c1-16
}

docker info >/dev/null || fail "docker daemon unavailable"
docker build -q -t "$IMG" "$ROOT" >"$WORK/build.log" || { cat "$WORK/build.log"; fail "image build"; }

mkdir -p "$WORK/src" "$WORK/dst"
python3 - "$WORK/src/onboarding_master_key" <<'PY'
import os, sys
open(sys.argv[1], 'wb').write(os.urandom(32))
PY
printf 'r22-admin-%s\n' "$(openssl rand -hex 16)" >"$WORK/src/onboard_admin_token"
chmod 0444 "$WORK/src/onboarding_master_key" "$WORK/src/onboard_admin_token"
KEY_SRC="$(digest_file "$WORK/src/onboarding_master_key")"
TOKEN_SRC="$(digest_file "$WORK/src/onboard_admin_token")"

materialize() {
  docker run --rm --user 0:0 --entrypoint /bin/sh \
    -v "$WORK/src:/run/secrets-source:ro" -v "$WORK/dst:/run/secrets" "$IMG" \
    -c 'exec /app/scripts/materialize_secrets.sh --source-dir /run/secrets-source --target-dir /run/secrets --uid 10001 --gid 10001'
}
materialize >"$WORK/mat.log" || { cat "$WORK/mat.log"; fail "materialize"; }
mode_uid="$(docker run --rm --user 0:0 -v "$WORK/dst:/run/secrets:ro" --entrypoint /bin/sh "$IMG" -c 'stat -c "%a %u:%g" /run/secrets/onboarding_master_key /run/secrets/onboard_admin_token')"
if [ "$(printf '%s' "$mode_uid" | tr -d '\n')" = "400 10001:10001400 10001:10001" ]; then
  ok "target mode/owner 0400 10001:10001"
else bad "target mode/owner: $mode_uid"; fi
[ "$(digest_in_image onboarding_master_key)" = "$KEY_SRC" ] && ok "materialized master key matches the source digest" || bad "master key digest mismatch"
[ "$(digest_in_image onboard_admin_token)" = "$TOKEN_SRC" ] && ok "materialized admin token matches the source digest" || bad "admin token digest mismatch"
listing="$(docker run --rm --user 10001:10001 -v "$WORK/dst:/run/secrets:ro" --entrypoint /bin/sh "$IMG" -c 'ls -A /run/secrets | sort | tr "\n" " "')"
[ "$listing" = "onboard_admin_token onboarding_master_key " ] && ok "target holds exactly the two secret files" || bad "target listing: $listing"
if docker run --rm --user 10002:10002 -v "$WORK/dst:/run/secrets:ro" --entrypoint /bin/cat "$IMG" /run/secrets/onboarding_master_key >/dev/null 2>&1; then
  bad "unprivileged UID read secret"; else ok "unprivileged UID denied"; fi

docker volume create "$VOL" >/dev/null
docker run -d --name "$C1" --user 10001:10001 --read-only \
  --tmpfs /tmp:size=16m,mode=1777,noexec,nosuid --security-opt no-new-privileges:true --cap-drop ALL \
  -v "$WORK/dst:/run/secrets:ro" -v "$VOL:/app/data" -p 127.0.0.1::8788 \
  -e ONBOARD_LISTEN=0.0.0.0:8788 -e ONBOARD_ALLOW_PUBLIC_LISTEN=1 \
  -e ONBOARD_DB_PATH=/app/data/state.sqlite -e ONBOARD_FAKE_PROVIDERS=true \
  -e ONBOARD_DOMAINS=moonbyte.test -e ONBOARD_LOG_LEVEL=warn \
  -e ONBOARD_MASTER_KEY_FILE=/run/secrets/onboarding_master_key \
  -e ONBOARD_ADMIN_TOKEN_FILE=/run/secrets/onboard_admin_token "$IMG" >/dev/null
port_of() { docker port "$C1" 8788/tcp | head -1 | sed 's/.*://'; }
wait_health() {
  local port
  for _ in $(seq 1 100); do
    port="$(port_of)"; [ -n "$port" ] || { sleep 0.2; continue; }
    curl -sf -m 2 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  return 1
}
wait_health || { docker logs "$C1"; fail "container health"; }
ok "non-root read-only container starts and reads materialized secret"
if grep -qF "$(cat "$WORK/src/onboard_admin_token")" "$WORK/mat.log"; then bad "secret in materializer log"; else ok "no secret in materializer log"; fi
if docker logs "$C1" 2>&1 | grep -qF "$(cat "$WORK/src/onboard_admin_token")"; then bad "secret in app log"; else ok "no secret in app log"; fi

docker restart "$C1" >/dev/null
wait_health && ok "restart health" || { docker logs "$C1" 2>&1 | tail -20; bad "restart health"; }

# ---- rotation: only the admin token changes --------------------------------
cp "$WORK/src/onboard_admin_token" "$WORK/admin.token.old"
printf 'r22-admin-rotated-%s\n' "$(openssl rand -hex 16)" >"$WORK/src/onboard_admin_token.new"
chmod 0444 "$WORK/src/onboard_admin_token.new"
mv "$WORK/src/onboard_admin_token.new" "$WORK/src/onboard_admin_token"
TOKEN_SRC_NEW="$(digest_file "$WORK/src/onboard_admin_token")"
[ "$TOKEN_SRC_NEW" != "$TOKEN_SRC" ] && ok "rotation produced a different admin token digest" || bad "rotation digest unchanged"
materialize >/dev/null || fail "rotation materialize"
[ "$(digest_in_image onboard_admin_token)" = "$TOKEN_SRC_NEW" ] && ok "rotated target matches the new source digest" || bad "rotated target digest mismatch"
[ "$(digest_in_image onboarding_master_key)" = "$KEY_SRC" ] && ok "master key unchanged by admin token rotation" || bad "master key changed unexpectedly"
docker restart "$C1" >/dev/null
wait_health && ok "rotation restart health" || { docker logs "$C1" 2>&1 | tail -20; bad "rotation restart health"; }

# ---- interrupted transaction replay ---------------------------------------
# The target is 0700 and owned by the runtime UID, so the killed-run state has to
# be recreated as root inside a container, exactly as a killed materializer
# leaves it behind.
docker run --rm --user 0:0 -v "$WORK/dst:/run/secrets" --entrypoint /bin/sh "$IMG" -c '
  mkdir -p /run/secrets/.materialize.txn/backup
  printf "killed-run-old-token\n" > /run/secrets/.materialize.txn/backup/onboard_admin_token
  printf "B onboard_admin_token %s\n" "$(printf "B onboard_admin_token" | sha256sum | cut -d" " -f1 | cut -c1-16)" > /run/secrets/.materialize.txn/journal
  printf "killed-run-partial\n" > /run/secrets/onboard_admin_token
' || fail "cannot create the interrupted transaction fixture"
materialize >"$WORK/recover.log" 2>&1 || { cat "$WORK/recover.log"; fail "recovery materialize"; }
[ "$(digest_in_image onboard_admin_token)" = "$TOKEN_SRC_NEW" ] && ok "interrupted transaction replayed before installing the new set" || bad "recovery left the wrong token"
if docker run --rm --user 0:0 -v "$WORK/dst:/run/secrets:ro" --entrypoint /bin/sh "$IMG" -c 'test -e /run/secrets/.materialize.txn'; then
  bad "transaction directory survived recovery"; else ok "transaction directory removed after recovery"; fi
if docker run --rm --user 0:0 -v "$WORK/dst:/run/secrets:ro" --entrypoint /bin/sh "$IMG" -c 'ls -A /run/secrets' | grep -q '^\.staging\.'; then
  bad "staging directory left behind"; else ok "no staging directory left behind"; fi

# ---- negatives -------------------------------------------------------------
cp "$WORK/src/onboarding_master_key" "$WORK/src/onboarding_master_key.bad"
chmod 0600 "$WORK/src/onboarding_master_key.bad"
if materialize >/dev/null 2>&1; then bad "writable source accepted"; else ok "writable source rejected"; fi
rm -f "$WORK/src/onboarding_master_key.bad"
[ "$(digest_in_image onboarding_master_key)" = "$KEY_SRC" ] && ok "rejected batch left the live set untouched" || bad "rejected batch changed the live set"

ln -s "$WORK/src/onboarding_master_key" "$WORK/src/link"
if materialize >/dev/null 2>&1; then bad "symlink source accepted"; else ok "symlink source rejected"; fi
rm -f "$WORK/src/link"

printf 'not-a-key' >"$WORK/src/badkey"
chmod 0444 "$WORK/src/badkey"
if materialize >/dev/null 2>&1; then ok "valid-bytes projection still materializes; the service fails closed at startup"; else bad "materialize rejected a readable projection"; fi
rm -f "$WORK/src/badkey"
materialize >/dev/null 2>&1 || fail "final materialize"

echo "SUMMARY pass=$P fail=$F"
[ "$F" -eq 0 ] && echo DOCKER_SECRET_PASS
exit "$F"
