#!/usr/bin/env bash
# PR-stack merge dry run.
#
# Simulates the operator's integration path for the R8..R22 stack in an isolated
# worktree: merge every branch in dependency order onto main, verify the result
# is conflict free and produces exactly the accepted candidate tree, then build
# and test the merged tree. main is never modified and nothing is pushed.
#
# Usage: scripts/r22_merge_dryrun.sh [--evidence <file>] [--skip-tests]
set -o pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MAIN_REF="origin/main"
TIP_REF="origin/feat/r22-product-first-final-acceptance"
EVIDENCE=""; SKIP_TESTS=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --evidence) EVIDENCE="$2"; shift 2 ;;
    --skip-tests) SKIP_TESTS=1; shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
STACK="feat/r8-staging-readiness
feat/r9-release-hardening
feat/r10-live-preflight
feat/r11-integration-readiness
feat/r12-acceptance-integrity
feat/r13-integration-reliability
feat/r14-security-release-closure
feat/r14-1-final-safety-closure
feat/r15-final-engineering-readiness
feat/r15-1-outbox-security-closure
feat/r16-r17-final-sandbox-release-readiness
feat/r18-final-release-candidate-closure
feat/r19-trusted-release-full-acceptance
feat/r20-final-product-release-closure
feat/r21-autonomous-final-qualification
feat/r22-product-first-final-acceptance"

OUT="$(mktemp -d)"; WT="$OUT/merge-wt"; FAIL=0
emit() { printf '%s\n' "$*" | tee -a "$OUT/report.txt"; }
cleanup() {
  git -C "$ROOT" worktree remove --force "$WT" >/dev/null 2>&1
  rmdir "$OUT" 2>/dev/null
}
trap cleanup EXIT

exec 3>&1
{
emit "=== PR stack merge dry run ==="
emit "repo: $ROOT"
emit "main: $(git -C "$ROOT" rev-parse "$MAIN_REF")"
emit "tip:  $(git -C "$ROOT" rev-parse "$TIP_REF")"
emit ""

# 1. dependency order must be real: every branch has to contain the previous head.
prev="$MAIN_REF"; prev_name="main"
for b in $STACK; do
  mb="$(git -C "$ROOT" merge-base "origin/$b" "$prev")"
  prev_sha="$(git -C "$ROOT" rev-parse "$prev")"
  ahead="$(git -C "$ROOT" rev-list --count "$prev".."origin/$b")"
  behind="$(git -C "$ROOT" rev-list --count "origin/$b".."$prev")"
  if [ "$mb" = "$prev_sha" ] && [ "$behind" = "0" ]; then
    emit "ORDER OK   $b  head=$(git -C "$ROOT" rev-parse --short "origin/$b") ahead=$ahead behind=$behind based-on=$prev_name"
  else
    emit "ORDER FAIL $b  merge-base=$(git -C "$ROOT" rev-parse --short "$mb") expected=$prev_name ahead=$ahead behind=$behind"
    FAIL=1
  fi
  prev="origin/$b"; prev_name="$b"
done
emit ""

# 2. real merges in an isolated worktree (never touches main).
git -C "$ROOT" worktree add --detach "$WT" "$MAIN_REF" >"$OUT/wt.log" 2>&1 || { emit "FATAL: worktree add failed"; cat "$OUT/wt.log"; exit 1; }
merge_ok=0; merge_fail=0
for b in $STACK; do
  log="$OUT/merge-$(printf '%s' "$b" | tr '/' '-').log"
  if git -C "$WT" merge --no-ff --no-edit -m "dry-run: integrate $b" "origin/$b" >"$log" 2>&1; then
    emit "MERGE OK   $b -> $(git -C "$WT" rev-parse --short HEAD) tree=$(git -C "$WT" rev-parse --short 'HEAD^{tree}')"
    merge_ok=$((merge_ok+1))
  else
    emit "MERGE FAIL $b"; sed -n '1,12p' "$log" >>"$OUT/report.txt"
    git -C "$WT" merge --abort >/dev/null 2>&1
    merge_fail=$((merge_fail+1)); FAIL=1
    break
  fi
done
emit ""
emit "merges_ok=$merge_ok merges_failed=$merge_fail"

# 3. the integrated tree must equal the accepted tip tree.
if [ "$merge_fail" = "0" ]; then
  merged_tree="$(git -C "$WT" rev-parse 'HEAD^{tree}')"
  tip_tree="$(git -C "$ROOT" rev-parse "$TIP_REF^{tree}")"
  merged_sha="$(git -C "$WT" rev-parse HEAD)"
  if [ "$merged_tree" = "$tip_tree" ]; then
    emit "TREE MATCH merged=$merged_tree tip=$tip_tree"
  else
    emit "TREE MISMATCH merged=$merged_tree tip=$tip_tree"; FAIL=1
  fi
  emit "dry_run_merge_sha=$merged_sha"
  emit ""
  emit "conflict_markers=$(grep -rIl --exclude-dir=.git -e '^<<<<<<< ' -e '^>>>>>>> ' "$WT" 2>/dev/null | wc -l | tr -d ' ')"
  emit "unmerged_paths=$(git -C "$WT" diff --name-only --diff-filter=U | wc -l | tr -d ' ')"
  emit ""
  if [ "$SKIP_TESTS" = "0" ]; then
    ( cd "$WT" && go build ./... >"$OUT/build.log" 2>&1 ); rc_build=$?
    emit "go_build_exit=$rc_build"
    [ "$rc_build" = "0" ] || { sed -n '1,20p' "$OUT/build.log" >>"$OUT/report.txt"; FAIL=1; }
    ( cd "$WT" && go vet ./... >"$OUT/vet.log" 2>&1 ); rc_vet=$?
    emit "go_vet_exit=$rc_vet"
    [ "$rc_vet" = "0" ] || { sed -n '1,20p' "$OUT/vet.log" >>"$OUT/report.txt"; FAIL=1; }
    ( cd "$WT" && go test ./... -count=1 >"$OUT/test.log" 2>&1 ); rc_test=$?
    emit "go_test_exit=$rc_test"
    if [ "$rc_test" != "0" ]; then
      grep -E '^(FAIL|--- FAIL)' "$OUT/test.log" | head -20 >>"$OUT/report.txt"; FAIL=1
    fi
  else
    emit "tests=SKIPPED"
  fi
fi
emit ""
if [ "$FAIL" = "0" ]; then emit "MERGE_DRYRUN_PASS"; else emit "MERGE_DRYRUN_FAIL"; fi
} 2>&1
rc=$?
if [ -n "$EVIDENCE" ]; then
  mkdir -p "$(dirname "$EVIDENCE")"
  cp "$OUT/report.txt" "$EVIDENCE"
  echo "evidence written to $EVIDENCE" >&2
fi
if [ "$FAIL" = "0" ] && [ "$rc" = "0" ]; then exit 0; fi
exit 1
