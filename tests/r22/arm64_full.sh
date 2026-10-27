#!/usr/bin/env bash
# ARM64 full acceptance (linux/arm64 emulated runtime).
#
# Everything that touches state runs *inside* the arm64 container: migrations,
# SQLite/WAL, run creation, outbox, backup, verify, restore and the rollback
# replay. The host only drives the HTTP client and asserts container-level
# properties. There is no "emulator unavailable -> still pass" path: a missing
# runtime is a failure, not a skip.
set -o pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
PLATFORM="linux/arm64"
COMMIT="$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
SUFFIX="$$-$(date -u +%s)"
IMG="onboard-r22-arm64:$SUFFIX"
APP="r22-arm64-app-$SUFFIX"
APP2="r22-arm64-restored-$SUFFIX"
WORK="$(mktemp -d /tmp/r22-arm64.XXXXXX)"
pick_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }
PORT="$(pick_port)"; PORT2="$(pick_port)"
EVIDENCE=""; CLEAN_RC=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --evidence-dir) EVIDENCE="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -z "$EVIDENCE" ] || mkdir -p "$EVIDENCE"
P=0; F=0; SKIP=0
ok() { P=$((P+1)); echo "PASS $1"; }
bad() { F=$((F+1)); echo "FAIL $1"; }
skip() { SKIP=$((SKIP+1)); echo "SKIP $1"; }
fail() { echo "FATAL: $*" >&2; exit 1; }
cleanup() {
  local rc=$?
  docker rm -f "$APP" >/dev/null 2>&1
  docker rm -f "$APP2" >/dev/null 2>&1
  docker image rm "$IMG" >/dev/null 2>&1
  if docker ps -a --format '{{.Names}}' | grep -qx "$APP"; then echo "cleanup: app container left" >&2; CLEAN_RC=1; fi
  if docker images --format '{{.Repository}}:{{.Tag}}' | grep -qx "$IMG"; then echo "cleanup: image left" >&2; CLEAN_RC=1; fi
  rm -rf "$WORK" 2>/dev/null
  if [ -d "$WORK" ]; then echo "cleanup: work dir left" >&2; CLEAN_RC=1; fi
  echo "SUMMARY pass=$P fail=$F skip=$SKIP"
  if [ "$rc" -ne 0 ]; then echo "test exit=$rc cleanup_exit=$CLEAN_RC" >&2; exit "$rc"; fi
  if [ "$CLEAN_RC" -ne 0 ]; then echo "CLEANUP_FAILED" >&2; exit 1; fi
  if [ "$F" -ne 0 ]; then exit 1; fi
  exit 0
}
trap cleanup EXIT
in_app() { docker exec "$APP" "$@"; }
api() {
  local port="$1" method="$2" path="$3" body=""
  if [ "$#" -ge 4 ]; then body="$4"; fi
  if [ -n "$body" ]; then
    curl -s -m 10 -o "$WORK/resp.json" -w '%{http_code}' -X "$method" -H @"$WORK/hdr.cfg" -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$port$path"
  else
    curl -s -m 10 -o "$WORK/resp.json" -w '%{http_code}' -X "$method" -H @"$WORK/hdr.cfg" "http://127.0.0.1:$port$path"
  fi
}
wait_ready() {
  local port="$1" c="$2"
  for _ in $(seq 1 150); do
    curl -sf -m 2 "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && return 0
    [ "$(docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null)" = "true" ] || return 1
    sleep 0.5
  done
  return 1
}
run_id_of() { python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("id") or d.get("run",{}).get("id",""))' "$WORK/resp.json" 2>/dev/null; }
snap_of() { python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("snapshot") or d.get("path") or "")' "$WORK/bak.json" 2>/dev/null; }

echo "=== ARM64 full acceptance ==="
docker buildx version >/dev/null 2>&1 || fail "buildx missing"
docker buildx build --platform "$PLATFORM" -f "$ROOT/tests/r22/Dockerfile.arm64test" \
  --build-arg VERSION=0.1.0 --build-arg COMMIT="$COMMIT" -t "$IMG" --load "$ROOT" >"$WORK/build.log" 2>&1 \
  || { tail -30 "$WORK/build.log"; fail "arm64 image build"; }
