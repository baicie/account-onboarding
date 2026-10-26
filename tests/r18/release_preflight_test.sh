#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
W="$(mktemp -d)"; trap 'rm -rf "$W"' EXIT
mkdir -p "$W/c/supply" "$W/ev"
COMMIT=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
printf '%s\n' "$COMMIT" > "$W/c/commit.txt"
python3 - "$W" "$COMMIT" <<'PY'
import hashlib,json,sys,pathlib,tarfile,io
w=pathlib.Path(sys.argv[1]); commit=sys.argv[2]; c=w/'c'
config=json.dumps({"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}).encode()
image_id='sha256:'+hashlib.sha256(config).hexdigest()
manifest=[{"Config":"config.json","RepoTags":["account-onboarding:candidate"],"Layers":[]}]
with tarfile.open(c/'image.tar','w') as tf:
    for name,data in [('manifest.json',json.dumps(manifest).encode()),('config.json',config)]:
        ti=tarfile.TarInfo(name); ti.size=len(data); tf.addfile(ti,io.BytesIO(data))
(c/'image-id.txt').write_text(image_id+'\n')
(c/'supply'/'sbom.cdx.json').write_text('{"sbom":true}\n')
sha=lambda p: hashlib.sha256(p.read_bytes()).hexdigest()
prov={"commit":commit,"image_id":image_id,"image_tar_sha256":sha(c/'image.tar'),"sbom_sha256":sha(c/'supply'/'sbom.cdx.json')}
(c/'candidate-provenance.json').write_text(json.dumps(prov))
digest='sha256:'+'1'*64
(w/'ev'/'log').write_text(f'SAME_CANDIDATE_ARTIFACT=x\nSUMMARY pass=14 fail=0 digest={digest} image_id={image_id}\ncosign verify passed\nattestation verified\nATTESTATION_VALID subject_digest={digest} commit={commit}\n')
gates={"commit":commit,"runs":{"ci":1},"gates":{"go_unit_race":"PASS","provider_contract":"PASS","sandbox_infra":"PASS","application_e2e":"PASS","browser_admin_core":"PASS","backup_restore":"PASS","supply_chain":"PASS","same_artifact":"PASS","arm64_service":"PASS","staging_acceptance":"PASS","authorized_live":"NOT_RUN","ghcr_publish":"NOT_RUN","github_release":"NOT_RUN","production_deploy":"NOT_RUN"}}
(w/'gates.json').write_text(json.dumps(gates))
PY
run(){ python3 "$ROOT/scripts/release_preflight.py" --candidate-dir "$W/c" --same-artifact-evidence "$W/ev" --expected-commit "$COMMIT" --gates "$W/gates.json" --out "$W/out.json" >/dev/null; }
run
grep -q '"status": "PASS"' "$W/out.json"
cp "$W/ev/log" "$W/log.orig"
python3 - "$W/ev/log" <<'PY'
import re,sys
p=sys.argv[1]; s=open(p).read(); s=re.sub(r'image_id=sha256:[0-9a-f]+','image_id=sha256:'+'2'*64,s,count=1); open(p,'w').write(s)
PY
if run >/dev/null 2>&1; then echo "FAIL wrong evidence image id accepted"; exit 1; fi
cp "$W/log.orig" "$W/ev/log"
cp "$W/gates.json" "$W/gates.orig"
python3 - "$W/gates.json" <<'PY'
import json,sys
p=sys.argv[1]; d=json.load(open(p)); d['commit']='cafebabecafebabecafebabecafebabecafebabe'; json.dump(d,open(p,'w'))
PY
if run >/dev/null 2>&1; then echo "FAIL wrong gate commit accepted"; exit 1; fi
cp "$W/gates.orig" "$W/gates.json"
printf x >> "$W/c/image.tar"
if run >/dev/null 2>&1; then echo "FAIL tampered tar accepted"; exit 1; fi
echo "RELEASE_PREFLIGHT_TEST_PASS"
