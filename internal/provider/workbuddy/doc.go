// Package workbuddy adapts the EXISTING WorkBuddy gateway
// (workbuddy2api-panel) as an OAuth control plane for an account that the
// operator already owns. It is permission gated and disabled by default.
//
// # What this adapter is allowed to do
//
// Two control-plane endpoints of the operator's own gateway are the only
// network surface:
//
//	POST /panel/api/login/start  {"realm":"global"}
//	GET  /panel/api/login/poll?state=<state>
//
// The gateway holds the platform session and writes the credential itself
// (auth.SaveAtomic in the gateway process). This adapter never sees, parses,
// copies or re-stores a platform token: on completion it only records an opaque
// credential reference that points at gateway-owned storage.
//
// # What this adapter must never do
//
//   - no scraping of the WorkBuddy web app, no browser-login automation, no
//     token extraction, no bypass of the platform's authorization flow;
//   - no call to any public internet host: the base URL must be loopback or
//     RFC1918 private (docs/10-integration-workbuddy.md, docs/07);
//   - no reuse of an authorization state. The gateway keeps states in process
//     memory for loginTTL = 15 minutes and answers 410 for unknown/expired
//     states. A 410 (or the local 15 minute deadline) is mapped to AUTH_EXPIRED,
//     which the engine turns into REAUTHORIZE_REQUIRED: the operator starts a
//     new authorization instead of replaying the old state;
//   - no concurrent polling of one state. loginPoll has side effects inside the
//     gateway (credential write, account adoption, hot reload), so Poll
//     serializes per session reference and reuses the terminal result.
//
// # Permission gate
//
// The real adapter is disabled by default with an explicit RestrictionReason:
// a live connection requires verified written permission from the platform.
// When disabled, Descriptor().Enabled is false and every capability returns
// NOT_AUTHORIZED before any network call or secret resolution. NewDisabled and
// New(Config{}) both produce that state, so a registry lookup fails closed.
//
// # Failure taxonomy
//
//	410                       -> AUTH_EXPIRED
//	401/403                   -> NOT_AUTHORIZED
//	429                       -> RATE_LIMITED (+ Retry-After)
//	400/404                   -> VALIDATION
//	5xx / network             -> UPSTREAM_TRANSIENT
//	timeout                   -> TIMEOUT_UNCERTAIN (the gateway may have acted)
//	malformed JSON / schema   -> PROVIDER_BUG
//	cross-host redirect       -> PROVIDER_BUG (credential must never travel)
package workbuddy
