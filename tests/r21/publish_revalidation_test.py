import hashlib, io, json, os, subprocess, sys, tarfile, tempfile
W=tempfile.mkdtemp(); c=os.path.join(W,'c'); os.makedirs(os.path.join(c,'supply'))
commit='a'*40
open(os.path.join(c,'commit.txt'),'w').write(commit+'\n')
config=b'{"architecture":"amd64"}'; iid='sha256:'+hashlib.sha256(config).hexdigest()
with tarfile.open(os.path.join(c,'image.tar'),'w') as tf:
    for n,d in [('manifest.json',json.dumps([{"Config":"config.json"}]).encode()),('config.json',config)]:
        ti=tarfile.TarInfo(n); ti.size=len(d); tf.addfile(ti,io.BytesIO(d))
open(os.path.join(c,'image-id.txt'),'w').write(iid+'\n'); open(os.path.join(c,'supply','sbom.cdx.json'),'w').write('{}\n')
def sha(p): return hashlib.sha256(open(p,'rb').read()).hexdigest()
pf={'status':'PASS','release_authorized':False,'commit':commit,'gate_source':'github-actions-api',
 'candidate_image_tar_sha256':sha(os.path.join(c,'image.tar')),'candidate_image_id':iid,
 'sbom_sha256':sha(os.path.join(c,'supply','sbom.cdx.json')),
 'generation_run_id':123,'generation_run_attempt':1,
 'gates':{g:'PASS' for g in ['go_unit_race','provider_contract','sandbox_infra','application_e2e','browser_admin_core','backup_restore','supply_chain','same_artifact','arm64_service','staging_acceptance']},
 'ci_runs':{n:{'verified':True,'event':'push' if n!='same-artifact' else None,'commit':commit} for n in ['ci','r12-integration','r16-sandbox-e2e','r17-browser-e2e','supply-chain','staging-acceptance','same-artifact']}}
p=os.path.join(W,'pf.json'); json.dump(pf,open(p,'w'))
open(os.path.join(c,'candidate-provenance.json'),'w').write(json.dumps({'commit':commit,'image_id':iid,'image_tar_sha256':sha(os.path.join(c,'image.tar')),'sbom_sha256':sha(os.path.join(c,'supply','sbom.cdx.json'))}))
def run(): return subprocess.run([sys.executable,'scripts/verify_trusted_artifact.py','--preflight',p,'--candidate',c,'--commit',commit,'--run-id','123','--run-attempt','1'],capture_output=True,text=True)
assert run().returncode==0,run().stderr
for mut in ('empty-runs','missing-gate','wrong-image-id','wrong-run-id'):
    q=json.loads(json.dumps(pf)); q['gates']=dict(pf['gates']); q['ci_runs']=dict(pf['ci_runs'])
    if mut=='empty-runs': q['ci_runs']={}
    if mut=='missing-gate': q['gates'].pop('browser_admin_core')
    if mut=='wrong-image-id': q['candidate_image_id']='sha256:'+'f'*64
    if mut=='wrong-run-id': q['generation_run_id']=999
    json.dump(q,open(p,'w')); assert run().returncode!=0,mut
json.dump(pf,open(p,'w'))
open(os.path.join(c,'image-id.txt'),'w').write('sha256:'+'f'*64+'\n'); assert run().returncode!=0
print('PUBLISH_REVALIDATION_TEST_PASS')
