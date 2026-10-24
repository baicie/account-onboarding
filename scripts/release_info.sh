#!/usr/bin/env bash
# R6 P3: reproducible-build + provenance evidence (no publishing, no network).
# Builds the binary twice with the same version/commit/trimpath flags, compares
# sha256, records Go/module metadata and the availability of supply-chain tools.
# Exit 0 = reproducible; 1 = builds differ or build failed.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
OUT="${1:-$ROOT/docs/acceptance/evidence}"
mkdir -p "$OUT"
VERSION="${VERSION:-0.1.0}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
LDFLAGS="-X github.com/baicie/account-onboarding/internal/api.Version=$VERSION -X github.com/baicie/account-onboarding/internal/api.Commit=$COMMIT"
for i in 1 2; do
  CGO_ENABLED=1 go build -trimpath -ldflags "$LDFLAGS" -o "$TMP/onboardd-$i" ./cmd/onboardd
done
SHA1="$(shasum -a 256 "$TMP/onboardd-1" | awk '{print $1}')"
SHA2="$(shasum -a 256 "$TMP/onboardd-2" | awk '{print $1}')"
REPRO="false"; [ "$SHA1" = "$SHA2" ] && REPRO="true"
go list -m -json all > "$TMP/modules.json" 2>/dev/null || true
MOD_SHA="$(shasum -a 256 "$TMP/modules.json" | awk '{print $1}')"
MOD_COUNT="$(go list -m all 2>/dev/null | wc -l | tr -d ' ')"

DIRTY="false"
if ! git diff --quiet 2>/dev/null || ! git diff --cached --quiet 2>/dev/null; then DIRTY="true"; fi
CGO_ENV="$(go env CGO_ENABLED)"
TOOLS="{}"
for t in govulncheck syft trivy shellcheck; do
  if command -v "$t" >/dev/null 2>&1; then v="$("$t" --version 2>&1 | head -1)"; else v="NOT_INSTALLED"; fi
  TOOLS="$(python3 -c 'import json,sys;d=json.loads(sys.argv[1]);d[sys.argv[2]]=sys.argv[3];print(json.dumps(d))' "$TOOLS" "$t" "$v")"
done
python3 - "$OUT" "$VERSION" "$COMMIT" "$SHA1" "$SHA2" "$REPRO" "$MOD_COUNT" "$MOD_SHA" "$TOOLS" "$TMP/onboardd-1" <<'PY'
import json, os, subprocess, sys
out, version, commit, sha1, sha2, repro, mod_count, mod_sha, tools, binary, dirty, cgo_env, ldflags = sys.argv[1:14]
go_version = subprocess.run(["go","version"],capture_output=True,text=True).stdout.strip()
build_meta = subprocess.run(["go","version","-m",binary],capture_output=True,text=True).stdout.strip()
data = {
  "version": version,
  "commit": commit,
  "go_version": go_version,
  "build_platform": os.uname().machine + "/" + os.uname().sysname,
  "cgo": os.environ.get("CGO_ENABLED", "1") != "0",
  "cgo_env": cgo_env,
  "trimpath": True,
  "vcs_dirty": dirty == "true",
  "build_ldflags": ldflags,
  "reproducible": repro == "true",
  "binary_sha256_build1": sha1,
  "binary_sha256_build2": sha2,
  "module_count": int(mod_count),
  "module_metadata_sha256": mod_sha,
  "supply_chain_tools": json.loads(tools),
  "build_info": build_meta.splitlines(),
}
json.dump(data, open(os.path.join(out, "R6-P3-build-info.json"), "w"), indent=2)
print(json.dumps({"reproducible": data["reproducible"], "sha256": sha1, "commit": commit}))
PY
cp "$TMP/modules.json" "$OUT/R6-P3-go-modules.json"
[ "$REPRO" = "true" ]
