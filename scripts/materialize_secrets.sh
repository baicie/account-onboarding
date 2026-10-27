#!/usr/bin/env bash
# Materialize read-only projected secrets (Docker / Kubernetes) into a private,
# owner-only target directory.
#
# The service keeps its strict reader contract (internal/config/config.go): the
# materialized object must be a regular, non-symlink file with exactly 0400 or
# 0600 owned by the runtime UID. This script never weakens that rule, which is
# also why the target layout stays flat: a versioned directory behind a "current"
# symlink would make the secret path itself a symlink and the service would
# (correctly) reject it.
#
# Transactional protocol, one writer at a time per target directory:
#   1. lock the target directory itself with flock(2). No lock file exists, so
#      there is no lock path an attacker could pre-create, no lock file to
#      become stale, and no PID-named file to collide with another writer.
#   2. replay any interrupted transaction (see recover_transaction) before
#      staging, then drop staging directories orphaned by a kill -9.
#   3. validate every source through one already-open file descriptor and copy
#      from that same descriptor, so "checked" and "read" are the same object.
#   4. journal every live-set mutation before performing it, then rename.
#   5. drop the journal on success; on failure or signal replay it.
#
# Guarantees, stated precisely (see deploy/secret-materializer/README.md):
#   * single file: atomic. A reader of one target name sees the old bytes or the
#     new bytes, never a partial file (private staging + rename).
#   * whole set: not one filesystem transaction. Each name is versioned on its
#     own and the service validates each file independently; a keyring rotation
#     is expressed inside the master-key payload, so a mixed set is not a corrupt
#     state. Any failure, signal or crash still rolls the set back to the
#     previously served version on the next run.
#   * crash recovery: the journal lives in DST/.materialize.txn, survives
#     kill -9 (no trap involved) and is replayed by the next invocation.
#   * readers: see the old version until that name is renamed, then the new one.
#   * rotation: effective per file at rename; a running process keeps the bytes
#     it already read, so a rotation is completed by restarting the service.
set -euo pipefail
umask 077

SRC="" DST="" UID_N="" GID_N="" ALLOW_K8S=0 REVOKE=""
usage() {
  echo "usage: materialize_secrets.sh --source-dir SRC --target-dir DST [--uid U] [--gid G] [--allow-k8s-projection] [--revoke NAME]" >&2
}
die() { echo "materialize: $*" >&2; exit 1; }

# valid_secret_name is the one name rule for source files, journal records and
# revoke arguments: no separators, no traversal, no control characters.
valid_secret_name() {
  case "$1" in
    ''|.|..) return 1 ;;
  esac
  case "$1" in
    *[!A-Za-z0-9_.-]*) return 1 ;;
  esac
  return 0
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --source-dir|--target-dir|--uid|--gid)
      [ "$#" -ge 2 ] || { usage; exit 2; }
      case "$1" in
        --source-dir) SRC="$2" ;;
        --target-dir) DST="$2" ;;
        --uid) UID_N="$2" ;;
        --gid) GID_N="$2" ;;
      esac
      shift 2 ;;
    --allow-k8s-projection) ALLOW_K8S=1; shift ;;
    --revoke)
      [ "$#" -ge 2 ] || { usage; exit 2; }
      REVOKE="$REVOKE $2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -n "$SRC" ] && [ -n "$DST" ] || { usage; exit 2; }
if [ -n "$UID_N" ]; then case "$UID_N" in *[!0-9]*) die "--uid must be numeric" ;; esac; fi
if [ -n "$GID_N" ]; then case "$GID_N" in *[!0-9]*) die "--gid must be numeric" ;; esac; fi

if command -v sha256sum >/dev/null 2>&1; then SHACMD="sha256sum"
elif command -v shasum >/dev/null 2>&1; then SHACMD="shasum -a 256"
else die "no sha256 tool available"; fi

REQ_UID="$UID_N"; [ -n "$REQ_UID" ] || REQ_UID="$(id -u)"
REQ_GID="$GID_N"; [ -n "$REQ_GID" ] || REQ_GID="$(id -g)"
OWN="$REQ_UID:$REQ_GID"