ok "arm64 image built for $PLATFORM"
img_arch="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$IMG")"
[ "$img_arch" = "$PLATFORM" ] && ok "image platform is $PLATFORM" || bad "image platform: $img_arch"
runtime_arch="$(docker run --rm --platform "$PLATFORM" --entrypoint uname "$IMG" -m)"
[ "$runtime_arch" = "aarch64" ] && ok "runtime reports aarch64 (uname -m)" || bad "runtime arch: $runtime_arch"
host_arch="$(docker version --format '{{.Server.Arch}}' 2>/dev/null)"
echo "host_arch=$host_arch runtime_arch=$runtime_arch image=$IMG commit=$COMMIT"
[ -z "$EVIDENCE" ] || printf 'host_arch=%s\nruntime_arch=%s\nimage=%s\ncommit=%s\nplatform=%s\n' "$host_arch" "$runtime_arch" "$IMG" "$COMMIT" "$PLATFORM" >"$EVIDENCE/arm64.txt"

mkdir -p "$WORK/src" "$WORK/dst" "$WORK/data" "$WORK/backups"
python3 - "$WORK/src/onboarding_master_key" "$WORK/src/onboard_admin_token" <<'PY'
import os, sys
open(sys.argv[1], 'wb').write(os.urandom(32))
open(sys.argv[2], 'w').write('arm64-admin-' + os.urandom(16).hex() + '\n')
PY
chmod 0444 "$WORK/src/onboarding_master_key" "$WORK/src/onboard_admin_token"
docker run --rm --platform "$PLATFORM" --user 0:0 --entrypoint /bin/sh \
  -v "$WORK/src:/src:ro" -v "$WORK/dst:/run/secrets" "$IMG" \
  -c 'exec /app/scripts/materialize_secrets.sh --source-dir /src --target-dir /run/secrets --uid 10001 --gid 10001' >"$WORK/mat.log" 2>&1 \
  || { cat "$WORK/mat.log"; fail "in-container secret materialization"; }
mat_mode="$(docker run --rm --platform "$PLATFORM" --user 0:0 -v "$WORK/dst:/run/secrets:ro" --entrypoint /bin/sh "$IMG" -c 'stat -c "%a %u:%g" /run/secrets/onboarding_master_key')"
[ "$mat_mode" = "400 10001:10001" ] && ok "secret materialized inside the arm64 container (0400 10001:10001)" || bad "materialized mode: $mat_mode"
if grep -qF "$(cat "$WORK/src/onboard_admin_token")" "$WORK/mat.log"; then bad "materializer logged the secret"; else ok "materializer logged no secret value"; fi

printf 'header = "Authorization: Bearer %s"\n' "$(cat "$WORK/src/onboard_admin_token")" >"$WORK/hdr.cfg"

start_app() {
  docker run -d --platform "$PLATFORM" --name "$1" --user 10001:10001 --read-only \
    --security-opt no-new-privileges:true --cap-drop ALL \
    --tmpfs /tmp:size=32m,mode=1777,noexec,nosuid \
    -v "$WORK/dst:/run/secrets:ro" -v "$WORK/data:/app/data" -p "127.0.0.1:$2:8788" \
    -e ONBOARD_LISTEN=0.0.0.0:8788 -e ONBOARD_ALLOW_PUBLIC_LISTEN=1 \
    -e ONBOARD_DB_PATH="$3" -e ONBOARD_FAKE_PROVIDERS=true \
    -e ONBOARD_DOMAINS=moonbyte.test -e ONBOARD_LOG_LEVEL=warn \
    -e ONBOARD_MASTER_KEY_FILE=/run/secrets/onboarding_master_key \
    -e ONBOARD_ADMIN_TOKEN_FILE=/run/secrets/onboard_admin_token "$IMG" >/dev/null
}
start_app "$APP" "$PORT" /app/data/state.sqlite
wait_ready "$PORT" "$APP" || { docker logs "$APP" 2>&1 | tail -30; fail "arm64 service not ready"; }
ok "arm64 service starts and becomes ready"
[ "$(in_app id -u)" = "10001" ] && ok "runtime UID is 10001" || bad "runtime UID: $(in_app id -u)"
[ "$(in_app id -g)" = "10001" ] && ok "runtime GID is 10001" || bad "runtime GID: $(in_app id -g)"
if in_app sh -c 'touch /app/readonly-probe' >/dev/null 2>&1; then bad "root filesystem is writable"; else ok "root filesystem is read-only"; fi
in_app sh -c 'ls /app/data/state.sqlite' >/dev/null 2>&1 && ok "SQLite database created inside the arm64 container" || bad "no database file"
in_app sh -c 'ls /app/data | grep -q state.sqlite-wal' >/dev/null 2>&1 && ok "SQLite WAL file present" || bad "no WAL file"

