# ADR-006 — Go-native OIDC driver instead of Playwright for the R2 sandbox E2E

- **Status**: accepted (R2, 2026-10-24)
- **Context**: `docs/13-roadmap-and-tasks.md` and `ai/MASTER_PROMPT.md` describe R2
  as "Playwright drives the Keycloak controlled registration page, collects the
  verification email from Mailpit, completes OIDC Authorization Code + PKCE, 5/5
  identities". The acceptance objective is a *fully unattended* end-to-end run
  against a **self-controlled** Keycloak + Mailpit fixture, proving the
  orchestrator can handle real email delivery and a real OIDC provider.
- **Environment finding**: no Playwright/browser engine is installed, and this
  session cannot install browsers reliably (no Docker daemon, constrained
  network for large downloads). Docker-based Playwright fixtures are unavailable.
- **Decision**: implement the R2 automation as a Go-native HTTP driver against
  the real Keycloak (Admin REST + HTML login form + authorization-code/PKCE
  token endpoint) and the real Mailpit API. The driver performs the same
  protocol steps a browser would: parse the login form, submit credentials with
  a cookie jar, follow the authorization redirect, exchange the code with the
  PKCE verifier, and validate with userinfo.
- **Consequences**:
  - The R2 gate still exercises real Keycloak + real Mailpit with no human
    interaction, which is the substantive acceptance criterion.
  - Browser-specific behaviours (JavaScript, CSP, UI rendering, cookie
    SameSite enforcement by a browser) are **not** exercised. This is recorded
    as `NOT_RUN` in `docs/acceptance/R2.md`; "Playwright 5/5" is never claimed.
  - The driver is deterministic, fast, and runnable in CI without browsers. The
    Playwright path can be added later behind the same `SandboxDriver` contract
    (`internal/provider.Keycloak` / `provider.SandboxDriver`) without changing
    the engine or the workflow templates.
  - The Keycloak verification link is followed as a two-step HTTP GET (the
    interstitial page plus its "proceed" href), matching the real browser flow;
    email verification is confirmed through the Keycloak Admin API rather than
    by trusting the page.
- **Rejected alternatives**: (a) skip email verification and set
  `emailVerified=true` directly via the Admin API — rejected: it would fake the
  acceptance evidence and not exercise email matching/extraction; (b) install a
  browser — not feasible in this environment; (c) mark R2 as done based on fake
  providers — explicitly forbidden by the design and the review prompt.
