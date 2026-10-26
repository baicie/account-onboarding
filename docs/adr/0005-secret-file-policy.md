# ADR 0005 — Single strict secret-file policy

Status: accepted (R14)

All configurable secrets (`*_FILE` admin/readonly tokens, master key, Cloudflare
admin/site passwords, Keycloak admin password/admin client/client secret, GitHub
token, WorkBuddy token, OIDC client secret) go through one entry point:

- must be a regular non-symlink file;
- mode must have no group/other bits (0400/0600 are accepted; 0450 is rejected (no group bits));
- size 1..64 KiB;
- the opened file descriptor is re-statted and compared with the pre-open inode
  (`os.SameFile`), so a path swap between check and read is rejected;
- content is read from the verified descriptor, never from the path;
- empty/oversized files are rejected;
- errors name the setting and path but never the secret value.

## Docker / Kubernetes projections

Docker secrets are commonly mounted 0444 and Kubernetes projected secrets are
often symlinks/0644. The default policy **rejects** these rather than silently
weakening the rule. Operators must copy the value to a regular owner-only file
(for example an init container / `install -m 0400` into a tmpfs) or configure a
Kubernetes `defaultMode` that yields 0400 and avoid the projected symlink path.
This is deliberately an explicit adaptation, not a permissive default.

ACLs are not evaluated by Go's portable mode check; deployments using POSIX ACLs
must ensure the effective access is owner-only and should prefer the copy
mechanism. Windows ACL semantics are out of scope; the supported deployment
targets are Linux containers.
