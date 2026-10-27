#!/usr/bin/env bash
# Real Kind + projected Secret acceptance. Run on a Linux GitHub runner.
#
# Rotation is proven by identity, not by a substring: the source digest, the
# in-pod digest and the credential the running service actually accepts are all
# compared across the rotation, and the previous admin credential is proven to be
# rejected afterwards. Only digests are printed, never secret values.
set -o pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
STAMP="$(date -u +%Y%m%d%H%M%S)"
CLUSTER="r22-kind-$STAMP-$$"
NS="r22-kind-$STAMP-$$"
WORK="$(mktemp -d /tmp/r22-kind.XXXXXX)"
IMG="r22-candidate:local"
PF_PID=""; CLEAN_RC=0; POD_NAME="r22-onboard"
cleanup() {
  local rc=$?
  [ -z "$PF_PID" ] || kill "$PF_PID" 2>/dev/null
  if command -v kind >/dev/null 2>&1 && kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || { echo "cleanup: kind delete failed" >&2; CLEAN_RC=1; }
    if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then echo "cleanup: cluster still present" >&2; CLEAN_RC=1; fi
  fi
  docker image rm "$IMG" >/dev/null 2>&1
  if [ -d "$WORK" ]; then rm -rf "$WORK" || { echo "cleanup: work dir removal failed" >&2; CLEAN_RC=1; }; fi
  if [ "$rc" -ne 0 ]; then
    echo "test exit=$rc cleanup_exit=$CLEAN_RC" >&2
    exit "$rc"
  fi
  if [ "$CLEAN_RC" -ne 0 ]; then echo "CLEANUP_FAILED" >&2; exit 1; fi
  exit 0
}
trap cleanup EXIT
P=0; F=0
ok() { P=$((P+1)); echo "PASS $1"; }
bad() { F=$((F+1)); echo "FAIL $1"; }
fail() { echo "ERROR: $*" >&2; exit 1; }
command -v kind >/dev/null || fail "kind missing"
command -v kubectl >/dev/null || fail "kubectl missing"
docker info >/dev/null || fail "docker unavailable"

# digest helpers: identity only, never the value.
digest_file() { sha256sum "$1" | cut -d' ' -f1 | cut -c1-16; }
digest_pod() {
  kubectl -n "$NS" exec "$POD_NAME" -c onboardd -- sha256sum "$1" 2>/dev/null | cut -d' ' -f1 | cut -c1-16
}
# auth_status <file-with-bearer-token> <port>: header from a file so the token
# never appears in a process listing.
auth_status() {
  local token_file="$1" port="$2"
  printf 'header = "Authorization: Bearer %s"\n' "$(cat "$token_file")" >"$WORK/hdr.cfg"
  curl -s -o /dev/null -w '%{http_code}' --max-time 5 -K "$WORK/hdr.cfg" "http://127.0.0.1:$port/v1/ops/metrics"
}
wait_ready() {
  local port="$1" phase=""
  for _ in $(seq 1 90); do
    phase="$(kubectl -n "$NS" get pod "$POD_NAME" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Running" ] || { sleep 2; continue; }
    [ "$(kubectl -n "$NS" get pod "$POD_NAME" -o jsonpath='{range .status.containerStatuses[*]}{.ready}{end}' 2>/dev/null)" = "true" ] && break
    sleep 2
  done
  for _ in $(seq 1 60); do curl -sf -m 2 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && return 0; sleep 0.5; done
  return 1
}

kind create cluster --name "$CLUSTER" --wait 120s >/dev/null || fail "kind create"
kubectl create namespace "$NS" >/dev/null
docker build -q -t "$IMG" "$ROOT" >"$WORK/build.log" || { cat "$WORK/build.log"; fail "image build"; }
kind load docker-image "$IMG" --name "$CLUSTER" >/dev/null

python3 - "$WORK/master.key" "$WORK/admin.token" <<'PY'
import os, sys
open(sys.argv[1], 'wb').write(os.urandom(32))
open(sys.argv[2], 'w').write('kind-admin-' + os.urandom(16).hex() + '\n')
PY
kubectl -n "$NS" create secret generic onboarding-secrets \
  --from-file=onboarding_master_key="$WORK/master.key" \
  --from-file=onboard_admin_token="$WORK/admin.token" >/dev/null