[ -d "$SRC" ] || die "source dir missing"
SRC_REAL="$(cd -- "$SRC" && pwd -P)"
case "$DST" in /*) ;; *) die "target must be an absolute private path" ;; esac
[ -L "$DST" ] && die "target must not be a symlink"
mkdir -p "$DST"

# Private target directory. A Kubernetes emptyDir with fsGroup is root:10001
# mode 0770: acceptable only because "other" keeps no access.
require_private_target() {
  if chmod 0700 "$DST" 2>/dev/null; then return 0; fi
  local dmode duid dgid ok=0
  read -r dmode duid dgid <<<"$(stat -c '%a %u %g' "$DST" 2>/dev/null || stat -f '%Lp %u %g' "$DST")"
  (( (8#$dmode & 8#007) == 0 )) || die "target directory is not private (mode $dmode)"
  if [ -z "$UID_N" ] && [ -z "$GID_N" ]; then ok=1; fi
  if [ -n "$UID_N" ] && [ "$duid" = "$UID_N" ]; then ok=1; fi
  if [ -n "$GID_N" ] && [ "$dgid" = "$GID_N" ]; then ok=1; fi
  [ "$ok" = 1 ] || die "target directory owner/group mismatch"
  return 0
}
require_private_target
if [ -n "$UID_N" ] || [ -n "$GID_N" ]; then
  if ! chown "$OWN" "$DST" 2>/dev/null; then
    read -r _ duid dgid <<<"$(stat -c '%u %g' "$DST" 2>/dev/null || stat -f '%u %g' "$DST")"
    [ "$duid" = "$REQ_UID" ] || [ "$dgid" = "$REQ_GID" ] || die "target directory chown failed"
  fi
fi

# Serialize on the directory itself. Fail closed when flock is missing: racing
# two materializers on one target is exactly the failure this must prevent.
command -v flock >/dev/null 2>&1 || die "flock is required to serialize the target directory; refusing to run without it"
exec 9<"$DST" || die "cannot open target directory for locking"
flock -n 9 || die "another materializer holds the lock on this target directory"

TXNDIR="$DST/.materialize.txn"
BAK="$TXNDIR/backup"
JOURNAL="$TXNDIR/journal"
STAGE=""
STAGED=0

# Append a self-verifying journal record, then make it durable before the
# mutation it describes. A torn final line fails its checksum and is dropped,
# which is safe because the mutation it announces had not run yet.
journal_append() {
  local kind="$1" name="$2" record sum
  case "$kind" in B|I|R) ;; *) die "internal: bad journal record kind" ;; esac
  valid_secret_name "$name" || die "refusing to journal an unsafe secret name"
  record="$kind $name"
  sum="$(printf '%s' "$record" | $SHACMD | cut -d' ' -f1 | cut -c1-16)"
  printf '%s %s\n' "$record" "$sum" >>"$JOURNAL" || return 1
  sync
}

# Replay an interrupted transaction: restore every backed-up name, delete names
# that were installed where nothing existed before, then drop the transaction.
recover_transaction() {
  [ -d "$TXNDIR" ] || return 0
  local backed="" installed="" revoked="" kind="" name="" sum="" extra=""
  local i=0 total=0 line="" torn=0
  if [ -f "$JOURNAL" ]; then
    while IFS= read -r line || [ -n "$line" ]; do total=$((total + 1)); done <"$JOURNAL"
    while IFS= read -r line || [ -n "$line" ]; do
      i=$((i + 1))
      kind=""; name=""; sum=""; extra=""; torn=0
      read -r kind name sum extra <<<"$line"
      # A truncated tail IS the crash window: the mutation its record would have
      # announced had not run yet, so dropping it is safe. Anything else that does
      # not verify - a complete record with a bad checksum, an unknown kind, or an
      # illegal name - means the journal cannot be trusted, so recovery stops
      # instead of acting on it. The checksum detects corruption; it is not
      # authentication, and the name rule below is what bounds the file system
      # effects of a record an attacker could have planted.
      if [ -n "$extra" ] || [ -z "$name" ] || [ -z "$sum" ]; then
        torn=1
      elif [ "$(printf "%s %s" "$kind" "$name" | $SHACMD | cut -d" " -f1 | cut -c1-16)" != "$sum" ]; then
        torn=1
      fi
      if [ "$torn" = "1" ]; then
        if [ "$i" -eq "$total" ]; then
          echo "materialize: ignoring a torn final journal record (crash window)" >&2
          continue
        fi
        echo "materialize: journal record $i is truncated or corrupt; refusing to recover" >&2
        return 1
      fi
      case "$kind" in
        B|I|R) ;;
        *) echo "materialize: journal record $i has an unknown kind; refusing to recover" >&2; return 1 ;;
      esac
      if ! valid_secret_name "$name"; then
        echo "materialize: journal record $i names an unsafe target; refusing to recover" >&2
        return 1
      fi
      case "$kind" in
        B) backed="$backed $name" ;;
        I) installed="$installed $name" ;;
        R) revoked="$revoked $name" ;;
      esac
    done <"$JOURNAL"
  fi
  # Only a regular, non-symlink file inside the transaction backup may be
  # promoted back into the target.
  for name in $backed $revoked; do
    if [ -e "$BAK/$name" ] || [ -L "$BAK/$name" ]; then
      if [ ! -f "$BAK/$name" ] || [ -L "$BAK/$name" ]; then
        echo "materialize: transaction backup entry is not a regular file; refusing to recover" >&2
        return 1
      fi
      # The backup is a hard link to the previous live inode. When the live name
      # still points at that inode the install never happened, so dropping the extra
      # link is the restore; mv between two names of the same file is refused by
      # GNU mv. Any other case means the live name was replaced and is swapped back.
      if [ -f "$DST/$name" ] && [ "$BAK/$name" -ef "$DST/$name" ]; then
        rm -f -- "$BAK/$name" || return 1
      else
        mv -f -- "$BAK/$name" "$DST/$name" || return 1
      fi
    fi
  done
  for name in $installed; do
    case " $backed $revoked " in *" $name "*) continue ;; esac
    rm -f -- "$DST/$name" || return 1
  done
  rm -rf -- "$TXNDIR" || return 1
  sync
  return 0
}

finish() {
  local status=$?
  if [ "$STAGED" = 1 ] && [ -n "$STAGE" ]; then
    rm -rf -- "$STAGE" || true
  fi
  if [ -d "$TXNDIR" ]; then
    if recover_transaction; then
      [ "$status" -eq 0 ] || echo "materialize: rolled back to the previous secret version (status $status)" >&2
    else
      echo "materialize: recovery FAILED; inspect $TXNDIR before reuse" >&2
      [ "$status" -ne 0 ] || status=1
    fi
  fi
  exit "$status"
}
trap finish EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# Clear transactions/staging left by a previous kill -9. Safe here: we hold the
# directory lock, so no other materializer can own them.
recover_transaction || die "cannot recover the previous transaction"
for stale in "$DST"/.staging.*; do
  [ -e "$stale" ] || continue
  rm -rf -- "$stale" || die "cannot clear stale staging directory"
done

STAGE="$(mktemp -d "$DST/.staging.XXXXXX")"
STAGED=1
chmod 0700 "$STAGE"

# Validate and copy each source through one open descriptor. The checks below
# describe the object that is actually copied, so a concurrent swap of the
# source path cannot redirect them.
# NAMES is a space separated list: source names are restricted to
# [A-Za-z0-9_.-] below, so word splitting is safe.
NAMES=""
count=0
for f in "$SRC"/*; do
  if [ ! -e "$f" ] && [ ! -L "$f" ]; then continue; fi
  name="$(basename "$f")"
  case "$name" in
    .*|*/*) die "refusing unsafe source filename" ;;
    *[!A-Za-z0-9_.-]*) die "refusing source filename outside [A-Za-z0-9_.-]" ;;
  esac
  if [ -L "$f" ] && [ "$ALLOW_K8S" != "1" ]; then die "refusing symlink source $name"; fi
  exec 7<"$f" || die "cannot open source $name"
  # Resolve the path of the object we actually opened (/proc/self/fd is
  # authoritative on Linux, which is every deploy target). A non-procfs host
  # (developer macOS) falls back to the path that was opened; the type/mode/link
  # checks below stay descriptor-based either way.
  resolved=""
  if [ -e /proc/self/fd ]; then resolved="$(readlink -f /proc/self/fd/7 2>/dev/null || true)"; fi
  if [ -z "$resolved" ]; then resolved="$(readlink -f "$f" 2>/dev/null || true)"; fi
  if [ -z "$resolved" ]; then exec 7<&-; die "cannot resolve source $name"; fi
  case "$resolved" in
    "$SRC_REAL"/*) ;;
    *) exec 7<&-; die "refusing source outside the projected root: $name" ;;
  esac
  ftype="$(stat -L -c '%F' /dev/fd/7 2>/dev/null || stat -L -f '%HT' /dev/fd/7 2>/dev/null || true)"
  case "$ftype" in
    *[Rr]egular*) ;;
    *) exec 7<&-; die "refusing non-regular source $name" ;;
  esac
  fmode="$(stat -L -c '%a' /dev/fd/7 2>/dev/null || stat -L -f '%Lp' /dev/fd/7)"
  if [ -n "$fmode" ] && (( (8#$fmode & 8#222) != 0 )); then
    exec 7<&-; die "refusing writable source $name mode $fmode"
  fi
  flinks="$(stat -L -c '%h' /dev/fd/7 2>/dev/null || stat -L -f '%l' /dev/fd/7 2>/dev/null || echo 1)"
  if [ -n "$flinks" ] && [ "$flinks" -gt 1 ]; then exec 7<&-; die "refusing hard-linked source $name"; fi
  fsize="$(stat -L -c '%s' /dev/fd/7 2>/dev/null || stat -L -f '%z' /dev/fd/7)"
  out="$STAGE/$name"
  if ! cat <&7 >"$out"; then exec 7<&-; die "cannot stage source $name"; fi
  exec 7<&-
  outsize="$(stat -c '%s' "$out" 2>/dev/null || stat -f '%z' "$out")"
  [ "$outsize" = "$fsize" ] || die "source $name changed while being read"
  chmod 0400 "$out" || die "cannot chmod staged $name"
  if [ -n "$UID_N" ] || [ -n "$GID_N" ]; then
    chown "$OWN" "$out" || die "cannot chown staged $name"
  fi
  chmod 0400 "$out" || die "cannot chmod staged $name"
  NAMES="$NAMES $name"
  count=$((count + 1))
done
[ "$count" -gt 0 ] || [ -n "$REVOKE" ] || die "no secret files materialized"

mkdir -p "$TXNDIR" "$BAK"
chmod 0700 "$TXNDIR" "$BAK"
: >"$JOURNAL"
chmod 0600 "$JOURNAL"
sync

# Explicit revocation: only names that already exist as regular files are removed,
# each journaled (R) with a hard-linked copy so recovery can restore it.
for name in $REVOKE; do
  valid_secret_name "$name" || die "refusing to revoke an unsafe secret name"
  if [ -e "$DST/$name" ] || [ -L "$DST/$name" ]; then
    if [ ! -f "$DST/$name" ] || [ -L "$DST/$name" ]; then
      die "refusing to revoke a non-regular live secret"
    fi
    journal_append R "$name" || die "cannot journal revoke of $name"
    ln -f -- "$DST/$name" "$BAK/$name" || die "cannot snapshot the revoked secret"
    rm -f -- "$DST/$name" || die "cannot revoke $name"
  else
    echo "materialize: nothing to revoke for $name" >&2
  fi
done
for name in $NAMES; do
  if [ -e "$DST/$name" ] || [ -L "$DST/$name" ]; then
    journal_append B "$name" || die "cannot journal backup of $name"
    if [ ! -f "$DST/$name" ] || [ -L "$DST/$name" ]; then
      die "refusing to replace a non-regular live secret"
    fi
    # Hard link, not a move: the live name keeps resolving to the old inode until
    # the rename below swaps it, so a reader never sees the name disappear.
    ln -f -- "$DST/$name" "$BAK/$name" || die "cannot snapshot the live secret into the transaction backup"
  fi
  journal_append I "$name" || die "cannot journal install of $name"
  mv -f -- "$STAGE/$name" "$DST/$name" || die "cannot install $name"
  final_mode="$(stat -c '%a' "$DST/$name" 2>/dev/null || stat -f '%Lp' "$DST/$name")"
  [ "$final_mode" = "400" ] || die "materialized mode is not 0400 for $name"
done

rm -rf -- "$STAGE" || true
STAGED=0
rm -rf -- "$TXNDIR" || die "cannot commit transaction (journal removal failed)"
sync
if [ -n "$UID_N" ] || [ -n "$GID_N" ]; then
  chown "$OWN" "$DST" 2>/dev/null || true
fi
chmod 0700 "$DST" 2>/dev/null || true
echo "materialized $count secret file(s) into private target"
