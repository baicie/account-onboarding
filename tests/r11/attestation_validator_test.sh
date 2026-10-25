#!/usr/bin/env bash
# Negative/positive tests for the strict in-toto attestation validator. Signature
# verification itself is cosign's job; this tests statement structure/exact values.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
DIGEST="sha256:$(printf 'a%.0s' {1..64})"
COMMIT="$(printf 'b%.0s' {1..40})"
python3 - "$WORK" "$DIGEST" "$COMMIT" <<'PY'
import base64,json,os,sys
w,d,c=sys.argv[1:4]
sha=d.split(':',1)[1]
def st(**over):
    x={"_type":"https://in-toto.io/Statement/v0.1","predicateType":"https://cosign.sigstore.dev/attestation/v1",
       "subject":[{"name":"image","digest":{"sha256":sha}}],
       "predicate":{"commit":c,"registry_digest":d}}
    x.update(over); return x
def env(statement):
    return [{"payloadType":"application/vnd.in-toto+json","payload":base64.b64encode(json.dumps(statement).encode()).decode(),"signatures":[{"keyid":"","sig":"test"}]}]
json.dump(env(st()),open(os.path.join(w,'valid.json'),'w'))
json.dump(env(st(predicate={"commit":c,"registry_digest":"sha256:"+"0"*64})),open(os.path.join(w,'wrong-digest.json'),'w'))
json.dump(env(st(predicate={"commit":"c"*40,"registry_digest":d})),open(os.path.join(w,'wrong-commit.json'),'w'))
x=st(predicate={}); x['annotations']={'commit':c}; json.dump(env(x),open(os.path.join(w,'commit-annotation-only.json'),'w'))
json.dump(env({"_type":"https://in-toto.io/Statement/v0.1","predicateType":"https://cosign.sigstore.dev/attestation/v1","subject":[{"digest":{"sha256":sha}}]}),open(os.path.join(w,'missing-predicate.json'),'w'))
json.dump([{"payloadType":"application/vnd.in-toto+json","payload":"not-base64!!"}],open(os.path.join(w,'invalid-payload.json'),'w'))
x=st(); x['subject'].append({"digest":{"sha256":sha}}); json.dump(env(x),open(os.path.join(w,'duplicate-subject.json'),'w'))
json.dump(env(st(predicateType="https://example.com/wrong")),open(os.path.join(w,'wrong-type.json'),'w'))
PY
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); printf 'PASS %s\n' "$1"; }
bad(){ FAIL=$((FAIL+1)); printf 'FAIL %s\n' "$1"; }
expect_ok(){ if python3 "$ROOT/tests/r10/validate_attestation.py" "$1" "$DIGEST" "$COMMIT" >/dev/null 2>&1; then ok "$2"; else bad "$2"; fi; }
expect_fail(){ if python3 "$ROOT/tests/r10/validate_attestation.py" "$1" "$DIGEST" "$COMMIT" >/dev/null 2>&1; then bad "$2"; else ok "$2"; fi; }
expect_ok "$WORK/valid.json" "valid attestation accepted"
expect_fail "$WORK/wrong-digest.json" "wrong subject/predicate digest rejected"
expect_fail "$WORK/wrong-commit.json" "wrong commit rejected"
expect_fail "$WORK/commit-annotation-only.json" "commit only in annotations rejected"
expect_fail "$WORK/missing-predicate.json" "missing predicate rejected"
expect_fail "$WORK/invalid-payload.json" "invalid payload rejected"
expect_fail "$WORK/duplicate-subject.json" "duplicate subject rejected"
expect_fail "$WORK/wrong-type.json" "wrong predicate type rejected"
echo "SUMMARY pass=$PASS fail=$FAIL"
[ "$FAIL" = "0" ]
