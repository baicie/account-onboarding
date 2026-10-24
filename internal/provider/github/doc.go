// Package github implements provider.EmailManager for an EXISTING,
// operator-authorized GitHub account through the official REST API.
//
// # Compliance boundary
//
// The rules below are the contract from docs/09-integration-github.md and
// tests/acceptance-matrix.md; they are enforced by the package itself, not by
// convention:
//
//   - Only two endpoints are ever called: POST /user/emails and GET /user/emails
//     (plus GET /user for the optional credential self-check). Every request
//     path is re-checked against a fixed allowlist at call time.
//   - No account creation, no CAPTCHA/anti-bot handling, no proxy rotation, no
//     multi-account orchestration, no browser automation. A request for such
//     behaviour must be rejected by review, not implemented here.
//   - Human confirmation is mandatory: the adapter registers a self-owned
//     address and polls "verified". It never clicks GitHub's verification link
//     and never claims VERIFIED on its own.
//   - The access token (PAT or GitHub App installation token) is resolved from
//     an opaque secret reference through provider.SecretResolver at call time.
//     It is never stored on the adapter, never logged, never serialized into an
//     error summary or a DTO, and never sent to a host other than the
//     allowlisted API base (cross-host redirects are refused before the header
//     is written).
//
// # Failure taxonomy
//
// HTTP status, JSON envelope quirks and transport faults are translated into
// internal/errs classes at the adapter boundary:
//
//	401/403            -> NOT_AUTHORIZED (403 with an exhausted rate-limit
//	                      budget is reported as RATE_LIMITED instead)
//	404                -> VALIDATION
//	409/422 duplicate  -> reconciled through GET /user/emails; success when the
//	                      address is already visible, VALIDATION otherwise
//	422 (other)        -> VALIDATION
//	429                -> RATE_LIMITED with Retry-After when supplied
//	5xx                -> UPSTREAM_TRANSIENT
//	timeout            -> TIMEOUT_UNCERTAIN
//	non-JSON / schema  -> PROVIDER_BUG
//	cross-host redirect-> PROVIDER_BUG (credential must never travel)
//
// Error summaries are fixed, public-safe strings. Response bodies are parsed
// only for schema and duplicate detection and are never echoed into errors.
package github
