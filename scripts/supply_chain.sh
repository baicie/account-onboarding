#!/usr/bin/env bash
# R8 supply-chain evidence: bind image -> commit -> Go modules -> base images ->
# binary hash -> SBOM/metadata. This script does NOT scan; scanners are run by
# .github/workflows/supply-chain.yml (govulncheck/syft/trivy) so a scanner
# failure cannot be mistaken for a PASS here.
#
# Usage: scripts/supply_chain.sh IMAGE OUT_DIR
set -euo pipefail
IMAGE="${1:?usage: supply_chain.sh IMAGE OUT_DIR}"
OUT="${2:-docs/acceptance/evidence/r8-supply-chain}"
mkdir -p "$OUT"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
COMMIT="$(git rev-parse HEAD)"
GO_VERSION="$(go version | awk '{print $3}')"
IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$IMAGE")"
IMAGE_ID_HEX="${IMAGE_ID#sha256:}"
SIZE="$(docker image inspect --format '{{.Size}}' "$IMAGE")"
ARCH="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$IMAGE")"
USER_CFG="$(docker image inspect --format '{{.Config.User}}' "$IMAGE")"
BINARY_SHA="$(docker run --rm --entrypoint cat "$IMAGE" /app/onboardd \
  | if command -v sha256sum >/dev/null 2>&1; then sha256sum; else shasum -a 256; fi \
  | awk '{print $1}')"
GOLANG_REF="$(awk -F= '/^ARG GOLANG_IMAGE=/{print $2; exit}' Dockerfile)"
RUNTIME_REF="$(awk -F= '/^ARG RUNTIME_IMAGE=/{print $2; exit}' Dockerfile)"
BASE_REFS="golang_image=${GOLANG_REF:-NOT_RUN} runtime_image=${RUNTIME_REF:-NOT_RUN}"
BASE_DIGESTS=""
if command -v docker >/dev/null 2>&1; then
  for ref in "${GOLANG_REF:-}" "${RUNTIME_REF:-}"; do
    [ -n "$ref" ] || continue
    d="$(docker buildx imagetools inspect "$ref" --format '{{json .Manifest}}' 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin).get("digest",""))' 2>/dev/null || true)"
    if [ -n "$d" ]; then
      case "$ref" in
        *@sha256:*) entry="$ref" ;;
        *) entry="$ref@$d" ;;
      esac
      BASE_DIGESTS="${BASE_DIGESTS:+$BASE_DIGESTS; }$entry"
    fi
  done
fi
[ -z "$BASE_DIGESTS" ] && BASE_DIGESTS="NOT_RUN (tag-pinned; registry digest not resolved)"
go mod verify > "$OUT/go-mod-verify.txt"
go list -m -json all > "$OUT/go-modules.jsonl" 2>/dev/null
python3 - "$OUT" "$COMMIT" "$GO_VERSION" "$IMAGE_ID_HEX" "$SIZE" "$ARCH" "$USER_CFG" "$BINARY_SHA" "$BASE_REFS" "$BASE_DIGESTS" <<'PY'
import datetime, hashlib, json, os, sys
out, commit, go_version, image_id, size, arch, user, binary_sha, base_refs, base_digests = sys.argv[1:11]
modules = []
dec = json.JSONDecoder()
raw = open(os.path.join(out, "go-modules.jsonl")).read()
idx = 0
while idx < len(raw):
    while idx < len(raw) and raw[idx].isspace():
        idx += 1
    if idx >= len(raw):
        break
    obj, idx = dec.raw_decode(raw, idx)
    if obj.get("Main"):
        continue
    modules.append({
        "type": "library",
        "name": obj.get("Path", ""),
        "version": obj.get("Version", ""),
        "purl": "pkg:golang/" + obj.get("Path", "") + ("@" + obj.get("Version", "") if obj.get("Version") else ""),
    })
components = [{
    "type": "container",
    "name": "account-onboarding",
    "version": commit,
    "hashes": [{"alg": "SHA-256", "content": image_id}],
}] + modules
bom = {
    "bomFormat": "CycloneDX",
    "specVersion": "1.5",
    "version": 1,
    "metadata": {
        "timestamp": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "component": {
            "type": "container",
            "name": "account-onboarding",
            "version": commit,
            "hashes": [{"alg": "SHA-256", "content": image_id}],
        },
        "properties": [
            {"name": "git.commit", "value": commit},
            {"name": "go.version", "value": go_version},
            {"name": "image.id", "value": image_id},
            {"name": "image.size_bytes", "value": size},
            {"name": "image.platform", "value": arch},
            {"name": "image.user", "value": user},
            {"name": "binary.sha256", "value": binary_sha},
            {"name": "base_image_refs", "value": base_refs[:512]},
            {"name": "base_image_digests", "value": base_digests[:512]},
        ],
    },
    "components": components,
}
json.dump(bom, open(os.path.join(out, "sbom.cdx.json"), "w"), indent=2)
json.dump({
    "commit": commit,
    "go_version": go_version,
    "image_id": image_id,
    "image_size_bytes": int(size),
    "platform": arch,
    "user": user,
    "binary_sha256": binary_sha,
    "base_image_refs": base_refs,
    "base_image_digests": base_digests,
    "sbom_sha256": hashlib.sha256(open(os.path.join(out, "sbom.cdx.json"), "rb").read()).hexdigest(),
    "note": "Image ID is local; no registry manifest digest unless the image was actually pushed. Base-image digests are NOT_RUN when the builder tag is not resolvable.",
}, open(os.path.join(out, "metadata.json"), "w"), indent=2)
print("supply-chain evidence written:", os.path.join(out, "sbom.cdx.json"))
PY
cat "$OUT/metadata.json"
