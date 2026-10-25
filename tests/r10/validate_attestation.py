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
        digests = []
        for s in subjects:
            d = (s or {}).get("digest") or {}
            sha = d.get("sha256")
            if sha:
                digests.append(sha)
        if len(digests) != 1:
            fail("expected exactly one subject digest")
        if "sha256:" + digests[0] != want_digest:
            fail("subject digest mismatch")
        pred = st.get("predicate")
        if not isinstance(pred, dict):
            fail("missing predicate object")
        if pred.get("commit") != want_commit:
            fail("predicate commit mismatch (must be exact full SHA); predicate keys=%s statement keys=%s" %
                 (sorted(pred.keys()), sorted(st.keys())))
        if pred.get("registry_digest") != want_digest:
            fail("predicate registry_digest mismatch")
        pt = st.get("predicateType", "")
        if not pt.startswith("https://cosign.sigstore.dev/attestation/"):
            fail("unexpected predicateType %r" % pt)
        matched += 1
    if matched != 1:
        fail("expected one matching attestation, got %d" % matched)
    print("ATTESTATION_VALID subject_digest=%s commit=%s" % (want_digest, want_commit))

if __name__ == "__main__":
    main()
