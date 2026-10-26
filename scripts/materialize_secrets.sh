#!/usr/bin/env bash
# Materialize read-only projected secrets (Docker/K8s) into an owner-only
# private directory. The service still enforces exact 0400/0600; this script
# never weakens that rule. Source may be a read-only 0444 projection, but
# symlinks and writer bits are rejected.
set -euo pipefail
umask 077
SRC="" DST="" UID_N="" GID_N=""
while [ $# -gt 0 ]; do
  case "$1" in
    --source-dir) SRC="$2"; shift 2 ;;
    --target-dir) DST="$2"; shift 2 ;;
    --uid) UID_N="$2"; shift 2 ;;
    --gid) GID_N="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -n "$SRC" ] && [ -n "$DST" ] || { echo "usage: materialize_secrets.sh --source-dir SRC --target-dir DST [--uid U --gid G]" >&2; exit 2; }
[ -d "$SRC" ] || { echo "source dir missing" >&2; exit 1; }
case "$DST" in /*) ;; *) echo "target must be an absolute private path" >&2; exit 2 ;; esac
[ -L "$DST" ] && { echo "target must not be a symlink" >&2; exit 2; }
mkdir -p "$DST"; chmod 0700 "$DST"
TMP="$DST/.materialize-$$"; mkdir -m 0700 "$TMP"
trap 'rm -rf "$TMP"' EXIT
count=0
for f in "$SRC"/*; do
  [ -e "$f" ] || continue
  name="$(basename "$f")"
  [ -L "$f" ] && { echo "refusing symlink source $name" >&2; exit 1; }
  [ -f "$f" ] || { echo "refusing non-regular source $name" >&2; exit 1; }
  mode="$(stat -c '%a' "$f" 2>/dev/null || stat -f '%Lp' "$f")"
  # Reject writable source projections; read-only bits are acceptable because
  # the destination is private 0400/0600 and owned by the runtime user.
  case "$mode" in *2|*3|*6|*7) echo "refusing writable source $name mode $mode" >&2; exit 1 ;; esac
  out="$DST/$name.new.$$"
  cp -- "$f" "$out"
  chmod 0400 "$out"
  if [ -n "$UID_N" ] || [ -n "$GID_N" ]; then
    chown "${UID_N:-$(id -u)}:${GID_N:-$(id -g)}" "$out"
  fi
  chmod 0400 "$out"
  mv -f -- "$out" "$DST/$name"
  final_mode="$(stat -c '%a' "$DST/$name" 2>/dev/null || stat -f '%Lp' "$DST/$name")"
  [ "$final_mode" = "400" ] || { echo "materialized mode is not 0400" >&2; exit 1; }
  count=$((count+1))
done
[ "$count" -gt 0 ] || { echo "no secret files materialized" >&2; exit 1; }
echo "materialized $count secret file(s) into private target"
