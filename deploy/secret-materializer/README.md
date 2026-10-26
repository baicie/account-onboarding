# Secret materialization

Docker/Kubernetes often project secrets as 0444 files or symlinks, while the
service accepts only regular **0400/0600** files. Do not change the service
policy. Use `scripts/materialize_secrets.sh` in an init container/startup hook:

1. mount the read-only projection under `/run/secrets-source`;
2. create a private tmpfs target (0700);
3. copy atomically, chown to the runtime UID/GID, chmod 0400;
4. point the `*_FILE` settings at the materialized 0400 files.

The materializer rejects symlinks and writable source files; it never logs
secret contents. Source read-only 0444 is accepted only because the destination
is private and owner-only. POSIX ACLs are not inspected; use this copy path
when ACLs cannot be guaranteed.
