# TLS ingress design (production, not yet deployed)

The application serves **plain HTTP on 127.0.0.1:8788** and trusts proxy headers
only when `ONBOARD_TRUST_PROXY_HEADERS=1`. Production must terminate TLS in a
reviewed reverse proxy on a private network; the app port must never be
published beyond loopback.

## Topology

```text
client --HTTPS(443, TLS1.2+/h2)--> reverse proxy --HTTP over loopback--> onboardd 127.0.0.1:8788
                                      | sets X-Forwarded-Proto: https, X-Forwarded-For, Host
                                      | strips client-supplied Forwarded / X-Forwarded-* / X-Real-IP
```

Requirements:

1. **TLS 1.2+ only**; disable TLS 1.0/1.1 and weak ciphers; enable HTTP/2.
2. Certificate via ACME (Let's Encrypt) with auto-renew; monitor expiry; keep a
   reload hook. No wildcard assumptions unless DNS-01 is used deliberately.
3. **HSTS** after the domain is confirmed HTTPS-only: `max-age=31536000` (start
   with a short max-age, no `preload` until reviewed).
4. Proxy **overwrites** forwarding headers; client-supplied `X-Forwarded-*`
   must never pass through (the app trusts `X-Forwarded-Proto` when configured).
5. App binds loopback (`127.0.0.1:8788`) or a private Docker network only; compose
   publishes `127.0.0.1:8788`; the proxy reaches it over loopback/private net.
6. Request body limit at the proxy (e.g. 1 MB) plus app limits; timeouts:
   read header 10s, body 30s, proxy read 60s; no buffering of `/healthz`/`/readyz`.
7. Do **not** cache authenticated API/console responses; `Cache-Control: no-store`
   is set by the app for the console; add proxy rules if needed.
8. Access logs: redact `Authorization`, `Cookie`, query strings; never log bodies.
9. Restrict `/ui`, `/v1` by network policy if the console is internal only; the
   API remains bearer-token protected.
10. `/healthz` is liveness; `/readyz` requires a bearer token and is the real
    readiness signal (DB/schema/revocation table/WAL/master key). Use loopback
    checks for Docker/compose health rather than exposing `/readyz` publicly.

## Caddy reference

See [`../../deploy/examples/Caddyfile`](../../deploy/examples/Caddyfile). Key
directive: `reverse_proxy 127.0.0.1:8788` plus `header_up X-Forwarded-Proto https`
and `header_up -Forwarded` / `header_up -X-Forwarded-For` to strip client input
(Caddy already overwrites most forwarding headers; keep it explicit).

## nginx reference

See [`../../deploy/examples/nginx.conf`](../../deploy/examples/nginx.conf). Key
directives: `proxy_set_header Host $host; proxy_set_header X-Forwarded-Proto https;
proxy_set_header X-Forwarded-For $remote_addr;` and **do not** pass through
`proxy_set_header Forwarded ...` from clients.

## Verification before go-live

```bash
curl -fsSI https://onboarding.example.com/healthz           # 200, TLS1.2/1.3
openssl s_client -connect onboarding.example.com:443 -servername onboarding.example.com </dev/null 2>/dev/null | grep -E 'Protocol|Cipher'
curl -fsS -D- -o /dev/null https://onboarding.example.com/ui/login | grep -i -E 'strict-transport-security|x-frame-options|content-security-policy'
# Behind the proxy, login must set Secure; a direct request to the app port must not be reachable
curl -sS -o /dev/null -w '%{http_code}\n' -H 'X-Forwarded-Proto: https' http://127.0.0.1:8788/ui/login   # only on the host
```

The R5.1 test ingress (`tests/r51/ingress`) is a **test fixture**, not a
production proxy; it proves certificate/hostname/cookie/header behavior but not
ACME, HSTS policy or internet exposure.
