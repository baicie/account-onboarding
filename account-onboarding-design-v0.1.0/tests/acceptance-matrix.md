# Acceptance Matrix — executable specification

每条用例进入测试代码前先明确执行场景：`FAKE`, `LOCAL SANDBOX`, `CLOUDFLARE LIVE OPT-IN`, `GITHUB EXISTING ACCOUNT OPT-IN`, `WORKBUDDY CONTRACT ONLY`。

```gherkin
Feature: Account onboarding
  Scenario: Idempotent run creation
    Given an authenticated local operator
    When the same template and idempotency_key is submitted twice
    Then the service returns the same run_id
    And exactly one run record exists

  Scenario: Durable retry after crash
    Given a sandbox run is waiting for an email
    When onboardd restarts
    Then the run resumes from its persisted state
    And does not create another sandbox identity

  Scenario: Five sandbox identities
    Given a running sandbox Keycloak and Mailpit
    When five test onboarding runs are submitted with distinct keys
    Then all five test identities verify their emails
    And complete OIDC authorization via the sandbox
    And no real GitHub or WorkBuddy account is created

  Scenario: Forbidden provider
    Given WorkBuddy live adapter is disabled
    When a run requests it
    Then the run fails as NOT_AUTHORIZED
    And no network call reaches the WorkBuddy upstream

  Scenario: OAuth state expiration
    Given an OAuth fake adapter returns 410
    Then the run is REAUTHORIZE_REQUIRED
    And the existing session state is never replayed

  Scenario: Secret redaction
    Given fake providers return sensitive tokens and OTPs
    When a workflow completes
    Then public API and event history contain no secret values
```

Additional gate: SQLite live backup/restore with WAL, auth-cookie CSRF tests, 429 Retry-After handling, redirect with Authorization header rejected, malformed JSON, unknown email subject, OTP reuse, email missing, lease expiry fencing.
