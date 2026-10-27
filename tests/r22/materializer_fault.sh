#!/usr/bin/env bash
# Fault injection for scripts/materialize_secrets.sh.
#
# Every case injects a real filesystem/process fault at one protocol step and
# asserts the previously served secret set survives byte-for-byte at mode 0400
# with no transaction or staging residue. The real script runs against a real
# directory; only the utility it calls at the injected step is replaced.
set -o pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="$ROOT/scripts/materialize_secrets.sh"
TMPROOT="$TMPDIR"; [ -n "$TMPROOT" ] || TMPROOT="/tmp"
WORK="$(mktemp -d "$TMPROOT/r22-matfault.XXXXXX")"
cleanup() { chmod -R u+rwx "$WORK" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT
P=0; F=0
ok() { P=$((P+1)); echo "PASS $1"; }
bad() { F=$((F+1)); echo "FAIL $1"; }

REAL_MV="$(command -v mv)"; REAL_CAT="$(command -v cat)"
REAL_CHMOD="$(command -v chmod)"; REAL_CHOWN="$(command -v chown)"
for t in "$REAL_MV" "$REAL_CAT" "$REAL_CHMOD" "$REAL_CHOWN"; do
  [ -n "$t" ] || { echo "missing core utility" >&2; exit 1; }
done
SHIM="$WORK/shim"; STATE="$WORK/state"; mkdir -p "$SHIM" "$STATE"
write_shim() { printf '%s\n' "$2" >"$SHIM/$1"; chmod +x "$SHIM/$1"; }

# mv: FAIL_MV_NAME refuses the install rename of that name, SIGNAL_MV_NAME
# signals the materializer instead of installing (kill -9 cannot be trapped,
# -TERM can), FAIL_BACKUP_NAME refuses the live -> backup rename.
write_shim mv '#!/bin/sh
src=""; dst=""; prev=""
for a in "$@"; do
  case "$a" in -*) continue ;; esac
  src="$prev"; dst="$a"; prev="$a"
done
base=""
[ -n "$dst" ] && base=$(basename "$dst")
case "$src" in
  */.staging.*)
    if [ -n "$FAIL_MV_NAME" ] && [ "$base" = "$FAIL_MV_NAME" ] && [ ! -e "$SHIM_STATE/mvfired" ]; then
      : >"$SHIM_STATE/mvfired"; echo "shim: refusing install of $dst" >&2; exit 1
    fi
    if [ -n "$SIGNAL_MV_NAME" ] && [ "$base" = "$SIGNAL_MV_NAME" ] && [ ! -e "$SHIM_STATE/sigfired" ]; then
      : >"$SHIM_STATE/sigfired"; kill -"$SIGNAL_MV_SIG" "$PPID"; sleep 1; exit 1
    fi ;;
  *)
    case "$dst" in
      */.materialize.txn/backup/*)
        if [ -n "$FAIL_BACKUP_NAME" ] && [ "$base" = "$FAIL_BACKUP_NAME" ] && [ ! -e "$SHIM_STATE/bkfired" ]; then
          : >"$SHIM_STATE/bkfired"; echo "shim: refusing backup rename of $dst" >&2; exit 1
        fi ;;
    esac ;;
esac
exec '"$REAL_MV"' "$@"'

# cat: FAIL_CAT_AT fails the N-th staging copy (1-based).
write_shim cat '#!/bin/sh
n=0
if [ -f "$SHIM_STATE/catcount" ]; then n=$('"'$REAL_CAT'"' "$SHIM_STATE/catcount" 2>/dev/null); fi
[ -n "$n" ] || n=0
n=$((n + 1))
printf "%s" "$n" >"$SHIM_STATE/catcount"
if [ -n "$FAIL_CAT_AT" ] && [ "$n" = "$FAIL_CAT_AT" ]; then
  echo "shim: injected copy failure (call $n)" >&2; exit 1
fi
exec '"$REAL_CAT"' "$@"'

write_shim chmod '#!/bin/sh
last=""
for a in "$@"; do last="$a"; done
case "$last" in
  */.staging.*)
    if [ -n "$FAIL_CHMOD" ] && [ ! -e "$SHIM_STATE/chmodfired" ]; then
      : >"$SHIM_STATE/chmodfired"; echo "shim: injected chmod failure" >&2; exit 1
    fi ;;
