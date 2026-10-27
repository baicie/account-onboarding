#!/usr/bin/env python3
"""Verify BuildKit provenance and SBOM attestations for an OCI image.

This reads the registry directly: it walks the OCI index, finds the attestation
manifests BuildKit records next to the image manifest, fetches the in-toto
statement blobs and checks them against explicit expectations. It is not a
string search over a report.

Statuses it can justify:

    PROVENANCE_GENERATED   attestations exist in the registry
    PROVENANCE_VERIFIED    subject digest, predicate type, builder, invocation,
                           source repository/commit and platform all match

It never claims TRUSTED_BUILD_VERIFIED: that needs a signature whose issuer and
signing identity are verified against a trust root, which this tool does not do.

Usage:
  verify_buildkit_provenance.py --registry 127.0.0.1:5000 --repository app \
      --digest sha256:... --expect-commit <sha> --expect-repository <url> \
      [--expect-platform linux/amd64] [--out report.json]

Offline forgery check (used by the negative tests):
  verify_buildkit_provenance.py --statement-file statement.json \
      --expect-digest sha256:... --expect-commit <sha> --expect-repository <url>
"""

import argparse
import gzip
import hashlib
import json
import sys
import urllib.error
import urllib.request

SLSA_V02 = "https://slsa.dev/provenance/v0.2"
SLSA_V1 = "https://slsa.dev/provenance/v1"
SPDX = "https://spdx.dev/Document"
KNOWN_BUILD_TYPES = {
    "https://mobyproject.org/buildkit@v1",
    "https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-provenance.md",
    "https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-definitions.md",
    "https://slsa-framework.github.io/github-actions-buildtypes/workflow/v1",
}


def _http(url, accept=None):
    req = urllib.request.Request(url, headers={"Accept": accept} if accept else {})
    with urllib.request.urlopen(req, timeout=30) as resp:
        return resp.read(), dict(resp.headers)


def _json(raw):
    if raw[:2] == b"\x1f\x8b":
        raw = gzip.decompress(raw)
    return json.loads(raw.decode("utf-8"))


def get_manifest(base, repo, ref):
    accept = ", ".join([
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    ])
    raw, _ = _http("%s/v2/%s/manifests/%s" % (base, repo, ref), accept=accept)
    return _json(raw)


def get_blob(base, repo, digest):
    raw, _ = _http("%s/v2/%s/blobs/%s" % (base, repo, digest))
    return raw


def collect_statements(base, repo, image_digest):
    """Return (items, errors) for every attestation attached to the image."""
    items, errors = [], []
    index = get_manifest(base, repo, image_digest)
    manifests = index.get("manifests")
    if not manifests:
        # A single-platform image manifest: no attestation can be attached here.
        return items, ["image digest %s is not an index; no attestation manifests" % image_digest]
    # Attestation subjects reference the *platform* manifest digest, not the index.
    platform_digests = [
        e.get("digest", "") for e in manifests
        if (e.get("annotations") or {}).get("vnd.docker.reference.type") != "attestation-manifest"
    ]
    platform_digests = [d for d in platform_digests if d]
    for entry in manifests:
        annot = entry.get("annotations") or {}
        if annot.get("vnd.docker.reference.type") != "attestation-manifest":
            continue
        aim = get_manifest(base, repo, entry["digest"])
        platform = annot.get("vnd.docker.reference.digest", "")
        for layer in aim.get("layers") or []:
            blob = get_blob(base, repo, layer["digest"])
            try:
                stmt = _json(blob)
            except Exception as exc:  # noqa: BLE001 - report, do not crash
                errors.append("attestation blob %s is not JSON: %s" % (layer["digest"], exc))
                continue
            plat = entry.get("platform") or {}
            platform_label = ""
            if isinstance(plat, dict) and plat.get("os") and plat.get("architecture"):
                platform_label = "%s/%s" % (plat["os"], plat["architecture"])
            items.append({
                "kind": "provenance" if "provenance" in str(stmt.get("predicateType", "")) else "sbom",
                "statement": stmt,
                "attestation_manifest": entry["digest"],
                "predicate_digest": layer["digest"],
                "subject_hint": platform,
                "platform": platform_label,
                "platform_digests": platform_digests,
            })
    if not items:
        errors.append("no attestation statements found in the index")
    return items, errors


def _digest_hex(expected):
    return expected.split(":", 1)[-1]


def _build_args(predicate):
    """Collect build args from every shape BuildKit has used."""
    out = {}
    params = (predicate.get("invocation") or {}).get("parameters") or {}
    raw = params.get("build-args") or []
    if isinstance(raw, list):
        for item in raw:
            if isinstance(item, dict) and "key" in item:
                out[item["key"]] = item.get("value", "")
    elif isinstance(raw, dict):
        out.update(raw)
    build_def = predicate.get("buildDefinition") or {}
    external = build_def.get("externalParameters") or {}
    for holder in (external, external.get("config") or {}, external.get("request") or {},
                   build_def.get("resolvedDependencies") and {} or {}):
        if not isinstance(holder, dict):
            continue
        candidate = holder.get("build-args")
        if isinstance(candidate, dict):
            out.update(candidate)
        elif isinstance(candidate, list):
            for item in candidate:
                if isinstance(item, dict) and "key" in item:
                    out[item["key"]] = item.get("value", "")
    return out


