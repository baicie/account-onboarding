#!/usr/bin/env bash
# Fast negative/positive tests for the Kubernetes projection path in the
# materializer (Kubernetes itself is exercised by kind_secret.sh).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
W="$(mktemp -d /tmp/r22-k8sunit.XXXXXX)"; trap 'chmod -R u+rwx "$W" 2>/dev/null || true; rm -rf "$W"' EXIT
mkdir -p "$W/src/..2024_01_01" "$W/dst"; chmod 0755 "$W/src" "$W/src/..2024_01_01"
printf 'key-material' > "$W/src/..2024_01_01/onboarding_master_key"; chmod 0444 "$W/src/..2024_01_01/onboarding_master_key"
ln -s ..2024_01_01 "$W/src/..data"
ln -s ..data/onboarding_master_key "$W/src/onboarding_master_key"
bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$W/src" --target-dir "$W/dst" --allow-k8s-projection >/dev/null
[ "$(cat "$W/dst/onboarding_master_key")" = key-material ] && echo PASS projected-symlink
# Escape outside the trusted root must fail.
ln -s /etc/passwd "$W/src/escape"
if bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$W/src" --target-dir "$W/dst2" --allow-k8s-projection >/dev/null 2>&1; then echo FAIL escape-accepted; exit 1; else echo PASS escape-rejected; fi
rm -f "$W/src/escape"
# Ordinary symlinks remain rejected without the explicit Kubernetes flag.
if bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$W/src" --target-dir "$W/dst3" >/dev/null 2>&1; then echo FAIL ordinary-symlink-accepted; exit 1; else echo PASS ordinary-symlink-rejected; fi