esac
exec '"$REAL_CHMOD"' "$@"'

write_shim chown '#!/bin/sh
last=""
for a in "$@"; do last="$a"; done
case "$last" in
  */.staging.*)
    if [ -n "$FAIL_CHOWN" ] && [ ! -e "$SHIM_STATE/chownfired" ]; then
      : >"$SHIM_STATE/chownfired"; echo "shim: injected chown failure" >&2; exit 1
    fi ;;
esac
exec '"$REAL_CHOWN"' "$@"'

reset_state() { rm -f "$STATE"/*; }
mat() { SHIM_STATE="$STATE" PATH="$SHIM:$PATH" bash "$SCRIPT" "$@"; }
mode_of() { stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1"; }

prepare() {
  local c="$1"
  SRC="$WORK/$c/src"; DST="$WORK/$c/dst"
  mkdir -p "$SRC" "$DST"; chmod 0700 "$SRC" "$DST"
  printf 'OLD-ALPHA' >"$DST/alpha"; printf 'OLD-BETA' >"$DST/beta"
  chmod 0400 "$DST/alpha" "$DST/beta"
  reset_state
}
stage_new() {
  chmod u+w "$SRC/alpha" "$SRC/beta" 2>/dev/null || true
  printf '%s' "$1" >"$SRC/alpha"; printf '%s' "$2" >"$SRC/beta"
  chmod 0444 "$SRC/alpha" "$SRC/beta"
}
expect_old_set() {
  local label="$1" fail=0
  [ "$(cat "$DST/alpha" 2>/dev/null)" = "OLD-ALPHA" ] || fail=1
  [ "$(cat "$DST/beta" 2>/dev/null)" = "OLD-BETA" ] || fail=1
  [ "$(mode_of "$DST/alpha")" = "400" ] || fail=1
  [ "$(mode_of "$DST/beta")" = "400" ] || fail=1
  if [ "$fail" = 0 ]; then ok "$label: previous secret set preserved"; else bad "$label: previous secret set damaged"; fi
}
expect_no_residue() {
  local label="$1"
  if [ -n "$(find "$DST" -maxdepth 1 \( -name '.staging.*' -o -name '.materialize.txn' \) 2>/dev/null)" ]; then
    bad "$label: transaction/staging residue left behind"
  else ok "$label: no transaction or staging residue"; fi
}

echo "=== materializer fault injection (real script, real filesystem) ==="

# 1. install rename of the second secret fails after the first one committed.
prepare install-fail
stage_new NEW-ALPHA NEW-BETA
FAIL_MV_NAME=beta mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/c1.log" 2>&1
rc=$?
[ "$rc" -ne 0 ] && ok "install failure: non-zero exit ($rc)" || bad "install failure: exit 0"
expect_old_set "install failure (2nd file)"
expect_no_residue "install failure"

# 2/3. staging copy fails on the first and on the second secret.
for at in 1 2; do
  prepare "copy-fail-$at"
  stage_new NEW-ALPHA NEW-BETA
  FAIL_CAT_AT="$at" mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/copy-$at.log" 2>&1
  rc=$?
  [ "$rc" -ne 0 ] && ok "copy failure at file $at: non-zero exit" || bad "copy failure at file $at: exit 0"
  expect_old_set "copy failure at file $at"
  expect_no_residue "copy failure at file $at"
  reset_state
done

# 4. chmod of a staged secret fails.
prepare chmod-fail
stage_new NEW-ALPHA NEW-BETA
FAIL_CHMOD=1 mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/c4.log" 2>&1
rc=$?
[ "$rc" -ne 0 ] && ok "chmod failure: non-zero exit" || bad "chmod failure: exit 0"
expect_old_set "chmod failure"
expect_no_residue "chmod failure"
if grep -q 'OLD-ALPHA\|NEW-ALPHA' "$WORK/c4.log"; then bad "chmod failure: secret value in output"; else ok "chmod failure: no secret value logged"; fi

# 5. chown of a staged secret fails.
prepare chown-fail
stage_new NEW-ALPHA NEW-BETA
FAIL_CHOWN=1 mat --source-dir "$SRC" --target-dir "$DST" --uid "$(id -u)" --gid "$(id -g)" >"$WORK/c5.log" 2>&1
rc=$?
[ "$rc" -ne 0 ] && ok "chown failure: non-zero exit" || bad "chown failure: exit 0"
expect_old_set "chown failure"
expect_no_residue "chown failure"

# 6. the backup rename (live -> backup) fails.
prepare backup-fail
stage_new NEW-ALPHA NEW-BETA
FAIL_BACKUP_NAME=alpha mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/c6.log" 2>&1
rc=$?
[ "$rc" -ne 0 ] && ok "backup rename failure: non-zero exit" || bad "backup rename failure: exit 0"
expect_old_set "backup rename failure"
expect_no_residue "backup rename failure"

# 7. SIGTERM in the middle of the commit.
prepare sigterm
stage_new NEW-ALPHA NEW-BETA
SIGNAL_MV_NAME=beta SIGNAL_MV_SIG=TERM mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/c7.log" 2>&1
rc=$?
[ "$rc" -ne 0 ] && ok "SIGTERM mid-commit: non-zero exit ($rc)" || bad "SIGTERM mid-commit: exit 0"
expect_old_set "SIGTERM mid-commit"
expect_no_residue "SIGTERM mid-commit"

# 8. SIGKILL mid-commit: no trap can run, the journal must recover on next start.
prepare sigkill
stage_new NEW-ALPHA NEW-BETA
SIGNAL_MV_NAME=beta SIGNAL_MV_SIG=KILL mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/c8.log" 2>&1
rc=$?
[ "$rc" -ne 0 ] && ok "SIGKILL mid-commit: process died ($rc)" || bad "SIGKILL mid-commit: exit 0"
if [ -d "$DST/.materialize.txn" ]; then ok "SIGKILL mid-commit: journal survives the kill"; else bad "SIGKILL mid-commit: journal missing"; fi
# A killed materializer's still-running children keep an inherited copy of the
# directory lock; the next run must wait for them (bounded) or refuse.
for _ in $(seq 1 20); do
  if ( exec 9<"$DST"; flock -n 9 ) 2>/dev/null; then break; fi
  sleep 0.5
done
reset_state
mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/c8b.log" 2>&1
[ "$?" -eq 0 ] && ok "recovery run succeeds" || { cat "$WORK/c8b.log" >&2; bad "recovery run failed"; }
if [ "$(cat "$DST/alpha")" = "NEW-ALPHA" ] && [ "$(cat "$DST/beta")" = "NEW-BETA" ]; then
  ok "recovery run installs the new set completely"
else bad "recovery run left a mixed set"; fi
expect_no_residue "after recovery run"

# 9. concurrent invocations: serialized or refused, never a partial file.
concurrency_ok=1
for round in 1 2 3 4 5; do
  SRC="$WORK/con-$round/src"; DST="$WORK/con-$round/dst"
  mkdir -p "$SRC" "$DST"; chmod 0700 "$SRC" "$DST"
  printf 'OLD-ALPHA' >"$DST/alpha"; printf 'OLD-BETA' >"$DST/beta"; chmod 0400 "$DST/alpha" "$DST/beta"
  reset_state
  printf 'NEW-ALPHA-%s' "$round" >"$SRC/alpha"; printf 'NEW-BETA-%s' "$round" >"$SRC/beta"
  chmod 0444 "$SRC/alpha" "$SRC/beta"
  mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/con-$round-a.log" 2>&1 &
  a=$!
  mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/con-$round-b.log" 2>&1 &
  b=$!
  wait "$a"; ra=$?
  wait "$b"; rb=$?
  if [ "$ra" -ne 0 ] && [ "$rb" -ne 0 ]; then concurrency_ok=0; echo "  round $round: both invocations failed"; fi
  for f in alpha beta; do
    m="$(mode_of "$DST/$f")"
    [ "$m" = "400" ] || { concurrency_ok=0; echo "  round $round: $f mode $m"; }
    [ -s "$DST/$f" ] || { concurrency_ok=0; echo "  round $round: $f empty"; }
  done
  if [ -n "$(find "$DST" -maxdepth 1 \( -name '.staging.*' -o -name '.materialize.txn' \) 2>/dev/null)" ]; then
    concurrency_ok=0; echo "  round $round: residue"
  fi
done
[ "$concurrency_ok" = 1 ] && ok "concurrent invocations: serialized/refused with a complete target" || bad "concurrent invocations left an incomplete target"

# 10. no locking primitive available: must fail closed and change nothing.
prepare no-flock
stage_new NEW-ALPHA NEW-BETA
BIN="$WORK/bin-nolock"; mkdir -p "$BIN"
for tool in bash mktemp mkdir stat cat chmod chown mv rm basename readlink cut sync id sha256sum shasum sleep find printf sort tr wc grep; do
  real="$(command -v "$tool" 2>/dev/null)"
  [ -n "$real" ] && ln -sf "$real" "$BIN/$tool"
done
PATH="$BIN" /bin/bash "$SCRIPT" --source-dir "$SRC" --target-dir "$DST" >"$WORK/c10.log" 2>&1
rc=$?
if [ "$rc" -ne 0 ] && grep -q 'flock is required' "$WORK/c10.log"; then ok "missing flock: fails closed"; else bad "missing flock: did not fail closed (rc=$rc)"; fi
expect_old_set "missing flock"
expect_no_residue "missing flock"

# 11. a clean rotation installs both secrets and leaves exactly two entries.
prepare success
stage_new NEW-ALPHA NEW-BETA
mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/c11.log" 2>&1
rc=$?
[ "$rc" -eq 0 ] && ok "clean rotation succeeds" || { cat "$WORK/c11.log" >&2; bad "clean rotation failed"; }
if [ "$(cat "$DST/alpha")" = "NEW-ALPHA" ] && [ "$(cat "$DST/beta")" = "NEW-BETA" ]; then
  ok "clean rotation installs both secrets"; else bad "clean rotation content"; fi
expect_no_residue "clean rotation"
entries="$(find "$DST" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')"
[ "$entries" = "2" ] && ok "clean rotation leaves exactly the two secret files" || bad "clean rotation left $entries entries"

# 12. disk full: the previously committed version must survive.
if [ "$(id -u)" = 0 ] && [ "$(uname -s)" = "Linux" ]; then
  FULL="$WORK/full"; mkdir -p "$FULL"
  if mount -t tmpfs -o size=256k tmpfs "$FULL" 2>/dev/null; then
    DST="$FULL"; chmod 0700 "$DST"
    printf 'OLD-ALPHA' >"$DST/alpha"; printf 'OLD-BETA' >"$DST/beta"; chmod 0400 "$DST/alpha" "$DST/beta"
    SRC="$WORK/diskfull-src"; mkdir -p "$SRC"
    printf 'NEW-ALPHA' >"$SRC/alpha"; printf 'NEW-BETA' >"$SRC/beta"; chmod 0444 "$SRC/alpha" "$SRC/beta"
    dd if=/dev/zero of="$FULL/filler" bs=1k count=250 2>/dev/null
    mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/c12.log" 2>&1
    rc=$?
    [ "$rc" -ne 0 ] && ok "disk full: rotation refused ($rc)" || bad "disk full: rotation reported success"
    expect_old_set "disk full"
    rm -f "$FULL/filler"
    expect_no_residue "disk full"
    umount "$FULL" 2>/dev/null || true
  else
    echo "SKIP disk full: tmpfs mount unavailable"
  fi
else
  echo "SKIP disk full: requires root on Linux"
fi

echo "SUMMARY pass=$P fail=$F"
[ "$F" -eq 0 ]