create_body='{"template":"handle_management","domain":"moonbyte.test","desired_username":"arm64probe","idempotency_key":"arm64-'$SUFFIX'"}'
code="$(api "$PORT" POST /v1/runs "$create_body")"
case "$code" in 200|201|202) ok "run created through the arm64 service (HTTP $code)";; *) bad "run create HTTP $code: $(head -c 160 "$WORK/resp.json")";; esac
RUN_ID="$(run_id_of)"
[ -n "$RUN_ID" ] && ok "run id returned" || bad "no run id in the response"
code="$(api "$PORT" GET /v1/ops/external)"
[ "$code" = "200" ] && ok "external operations readable inside the arm64 service" || bad "external ops HTTP $code"
code="$(api "$PORT" GET /v1/ops/alerts/outbox)"
[ "$code" = "200" ] && ok "durable alert outbox readable inside the arm64 service" || bad "outbox HTTP $code"
code="$(api "$PORT" GET /v1/audit)"
[ "$code" = "200" ] && ok "audit trail readable inside the arm64 service" || bad "audit HTTP $code"

docker kill -s KILL "$APP" >/dev/null
docker rm -f "$APP" >/dev/null 2>&1
start_app "$APP" "$PORT" /app/data/state.sqlite
wait_ready "$PORT" "$APP" || { docker logs "$APP" 2>&1 | tail -30; fail "service did not recover after SIGKILL"; }
ok "service recovers after SIGKILL and restart"
if [ -n "$RUN_ID" ]; then
  code="$(api "$PORT" GET "/v1/runs/$RUN_ID")"
  [ "$code" = "200" ] && ok "run survived SIGKILL (persistence)" || bad "run lost after restart (HTTP $code)"
fi

in_app backup -db /app/data/state.sqlite -out-dir /app/backups -encrypt -keyring /run/secrets/onboarding_master_key >"$WORK/bak.json" 2>"$WORK/bak.err" \
  || { cat "$WORK/bak.err"; bad "in-container backup failed"; }
