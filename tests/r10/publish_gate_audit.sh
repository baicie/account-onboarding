#!/usr/bin/env bash
# Static audit of the release workflow's publish safety gates. This does not
# execute a publish; it proves the default path cannot hold write permission and
# that the guarded job has the expected environment/artifact checks.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WF="$ROOT/.github/workflows/release-image.yml"
python3 - "$WF" <<'PY'
import sys,yaml
p=sys.argv[1]
d=yaml.safe_load(open(p))
jobs=d["jobs"]
cand=jobs["candidate"]; pub=jobs["publish"]
checks=[]
def check(name, cond):
    checks.append((name,bool(cond)))
perms=lambda j: (j.get("permissions") or {})
check("candidate has contents:read", perms(cand).get("contents")=="read")
check("candidate has no packages:write", perms(cand).get("packages")!="write")
check("publish needs candidate", pub.get("needs")=="candidate")
check("publish guarded by inputs.publish==true", "inputs.publish == true" in str(pub.get("if","")))
check("publish has protected environment", pub.get("environment")=="release")
check("only publish has packages:write", perms(pub).get("packages")=="write")
text=open(p).read()
check("publish asserts candidate govulncheck", "candidate/supply/govulncheck.txt" in text)
check("publish asserts candidate trivy", "candidate/supply/trivy-critical.txt" in text)
check("publish rejects CVE/GHSA", "CVE-[0-9]{4}" in text and "GHSA-" in text)
check("publish validates semantic tag", "invalid release tag" in text)
check("publish validates allowed ref", "allowed protected tag or main" in text)
check("publish verifies candidate commit", 'test "$(cat candidate/commit.txt)" = "$COMMIT"' in text)
check("publish verifies image tar hash", "image_tar_sha256" in text)
check("publish loads and re-checks binary commit", "grep -q \"commit $COMMIT\"" in text)
failed=[n for n,ok in checks if not ok]
for n,ok in checks:
    print(("PASS " if ok else "FAIL ")+n)
if failed:
    raise SystemExit("publish gate audit failed: "+", ".join(failed))
PY
