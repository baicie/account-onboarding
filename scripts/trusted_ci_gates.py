#!/usr/bin/env python3
"""Build release gate records from trusted GitHub Actions API metadata.

The caller supplies only repository + exact commit + token. No gate PASS can be
self-asserted: every record is fetched from the Actions API and must match
repository, full head SHA, workflow, conclusion and run attempt.
"""
import argparse, json, os, sys, urllib.request, urllib.error

# Protected workflow identity: name -> (exact workflow file path, gate names).
DEFAULT_WORKFLOWS = {
    "ci": (".github/workflows/ci.yml", ["go_unit_race", "provider_contract"]),
    "r12-integration": (".github/workflows/r11-integration.yml", ["backup_restore", "arm64_service"]),
    "r16-sandbox-e2e": (".github/workflows/r16-sandbox-e2e.yml", ["sandbox_infra", "application_e2e"]),
    "r17-browser-e2e": (".github/workflows/r17-browser-e2e.yml", ["browser_admin_core"]),
    "supply-chain": (".github/workflows/supply-chain.yml", ["supply_chain"]),
    "staging-acceptance": (".github/workflows/staging-acceptance.yml", ["staging_acceptance"]),
}
TRUSTED_EVENTS = {"push", "workflow_dispatch"}

def api_get(url, token):
    req = urllib.request.Request(url, headers={
        "Accept": "application/vnd.github+json",
        "Authorization": "Bearer " + token,
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "account-onboarding-release-preflight",
    })
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return json.loads(r.read())
    except Exception as ex:
        raise RuntimeError(f"GitHub API failure: {ex}")

def build_report(repo, commit, token, api_base="https://api.github.com", workflows=None):
    workflows = workflows or DEFAULT_WORKFLOWS
    gates, runs, blockers = {}, {}, []
    data = api_get(f"{api_base}/repos/{repo}/actions/runs?head_sha={commit}&per_page=100", token)
    all_runs = data.get("workflow_runs", [])
    for wf, spec in workflows.items():
        path, gate_names = spec
        candidates = [r for r in all_runs
                      if r.get("head_sha") == commit and r.get("name") == wf
                      and r.get("path") == path and r.get("event") in TRUSTED_EVENTS]
        if not candidates:
            blockers.append({"gate": gate_names[0], "reason": f"no trusted push/workflow_dispatch {wf} run at {path} for exact SHA"})
            for g in gate_names: gates[g] = "MISSING"
            continue
        # A same-name workflow at a different path is never trusted, even if it
        # reports success; reject ambiguity instead of selecting by recency.
        other_paths = {r.get("path") for r in all_runs
                       if r.get("name") == wf and r.get("path") != path
                       and r.get("head_sha") == commit}
        if other_paths:
            blockers.append({"gate": gate_names[0], "reason": "same-name workflow at untrusted path"})
            for g in gate_names: gates[g] = "FAIL"
            continue
        # deterministic: latest created run, then highest attempt
        run = sorted(candidates, key=lambda r: (r.get("created_at", ""), r.get("run_attempt", 0)))[-1]
        if run.get("repository", {}).get("full_name") != repo:
            blockers.append({"gate": gate_names[0], "reason": "repository mismatch"})
        if run.get("conclusion") != "success":
            blockers.append({"gate": gate_names[0], "reason": f"workflow conclusion {run.get('conclusion')}"})
        if run.get("status") != "completed":
            blockers.append({"gate": gate_names[0], "reason": f"workflow status {run.get('status')}"})
        for g in gate_names:
            gates[g] = "PASS" if not any(b["gate"] == gate_names[0] for b in blockers) else "FAIL"
        runs[wf] = {"repository": repo, "commit": commit, "workflow": wf,
                    "path": run.get("path"), "workflowId": run.get("workflow_id"),
                    "runId": run.get("id"), "runAttempt": run.get("run_attempt"),
                    "event": run.get("event"), "ref": run.get("head_branch"),
                    "conclusion": run.get("conclusion"), "status": run.get("status"),
                    "verified": (run.get("head_sha") == commit and run.get("conclusion") == "success"
                                 and run.get("path") == path and run.get("event") in TRUSTED_EVENTS)}
    return {"schema": 2, "commit": commit, "repository": repo, "gates": gates,
            "runs": runs, "blockers": blockers, "trusted_source": "github-actions-api"}

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True)
    ap.add_argument("--commit", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--api-base", default=os.environ.get("GITHUB_API_URL", "https://api.github.com"))
    ap.add_argument("--token-env", default="GITHUB_TOKEN")
    a = ap.parse_args()
    token = os.environ.get(a.token_env, "")
    if not token:
        print("TRUSTED_GATES_FAIL: missing token", file=sys.stderr); return 1
    try:
        report = build_report(a.repo, a.commit, token, a.api_base)
    except RuntimeError as ex:
        print("TRUSTED_GATES_FAIL: " + str(ex), file=sys.stderr); return 1
    open(a.out, "w").write(json.dumps(report, indent=2) + "\n")
    if report["blockers"]:
        print("TRUSTED_GATES_FAIL: " + json.dumps(report["blockers"]), file=sys.stderr); return 1
    print("TRUSTED_GATES_PASS")
    return 0

if __name__ == "__main__":
    sys.exit(main())