SNAP="$(snap_of)"
[ -n "$SNAP" ] && ok "encrypted backup created inside the arm64 container" || bad "backup produced no snapshot: $(head -c 120 "$WORK/bak.json")"
if [ -n "$SNAP" ]; then
  if in_app verify -snapshot "$SNAP" -keyring /run/secrets/onboarding_master_key >"$WORK/verify.json" 2>"$WORK/verify.err"; then
    ok "backup verified inside the arm64 container (checksum, integrity, schema)"
  else cat "$WORK/verify.err"; bad "in-container verify failed"; fi
  schema="$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("schema_version") or d.get("schemaVersion") or "")' "$WORK/verify.json" 2>/dev/null)"
  [ "$schema" = "6" ] && ok "snapshot schema_version is 6" || bad "snapshot schema_version was not 6"
  if in_app restore -snapshot "$SNAP" -target /app/data/restored.sqlite -keyring /run/secrets/onboarding_master_key -force >"$WORK/restore.json" 2>"$WORK/restore.err"; then
    ok "restore into a new database succeeded inside the arm64 container"
  else cat "$WORK/restore.err"; bad "in-container restore failed"; fi
  start_app "$APP2" "$PORT2" /app/data/restored.sqlite
  if wait_ready "$PORT2" "$APP2"; then
    ok "restored database boots the arm64 service"
    if [ -n "$RUN_ID" ]; then
      code="$(api "$PORT2" GET "/v1/runs/$RUN_ID")"
      [ "$code" = "200" ] && ok "restored database still contains the run" || bad "restored database lost the run (HTTP $code)"
    fi
  else docker logs "$APP2" 2>&1 | tail -20; bad "restored database failed to boot"; fi
  docker rm -f "$APP2" >/dev/null 2>&1

  python3 -c 'import os,sys;open(sys.argv[1],"wb").write(os.urandom(32))' "$WORK/dst/wrong.key"
  chmod 0400 "$WORK/dst/wrong.key" 2>/dev/null
  if in_app verify -snapshot "$SNAP" -keyring /run/secrets/wrong.key >/dev/null 2>&1; then
    bad "verify accepted a wrong master key"; else ok "verify rejects a wrong master key"; fi
  in_app sh -c "printf corrupt | dd of='$SNAP' bs=1 seek=64 conv=notrunc status=none" >/dev/null 2>&1
  if in_app verify -snapshot "$SNAP" -keyring /run/secrets/onboarding_master_key >/dev/null 2>&1; then
    bad "verify accepted a corrupted snapshot"; else ok "verify rejects a corrupted snapshot"; fi

  docker rm -f "$APP" >/dev/null 2>&1
  start_app "$APP" "$PORT" /app/data/state.sqlite
  wait_ready "$PORT" "$APP" || { docker logs "$APP" 2>&1 | tail -20; fail "service did not resume after the negative restore cases"; }
  ok "live database still serves after failed restore attempts"
fi

in_app backup -db /app/data/state.sqlite -out-dir /app/backups -encrypt -keyring /run/secrets/onboarding_master_key >"$WORK/bak2.json" 2>/dev/null
SNAP2="$(snap_of)"
if [ -n "$SNAP2" ]; then
  api "$PORT" POST /v1/runs '{"template":"handle_management","domain":"moonbyte.test","desired_username":"rollbackprobe","idempotency_key":"rb-'$SUFFIX'"}' >/dev/null
  NEW_RUN="$(run_id_of)"
  docker rm -f "$APP" >/dev/null 2>&1
  if in_app restore -snapshot "$SNAP2" -target /app/data/state.sqlite -live-db /app/data/state.sqlite -keyring /run/secrets/onboarding_master_key -force -keep-old >"$WORK/rb.json" 2>"$WORK/rb.err"; then
    ok "rollback replay: snapshot restored over the live database"
  else cat "$WORK/rb.err"; bad "rollback restore failed"; fi
  in_app sh -c 'ls /app/data/state.sqlite.pre-restore-*' >/dev/null 2>&1 && ok "rollback kept the replaced database" || bad "rollback did not keep the replaced database"
  start_app "$APP" "$PORT" /app/data/state.sqlite
  if wait_ready "$PORT" "$APP"; then
    if [ -n "$RUN_ID" ]; then
      code="$(api "$PORT" GET "/v1/runs/$RUN_ID")"
      [ "$code" = "200" ] && ok "rollback kept the pre-rollback run" || bad "rollback lost the pre-rollback run (HTTP $code)"
    fi
    if [ -n "$NEW_RUN" ]; then
      code="$(api "$PORT" GET "/v1/runs/$NEW_RUN")"
      [ "$code" = "404" ] && ok "rollback removed the post-snapshot run (HTTP 404)" || bad "post-snapshot run still present (HTTP $code)"
    fi
  else docker logs "$APP" 2>&1 | tail -20; bad "rollback service not ready"; fi
else bad "rollback: no snapshot produced"; fi

skip "Caddy TLS ingress on arm64 (staging gate covers TLS; not re-run here)"
skip "schema5 to schema6 migration from a historical binary (historical backup gate)"
echo "ARM64_IMAGE=$IMG RUNTIME=$runtime_arch HOST_ARCH=$host_arch"
if [ "$F" -eq 0 ]; then echo ARM64_FULL_EMULATED_PASS; else echo ARM64_FULL_EMULATED_FAIL; fi
exit "$F"