cat >"$WORK/pod.yaml" <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: r22-onboard
spec:
  securityContext:
    seccompProfile: {type: RuntimeDefault}
    fsGroup: 10001
  initContainers:
    - name: materialize
      image: r22-candidate:local
      imagePullPolicy: Never
      command: ["/bin/sh","-c"]
      args: ["exec /app/scripts/materialize_secrets.sh --source-dir /run/secrets-source --target-dir /run/secrets --uid 10001 --gid 10001 --allow-k8s-projection"]
      securityContext:
        runAsUser: 0
        runAsGroup: 0
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
          add: ["CHOWN", "DAC_OVERRIDE", "FOWNER"]
      volumeMounts:
        - {name: projected, mountPath: /run/secrets-source, readOnly: true}
        - {name: materialized, mountPath: /run/secrets}
  containers:
    - name: onboardd
      image: r22-candidate:local
      imagePullPolicy: Never
      securityContext:
        runAsUser: 10001
        runAsGroup: 10001
        runAsNonRoot: true
        readOnlyRootFilesystem: true
        allowPrivilegeEscalation: false
        capabilities: {drop: ["ALL"]}
      env:
        - {name: ONBOARD_LISTEN, value: "0.0.0.0:8788"}
        - {name: ONBOARD_ALLOW_PUBLIC_LISTEN, value: "1"}
        - {name: ONBOARD_DB_PATH, value: "/app/data/state.sqlite"}
        - {name: ONBOARD_MASTER_KEY_FILE, value: "/run/secrets/onboarding_master_key"}
        - {name: ONBOARD_ADMIN_TOKEN_FILE, value: "/run/secrets/onboard_admin_token"}
        - {name: ONBOARD_FAKE_PROVIDERS, value: "true"}
        - {name: ONBOARD_DOMAINS, value: "moonbyte.test"}
      ports: [{containerPort: 8788}]
      volumeMounts:
        - {name: projected, mountPath: /run/secrets-source, readOnly: true}
        - {name: materialized, mountPath: /run/secrets, readOnly: true}
        - {name: data, mountPath: /app/data}
      readinessProbe:
        httpGet: {path: /healthz, port: 8788}
        initialDelaySeconds: 2
        periodSeconds: 2
  volumes:
    - name: projected
      projected:
        sources:
          - secret:
              name: onboarding-secrets
              items:
                - {key: onboarding_master_key, path: onboarding_master_key, mode: 0444}
                - {key: onboard_admin_token, path: onboard_admin_token, mode: 0444}
    - name: materialized
      emptyDir: {medium: Memory}
    - name: data
      emptyDir: {}
YAML
: # pod name is fixed by the manifest above
kubectl -n "$NS" apply -f "$WORK/pod.yaml" >/dev/null
for _ in $(seq 1 60); do
  [ "$(kubectl -n "$NS" get pod "$POD_NAME" -o jsonpath='{.status.phase}' 2>/dev/null || true)" = "Running" ] && break
  sleep 2
done
if [ "$(kubectl -n "$NS" get pod "$POD_NAME" -o jsonpath='{.status.phase}' 2>/dev/null)" != "Running" ]; then
  kubectl -n "$NS" logs "$POD_NAME" -c materialize 2>&1 || true
  kubectl -n "$NS" describe pod "$POD_NAME" | tail -60
  fail "pod not running"
fi
ok "kind pod running with projected secret"

perm="$(kubectl -n "$NS" exec "$POD_NAME" -c onboardd -- stat -c '%a %u:%g' /run/secrets/onboarding_master_key)"
[ "$perm" = "400 10001:10001" ] && ok "materialized mode/owner in pod" || bad "pod target mode: $perm"
if kubectl -n "$NS" exec "$POD_NAME" -c onboardd -- sh -c 'test -s /run/secrets/onboarding_master_key && test -s /run/secrets/onboard_admin_token'; then
  ok "app reads both secrets"; else bad "app secret read"; fi
target_listing="$(kubectl -n "$NS" exec "$POD_NAME" -c onboardd -- ls -A /run/secrets 2>/dev/null | sort | tr '\n' ' ')"
if [ "$target_listing" = "onboard_admin_token onboarding_master_key " ]; then
  ok "target holds exactly the two secret files (no journal or staging residue)"
else bad "target listing: $target_listing"; fi

kubectl -n "$NS" port-forward "pod/$POD_NAME" 18788:8788 >"$WORK/pf.log" 2>&1 & PF_PID=$!
curl -sf -m 3 http://127.0.0.1:18788/healthz >/dev/null && ok "kind healthz" || bad "kind healthz"

# ---- identity before rotation ---------------------------------------------
old_token_src="$(digest_file "$WORK/admin.token")"
old_key_src="$(digest_file "$WORK/master.key")"
old_token_pod="$(digest_pod /run/secrets/onboard_admin_token)"
old_key_pod="$(digest_pod /run/secrets/onboarding_master_key)"
[ "$old_token_pod" = "$old_token_src" ] && ok "pre-rotation: materialized admin token digest matches source" || bad "pre-rotation admin digest mismatch"
[ "$old_key_pod" = "$old_key_src" ] && ok "pre-rotation: materialized master key digest matches source" || bad "pre-rotation key digest mismatch"
old_status="$(auth_status "$WORK/admin.token" 18788)"
[ "$old_status" = "200" ] && ok "pre-rotation: running app accepts the current admin credential" || bad "pre-rotation admin auth status=$old_status"
cp "$WORK/admin.token" "$WORK/admin.token.old"

