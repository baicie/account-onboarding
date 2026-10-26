#!/usr/bin/env python3
"""Fail-closed release preflight for an existing candidate artifact.

This does not build, sign, publish, or approve a release. It binds a frozen
candidate to provenance/SBOM/tar bytes, verifies same-artifact evidence, and
records an explicit gate matrix. Missing or failed required gates fail closed.
"""
import argparse, hashlib, json, pathlib, re, sys, tarfile

REQUIRED_GATES = [
    "go_unit_race", "provider_contract", "sandbox_infra", "application_e2e",
    "browser_admin_core", "backup_restore", "supply_chain", "same_artifact",
    "arm64_service", "staging_acceptance",
]
NON_BLOCKING = {"authorized_live": "NOT_RUN", "ghcr_publish": "NOT_RUN",
                "github_release": "NOT_RUN", "production_deploy": "NOT_RUN"}

def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()

def fail(msg):
    print("RELEASE_PREFLIGHT_FAIL: " + msg, file=sys.stderr)
    return 1

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--candidate-dir", required=True)
    ap.add_argument("--same-artifact-evidence", required=True)
    ap.add_argument("--expected-commit", required=True)
    ap.add_argument("--repo-root", default=str(pathlib.Path(__file__).resolve().parent.parent))
    ap.add_argument("--gates", required=True, help="JSON gate->status or path")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    c = pathlib.Path(a.candidate_dir)
    required = ["commit.txt", "candidate-provenance.json", "image-id.txt", "image.tar", "supply/sbom.cdx.json"]
    for rel in required:
        if not (c / rel).is_file() or (c / rel).stat().st_size == 0:
            return fail(f"missing candidate artifact {rel}")
    commit = (c / "commit.txt").read_text().strip()
    if commit != a.expected_commit:
        return fail(f"candidate commit {commit} != expected {a.expected_commit}")
    # Docker image ID is the sha256 of the config blob inside image.tar; bind it
    # to bytes rather than trusting image-id.txt.
    try:
        with tarfile.open(c / "image.tar", "r") as tf:
            manifest = json.loads(tf.extractfile("manifest.json").read())
            config_name = manifest[0]["Config"]
            config_bytes = tf.extractfile(config_name).read()
    except Exception as exc:
        return fail(f"image.tar manifest/config unreadable: {exc}")
    actual_image_id = "sha256:" + hashlib.sha256(config_bytes).hexdigest()
    recorded_image_id = (c / "image-id.txt").read_text().strip()
    if actual_image_id != recorded_image_id:
        return fail(f"image-id.txt {recorded_image_id} != image.tar config {actual_image_id}")
    prov = json.loads((c / "candidate-provenance.json").read_text())
    if prov.get("commit") != commit:
        return fail("provenance commit mismatch")
    if prov.get("image_tar_sha256") != sha256(c / "image.tar"):
        return fail("image.tar hash mismatch")
    if prov.get("image_id") != actual_image_id:
        return fail("provenance image-id mismatch")
    if prov.get("sbom_sha256") != sha256(c / "supply/sbom.cdx.json"):
        return fail("SBOM hash mismatch")
    ev = pathlib.Path(a.same_artifact_evidence)
    text = "\n".join(p.read_text(errors="replace") for p in ev.rglob("*") if p.is_file() and p.stat().st_size < 5_000_000) if ev.exists() else ""
    if "SAME_CANDIDATE_ARTIFACT=" not in text:
        return fail("same-artifact evidence missing SAME_CANDIDATE_ARTIFACT")
    summary = re.search(r"SUMMARY pass=14 fail=0 digest=(sha256:[0-9a-f]{64}) image_id=(sha256:[0-9a-f]{64})", text)
    if not summary:
        return fail("same-artifact SUMMARY/digests missing or not 14/14")
    registry_digest, evidence_image_id = summary.group(1), summary.group(2)
    if evidence_image_id != actual_image_id:
        return fail("same-artifact image ID does not match candidate image.tar")
    att = re.search(r"ATTESTATION_VALID subject_digest=(sha256:[0-9a-f]{64}) commit=([0-9a-f]{40})", text)
    if not att or att.group(1) != registry_digest or att.group(2) != commit:
        return fail("attestation subject digest/commit mismatch")
    if "cosign verify passed" not in text or "attestation verified" not in text:
        return fail("same-artifact evidence missing cosign verify/attestation proof")
    gates_raw = pathlib.Path(a.gates).read_text() if pathlib.Path(a.gates).exists() else a.gates
    report = json.loads(gates_raw)
    if report.get("commit") != commit:
        return fail("gate report commit does not match candidate")
    gates = report.get("gates", {})
    blockers = []
    for name in REQUIRED_GATES:
        if gates.get(name) != "PASS":
            blockers.append({"gate": name, "status": gates.get(name, "MISSING")})
    for name, allowed in NON_BLOCKING.items():
        if gates.get(name) not in (None, allowed):
            blockers.append({"gate": name, "status": gates.get(name), "must_be": allowed})
    result = {
        "schema": 1,
        "status": "PASS" if not blockers else "FAIL",
        "commit": commit,
        "candidate_image_tar_sha256": sha256(c / "image.tar"),
        "candidate_image_id": actual_image_id,
        "sbom_sha256": prov.get("sbom_sha256"),
        "provenance_commit": prov.get("commit"),
        "registry_manifest_digest": registry_digest,
        "gate_report_commit": report.get("commit"),
        "ci_runs": report.get("runs", {}),
        "gates": gates,
        "blockers": blockers,
        "release_authorized": False,
        "note": "PREFLIGHT ONLY. Release requires a separate operator approval; this tool never publishes, tags, merges, or deploys.",
    }
    pathlib.Path(a.out).write_text(json.dumps(result, indent=2) + "\n")
    if blockers:
        print("RELEASE_PREFLIGHT_FAIL: " + ", ".join(b["gate"] for b in blockers), file=sys.stderr)
        return 1
    print("RELEASE_PREFLIGHT_PASS")
    return 0

if __name__ == "__main__":
    sys.exit(main())
