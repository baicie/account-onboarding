#!/usr/bin/env bash
# Verify that the candidate's source SHA, image artifact, SBOM and provenance
# all identify the same bytes. This is a pre-publish integrity gate; it does not
# publish anything.
set -euo pipefail
CAND="${1:?usage: candidate_identity.sh CANDIDATE_DIR}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test -x "$ROOT/scripts/supply_chain.sh" || { echo "FAIL scripts/supply_chain.sh must be executable" >&2; exit 1; }
COMMIT_FILE="$CAND/commit.txt"
PROV="$CAND/candidate-provenance.json"
TAR="$CAND/image.tar"
IMGID="$CAND/image-id.txt"
SBOM="$CAND/supply/sbom.cdx.json"
for f in "$COMMIT_FILE" "$PROV" "$TAR" "$IMGID" "$SBOM"; do
  test -s "$f" || { echo "FAIL missing artifact $f" >&2; exit 1; }
done
python3 - "$CAND" "$COMMIT_FILE" "$PROV" "$TAR" "$IMGID" "$SBOM" <<'PY'
import hashlib,json,os,shutil,sys,subprocess
cand,commit_file,prov_file,tar,imgid_file,sbom=sys.argv[1:]
def sha(path):
    h=hashlib.sha256()
    with open(path,'rb') as f:
        for b in iter(lambda:f.read(1<<20),b''): h.update(b)
    return h.hexdigest()
commit=open(commit_file).read().strip()
imgid=open(imgid_file).read().strip()
p=json.load(open(prov_file))
want="sha256:"+sha(tar)
if p.get('commit') != commit: raise SystemExit('FAIL provenance commit mismatch')
if p.get('image_id') != imgid: raise SystemExit('FAIL provenance image_id mismatch')
if p.get('image_tar_sha256') != sha(tar): raise SystemExit('FAIL image tar hash mismatch')
if p.get('sbom_sha256') != sha(sbom): raise SystemExit('FAIL SBOM hash mismatch')
for key in ('base_image_refs','base_image_digests'):
    if not p.get(key): raise SystemExit(f'FAIL missing {key}')
digests=p['base_image_digests']
if isinstance(digests,str): digests=[x.strip() for x in digests.split(';') if x.strip()]
if not digests or not all('@sha256:' in str(x) for x in digests): raise SystemExit('FAIL base image digest not pinned')
# Bind metadata to actual image bytes: load the saved tar and compare Docker's
# content-addressed image ID. Metadata agreement alone is not sufficient.
if not shutil.which('docker'):
    raise SystemExit('FAIL docker unavailable: cannot bind image.tar to image ID')
loaded = subprocess.run(['docker','load','-i',tar], capture_output=True, text=True)
if loaded.returncode != 0:
    raise SystemExit('FAIL docker load failed: '+loaded.stderr[-300:])
inspect = subprocess.run(['docker','image','inspect','--format','{{.Id}}','account-onboarding:candidate'], capture_output=True, text=True)
if inspect.returncode != 0:
    raise SystemExit('FAIL docker inspect after load failed: '+inspect.stderr[-300:])
actual = inspect.stdout.strip()
if actual != 'sha256:'+sha(tar).hexdigest() and actual != imgid:
    # Docker image ID is the config digest, not the tar hash; require agreement
    # with the recorded image ID and independently run the version binary.
    raise SystemExit(f'FAIL loaded image ID {actual} != recorded {imgid}')
version = subprocess.run(['docker','run','--rm','account-onboarding:candidate','version'], capture_output=True, text=True)
if version.returncode != 0 or ('commit '+commit) not in version.stdout:
    raise SystemExit('FAIL loaded image commit mismatch: '+(version.stdout+version.stderr)[-300:])
print('PASS candidate identity: commit, image tar sha256, loaded image id, binary commit, SBOM sha256, base digests')
PY
# The same verifier must reject a tampered copy, so a green is not tautological.
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
cp -a "$CAND/." "$TMP/"
chmod +w "$TMP/image-id.txt" 2>/dev/null || true
printf 'sha256:%064d\n' 0 > "$TMP/image-id.txt"
python3 - "$TMP/candidate-provenance.json" <<'PY'
import json,sys
p=json.load(open(sys.argv[1])); p['image_id']='sha256:'+'0'*64
json.dump(p,open(sys.argv[1],'w'))
PY
if bash "$0" "$TMP" >/dev/null 2>&1; then echo "FAIL tampered candidate accepted" >&2; exit 1; fi
echo "PASS candidate identity rejects tampered provenance"
