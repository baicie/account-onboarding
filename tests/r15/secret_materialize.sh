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

# A failure before commit must preserve the previously usable secret set and
# leave no staging/temp plaintext behind.
mkdir -p "$W/keep-src" "$W/keep-dst"; chmod 700 "$W/keep-src" "$W/keep-dst"
printf 'old-value' > "$W/keep-dst/token"; chmod 0400 "$W/keep-dst/token"
printf 'new-value' > "$W/keep-src/token"; chmod 0444 "$W/keep-src/token"
printf 'bad' > "$W/keep-src/writable"; chmod 0600 "$W/keep-src/writable"
if bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$W/keep-src" --target-dir "$W/keep-dst" >/dev/null 2>&1; then bad "invalid batch accepted"; else ok "invalid batch rejected"; fi
[ "$(cat "$W/keep-dst/token")" = "old-value" ] && ok "old secret preserved after failed batch" || bad "old secret changed"
[ -z "$(find "$W/keep-dst" -maxdepth 1 -name '.staging.*' -o -name '*.new.*' 2>/dev/null)" ] && ok "no staging/temp residue" || bad "staging/temp residue"

# Hard links are aliases, not a trustworthy projection.
mkdir -p "$W/hard"; printf 'hard' > "$W/hard/a"; chmod 0444 "$W/hard/a"; ln "$W/hard/a" "$W/hard/b"
if bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$W/hard" --target-dir "$W/hard-out" >/dev/null 2>&1; then bad "hard link accepted"; else ok "hard link rejected"; fi

# Concurrent materializers must serialize or fail closed, never corrupt.
mkdir -p "$W/con-src" "$W/con-dst"; printf 'concurrent' > "$W/con-src/token"; chmod 0444 "$W/con-src/token"
bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$W/con-src" --target-dir "$W/con-dst" >/tmp/r15-con1.log 2>&1 & p1=$!
bash "$ROOT/scripts/materialize_secrets.sh" --source-dir "$W/con-src" --target-dir "$W/con-dst" >/tmp/r15-con2.log 2>&1 & p2=$!
wait "$p1" || true; wait "$p2" || true
[ "$(cat "$W/con-dst/token")" = "concurrent" ] && [ "$(stat -c '%a' "$W/con-dst/token" 2>/dev/null || stat -f '%Lp' "$W/con-dst/token")" = "400" ] && ok "concurrent target intact" || bad "concurrent target corrupt"
echo "SUMMARY pass=$P fail=$F"
[ "$F" = "0" ]