# ---- rotate only the admin token ------------------------------------------
python3 - "$WORK/admin.token" <<'PY'
import os, sys
open(sys.argv[1], 'w').write('kind-admin-rotated-' + os.urandom(16).hex() + '\n')
PY
new_token_src="$(digest_file "$WORK/admin.token")"
[ "$new_token_src" != "$old_token_src" ] && ok "rotation produced a different admin token digest" || bad "rotation digest unchanged"
kubectl -n "$NS" create secret generic onboarding-secrets \
  --from-file=onboarding_master_key="$WORK/master.key" \
  --from-file=onboard_admin_token="$WORK/admin.token" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" delete pod "$POD_NAME" --wait=true >/dev/null
kubectl -n "$NS" apply -f "$WORK/pod.yaml" >/dev/null
for _ in $(seq 1 60); do
  [ "$(kubectl -n "$NS" get pod "$POD_NAME" -o jsonpath='{.status.phase}' 2>/dev/null || true)" = "Running" ] && break
  sleep 2
done
[ "$(kubectl -n "$NS" get pod "$POD_NAME" -o jsonpath='{.status.phase}')" = "Running" ] || { kubectl -n "$NS" describe pod "$POD_NAME" | tail -40; fail "rotated pod"; }
ok "rotation pod running"
kill "$PF_PID" 2>/dev/null; PF_PID=""
kubectl -n "$NS" port-forward "pod/$POD_NAME" 18788:8788 >"$WORK/pf2.log" 2>&1 & PF_PID=$!
wait_ready 18788 || { kubectl -n "$NS" logs "$POD_NAME" -c onboardd 2>&1 | tail -30; fail "rotated pod not ready"; }
[ "$(curl -sf -m 3 http://127.0.0.1:18788/healthz >/dev/null && echo ok)" = ok ] && ok "rotated pod healthz" || bad "rotated pod healthz"

# ---- identity after rotation ----------------------------------------------
new_token_pod="$(digest_pod /run/secrets/onboard_admin_token)"
new_key_pod="$(digest_pod /run/secrets/onboarding_master_key)"
[ "$new_token_pod" = "$new_token_src" ] && ok "post-rotation: materialized admin token digest matches new source" || bad "post-rotation admin digest mismatch"
[ "$new_token_pod" != "$old_token_pod" ] && ok "post-rotation: admin token identity actually changed" || bad "post-rotation admin token identity unchanged"
[ "$new_key_pod" = "$old_key_pod" ] && ok "post-rotation: master key did not rotate (only the admin token did)" || bad "master key changed unexpectedly"
new_status="$(auth_status "$WORK/admin.token" 18788)"
[ "$new_status" = "200" ] && ok "post-rotation: running app accepts the new admin credential" || bad "post-rotation new admin auth status=$new_status"
old_after="$(auth_status "$WORK/admin.token.old" 18788)"
[ "$old_after" = "401" ] && ok "post-rotation: running app rejects the previous admin credential" || bad "post-rotation old admin auth status=$old_after"

# The materialized file must still be owner-only after recovery/restart.
perm_after="$(kubectl -n "$NS" exec "$POD_NAME" -c onboardd -- stat -c '%a %u:%g' /run/secrets/onboard_admin_token)"
[ "$perm_after" = "400 10001:10001" ] && ok "post-rotation target mode/owner" || bad "post-rotation target mode: $perm_after"

# ---- negative: symlink escape out of the projected root must be refused ----
cat >"$WORK/negative.yaml" <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: r22-negative
  namespace: $NS
spec:
  restartPolicy: Never
  containers:
    - name: neg
      image: r22-candidate:local
      imagePullPolicy: Never
      command: ["/bin/sh","-c"]
      args:
        - |
          mkdir -p /tmp/nsrc /tmp/ndst
          printf 'outside' > /tmp/noutside
          ln -s /tmp/noutside /tmp/nsrc/onboarding_master_key
          /app/scripts/materialize_secrets.sh --source-dir /tmp/nsrc --target-dir /tmp/ndst --allow-k8s-projection --uid 10001 --gid 10001
          echo UNEXPECTED_ACCEPT
YAML
kubectl -n "$NS" apply -f "$WORK/negative.yaml" >/dev/null
for _ in $(seq 1 60); do
  ph="$(kubectl -n "$NS" get pod r22-negative -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  case "$ph" in Succeeded|Failed) break ;; esac
  sleep 2
done
ph="$(kubectl -n "$NS" get pod r22-negative -o jsonpath='{.status.phase}' 2>/dev/null || echo missing)"
neg_log="$(kubectl -n "$NS" logs r22-negative 2>&1 || true)"
if [ "$ph" = "Failed" ]; then ok "symlink escape: materializer refused (pod failed)"; else bad "symlink escape: pod phase=$ph"; fi
if printf '%s' "$neg_log" | grep -q 'outside the projected root'; then
  ok "symlink escape: refusal reason recorded"; else bad "symlink escape: no refusal reason in log"; fi
if printf '%s' "$neg_log" | grep -q 'UNEXPECTED_ACCEPT'; then bad "symlink escape: materializer accepted an escaping link"; else ok "symlink escape: nothing was materialized"; fi

echo "SUMMARY pass=$P fail=$F"
[ "$F" -eq 0 ] && echo K8S_PROJECTED_SECRET_PASS
exit "$F"
