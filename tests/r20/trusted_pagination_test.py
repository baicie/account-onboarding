import json, os, subprocess, sys, threading
from http.server import BaseHTTPRequestHandler, HTTPServer
repo='baicie/account-onboarding'; sha='a'*40
paths={'ci':'.github/workflows/ci.yml','r12-integration':'.github/workflows/r11-integration.yml','r16-sandbox-e2e':'.github/workflows/r16-sandbox-e2e.yml','r17-browser-e2e':'.github/workflows/r17-browser-e2e.yml','supply-chain':'.github/workflows/supply-chain.yml','staging-acceptance':'.github/workflows/staging-acceptance.yml'}
def run(i,w,attempt=1): return {"id":i,"name":w,"path":paths[w],"head_sha":sha,"status":"completed","conclusion":"success","run_attempt":attempt,"event":"push","head_branch":"feat/x","created_at":"2026-01-01T00:00:00Z","repository":{"full_name":repo}}
def serve(mode):
    class H(BaseHTTPRequestHandler):
        def log_message(self,*a): pass
        def do_GET(self):
            if mode=='loop':
                body=json.dumps({"workflow_runs":[]}).encode(); self.send_response(200); self.send_header('Link', f'<http://127.0.0.1:{self.server.server_port}/repos/x/actions/runs?page=1>; rel="next"'); self.end_headers(); self.wfile.write(body); return
            if 'page=2' in self.path:
                if mode=='page2fail': self.send_response(500); self.end_headers(); return
                body=json.dumps({"workflow_runs":[]}).encode(); self.send_response(200); self.end_headers(); self.wfile.write(body); return
            runs=[]
            for i,w in enumerate(paths): runs += [run(1000+i*2,w),run(1000+i*2+1,w)]
            body=json.dumps({"workflow_runs":runs}).encode(); self.send_response(200)
            self.send_header('Link', f'<http://127.0.0.1:{self.server.server_port}/repos/x/actions/runs?page=2>; rel="next"'); self.end_headers(); self.wfile.write(body)
    s=HTTPServer(('127.0.0.1',0),H); threading.Thread(target=s.serve_forever,daemon=True).start(); return s,f'http://127.0.0.1:{s.server_port}'
def run_script(api):
    out='/tmp/r20-trusted-pg.json'; env=dict(os.environ,TEST_TOKEN='t')
    p=subprocess.run([sys.executable,'scripts/trusted_ci_gates.py','--repo',repo,'--commit',sha,'--out',out,'--api-base',api,'--token-env','TEST_TOKEN'],capture_output=True,text=True,env=env)
    return p,out
for mode in ('ok','page2fail','loop'):
    s,api=serve(mode); p,_=run_script(api); s.shutdown()
    if mode=='ok': assert p.returncode==0,p.stderr
    else: assert p.returncode!=0,mode
print('TRUSTED_PAGINATION_TEST_PASS')
