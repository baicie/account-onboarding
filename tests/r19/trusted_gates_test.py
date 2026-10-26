import json, os, subprocess, sys, tempfile, threading
from http.server import BaseHTTPRequestHandler, HTTPServer

def handler_factory(runs, fail=False):
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a): pass
        def do_GET(self):
            if fail:
                self.send_response(500); self.end_headers(); return
            data = {"workflow_runs": [r for rs in runs.values() for r in rs]}
            body = json.dumps(data).encode(); self.send_response(200)
            self.send_header('Content-Type','application/json'); self.end_headers(); self.wfile.write(body)
    return H

def run_server(runs, fail=False):
    srv = HTTPServer(('127.0.0.1', 0), handler_factory(runs, fail))
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, f"http://127.0.0.1:{srv.server_port}"

def run_script(api, commit, token='t'):
    out = tempfile.mktemp()
    env = dict(os.environ, TEST_TOKEN=token)
    p = subprocess.run([sys.executable, 'scripts/trusted_ci_gates.py', '--repo', 'baicie/account-onboarding',
                        '--commit', commit, '--out', out, '--api-base', api, '--token-env', 'TEST_TOKEN'],
                       capture_output=True, text=True, env=env)
    return p, out

paths = {'ci': '.github/workflows/ci.yml', 'r12-integration': '.github/workflows/r11-integration.yml', 'r16-sandbox-e2e': '.github/workflows/r16-sandbox-e2e.yml', 'r17-browser-e2e': '.github/workflows/r17-browser-e2e.yml', 'supply-chain': '.github/workflows/supply-chain.yml', 'staging-acceptance': '.github/workflows/staging-acceptance.yml'}
repo = 'baicie/account-onboarding'
sha = 'a'*40
wfs = ['ci','r12-integration','r16-sandbox-e2e','r17-browser-e2e','supply-chain','staging-acceptance']
runs = {w: [{"id": i+1, "name": w, "head_sha": sha, "status": "completed", "conclusion": "success",
             "run_attempt": 1, "event": "push", "head_branch": "feat/x", "path": paths[w], "workflow_id": 1, "created_at": "2026-01-01T00:00:00Z",
             "repository": {"full_name": repo}}] for i, w in enumerate(wfs)}
srv, api = run_server(runs)
p, out = run_script(api, sha)
srv.shutdown()
assert p.returncode == 0, p.stderr
d = json.load(open(out)); assert d['gates']['go_unit_race'] == 'PASS' and all(v['verified'] for v in d['runs'].values())

# wrong SHA
srv, api = run_server(runs)
p, _ = run_script(api, 'b'*40); srv.shutdown(); assert p.returncode != 0
# failed run
bad = json.loads(json.dumps(runs)); bad['ci'][0]['conclusion'] = 'failure'
srv, api = run_server(bad); p, _ = run_script(api, sha); srv.shutdown(); assert p.returncode != 0
# API failure
srv, api = run_server(runs, fail=True); p, _ = run_script(api, sha); srv.shutdown(); assert p.returncode != 0
# wrong repository
bad = json.loads(json.dumps(runs)); bad['ci'][0]['repository']['full_name'] = 'evil/repo'
srv, api = run_server(bad); p, _ = run_script(api, sha); srv.shutdown(); assert p.returncode != 0
# PR-event run is not trusted even if successful.
pr = json.loads(json.dumps(runs)); pr['ci'][0]['event'] = 'pull_request'
srv, api = run_server(pr); p, _ = run_script(api, sha); srv.shutdown(); assert p.returncode != 0
# Same-name workflow at another path fails closed.
wrong = json.loads(json.dumps(runs)); wrong['ci'][0]['path'] = '.github/workflows/evil-ci.yml'
srv, api = run_server(wrong); p, _ = run_script(api, sha); srv.shutdown(); assert p.returncode != 0
print('TRUSTED_GATES_TEST_PASS')
