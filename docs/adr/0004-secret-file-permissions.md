# ADR 0004 — Secret files must be owner-only

Status: accepted (R13)

The service accepts secrets through `*_FILE` settings. R13 enforces that every
secret file is a regular file with no group/other permission bits (0600 or 0400)
before reading it. A world/group-readable secret is a configuration error and
startup fails closed; the error names the setting and observed mode, never the
secret value.

This is defense in depth for bind-mounted Docker secrets and host deployments.
It does not replace the deployment checks that keep the secret directory
non-world-readable and does not cover environment-variable secrets (which the
process environment exposes to other same-user processes).
