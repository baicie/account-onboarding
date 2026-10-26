import hashlib, io, json, os, subprocess, sys, tarfile, tempfile
W=tempfile.mkdtemp(); c=os.path.join(W,'c'); os.makedirs(os.path.join(c,'supply'))
open(os.path.join(c,'commit.txt'),'w').write('a'*40+'\n')
config=b'{"architecture":"amd64"}'; iid='sha256:'+hashlib.sha256(config).hexdigest()
with tarfile.open(os.path.join(c,'image.tar'),'w') as tf:
    for n,d in [('manifest.json',json.dumps([{"Config":"config.json"}]).encode()),('config.json',config)]:
        ti=tarfile.TarInfo(n); ti.size=len(d); tf.addfile(ti,io.BytesIO(d))
open(os.path.join(c,'image-id.txt'),'w').write(iid+'\n'); open(os.path.join(c,'supply','sbom.cdx.json'),'w').write('{}\n')
def sha(p): return hashlib.sha256(open(p,'rb').read()).hexdigest()
pf={'status':'PASS','release_authorized':False,'commit':'a'*40,'gate_source':'github-actions-api','candidate_image_tar_sha256':sha(os.path.join(c,'image.tar')),'candidate_image_id':iid,'sbom_sha256':sha(os.path.join(c,'supply','sbom.cdx.json')),'ci_runs':{'ci':{'verified':True,'event':'push','commit':'a'*40}}}
p=os.path.join(W,'pf.json'); json.dump(pf,open(p,'w'))
def run(): return subprocess.run([sys.executable,'scripts/verify_trusted_artifact.py','--preflight',p,'--candidate',c,'--commit','a'*40,'--run-id','1','--run-attempt','1'],capture_output=True,text=True)
r=run(); assert r.returncode==0,r.stderr
for mutate in ('commit','gate_source','status'):
    q=json.loads(json.dumps(pf)); q[mutate]='bad' if mutate!='status' else 'FAIL'; json.dump(q,open(p,'w'))
    assert run().returncode!=0,mutate
json.dump(pf,open(p,'w')); open(os.path.join(c,'image-id.txt'),'w').write('sha256:'+'0'*64+'\n')
assert run().returncode!=0
print('PUBLISH_REVALIDATION_TEST_PASS')
