#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
printf '{"schema":1,"tests":[]}\n' > "$D/browser-evidence.json"
bash "$ROOT/tests/browser/artifact_allowlist.sh" "$D" >/dev/null
printf 'not evidence\n' > "$D/trace.zip"
if bash "$ROOT/tests/browser/artifact_allowlist.sh" "$D" >/dev/null 2>&1; then echo "FAIL raw trace accepted"; exit 1; fi
rm "$D/trace.zip"; printf '{"authorization":"Bearer secret"}\n' > "$D/browser-evidence.json"
if bash "$ROOT/tests/browser/artifact_allowlist.sh" "$D" >"$D/log" 2>&1; then echo "FAIL token field accepted"; exit 1; fi
if grep -q 'Bearer secret' "$D/log"; then echo "FAIL token leaked to log"; exit 1; fi
echo "BROWSER_ARTIFACT_ALLOWLIST_TEST_PASS"
