#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
W="$(mktemp -d)"; trap 'rm -rf "$W"' EXIT
mkdir -p "$W/c/supply" "$W/ev"
printf 'deadbeef\n' > "$W/c/commit.txt"; printf 'sha256:%064d\n' 0 > "$W/c/image-id.txt"
printf 'image-bytes' > "$W/c/image.tar"; printf '{"sbom":true}\n' > "$W/c/supply/sbom.cdx.json"
python3 - "$W" <<'PY'
import hashlib,json,sys,pathlib
w=pathlib.Path(sys.argv[1])
def sha(p): return hashlib.sha256(p.read_bytes()).hexdigest()
prov={"commit":"deadbeef","image_id":"sha256:"+"0"*64,"image_tar_sha256":sha(w/"c/image.tar"),"sbom_sha256":sha(w/"c/supply/sbom.cdx.json")}
(w/"c/candidate-provenance.json").write_text(json.dumps(prov))
PY
printf 'SAME_CANDIDATE_ARTIFACT=x\nSUMMARY pass=14 fail=0\ncosign verify PASS\ncosign attest PASS\n' > "$W/ev/log"
cat > "$W/gates.json" <<'JSON'
{"go_unit_race":"PASS","provider_contract":"PASS","sandbox_infra":"PASS","application_e2e":"PASS","browser_admin_core":"PASS","backup_restore":"PASS","supply_chain":"PASS","same_artifact":"PASS","arm64_service":"PASS","staging_acceptance":"PASS","authorized_live":"NOT_RUN","ghcr_publish":"NOT_RUN","github_release":"NOT_RUN","production_deploy":"NOT_RUN"}
JSON
python3 "$ROOT/scripts/release_preflight.py" --candidate-dir "$W/c" --same-artifact-evidence "$W/ev" --expected-commit deadbeef --gates "$W/gates.json" --out "$W/out.json" >/dev/null
grep -q '"status": "PASS"' "$W/out.json"
printf 'tampered' >> "$W/c/image.tar"
if python3 "$ROOT/scripts/release_preflight.py" --candidate-dir "$W/c" --same-artifact-evidence "$W/ev" --expected-commit deadbeef --gates "$W/gates.json" --out "$W/out2.json" >/dev/null 2>&1; then echo "FAIL tampered tar accepted"; exit 1; fi
echo "RELEASE_PREFLIGHT_TEST_PASS"
