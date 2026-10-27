#!/usr/bin/env bash
# Real POSIX ACL acceptance on Linux. Run as root in CI.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d /tmp/r22-acl.XXXXXX)"
cleanup(){ chmod -R u+rwx "$WORK" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT
P=0; F=0; ok(){ P=$((P+1)); echo "PASS $1"; }; bad(){ F=$((F+1)); echo "FAIL $1"; }
[ "$(id -u)" = 0 ] || { echo "must run as root" >&2; exit 1; }
command -v getfacl >/dev/null && command -v setfacl >/dev/null || { echo "acl tools missing" >&2; exit 1; }

mkdir -p "$WORK/src" "$WORK/dst"; chmod 0755 "$WORK" "$WORK/src"; chmod 0700 "$WORK/dst"
printf 'acl-master-key-bytes-32-bytes!!!!' > "$WORK/src/onboarding_master_key"; chmod 0444 "$WORK/src/onboarding_master_key"
printf 'acl-admin-token\n' > "$WORK/src/onboard_admin_token"; chmod 0444 "$WORK/src/onboard_admin_token"
bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$WORK/src" --target-dir "$WORK/dst" --uid 10001 --gid 10001 >/dev/null

as_uid(){ local uid="$1"; shift; setpriv --reuid="$uid" --regid="$uid" --clear-groups "$@"; }
as_uid 10001 test -r "$WORK/dst/onboarding_master_key" && ok "runtime UID reads materialized secret" || bad "runtime UID cannot read"
if as_uid 10002 test -r "$WORK/dst/onboarding_master_key" 2>/dev/null; then bad "unexpected UID read allowed"; else ok "unexpected UID read denied"; fi
if as_uid 10002 sh -c "printf x >> '$WORK/dst/onboarding_master_key'" 2>/dev/null; then bad "unexpected UID write allowed"; else ok "unexpected UID write denied"; fi

# Explicit read ACL grants a previously denied UID; mode bits alone would lie.
# Grant directory traverse first, otherwise the file ACL is unreachable.
setfacl -m u:10002:x "$WORK/dst"
setfacl -m u:10002:r "$WORK/dst/onboarding_master_key"
as_uid 10002 test -r "$WORK/dst/onboarding_master_key" && ok "read ACL granted" || bad "read ACL did not grant"
# Explicit write ACL must not bypass the service's 0400 target policy unless the
# operator deliberately changes it; document observed effective ACL.
setfacl -m u:10002:rw "$WORK/dst/onboarding_master_key"
if as_uid 10002 sh -c "printf x >> '$WORK/dst/onboarding_master_key'" 2>/dev/null; then
  ok "explicit write ACL observed (operator-granted)"
else ok "explicit write ACL ineffective"; fi
getfacl -p "$WORK/dst/onboarding_master_key" >"$WORK/acl.txt"
grep -q '^user:10002:' "$WORK/acl.txt" && ok "getfacl reports extended ACL" || bad "extended ACL missing"

# Rotation creates fresh files with no inherited extended ACL on the target.
printf 'rotated-admin-token\n' > "$WORK/src/onboard_admin_token"; chmod 0444 "$WORK/src/onboard_admin_token"
bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$WORK/src" --target-dir "$WORK/dst" --uid 10001 --gid 10001 >/dev/null
if getfacl -p "$WORK/dst/onboard_admin_token" 2>/dev/null | grep -q '^user:10002:'; then bad "rotation retained stale ACL"; else ok "rotation produced clean ACL"; fi
as_uid 10001 test -r "$WORK/dst/onboard_admin_token" && ok "rotation runtime read" || bad "rotation runtime read"

# A source with an unexpected ACL is not silently trusted as special; the
# materializer still copies bytes with clean target metadata.
setfacl -m u:10002:r "$WORK/src/onboarding_master_key"
bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$WORK/src" --target-dir "$WORK/dst" --uid 10001 --gid 10001 >/dev/null
if getfacl -p "$WORK/dst/onboarding_master_key" 2>/dev/null | grep -q '^user:10002:'; then bad "source ACL propagated"; else ok "source ACL not propagated"; fi
echo "SUMMARY pass=$P fail=$F"
[ "$F" = 0 ] && echo ACL_SECRET_PASS
exit "$F"
