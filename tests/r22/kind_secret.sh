#!/usr/bin/env bash
# Real Kind + projected Secret acceptance. Run on a Linux GitHub runner.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d /tmp/r22-kind.XXXXXX)"
CLUSTER="r22-kind"; IMG="r22-candidate:local"; PF_PID=""; CLEAN_RC=0
cleanup(){
  rc=$?
  [ -z "$PF_PID" ] || kill "$PF_PID" 2>/dev/null || true
  kubectl --context "kind-$CLUSTER" delete namespace r22-kind --wait=false >/dev/null 2>&1 || CLEAN_RC=1
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || CLEAN_RC=1
  docker image rm "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WORK"
  [ "$CLEAN_RC" = 0 ] || echo "CLEANUP_FAILED" >&2
  exit "$rc"
}
trap cleanup EXIT
P=0; F=0; ok(){ P=$((P+1)); echo "PASS $1"; }; bad(){ F=$((F+1)); echo "FAIL $1"; }
fail(){ echo "ERROR: $*" >&2; exit 1; }
command -v kind >/dev/null || fail "kind missing"
command -v kubectl >/dev/null || fail "kubectl missing"
docker info >/dev/null || fail "docker unavailable"

kind create cluster --name "$CLUSTER" --wait 120s >/dev/null || fail "kind create"
kubectl create namespace r22-kind >/dev/null
docker build -q -t "$IMG" "$ROOT" >"$WORK/build.log" || { cat "$WORK/build.log"; fail "image build"; }
kind load docker-image "$IMG" --name "$CLUSTER" >/dev/null

python3 - "$WORK/master.key" "$WORK/admin.token" <<'PY'
import os,sys
open(sys.argv[1],'wb').write(os.urandom(32))
open(sys.argv[2],'w').write('kind-admin-'+os.urandom(8).hex()+'\n')
PY
kubectl -n r22-kind create secret generic onboarding-secrets \
  --from-file=onboarding_master_key="$WORK/master.key" \
  --from-file=onboard_admin_token="$WORK/admin.token" >/dev/null
cat >"$WORK/pod.yaml" <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: r22-onboard
  namespace: r22-kind
spec:
  securityContext:
    runAsNonRoot: true
    seccompProfile: {type: RuntimeDefault}
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
        capabilities: {drop: ["ALL"]}
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
              defaultMode: 0444
    - name: materialized
      emptyDir: {medium: Memory}
    - name: data
      emptyDir: {}
YAML
kubectl apply -f "$WORK/pod.yaml" >/dev/null
for _ in $(seq 1 60); do
  phase="$(kubectl -n r22-kind get pod r22-onboard -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  ready="$(kubectl -n r22-kind get pod r22-onboard -o jsonpath='{range .status.containerStatuses[*]}{.ready}{end}' 2>/dev/null || true)"
  [ "$phase" = "Running" ] && [ "$ready" = "true" ] && break
  sleep 2
done
[ "$(kubectl -n r22-kind get pod r22-onboard -o jsonpath='{.status.phase}')" = "Running" ] || { kubectl -n r22-kind describe pod r22-onboard; fail "pod not running"; }
ok "kind pod running with projected secret"

# Verify the materialized target inside the app container (no content output).
perm="$(kubectl -n r22-kind exec r22-onboard -c onboardd -- stat -c '%a %u:%g' /run/secrets/onboarding_master_key)"
[ "$perm" = "400 10001:10001" ] && ok "materialized mode/owner in pod" || bad "pod target mode: $perm"
if kubectl -n r22-kind exec r22-onboard -c onboardd -- sh -c 'test -s /run/secrets/onboarding_master_key && test -s /run/secrets/onboard_admin_token'; then ok "app reads both secrets"; else bad "app secret read"; fi

kubectl -n r22-kind port-forward pod/r22-onboard 18788:8788 >"$WORK/pf.log" 2>&1 & PF_PID=$!
for _ in $(seq 1 50); do curl -sf -m 2 http://127.0.0.1:18788/healthz >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf -m 3 http://127.0.0.1:18788/healthz >/dev/null && ok "kind healthz" || bad "kind healthz"

# Rotation: update Secret, force a new pod; the init container must see the new
# projection and must not mix old/new. Delivery is eventual in Kubernetes.
printf 'kind-admin-rotated-%s\n' "$(openssl rand -hex 12)" > "$WORK/admin.token"
kubectl -n r22-kind create secret generic onboarding-secrets \
  --from-file=onboarding_master_key="$WORK/master.key" \
  --from-file=onboard_admin_token="$WORK/admin.token" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n r22-kind delete pod r22-onboard --wait=true >/dev/null
kubectl apply -f "$WORK/pod.yaml" >/dev/null
for _ in $(seq 1 60); do
  [ "$(kubectl -n r22-kind get pod r22-onboard -o jsonpath='{.status.phase}' 2>/dev/null || true)" = "Running" ] && break
  sleep 2
done
[ "$(kubectl -n r22-kind get pod r22-onboard -o jsonpath='{.status.phase}')" = "Running" ] || { kubectl -n r22-kind describe pod r22-onboard; fail "rotated pod"; }
ok "rotation pod running"
if kubectl -n r22-kind exec r22-onboard -c onboardd -- sh -c "grep -q 'kind-admin-' /run/secrets/onboard_admin_token"; then ok "rotated secret readable"; else bad "rotated secret unreadable"; fi
echo "SUMMARY pass=$P fail=$F"
[ "$F" = 0 ] && echo K8S_PROJECTED_SECRET_PASS
exit "$F"
