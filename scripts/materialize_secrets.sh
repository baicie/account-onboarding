#!/usr/bin/env bash
# Materialize read-only projected secrets (Docker/K8s) into an owner-only
# private directory. The service still enforces exact 0400/0600; this script
# never weakens that rule.
#
# Protocol: validate every source first; stage private copies in a private
# directory inside the target; then commit all files while retaining the old
# files as rollback backups. A single-file rename is atomic, but a multi-file
# set is NOT one filesystem transaction: a kill -9 during commit can leave a
# mixed set. Callers needing all-or-none generation semantics must use the
# versioned directory mode (see deploy/secret-materializer/README.md).
set -euo pipefail
umask 077
SRC="" DST="" UID_N="" GID_N="" ALLOW_K8S=0
while [ $# -gt 0 ]; do
  case "$1" in
    --source-dir) SRC="${2:-}"; shift 2 ;;
    --target-dir) DST="${2:-}"; shift 2 ;;
    --uid) UID_N="${2:-}"; shift 2 ;;
    --gid) GID_N="${2:-}"; shift 2 ;;
    --allow-k8s-projection) ALLOW_K8S=1; shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -n "$SRC" ] && [ -n "$DST" ] || { echo "usage: materialize_secrets.sh --source-dir SRC --target-dir DST [--uid U --gid G]" >&2; exit 2; }
[ -d "$SRC" ] || { echo "source dir missing" >&2; exit 1; }
SRC_REAL="$(cd "$SRC" && pwd -P)"
case "$DST" in /*) ;; *) echo "target must be an absolute private path" >&2; exit 2 ;; esac
[ -L "$DST" ] && { echo "target must not be a symlink" >&2; exit 2; }
mkdir -p "$DST"; chmod 0700 "$DST"
if [ -n "$UID_N" ] || [ -n "$GID_N" ]; then
  chown "${UID_N:-$(id -u)}:${GID_N:-$(id -g)}" "$DST"
fi
# Serialize materializers. If flock is unavailable, fail closed rather than
# racing on the same target.
if command -v flock >/dev/null 2>&1; then
  exec 9>"$DST/.materialize.lock"
  chmod 0600 "$DST/.materialize.lock"
  flock -n 9 || { echo "another materializer holds the target lock" >&2; exit 1; }
fi
STAGE="$(mktemp -d "$DST/.staging.XXXXXX")"
chmod 0700 "$STAGE"
BACKUP="$STAGE/.backup"; mkdir -m 0700 "$BACKUP"
cleanup() { rm -rf -- "$STAGE"; }
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

names=()
for f in "$SRC"/*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  name="$(basename -- "$f")"
  case "$name" in .*|*/*) echo "refusing unsafe source filename" >&2; exit 1 ;; esac
  source_file="$f"
  if [ -L "$f" ]; then
    if [ "$ALLOW_K8S" != "1" ]; then echo "refusing symlink source $name" >&2; exit 1; fi
    # Kubernetes projected secrets are symlinks through ..data. Allow only a
    # final regular file whose resolved path remains under the trusted mount.
    source_file="$(readlink -f -- "$f" 2>/dev/null || true)"
    case "$source_file" in "$SRC_REAL"/*) ;; *) echo "refusing symlink outside projected root $name" >&2; exit 1 ;; esac
    [ -f "$source_file" ] && [ ! -L "$source_file" ] || { echo "refusing non-regular projected source $name" >&2; exit 1; }
  fi
  [ -f "$source_file" ] || { echo "refusing non-regular source $name" >&2; exit 1; }
  # A hard link is not a trustworthy projection: it aliases another path.
  links="$(stat -c '%h' "$source_file" 2>/dev/null || stat -f '%l' "$source_file")"
  [ "${links:-1}" -le 1 ] || { echo "refusing hard-linked source $name" >&2; exit 1; }
  mode="$(stat -c '%a' "$source_file" 2>/dev/null || stat -f '%Lp' "$source_file")"
  if (( (8#$mode & 8#222) != 0 )); then
    echo "refusing writable source $name mode $mode" >&2; exit 1
  fi
  names+=("$name")
done
[ "${#names[@]}" -gt 0 ] || { echo "no secret files materialized" >&2; exit 1; }

# Stage every file with final ownership/mode before touching the live set.
for name in "${names[@]}"; do
  source_file="$SRC/$name"
  if [ -L "$source_file" ]; then source_file="$(readlink -f -- "$source_file")"; fi
  out="$STAGE/$name"
  cp -- "$source_file" "$out"
  chmod 0400 "$out"
  if [ -n "$UID_N" ] || [ -n "$GID_N" ]; then
    chown "${UID_N:-$(id -u)}:${GID_N:-$(id -g)}" "$out"
  fi
  chmod 0400 "$out"
done

committed=()
rollback() {
  status=$?
  for name in "${committed[@]}"; do
    rm -f -- "$DST/$name" || true
    if [ -f "$BACKUP/$name" ]; then mv -f -- "$BACKUP/$name" "$DST/$name" || true; fi
  done
  cleanup
  exit "$status"
}
trap rollback EXIT

for name in "${names[@]}"; do
  if [ -e "$DST/$name" ]; then mv -f -- "$DST/$name" "$BACKUP/$name"; fi
  mv -f -- "$STAGE/$name" "$DST/$name"
  committed+=("$name")
  final_mode="$(stat -c '%a' "$DST/$name" 2>/dev/null || stat -f '%Lp' "$DST/$name")"
  [ "$final_mode" = "400" ] || { echo "materialized mode is not 0400" >&2; exit 1; }
done
if [ -n "$UID_N" ] || [ -n "$GID_N" ]; then
  chown "${UID_N:-$(id -u)}:${GID_N:-$(id -g)}" "$DST"
fi
chmod 0700 "$DST"
trap cleanup EXIT
echo "materialized ${#names[@]} secret file(s) into private target"
