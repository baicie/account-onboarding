#!/usr/bin/env bash
# R10 isolated OCI release dry run. Uses a throwaway local registry, test
# signing key and no production credentials. This is NOT a GHCR release.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if ! docker info >/dev/null 2>&1; then echo "NOT_RUN: Docker daemon unavailable"; exit 3; fi
ID="r10-dry-$(date +%s)-$$"
WORK="$ROOT/.sandbox/r10/dry-run-$ID"
REG_PORT="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"
REG="127.0.0.1:$REG_PORT"
IMAGE_LOCAL="onboarding-dry:$ID"
IMAGE_REG="$REG/onboarding-dry:$ID"
IMG_FINAL="$REG/onboarding-dry@"
mkdir -p "$WORK"
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad(){ FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
cleanup(){
  docker rm -f "$ID-registry" >/dev/null 2>&1 || true
  docker image rm "$IMAGE_LOCAL" "$REG/onboarding-dry:$ID" >/dev/null 2>&1 || true
  rm -rf "$WORK"
  return 0
}
trap cleanup EXIT
GIT_SHA="$(git -C "$ROOT" rev-parse HEAD)"
echo "=== build candidate $GIT_SHA ==="
docker build --build-arg VERSION=0.1.0 --build-arg COMMIT="$GIT_SHA" -t "$IMAGE_LOCAL" "$ROOT" >/dev/null
IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$IMAGE_LOCAL")"
docker run --rm "$IMAGE_LOCAL" version | tee "$WORK/version.txt"
grep -q "commit $GIT_SHA" "$WORK/version.txt" && ok "image-internal commit matches $GIT_SHA" || bad "image commit mismatch"
bash "$ROOT/scripts/supply_chain.sh" "$IMAGE_LOCAL" "$WORK/supply" >/dev/null
ok "SBOM/provenance generated"
# Isolated registry (local only).
docker run -d --name "$ID-registry" -p "127.0.0.1:$REG_PORT:5000" registry:2.8.3 >/dev/null
for _ in $(seq 1 60); do curl -fsS "http://$REG/v2/" >/dev/null 2>&1 && break; sleep 0.5; done
curl -fsS "http://$REG/v2/" >/dev/null && ok "throwaway OCI registry reachable" || { bad "registry unavailable"; exit 1; }
docker tag "$IMAGE_LOCAL" "$IMAGE_REG"
docker push "$IMAGE_REG" >/dev/null 2>&1 && ok "pushed to throwaway registry" || bad "push failed"
DIGEST_REF="$(docker inspect --format '{{index .RepoDigests 0}}' "$IMAGE_REG")"
DIGEST="${DIGEST_REF##*@}"
[ "${DIGEST#sha256:}" != "$DIGEST" ] && ok "registry manifest digest $DIGEST" || bad "no digest: $DIGEST_REF"
# Registry API must agree with the Docker-reported digest.
API_DIGEST="$(curl -fsSI "http://$REG/v2/onboarding-dry/manifests/$DIGEST" -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' | tr -d '\r' | awk -F': ' 'tolower($1)=="docker-content-digest"{print $2}')"
[ "$API_DIGEST" = "$DIGEST" ] && ok "registry Digest header agrees" || bad "digest mismatch API=$API_DIGEST"
IMMUTABLE="$(curl --http1.1 -sS -o /dev/null -w '%{http_code}' -X PUT "http://$REG/v2/onboarding-dry/manifests/$DIGEST" -H 'Content-Type: application/vnd.docker.distribution.manifest.v2+json' -d '{}' 2>/dev/null || true)"
case "$IMMUTABLE" in 400|405) ok "digest immutable (write refused $IMMUTABLE)" ;; *) bad "registry accepted digest overwrite ($IMMUTABLE)" ;; esac
# Pull by digest into a clean local image name; verify binary/commit again.
docker image rm "$IMAGE_LOCAL" >/dev/null 2>&1 || true
docker pull "$DIGEST_REF" >/dev/null 2>&1 && ok "pulled by immutable digest" || bad "digest pull failed"
docker tag "$DIGEST_REF" "$IMAGE_LOCAL"
docker run --rm "$IMAGE_LOCAL" version | tee "$WORK/pulled-version.txt"
grep -q "commit $GIT_SHA" "$WORK/pulled-version.txt" && ok "pulled image commit matches" || bad "pulled image commit mismatch"
# Pinned + checksum-verified cosign.
V=2.4.1
mkdir -p "$WORK/cosign"
curl -sSfL -o "$WORK/cosign/cosign-linux-amd64" "https://github.com/sigstore/cosign/releases/download/v${V}/cosign-linux-amd64"
curl -sSfL -o "$WORK/cosign/cosign_checksums.txt" "https://github.com/sigstore/cosign/releases/download/v${V}/cosign_checksums.txt"
( cd "$WORK/cosign" && grep " cosign-linux-amd64$" cosign_checksums.txt | sha256sum -c - >/dev/null )
chmod +x "$WORK/cosign/cosign-linux-amd64"
COSIGN="$WORK/cosign/cosign-linux-amd64"
"$COSIGN" version | tee "$WORK/cosign-version.txt" >/dev/null
ok "cosign v$V downloaded and SHA-256 verified"
( cd "$WORK" && COSIGN_PASSWORD=test-only-password "$COSIGN" generate-key-pair >/dev/null 2>&1 )
COSIGN_PASSWORD=test-only-password "$COSIGN" sign --key "$WORK/cosign.key" --yes --tlog-upload=false --allow-insecure-registry "$DIGEST_REF" >"$WORK/cosign-sign.out" 2>"$WORK/cosign-sign.err" \
  && ok "signed test image by digest" || { bad "cosign sign failed"; tail -5 "$WORK/cosign-sign.err"; }
