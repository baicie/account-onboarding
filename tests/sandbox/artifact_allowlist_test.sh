#!/usr/bin/env bash
# Deterministic tests for privileged CI artifact boundary.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
mkdir -p "$D/safe"; printf '{"completed":5}\n' > "$D/safe/evidence-five.json"; echo summary > "$D/safe/summary.txt"
bash "$ROOT/tests/sandbox/artifact_allowlist.sh" "$D/safe" >/dev/null
mkdir -p "$D/bad"; printf '{"completed":5}\n' > "$D/bad/evidence-five.json"; printf 'super-secret-value\n' > "$D/bad/admin-token"
if bash "$ROOT/tests/sandbox/artifact_allowlist.sh" "$D/bad" >"$D/bad.log" 2>&1; then echo "FAIL filename accepted"; exit 1; fi
grep -q 'admin-token' "$D/bad.log" || { echo "FAIL filename not reported"; exit 1; }
if grep -q 'super-secret-value' "$D/bad.log"; then echo "FAIL secret value leaked to log"; exit 1; fi
mkdir -p "$D/content"; printf 'master-key=super-secret-value\n' > "$D/content/evidence-bad.json"
if bash "$ROOT/tests/sandbox/artifact_allowlist.sh" "$D/content" >"$D/content.log" 2>&1; then echo "FAIL content accepted"; exit 1; fi
grep -q 'evidence-bad.json' "$D/content.log" || { echo "FAIL content file not reported"; exit 1; }
if grep -q 'super-secret-value' "$D/content.log"; then echo "FAIL content value leaked to log"; exit 1; fi
echo "ARTIFACT_ALLOWLIST_TEST_PASS"
