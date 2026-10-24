#!/usr/bin/env bash
# R6 P0-B: compose configuration matrix (static; no daemon, no containers).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$ROOT/.sandbox/r6/compose-matrix"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
rm -rf "$WORK"; mkdir -p "$WORK"/{data,secrets}; umask 077
head -c 32 /dev/urandom > "$WORK/secrets/master.key"
head -c 32 /dev/urandom | base64 > "$WORK/secrets/admin.token"
head -c 32 /dev/urandom | base64 > "$WORK/secrets/readonly.token"
chmod 600 "$WORK/secrets/"*
chmod 777 "$WORK/data"

assert_config() { # $1 yaml file, $2 label, $3 expected port
  python3 - "$1" "$2" "$3" <<'PY'
import sys, yaml
path, label, port = sys.argv[1], sys.argv[2], sys.argv[3]
d = yaml.safe_load(open(path))
svc = (d.get("services") or {}).get("onboarding")
if not svc:
    print(f"FAIL {label}: no onboarding service"); sys.exit(1)
fail = []
def check(cond, msg):
    if cond: print(f"PASS {label}: {msg}")
    else: fail.append(msg)
check(svc.get("read_only") is True, "read_only=true")
check(str(svc.get("user")) == "10001:10001", "user=10001:10001")
check("ALL" in [str(x) for x in (svc.get("cap_drop") or [])], "cap_drop=ALL")
check(any("no-new-privileges" in str(x) for x in (svc.get("security_opt") or [])), "no-new-privileges")
check((svc.get("restart") or "") == "unless-stopped", "restart=unless-stopped")
hc = svc.get("healthcheck") or {}
hc_test = " ".join(str(x) for x in (hc.get("test") or []))
check("healthcheck" in hc_test, "healthcheck uses onboardd healthcheck")
env = svc.get("environment") or {}
if isinstance(env, list): env = dict(e.split("=", 1) for e in env if "=" in e)
check("ONBOARD_ADMIN_TOKEN" not in env and "ONBOARD_MASTER_KEY" not in env, "no secret values in environment")
check(env.get("ONBOARD_ADMIN_TOKEN_FILE") == "/run/secrets/onboard_admin_token", "admin token via file")
check(env.get("ONBOARD_MASTER_KEY_FILE") == "/run/secrets/onboarding_master_key", "master key via file")
ports = svc.get("ports") or []
rendered = [p if isinstance(p, str) else f"{p.get('host_ip','')}:{p.get('published','')}:{p.get('target','')}" for p in ports]
check(any(("127.0.0.1" in p) and (f":{port}" in p) for p in rendered), f"published only on 127.0.0.1:{port} ({rendered})")
check(not any("0.0.0.0" in p or p.startswith("::") for p in rendered), "no wildcard port publish")
secrets = svc.get("secrets") or []
names = [s.get("source") if isinstance(s, dict) else s for s in secrets]
check("onboarding_master_key" in names and "onboard_admin_token" in names, "compose secrets mounted by name")
if len(fail):
    print("FAIL " + label + " assertions: " + "; ".join(fail)); sys.exit(1)
PY
}

docker compose -f "$ROOT/deploy/compose.yaml" config > "$WORK/default.yaml" 2>/dev/null \
  && ok "default compose config exit 0" || bad "default compose config"
assert_config "$WORK/default.yaml" "default" 8788

R51_IMAGE=account-onboarding:matrix R51_DATA="$WORK/data" R51_MASTER_KEY="$WORK/secrets/master.key" \
  R51_ADMIN_TOKEN="$WORK/secrets/admin.token" \
  docker compose -p r6-matrix -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" config \
  > "$WORK/override.yaml" 2>/dev/null && ok "isolated override config exit 0" || bad "isolated override config"
assert_config "$WORK/override.yaml" "override" 28788

set +e
docker compose -p r6-matrix -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" config >/dev/null 2>&1
RC=$?
set -e
[ "$RC" -ne 0 ] && ok "override without R51_* env fails ($RC)" || bad "override accepted missing env"

DEPLOY_SECRETS_DIR="$WORK/secrets" DEPLOY_DATA_DIR="$WORK/data" \
  bash "$ROOT/deploy/scripts/preflight.sh" --secrets-dir "$WORK/secrets" --data-dir "$WORK/data" \
  --master-key "$WORK/secrets/master.key" --admin-token "$WORK/secrets/admin.token" \
  --readonly-token "$WORK/secrets/readonly.token" --compose "$ROOT/deploy/compose.yaml" \
  > "$WORK/preflight-ok.log" 2>&1 && ok "preflight passes with valid test secrets" || bad "preflight valid"
grep -q "WARN" "$WORK/preflight-ok.log" && ok "preflight warns about owner UID != 10001" || bad "preflight ownership warning"

mkdir -p "$WORK/empty"
set +e
bash "$ROOT/deploy/scripts/preflight.sh" --secrets-dir "$WORK/empty" --data-dir "$WORK/data" \
  --compose "$ROOT/deploy/compose.yaml" >/dev/null 2>&1
RC=$?
set -e
[ "$RC" -eq 1 ] && ok "preflight fails closed when secrets are missing" || bad "preflight missing secrets exit=$RC"

echo "SUMMARY pass=$PASS fail=$FAIL"
[ "$FAIL" = "0" ]
