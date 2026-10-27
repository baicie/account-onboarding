#!/usr/bin/env python3
"""Offline negative tests for scripts/verify_buildkit_provenance.py.

Each case is a real in-toto statement (the shape BuildKit writes) and the
verifier's verdict for it. No registry and no docker are needed, so this runs
everywhere and covers the forgery paths that a registry test cannot produce
without an attacker.
"""

import copy
import json
import os
import subprocess
import sys
import tempfile

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
VERIFIER = os.path.join(ROOT, "scripts", "verify_buildkit_provenance.py")
DIGEST = "sha256:" + "a" * 64
COMMIT = "0123456789abcdef0123456789abcdef01234567"
REPO = "https://github.com/baicie/account-onboarding"

PASS = 0
FAIL = 0


def ok(name):
    global PASS
    PASS += 1
    print("PASS " + name)


def bad(name, detail=""):
    global FAIL
    FAIL += 1
    print("FAIL " + name + (": " + detail if detail else ""))


def statement_v02():
    return {
        "_type": "https://in-toto.io/Statement/v0.1",
        "predicateType": "https://slsa.dev/provenance/v0.2",
        "subject": [{"name": "app", "digest": {"sha256": DIGEST.split(":")[1]}}],
        "predicate": {
            "builder": {"id": "https://github.com/moby/buildkit@v1"},
            "buildType": "https://mobyproject.org/buildkit@v1",
            "invocation": {
                "id": "build-1",
                "parameters": {
                    "build-args": [
                        {"key": "SOURCE_COMMIT", "value": COMMIT},
                        {"key": "SOURCE_REPOSITORY", "value": REPO},
                    ]
                },
                "environment": {"platform": "linux/amd64"},
            },
            "materials": [{"uri": REPO, "digest": {"sha1": COMMIT}}],
        },
    }


def statement_v1():
    return {
        "_type": "https://in-toto.io/Statement/v1",
        "predicateType": "https://slsa.dev/provenance/v1",
        "subject": [{"name": "app", "digest": {"sha256": DIGEST.split(":")[1]}}],
        "predicate": {
            "buildDefinition": {
                "buildType": "https://mobyproject.org/buildkit@v1",
                "externalParameters": {
                    "platform": "linux/amd64",
                    "config": {"build-args": {"SOURCE_COMMIT": COMMIT, "SOURCE_REPOSITORY": REPO}},
                },
            },
            "runDetails": {"builder": {"id": "https://github.com/moby/buildkit@v1"},
                           "metadata": {"invocationId": "build-1"}},
        },
    }


def sbom_statement():
    return {
        "_type": "https://in-toto.io/Statement/v0.1",
        "predicateType": "https://spdx.dev/Document",
        "subject": [{"name": "app", "digest": {"sha256": DIGEST.split(":")[1]}}],
        "predicate": {"spdxVersion": "SPDX-2.3", "documentNamespace": "https://example/spdx/doc",
                      "packages": [{"name": "onboardd", "versionInfo": "0.1.0"}]},
    }


def run(stmt, kind="provenance", digest=DIGEST, commit=COMMIT, repo=REPO, platform="linux/amd64"):
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
        json.dump(stmt, fh)
        path = fh.name
    try:
        cmd = [sys.executable, VERIFIER, "--statement-file", path, "--statement-kind", kind,
               "--digest", digest, "--expect-commit", commit, "--expect-repository", repo,
               "--expect-platform", platform]
        proc = subprocess.run(cmd, capture_output=True, text=True)
        return proc.returncode, proc.stdout
    finally:
        os.unlink(path)


def expect_pass(name, stmt, **kw):
    rc, out = run(stmt, **kw)
    if rc == 0:
        ok(name)
    else:
        bad(name, out.strip()[:300])


def expect_fail(name, stmt, **kw):
    rc, out = run(stmt, **kw)
    if rc != 0:
        ok(name)
    else:
        bad(name, "verifier accepted a statement it must reject")


valid = statement_v02()
expect_pass("valid slsa v0.2 statement verifies", valid)
expect_pass("valid slsa v1 statement verifies", statement_v1())
expect_pass("valid spdx sbom verifies", sbom_statement(), kind="sbom")

forged_commit = copy.deepcopy(valid)
forged_commit["predicate"]["invocation"]["parameters"]["build-args"][0]["value"] = "b" * 40
expect_fail("wrong source commit is rejected", forged_commit)

forged_repo = copy.deepcopy(valid)
forged_repo["predicate"]["invocation"]["parameters"]["build-args"][1]["value"] = "https://github.com/attacker/fork"
expect_fail("wrong source repository is rejected", forged_repo)

forged_subject = copy.deepcopy(valid)
forged_subject["subject"][0]["digest"]["sha256"] = "c" * 64
expect_fail("wrong subject digest is rejected", forged_subject)

two_subjects = copy.deepcopy(valid)
two_subjects["subject"].append({"name": "other", "digest": {"sha256": DIGEST.split(":")[1]}})
expect_fail("multiple subjects are rejected", two_subjects)

wrong_type = copy.deepcopy(valid)
wrong_type["predicateType"] = "https://example.com/not-slsa"
expect_fail("unknown predicate type is rejected", wrong_type)

no_builder = copy.deepcopy(valid)
no_builder["predicate"]["builder"] = {}
expect_fail("missing builder identity is rejected", no_builder)

no_invocation = copy.deepcopy(valid)
no_invocation["predicate"]["invocation"]["id"] = ""
expect_fail("missing invocation id is rejected", no_invocation)

unknown_build_type = copy.deepcopy(valid)
unknown_build_type["predicate"]["buildType"] = "https://example.com/unknown-builder"
expect_fail("unknown build type is rejected", unknown_build_type)

expect_fail("wrong platform expectation is rejected", valid, platform="linux/arm64")
expect_fail("wrong digest expectation is rejected", valid, digest="sha256:" + "d" * 64)

sbom_wrong_subject = sbom_statement()
sbom_wrong_subject["subject"][0]["digest"]["sha256"] = "e" * 64
expect_fail("sbom subject mismatch is rejected", sbom_wrong_subject, kind="sbom")

sbom_empty = sbom_statement()
sbom_empty["predicate"]["packages"] = []
expect_fail("empty sbom is rejected", sbom_empty, kind="sbom")

print("SUMMARY pass=%d fail=%d" % (PASS, FAIL))
sys.exit(0 if FAIL == 0 else 1)