def verify_provenance(stmt, expect, platform_hint=""):
    """Return a list of error strings; empty means verified."""
    errs = []
    subjects = stmt.get("subject") or []
    if len(subjects) != 1:
        errs.append("provenance statement must have exactly one subject, got %d" % len(subjects))
        return errs
    got = (subjects[0].get("digest") or {}).get("sha256", "")
    allowed = {_digest_hex(expect["digest"])}
    for d in expect.get("subject_digests") or []:
        allowed.add(_digest_hex(d))
    if got not in allowed:
        errs.append("subject digest mismatch: statement=%s expected one of %s"
                    % (got, sorted(allowed)))

    ptype = stmt.get("predicateType", "")
    if ptype not in (SLSA_V02, SLSA_V1):
        errs.append("unexpected predicateType %r" % ptype)
    predicate = stmt.get("predicate") or {}

    if ptype == SLSA_V1:
        build_def = predicate.get("buildDefinition") or {}
        build_type = build_def.get("buildType", "")
        external = build_def.get("externalParameters") or {}
        builder = (predicate.get("runDetails") or {}).get("builder") or {}
        args = {k: v for k, v in (external.get("build-args") or {}).items()} if isinstance(external.get("build-args"), dict) else _build_args(predicate)
        if not args:
            params = external.get("config") or {}
            args = params.get("build-args") if isinstance(params.get("build-args"), dict) else {}
        platform = external.get("platform") or ""
    else:
        build_type = predicate.get("buildType", "")
        builder = predicate.get("builder") or {}
        args = _build_args(predicate)
        platform = ((predicate.get("invocation") or {}).get("environment") or {}).get("platform", "")

    if build_type and build_type not in KNOWN_BUILD_TYPES:
        errs.append("unexpected buildType %r" % build_type)
    if not builder.get("id"):
        errs.append("builder.id is empty")
    invocation = (predicate.get("invocation") or {}).get("id") or \
        ((predicate.get("runDetails") or {}).get("metadata") or {}).get("invocationId", "")
    if not invocation:
        errs.append("invocation id is empty")

    if expect.get("commit"):
        got_commit = args.get("SOURCE_COMMIT") if isinstance(args, dict) else None
        if got_commit != expect["commit"]:
            errs.append("source commit mismatch: provenance=%r expected=%r" % (got_commit, expect["commit"]))
    if expect.get("repository"):
        got_repo = args.get("SOURCE_REPOSITORY") if isinstance(args, dict) else None
        if got_repo != expect["repository"]:
            errs.append("source repository mismatch: provenance=%r expected=%r" % (got_repo, expect["repository"]))
    if expect.get("platform"):
        observed = platform or platform_hint
        if observed != expect["platform"]:
            errs.append("platform mismatch: provenance=%r index=%r expected=%r"
                        % (platform, platform_hint, expect["platform"]))
    return errs


def verify_sbom(stmt, expect):
    errs = []
    subjects = stmt.get("subject") or []
    if len(subjects) != 1:
        errs.append("sbom statement must have exactly one subject, got %d" % len(subjects))
        return errs
    got = (subjects[0].get("digest") or {}).get("sha256", "")
    if got != _digest_hex(expect["digest"]):
        errs.append("sbom subject digest mismatch: statement=%s expected=%s" % (got, _digest_hex(expect["digest"])))
    if stmt.get("predicateType") != SPDX:
        errs.append("unexpected sbom predicateType %r" % stmt.get("predicateType"))
    predicate = stmt.get("predicate") or {}
    if not predicate.get("packages"):
        errs.append("sbom has no packages")
    if not predicate.get("documentNamespace"):
        errs.append("sbom has no documentNamespace")
    return errs


