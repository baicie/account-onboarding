#!/usr/bin/env python3
"""Fail-closed release preflight for an existing candidate artifact.

This does not build, sign, publish, or approve a release. It binds a frozen
candidate to provenance/SBOM/tar bytes, verifies same-artifact evidence, and
records an explicit gate matrix. Missing or failed required gates fail closed.
"""
import argparse, hashlib, json, pathlib, re, sys

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
    prov = json.loads((c / "candidate-provenance.json").read_text())
    if prov.get("commit") != commit:
        return fail("provenance commit mismatch")
    if prov.get("image_tar_sha256") != sha256(c / "image.tar"):
        return fail("image.tar hash mismatch")
    if prov.get("image_id") != (c / "image-id.txt").read_text().strip():
        return fail("image-id mismatch")
    if prov.get("sbom_sha256") != sha256(c / "supply/sbom.cdx.json"):
        return fail("SBOM hash mismatch")
    ev = pathlib.Path(a.same_artifact_evidence)
    text = "\n".join(p.read_text(errors="replace") for p in ev.rglob("*") if p.is_file() and p.stat().st_size < 5_000_000) if ev.exists() else ""
    if "SAME_CANDIDATE_ARTIFACT=" not in text:
        return fail("same-artifact evidence missing SAME_CANDIDATE_ARTIFACT")
    if not re.search(r"SUMMARY pass=14 fail=0", text):
        return fail("same-artifact evidence missing 14/14 pass")
    if "cosign" not in text.lower() or "attest" not in text.lower():
        return fail("same-artifact evidence missing sign/attest proof")
    gates_raw = pathlib.Path(a.gates).read_text() if pathlib.Path(a.gates).exists() else a.gates
    gates = json.loads(gates_raw)
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
        "candidate_image_id": prov.get("image_id"),
        "sbom_sha256": prov.get("sbom_sha256"),
        "provenance_commit": prov.get("commit"),
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