"$COSIGN" verify --key "$WORK/cosign.pub" --insecure-ignore-tlog --allow-insecure-registry "$DIGEST_REF" >/dev/null 2>&1 \
  && ok "cosign verify passed" || bad "cosign verify failed"
python3 - "$GIT_SHA" "$IMAGE_ID" "$DIGEST" "$WORK/provenance.json" <<'PY'
import json,sys
commit,image_id,digest,out=sys.argv[1:5]
json.dump({"commit":commit,"image_id":image_id,"registry_digest":digest,
           "purpose":"R10 isolated release dry run; NOT a production publish"},
          open(out,"w"),indent=2)
PY
COSIGN_PASSWORD=test-only-password "$COSIGN" attest --key "$WORK/cosign.key" --yes --tlog-upload=false --allow-insecure-registry \
  --predicate "$WORK/provenance.json" --type custom "$DIGEST_REF" >"$WORK/cosign-attest.out" 2>"$WORK/cosign-attest.err" \
  && ok "attestation pushed" || { bad "attestation failed"; tail -5 "$WORK/cosign-attest.err"; }
"$COSIGN" verify-attestation --key "$WORK/cosign.pub" --insecure-ignore-tlog --allow-insecure-registry \
  --type custom "$DIGEST_REF" > "$WORK/attestation.json" 2>"$WORK/cosign-verify-attestation.err" \
  && ok "attestation verified" || { bad "attestation verification failed"; tail -5 "$WORK/cosign-verify-attestation.err"; }
python3 - "$WORK/attestation.json" "$GIT_SHA" <<'PY' && ok "attestation binds commit" || bad "attestation commit binding missing"
import base64,json,sys
want=sys.argv[2]
try:
    data=json.load(open(sys.argv[1]))
except Exception:
    sys.exit(1)
items=data if isinstance(data,list) else [data]
found=False
for item in items:
    payload=item.get('payload') if isinstance(item,dict) else None
    if payload:
        try:
            import json as j
            if want in j.dumps(j.loads(base64.b64decode(payload)),sort_keys=True):
                found=True
        except Exception:
            pass
    if isinstance(item,dict) and want in json.dumps(item):
        found=True
sys.exit(0 if found else 1)
PY
echo "SUMMARY pass=$PASS fail=$FAIL digest=$DIGEST image_id=$IMAGE_ID"
trap - EXIT
cleanup || true
[ "$FAIL" = "0" ]
exit $?
