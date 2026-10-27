#!/usr/bin/env bash
# Attack regression for the secret materializer.
#
# Covers the three findings fixed after the v0.1 independent review:
#   P1-1 journal replay must not act on an untrusted record (traversal, absolute
#        path, empty name, control characters, unknown kind, forged checksum,
#        truncated record, symlinked backup entry) and must fail closed;
#   P2-1 replacing a live secret must not make the name disappear for a reader;
#   P2-4 an explicit --revoke must remove exactly that name and be restorable.
# Every case uses a throwaway directory; nothing touches a real system path.
set -o pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="$ROOT/scripts/materialize_secrets.sh"
TMPROOT="$TMPDIR"; [ -n "$TMPROOT" ] || TMPROOT=/tmp
WORK="$(mktemp -d "$TMPROOT/r22-matattack.XXXXXX")"
cleanup() { chmod -R u+rwx "$WORK" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT
P=0; F=0
ok() { P=$((P+1)); echo "PASS $1"; }
bad() { F=$((F+1)); echo "FAIL $1"; }
SHACMD="sha256sum"; command -v sha256sum >/dev/null 2>&1 || SHACMD="shasum -a 256"
sumof() { printf "%s" "$1" | $SHACMD | cut -d" " -f1 | cut -c1-16; }
mat() { bash "$SCRIPT" "$@"; }

# fixture: a target with one live secret, a source with one secret, and a canary
# that lives OUTSIDE the target directory.
fixture() {
  local c="$1"
  SRC="$WORK/$c/src"; DST="$WORK/$c/dst"; VICTIM="$WORK/$c/victim"
  mkdir -p "$SRC" "$DST" "$VICTIM"; chmod 0700 "$SRC" "$DST" "$VICTIM"
  printf "OLD-TOKEN" > "$DST/token"; chmod 0400 "$DST/token"
  printf "NEW-TOKEN" > "$SRC/token"; chmod 0444 "$SRC/token"
  printf "CANARY" > "$VICTIM/canary"
  mkdir -p "$DST/.materialize.txn/backup"
}
plant() { printf "%s\n" "$1" > "$DST/.materialize.txn/journal"; }
expect_refused() { # expect_refused <label>: the run must fail and leave the set intact
  local label="$1" rc=0
  mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/$label.log" 2>&1 || rc=$?
  if [ "$rc" -eq 0 ]; then bad "$label: untrusted journal was accepted"; return; fi
  ok "$label: untrusted journal refused (exit $rc)"
  if [ "$(cat "$DST/token" 2>/dev/null)" = "OLD-TOKEN" ]; then
    ok "$label: live secret untouched"
  else bad "$label: live secret changed"; fi
  if [ "$(cat "$VICTIM/canary" 2>/dev/null)" = "CANARY" ]; then
    ok "$label: canary outside the target intact"
  else bad "$label: file outside the target was touched"; fi
}

echo "=== materializer attack regression ==="

# 1. path traversal through the journal name field (the reported P1)
fixture traversal
plant "I ../victim/canary $(sumof "I ../victim/canary")"
expect_refused traversal

# 2. absolute path
fixture absolute
plant "I /etc/passwd $(sumof "I /etc/passwd")"
expect_refused absolute

# 3. control character in the name
fixture control
CTRL=$(printf "I tok\001en")
plant "$CTRL $(sumof "$CTRL")"
expect_refused control

# 4. unknown record kind with a correct checksum
fixture badkind
plant "X token $(sumof "X token")"
expect_refused badkind

# 5. empty name in a NON-final record: must fail closed
fixture emptyname
printf "I  %s\nI token %s\n" "$(sumof "I ")" "$(sumof "I token")" > "$DST/.materialize.txn/journal"
expect_refused emptyname
# 6. symlinked backup entry: a restore must not promote a symlink into the target
fixture symlink
ln -s "$VICTIM/canary" "$DST/.materialize.txn/backup/token"
plant "B token $(sumof "B token")"
expect_refused symlink

# 7. truncated / forged records: a torn TAIL is tolerated (crash window), but only
#    as the final line.
fixture torn-tail
plant "I token $(sumof "I token") gibberish"
rc=0; mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/torn-tail.log" 2>&1 || rc=$?
if [ "$rc" -eq 0 ]; then ok "torn final record tolerated and the run completed"; else bad "torn final record blocked a normal run"; fi
grep -q "torn final journal record" "$WORK/torn-tail.log" && ok "torn tail reported" || bad "torn tail not reported"
if [ "$(cat "$DST/token")" = "NEW-TOKEN" ]; then ok "rotation still applied after a torn tail"; else bad "rotation lost after a torn tail"; fi

fixture corrupt-middle
printf "I token %s\nB ghost %s\nI token %s\n" "$(sumof "I token")" "deadbeefdeadbeef" "$(sumof "I token")" > "$DST/.materialize.txn/journal"
expect_refused corrupt-middle
# 8. a legitimate crash recovery (B + I with valid records) still works
fixture recovery
ln -f -- "$DST/token" "$DST/.materialize.txn/backup/token"
printf "B token %s\nI token %s\n" "$(sumof "B token")" "$(sumof "I token")" > "$DST/.materialize.txn/journal"
chmod u+w "$DST/token" 2>/dev/null || true
printf "HALF-WRITTEN" > "$DST/token"; chmod 0400 "$DST/token"
if mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/recovery.log" 2>&1; then
  ok "legitimate interrupted transaction replayed"
else bad "legitimate interrupted transaction was refused"; fi
if [ "$(cat "$DST/token")" = "NEW-TOKEN" ]; then ok "recovery then installed the new set"; else bad "recovery did not install the new set"; fi

# 9. no name-absent window while rotating (P2-1): a reader must never see ENOENT
fixture window
mkdir -p "$WORK/window/src2"; chmod 0700 "$WORK/window/src2"
( r=0; e=0; while [ "$r" -lt 400 ]; do
    if cat "$DST/token" >/dev/null 2>/dev/null; then r=$((r+1)); else e=$((e+1)); fi
    if [ ! -f "$DST/token" ]; then e=$((e+1)); fi
  done; echo "$r $e" > "$WORK/window/readers.txt" ) &
reader=$!
for n in 1 2 3 4 5 6 7 8 9 10; do
  chmod u+w "$SRC/token"; printf "ROT-%s" "$n" > "$SRC/token"; chmod 0444 "$SRC/token"
  mat --source-dir "$SRC" --target-dir "$DST" >>"$WORK/window/run.log" 2>&1 || true
done
wait "$reader" 2>/dev/null
read -r reads errs < "$WORK/window/readers.txt"
if [ "${errs:-1}" = "0" ]; then ok "no ENOENT window for a concurrent reader ($reads reads)"; else bad "reader saw $errs failures"; fi

# 10. explicit revoke removes exactly that name and recovery can restore it (P2-4)
fixture revoke
printf "SECOND" > "$DST/second"; chmod 0400 "$DST/second"
chmod u+w "$SRC/token"; printf "KEEP" > "$SRC/token"; chmod 0444 "$SRC/token"
if mat --source-dir "$SRC" --target-dir "$DST" --revoke second >"$WORK/revoke.log" 2>&1; then
  ok "revoke run completed"
else bad "revoke run failed"; fi
if [ -e "$DST/second" ]; then bad "revoked secret still present"; else ok "revoked secret removed"; fi
if [ "$(cat "$DST/token")" = "KEEP" ]; then ok "unrelated secret kept"; else bad "unrelated secret lost"; fi
if grep -q "SECOND" "$WORK/revoke.log"; then bad "revoke logged the secret value"; else ok "revoke logged no secret value"; fi

fixture revoke-recover
ln -f -- "$DST/token" "$DST/.materialize.txn/backup/token"
printf "R token %s\n" "$(sumof "R token")" > "$DST/.materialize.txn/journal"
rm -f -- "$DST/token"
if mat --source-dir "$SRC" --target-dir "$DST" >"$WORK/revoke-recover.log" 2>&1; then
  ok "interrupted revoke was replayed"
else bad "interrupted revoke could not be replayed"; fi
if [ "$(cat "$DST/token" 2>/dev/null)" = "NEW-TOKEN" ]; then ok "revoked secret restored from the transaction backup"; else bad "revoked secret not restored"; fi

# 11. an unsafe revoke argument is refused
fixture revoke-unsafe
rc=0; mat --source-dir "$SRC" --target-dir "$DST" --revoke "../victim/canary" >"$WORK/revoke-unsafe.log" 2>&1 || rc=$?
if [ "$rc" -ne 0 ] && [ "$(cat "$VICTIM/canary")" = "CANARY" ]; then ok "unsafe revoke name refused"; else bad "unsafe revoke name acted on"; fi

echo "SUMMARY pass=$P fail=$F"
[ "$F" -eq 0 ]
