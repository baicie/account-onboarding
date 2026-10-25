package security

import (
	"net/url"
	"regexp"
	"strings"
)

// Redactor removes secret-looking values before they reach logs, events or API
// errors. It is deliberately conservative: it only strips high-confidence token
// shapes and explicitly registered secret values.
type Redactor struct {
	secrets []string
}

// NewRedactor returns a redactor with the given exact secret values registered.
// Empty values are ignored.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		r.Register(s)
	}
	return r
}

// Register adds an exact secret value to be scrubbed.
func (r *Redactor) Register(secret string) {
	if len(strings.TrimSpace(secret)) < 4 {
		return
	}
	r.secrets = append(r.secrets, secret)
}

var (
	jwtPattern     = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`)
	bearerPattern  = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/\-]{8,}=*`)
	basicPattern   = regexp.MustCompile(`(?i)\bBasic\s+[A-Za-z0-9+/]{8,}=*`)
	queryParams    = regexp.MustCompile(`(?i)(\b(?:ACCESS_TOKEN|refresh_token|id_token|token|state|code|jwt|otp|password|passwd|secret|client_secret|x-admin-auth|x-custom-auth)=)[^&\s"']+`)
	longTokenShape = regexp.MustCompile(`\b[A-Za-z0-9_-]{48,}\b`)
)

// Redact returns a copy with token-looking substrings replaced by [REDACTED].
func (r *Redactor) Redact(s string) string {
	if s == "" {
		return s
	}
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	s = bearerPattern.ReplaceAllString(s, "Bearer [REDACTED]")
	s = basicPattern.ReplaceAllString(s, "Basic [REDACTED]")
	s = jwtPattern.ReplaceAllString(s, "[REDACTED_JWT]")
	s = queryParams.ReplaceAllString(s, "$1[REDACTED]")
	s = longTokenShape.ReplaceAllString(s, "[REDACTED_TOKEN]")
	return s
}

// SanitizeURL removes the query string from a URL for logging: query strings can
// carry OAuth state, codes and tokens.
func SanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable-url]"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// eventPayloadAllowlist is the only set of keys allowed in persisted events.
// See docs/03-workflows.md: no message bodies, OTPs, Authorization headers,
// state values or tokens.
var eventPayloadAllowlist = map[string]bool{
	"provider":            true,
	"resource_alias":      true,
	"http_status":         true,
	"latency_ms":          true,
	"failure_class":       true,
	"next_retry_at":       true,
	"attempt":             true,
	"operation_key":       true,
	"mailbox_alias":       true,
	"message_key_hmac":    true,
	"deadline_at":         true,
	"reason":              true,
	"contract_version":    true,
	"subject_alias":       true,
	"credential_ref_kind": true,
	"waiting_for":         true,
	"resume_state":        true,
	// External operation audit (non-secret enums/digest); see
	// docs/adr/0001-external-operation-reconciliation.md.
	"operation_type":      true,
	"request_fingerprint": true,
	"status":              true,
	"error_class":         true,
	// Operator resolution audit (R9): enum/ticket reference only; values still
	// pass through Redact and are length-capped.
	"resolution":         true,
	"evidence_reference": true,
	"operation_id":       true,
	"run_id":             true,
	"from_status":        true,
	"to_status":          true,
	"version_before":     true,
	"version_after":      true,
	"requested_at_ms":    true,
	"resolved_at_ms":     true,
	"resolved_by":        true,
	"actor":              true,
}

// SanitizeEventPayload filters an event payload down to the allowlist and
// redacts string values. It never returns nested objects.
func (r *Redactor) SanitizeEventPayload(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if !eventPayloadAllowlist[k] {
			continue
		}
		switch val := v.(type) {
		case nil, bool, int, int64, float64:
			out[k] = val
		case string:
			if len(val) > 256 {
				val = val[:256]
			}
			out[k] = r.Redact(val)
		default:
			// Structs, slices and maps are not event-payload material.
			continue
		}
	}
	return out
}

// IsEventPayloadKeyAllowed reports whether a key may be persisted.
func IsEventPayloadKeyAllowed(k string) bool { return eventPayloadAllowlist[k] }
