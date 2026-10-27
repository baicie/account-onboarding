#!/usr/bin/env bash
# BuildKit / SLSA provenance acceptance against a throwaway local registry.
#
# Build once, push once, then verify the attestations by reading the registry:
# subject digest, predicate type, builder, invocation, source repository/commit,
# platform and SBOM. A rebuild is proven to be a different artifact, and a
# statement fetched from the registry is forged on disk to prove the verifier
# rejects it.
#
# Statuses are reported separately and never conflated:
#   PROVENANCE_GENERATED      attestations exist in the registry
#   PROVENANCE_VERIFIED       every field matched the expectations
#   TRUSTED_BUILD_VERIFIED    NOT claimed: no signature issuer/identity was
#                             verified against a trust root by this job.
set -o pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
COMMIT="$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
REPO="https://github.com/baicie/account-onboarding"
REG_PORT="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"
REGISTRY="127.0.0.1:$REG_PORT"
REG_NAME="r22-registry-$$"
BUILDER="r22-builder-$$"
TAG="r22-$$"
IMG="$REGISTRY/r22-provenance"
WORK="$(mktemp -d /tmp/r22-prov.XXXXXX)"
EVIDENCE=""; CLEAN_RC=0
P=0; F=0
ok() { P=$((P+1)); echo "PASS $1"; }
bad() { F=$((F+1)); echo "FAIL $1"; }
fail() { echo "FATAL: $*" >&2; exit 1; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --evidence-dir) EVIDENCE="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -z "$EVIDENCE" ] || mkdir -p "$EVIDENCE"
verify() { python3 "$ROOT/scripts/verify_buildkit_provenance.py" "$@"; }
cleanup() {
  local rc=$?
  docker buildx rm "$BUILDER" >/dev/null 2>&1
  docker rm -f "$REG_NAME" >/dev/null 2>&1
  rm -rf "$WORK" 2>/dev/null
  [ ! -d "$WORK" ] || { echo "cleanup: work dir left" >&2; CLEAN_RC=1; }
  if docker ps -a --format '{{.Names}}' | grep -qx "$REG_NAME"; then echo "cleanup: registry left" >&2; CLEAN_RC=1; fi
  echo "SUMMARY pass=$P fail=$F"
  if [ "$rc" -ne 0 ]; then exit "$rc"; fi
  [ "$CLEAN_RC" -eq 0 ] || { echo "CLEANUP_FAILED" >&2; exit 1; }
  [ "$F" -eq 0 ] && exit 0
  exit 1
}
trap cleanup EXIT

echo "=== BuildKit provenance acceptance ==="
docker info >/dev/null 2>&1 || fail "docker unavailable"
docker buildx version >/dev/null 2>&1 || fail "buildx unavailable"
echo "commit=$COMMIT repository=$REPO registry=$REGISTRY"

docker run -d --name "$REG_NAME" -p "$REG_PORT:5000" registry:2 >/dev/null || fail "registry start"
for _ in $(seq 1 60); do curl -sf -m 2 "http://$REGISTRY/v2/" >/dev/null 2>&1 && break; sleep 0.5; done
curl -sf -m 3 "http://$REGISTRY/v2/" >/dev/null || fail "registry not ready"
ok "throwaway registry running at $REGISTRY"

# The container driver runs BuildKit in its own container, so it must share the
# host network to reach the throwaway registry on loopback.
docker buildx create --name "$BUILDER" --driver docker-container --driver-opt network=host >/dev/null || fail "buildx create"
docker buildx inspect --bootstrap "$BUILDER" >"$WORK/buildx.log" 2>&1 || { tail -20 "$WORK/buildx.log"; fail "buildx bootstrap"; }
ok "buildx builder with attestation support is ready"

docker buildx build --builder "$BUILDER" --platform linux/amd64 \
  --build-arg SOURCE_COMMIT="$COMMIT" --build-arg SOURCE_REPOSITORY="$REPO" \
  --build-arg VERSION=0.1.0 --build-arg COMMIT="$COMMIT" \
  --provenance=mode=max --sbom=true \
  -t "$IMG:$TAG" --push "$ROOT" >"$WORK/build.log" 2>&1 \
  || { tail -40 "$WORK/build.log"; fail "attested build and push"; }
ok "image built once with provenance and SBOM, pushed to the isolated registry"

DIGEST="$(curl -sI -H 'Accept: application/vnd.oci.image.index.v1+json' "http://$REGISTRY/v2/r22-provenance/manifests/$TAG" | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:"{print $2}')"
case "$DIGEST" in sha256:*) ok "registry reported manifest digest $(printf '%.19s' "$DIGEST")...";; *) fail "no manifest digest: '$DIGEST'";; esac

if verify --registry "$REGISTRY" --repository r22-provenance --digest "$DIGEST" \
     --expect-commit "$COMMIT" --expect-repository "$REPO" --expect-platform linux/amd64 \
     --dump-statement "$WORK/provenance.json" --out "$WORK/verify.json" >"$WORK/verify.out" 2>&1; then
  ok "provenance verified: subject, builder, invocation, source and platform"
else
  cat "$WORK/verify.out"; bad "provenance verification"
fi
python3 - "$WORK/verify.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print("status=%s generated=%s trusted=%s" % (d.get("status"), d.get("provenance_generated"), d.get("trusted_build_verified")))
kinds = sorted({a["kind"] for a in d.get("attestations", [])})
print("attestations=%s" % ",".join(kinds))
sys.exit(0 if d.get("status") == "PROVENANCE_VERIFIED" and "sbom" in kinds and "provenance" in kinds else 1)
PY
[ "$?" -eq 0 ] && ok "report shows PROVENANCE_VERIFIED with a provenance and an sbom attestation" || bad "verification report incomplete"
grep -q '"trusted_build_verified": false' "$WORK/verify.json" && ok "TRUSTED_BUILD_VERIFIED is explicitly not claimed" || bad "trusted flag missing from the report"

