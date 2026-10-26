import hashlib, io, json, os, subprocess, sys, tarfile, tempfile, threading
from http.server import BaseHTTPRequestHandler, HTTPServer
paths = {'ci': '.github/workflows/ci.yml', 'r12-integration': '.github/workflows/r11-integration.yml', 'r16-sandbox-e2e': '.github/workflows/r16-sandbox-e2e.yml', 'r17-browser-e2e': '.github/workflows/r17-browser-e2e.yml', 'supply-chain': '.github/workflows/supply-chain.yml', 'staging-acceptance': '.github/workflows/staging-acceptance.yml'}
repo='baicie/account-onboarding'; sha='a'*40
W=tempfile.mkdtemp(); c=os.path.join(W,'c'); ev=os.path.join(W,'ev'); os.makedirs(os.path.join(c,'supply')); os.makedirs(ev)
open(os.path.join(c,'commit.txt'),'w').write(sha+'\n')
config=json.dumps({"architecture":"amd64"}).encode(); iid='sha256:'+hashlib.sha256(config).hexdigest()
with tarfile.open(os.path.join(c,'image.tar'),'w') as tf:
    for n,d in [('manifest.json',json.dumps([{"Config":"config.json"}]).encode()),('config.json',config)]:
        ti=tarfile.TarInfo(n); ti.size=len(d); tf.addfile(ti,io.BytesIO(d))
open(os.path.join(c,'image-id.txt'),'w').write(iid+'\n'); open(os.path.join(c,'supply','sbom.cdx.json'),'w').write('{}\n')
sha256=lambda p: hashlib.sha256(open(p,'rb').read()).hexdigest()
prov={"commit":sha,"image_id":iid,"image_tar_sha256":sha256(os.path.join(c,'image.tar')),"sbom_sha256":sha256(os.path.join(c,'supply','sbom.cdx.json'))}
open(os.path.join(c,'candidate-provenance.json'),'w').write(json.dumps(prov))
digest='sha256:'+'1'*64
open(os.path.join(ev,'dry-run.log'),'w').write(f'SAME_CANDIDATE_ARTIFACT=x\nSUMMARY pass=14 fail=0 digest={digest} image_id={iid}\ncosign verify passed\nattestation verified\nATTESTATION_VALID subject_digest={digest} commit={sha}\n')
wfs=['ci','r12-integration','r16-sandbox-e2e','r17-browser-e2e','supply-chain','staging-acceptance']
def runs(conclusion='success'):
    return {w:[{"id":i+1,"name":w,"head_sha":sha,"status":"completed","conclusion":conclusion,"run_attempt":1,"event":"push","head_branch":"feat/x","path":paths[w],"workflow_id":i+1,"created_at":"2026-01-01T00:00:00Z","repository":{"full_name":repo}}] for i,w in enumerate(wfs)}
def serve(d):
    class H(BaseHTTPRequestHandler):
        def log_message(self,*a): pass
        def do_GET(self):
            body=json.dumps({"workflow_runs":[r for rs in d.values() for r in rs]}).encode(); self.send_response(200); self.end_headers(); self.wfile.write(body)
    s=HTTPServer(('127.0.0.1',0),H); threading.Thread(target=s.serve_forever,daemon=True).start(); return s,f'http://127.0.0.1:{s.server_port}'
def run(api):
    env=dict(os.environ,TEST_TOKEN='t')
    return subprocess.run([sys.executable,'scripts/release_preflight.py','--candidate-dir',c,'--same-artifact-evidence',ev,
        '--expected-commit',sha,'--trusted-gates','--api-base',api,'--token-env','TEST_TOKEN','--out',os.path.join(W,'pf.json')],capture_output=True,text=True,env=env)
s,api=serve(runs()); p=run(api); s.shutdown(); assert p.returncode==0,p.stderr
assert json.load(open(os.path.join(W,'pf.json')))['gate_source']=='github-actions-api'
s,api=serve(runs('failure')); p=run(api); s.shutdown(); assert p.returncode!=0
s,api=serve({}); p=run(api); s.shutdown(); assert p.returncode!=0
print('PREFLIGHT_TRUSTED_TEST_PASS')
