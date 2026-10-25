#!/usr/bin/env python3
"""Validate a cosign verify-attestation JSON result against the exact image digest
and build commit. Signature verification is done by cosign before this script;
this parser rejects malformed/incorrect in-toto statements and string-substring
false positives."""
import base64, json, sys

def fail(msg):
    print("ATTESTATION_INVALID: " + msg, file=sys.stderr)
    sys.exit(1)

def main():
    path, want_digest, want_commit = sys.argv[1:4]
    try:
        data = json.load(open(path))
    except Exception as e:
        fail("invalid verification JSON: %s" % e)
    envelopes = data if isinstance(data, list) else [data]
    if not envelopes:
        fail("no attestation envelopes")
    matched = 0
    for env in envelopes:
        if not isinstance(env, dict) or not env.get("payload"):
            fail("missing DSSE payload")
        payload_type = env.get("payloadType", "")
        if payload_type not in ("application/vnd.in-toto+json", "application/vnd.dsse.envelope.v1+json"):
            fail("unexpected payloadType %r" % payload_type)
        try:
            st = json.loads(base64.b64decode(env["payload"]))
        except Exception as e:
            fail("payload is not base64 in-toto JSON: %s" % e)
        if st.get("_type") != "https://in-toto.io/Statement/v0.1":
            fail("wrong in-toto statement type")
        subjects = st.get("subject") or []
        if not isinstance(subjects, list) or not subjects:
            fail("missing subject")
        if len(subjects) != 1:
            fail("expected exactly one subject (got %d)" % len(subjects))
        subj = subjects[0] if isinstance(subjects[0], dict) else {}
        digest = subj.get("digest")
        if not isinstance(digest, dict) or len(digest) != 1:
            fail("subject.digest must contain exactly one algorithm/digest")
        sha = digest.get("sha256")
        if not isinstance(sha, str) or not sha:
            fail("subject digest is not sha256")
        if "sha256:" + sha != want_digest:
            fail("subject digest mismatch")
        pred = st.get("predicate")
        if not isinstance(pred, dict):
            fail("missing predicate object")
        # cosign attest --type custom wraps the supplied predicate as
        # {"Data": <our JSON>, "Timestamp": "..."}. Require that exact shape;
        # arbitrary annotations/fields are never accepted as the commit.
        data = pred.get("Data")
        if isinstance(data, str):
            # cosign custom attestation may carry Data as a JSON string.
            try:
                data = json.loads(data)
            except Exception:
                fail("predicate.Data is not valid JSON")
        if not isinstance(data, dict):
            fail("predicate.Data missing (cosign custom predicate shape)")
        if data.get("commit") != want_commit:
            fail("predicate.Data.commit mismatch (must be exact full SHA); keys=%s" % sorted(data.keys()))
        if data.get("registry_digest") != want_digest:
            fail("predicate.Data.registry_digest mismatch")
        pt = st.get("predicateType", "")
        if not pt.startswith("https://cosign.sigstore.dev/attestation/"):
            fail("unexpected predicateType %r" % pt)
        matched += 1
    if matched != 1:
        fail("expected one matching attestation, got %d" % matched)
    print("ATTESTATION_VALID subject_digest=%s commit=%s" % (want_digest, want_commit))

if __name__ == "__main__":
    main()