docker pull "$IMG@$DIGEST" >"$WORK/pull.log" 2>&1 || { tail -20 "$WORK/pull.log"; fail "pull by digest"; }
pulled="$(docker image inspect "$IMG@$DIGEST" --format '{{index .RepoDigests 0}}' 2>/dev/null)"
case "$pulled" in *"$DIGEST") ok "pulled artifact digest matches the verified digest";; *) bad "pulled digest $pulled != $DIGEST";; esac
image_id="$(docker image inspect "$IMG@$DIGEST" --format '{{.Id}}')"
[ "$image_id" != "$DIGEST" ] && ok "image ID and OCI manifest digest are different identities" || bad "image ID equals the manifest digest"
tar_sha="$(docker save "$IMG@$DIGEST" | sha256sum | cut -d' ' -f1)"
bare_digest="$(printf '%s' "$DIGEST" | sed 's/^sha256://')"
[ "$tar_sha" != "$bare_digest" ] && ok "image tar SHA differs from the manifest digest (distinct identity)" || bad "tar SHA collides with the manifest digest"

verify --registry "$REGISTRY" --repository r22-provenance --digest "$DIGEST" \
  --expect-commit "b$(printf '0%.0s' $(seq 1 39))" --expect-repository "$REPO" --expect-platform linux/amd64 >/dev/null 2>&1 \
  && bad "wrong commit accepted" || ok "wrong commit rejected"
verify --registry "$REGISTRY" --repository r22-provenance --digest "$DIGEST" \
  --expect-commit "$COMMIT" --expect-repository "https://github.com/attacker/fork" --expect-platform linux/amd64 >/dev/null 2>&1 \
  && bad "wrong repository accepted" || ok "wrong repository rejected"
verify --registry "$REGISTRY" --repository r22-provenance --digest "$DIGEST" \
  --expect-commit "$COMMIT" --expect-repository "$REPO" --expect-platform linux/arm64 >/dev/null 2>&1 \
  && bad "wrong platform accepted" || ok "wrong platform rejected"
verify --registry "$REGISTRY" --repository r22-provenance --digest "sha256:$(printf 'c%.0s' $(seq 1 64))" \
  --expect-commit "$COMMIT" --expect-repository "$REPO" --expect-platform linux/amd64 >/dev/null 2>&1 \
  && bad "wrong subject digest accepted" || ok "wrong subject digest rejected"

python3 - "$WORK/provenance.json" "$WORK/provenance-forged.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for item in d["predicate"]["invocation"]["parameters"]["build-args"]:
    if item.get("key") == "SOURCE_COMMIT":
        item["value"] = "f" * 40
json.dump(d, open(sys.argv[2], "w"), indent=2)
print("forged SOURCE_COMMIT in the fetched statement")
PY
verify --statement-file "$WORK/provenance-forged.json" --digest "$DIGEST" \
  --expect-commit "$COMMIT" --expect-repository "$REPO" --expect-platform linux/amd64 >/dev/null 2>&1 \
  && bad "forged statement accepted" || ok "forged statement rejected"

docker buildx build --builder "$BUILDER" --platform linux/amd64 \
  --provenance=false --sbom=false -t "$IMG:$TAG-unsigned" --push "$ROOT" >"$WORK/build2.log" 2>&1 \
  || { tail -30 "$WORK/build2.log"; fail "unsigned build"; }
UNDIGEST="$(curl -sI -H 'Accept: application/vnd.oci.image.manifest.v1+json' "http://$REGISTRY/v2/r22-provenance/manifests/$TAG-unsigned" | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:"{print $2}')"
verify --registry "$REGISTRY" --repository r22-provenance --digest "$UNDIGEST" \
  --expect-commit "$COMMIT" --expect-repository "$REPO" --expect-platform linux/amd64 >/dev/null 2>&1 \
  && bad "image without attestations accepted" || ok "image without attestations rejected"

docker buildx build --builder "$BUILDER" --platform linux/amd64 \
  --build-arg SOURCE_COMMIT="$COMMIT" --build-arg SOURCE_REPOSITORY="$REPO" \
  --build-arg VERSION=0.1.0 --build-arg COMMIT="$COMMIT" \
  --provenance=mode=max --sbom=true -t "$IMG:$TAG-rebuilt" --push "$ROOT" >"$WORK/build3.log" 2>&1 \
  || { tail -30 "$WORK/build3.log"; fail "rebuild"; }
REBUILT="$(curl -sI -H 'Accept: application/vnd.oci.image.index.v1+json' "http://$REGISTRY/v2/r22-provenance/manifests/$TAG-rebuilt" | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:"{print $2}')"
[ -n "$REBUILT" ] && [ "$REBUILT" != "$DIGEST" ] && ok "a rebuild is a different artifact digest (never claimed as the candidate)" || bad "rebuild reused the verified digest"

{
  echo "commit=$COMMIT"
  echo "repository=$REPO"
  echo "registry=$REGISTRY"
  echo "image_digest=$DIGEST"
  echo "image_id=$image_id"
  echo "tar_sha256=$tar_sha"
  echo "rebuilt_digest=$REBUILT"
  echo "unsigned_digest=$UNDIGEST"
} >"$WORK/summary.txt"
[ -z "$EVIDENCE" ] || cp "$WORK/summary.txt" "$WORK/verify.json" "$EVIDENCE/" 2>/dev/null
cat "$WORK/summary.txt"
echo "PROVENANCE_VERIFIED"
echo "TRUSTED_BUILD_VERIFIED=NO (no signature issuer/identity verification in this job)"
exit 0