def sha256_of(obj):
    return hashlib.sha256(json.dumps(obj, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--registry", default="")
    ap.add_argument("--repository", default="")
    ap.add_argument("--digest", default="")
    ap.add_argument("--statement-file", default="")
    ap.add_argument("--statement-kind", default="provenance", choices=["provenance", "sbom"])
    ap.add_argument("--expect-commit", default="")
    ap.add_argument("--expect-repository", default="")
    ap.add_argument("--expect-platform", default="")
    ap.add_argument("--out", default="")
    ap.add_argument("--dump-statement", default="",
                    help="write the first provenance statement here (forgery tests)")
    args = ap.parse_args(argv)

    expect = {
        "digest": args.digest,
        "commit": args.expect_commit,
        "repository": args.expect_repository,
        "platform": args.expect_platform,
    }
    report = {
        "mode": "file" if args.statement_file else "registry",
        "expected": expect,
        "attestations": [],
        "errors": [],
        "status": "FAIL",
    }

    if args.statement_file:
        if not args.digest:
            ap.error("--digest is required with --statement-file")
        with open(args.statement_file, "r", encoding="utf-8") as fh:
            stmt = json.load(fh)
        errors = verify_provenance(stmt, expect) if args.statement_kind == "provenance" else verify_sbom(stmt, expect)
        report["attestations"].append({"kind": args.statement_kind, "statement_sha256": sha256_of(stmt), "errors": errors})
        report["errors"] = errors
        report["status"] = "PROVENANCE_VERIFIED" if not errors else "FAIL"
        if args.out:
            with open(args.out, "w", encoding="utf-8") as fh:
                json.dump(report, fh, indent=2)
        print(json.dumps(report, indent=2))
        return 0 if not errors else 1

    if not (args.registry and args.repository and args.digest):
        ap.error("--registry, --repository and --digest are required")
    base = "http://%s" % args.registry if not args.registry.startswith("http") else args.registry
    items, errors = collect_statements(base, args.repository, args.digest)
    report["errors"] = list(errors)

    provenance = [i for i in items if i["kind"] == "provenance"]
    sboms = [i for i in items if i["kind"] == "sbom"]
    if not provenance:
        report["errors"].append("no provenance attestation for %s" % args.digest)
    if not sboms:
        report["errors"].append("no sbom attestation for %s" % args.digest)

    dumped = False
    for item in items:
        stmt = item["statement"]
        if args.dump_statement and item["kind"] == "provenance" and not dumped:
            with open(args.dump_statement, "w", encoding="utf-8") as fh:
                json.dump(stmt, fh, indent=2)
            dumped = True
        expect_for_item = dict(expect)
        expect_for_item["subject_digests"] = item.get("platform_digests") or []
        errs = (verify_provenance(stmt, expect_for_item, item.get("platform", ""))
                if item["kind"] == "provenance" else verify_sbom(stmt, expect))
        entry = {
            "kind": item["kind"],
            "attestation_manifest": item["attestation_manifest"],
            "predicate_digest": item["predicate_digest"],
            "statement_sha256": sha256_of(stmt),
            "predicate_type": stmt.get("predicateType", ""),
            "subject": stmt.get("subject"),
            "builder": ((stmt.get("predicate") or {}).get("builder") or {}).get("id", ""),
            "build_type": ((stmt.get("predicate") or {}).get("buildType")
                           or ((stmt.get("predicate") or {}).get("buildDefinition") or {}).get("buildType", "")),
            "errors": errs,
        }
        report["attestations"].append(entry)
        report["errors"].extend("%s: %s" % (item["kind"], e) for e in errs)

    generated = bool(items)
    report["provenance_generated"] = generated
    report["errors"] = [e for e in report["errors"] if e]
    if not report["errors"]:
        report["status"] = "PROVENANCE_VERIFIED"
    elif generated:
        report["status"] = "PROVENANCE_GENERATED_UNVERIFIED"
    report["trusted_build_verified"] = False
    report["trusted_build_note"] = (
        "no signature/issuer verification was performed, so TRUSTED_BUILD_VERIFIED is not claimed")

    if args.out:
        with open(args.out, "w", encoding="utf-8") as fh:
            json.dump(report, fh, indent=2)
    print(json.dumps({k: report[k] for k in ("status", "provenance_generated", "errors", "trusted_build_verified")}, indent=2))
    return 0 if report["status"] == "PROVENANCE_VERIFIED" else 1


def run_main(argv=None):
    """Run main() but never leave the caller without evidence."""
    out = ""
    try:
        ns, _unknown = _peek_args(argv)
        out = ns
    except Exception:  # noqa: BLE001 - best effort only
        out = ""
    try:
        return main(argv)
    except SystemExit:
        raise
    except Exception as exc:  # noqa: BLE001 - report, never crash silently
        import traceback
        report = {"status": "FAIL", "errors": ["verifier crashed: %s" % exc],
                  "traceback": traceback.format_exc(), "trusted_build_verified": False}
        print(json.dumps(report, indent=2))
        if out:
            try:
                with open(out, "w", encoding="utf-8") as fh:
                    json.dump(report, fh, indent=2)
            except OSError:
                pass
        return 1


def _peek_args(argv):
    """Return the --out path from argv without consuming it."""
    args = list(sys.argv[1:] if argv is None else argv)
    for i, a in enumerate(args):
        if a == "--out" and i + 1 < len(args):
            return args[i + 1], True
        if a.startswith("--out="):
            return a.split("=", 1)[1], True
    return "", False


if __name__ == "__main__":
    sys.exit(run_main())
