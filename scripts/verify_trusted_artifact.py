#!/usr/bin/env python3
"""Re-verify the trusted preflight artifact immediately before publish.

This is intentionally independent of job dependency truth: publish downloads the
artifact and re-checks exact commit, gate records, candidate bytes and status.
"""
import argparse, hashlib, json, pathlib, sys, tarfile

def sha(p):
    h = hashlib.sha256()
    with open(p, 'rb') as f:
        for b in iter(lambda: f.read(1 << 20), b''):
            h.update(b)
    return h.hexdigest()

def fail(msg):
    print('PUBLISH_REVALIDATION_FAIL: ' + msg, file=sys.stderr); return 1

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--preflight', required=True)
    ap.add_argument('--candidate', required=True)
    ap.add_argument('--commit', required=True)
    ap.add_argument('--run-id', required=True)
    ap.add_argument('--run-attempt', required=True)
    a = ap.parse_args()
    try:
        pf = json.load(open(a.preflight))
    except Exception as ex:
        return fail(f'preflight unreadable: {ex}')
    if pf.get('status') != 'PASS' or pf.get('release_authorized') is not False:
        return fail('preflight not PASS or authorization field changed')
    if pf.get('commit') != a.commit:
        return fail('preflight commit mismatch')
    if pf.get('gate_source') != 'github-actions-api':
        return fail('preflight not from trusted GitHub API source')
    runs = pf.get('ci_runs') or {}
    for name, rec in runs.items():
        if not rec.get('verified'):
            return fail(f'{name} gate record not verified')
        if name != 'same-artifact' and rec.get('event') != 'push':
            return fail(f'{name} gate record event is not push')
        if name != 'same-artifact' and rec.get('commit') != a.commit:
            return fail(f'{name} gate record commit mismatch')
    c = pathlib.Path(a.candidate)
    if sha(c / 'image.tar') != pf.get('candidate_image_tar_sha256'):
        return fail('candidate image.tar does not match trusted preflight')
    if (c / 'image-id.txt').read_text().strip() != pf.get('candidate_image_id'):
        return fail('candidate image-id does not match trusted preflight')
    if sha(c / 'supply' / 'sbom.cdx.json') != pf.get('sbom_sha256'):
        return fail('candidate SBOM does not match trusted preflight')
    # Print no secrets: only identity hashes.
    print('PUBLISH_REVALIDATION_PASS')
    print('commit=' + a.commit)
    print('run_id=' + str(a.run_id) + ' run_attempt=' + str(a.run_attempt))
    print('candidate_image_tar_sha256=' + pf['candidate_image_tar_sha256'])
    return 0

if __name__ == '__main__':
    sys.exit(main())
