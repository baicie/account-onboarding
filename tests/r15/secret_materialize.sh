#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
W="$(mktemp -d "${TMPDIR:-/tmp}/r15-secret.XXXXXX")"
trap 'rm -rf "$W"' EXIT
P=0; F=0; ok(){ P=$((P+1)); echo "PASS $1"; }; bad(){ F=$((F+1)); echo "FAIL $1"; }
mkdir -p "$W/src" "$W/dst"; chmod 700 "$W/src" "$W/dst"
printf 'secret-value' > "$W/src/token"; chmod 0444 "$W/src/token"
if bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$W/src" --target-dir "$W/dst" >/dev/null; then ok "read-only projection materialized"; else bad "materialize"; fi
[ "$(stat -c '%a' "$W/dst/token" 2>/dev/null || stat -f '%Lp' "$W/dst/token")" = "400" ] && ok "target mode 0400" || bad "target mode"
[ "$(cat "$W/dst/token")" = "secret-value" ] && ok "content preserved" || bad "content"
ln -s "$W/src/token" "$W/src/link"
if bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$W/src" --target-dir "$W/dst2" >/dev/null 2>&1; then bad "symlink accepted"; else ok "symlink rejected"; fi
rm -f "$W/src/link"
for mode in 0600 0660 0620 0460 0666; do
  d="$W/mode-$mode"; mkdir -p "$d"; printf 'x' > "$d/token"; chmod "$mode" "$d/token"
  if bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$d" --target-dir "$W/out-$mode" >/dev/null 2>&1; then
    bad "writable source $mode accepted"
  else
    ok "writable source $mode rejected"
  fi
done
echo "SUMMARY pass=$P fail=$F"
[ "$F" = "0" ]
