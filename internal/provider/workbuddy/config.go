package workbuddy

import (
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

const (
	// ProviderName is the registry key for this adapter.
	ProviderName = "workbuddy"

	// ContractVersion changes only when the externally observable behaviour of
	// the adapter changes.
	ContractVersion = "1.0.0"

	// DefaultRestrictionReason is the explicit permission gate required by
	// docs/10-integration-workbuddy.md. It is surfaced through
	// Descriptor().RestrictionReason so GET /v1/providers explains why the
	// adapter is off instead of silently failing.
	DefaultRestrictionReason = "WorkBuddy live adapter requires verified platform written permission; disabled by default"

	// DefaultTimeout bounds one control-plane call. The gateway itself applies a
	// 30s upstream timeout, so the orchestrator stays below it.
	DefaultTimeout = 10 * time.Second

	// DefaultRealm selects the international WorkBuddy realm, as documented for
	// POST /panel/api/login/start.
	DefaultRealm = "global"

	// DefaultSessionTTL mirrors the gateway's loginTTL = 15 minutes.
	DefaultSessionTTL = 15 * time.Minute

	// MaxSessionTTL is the gateway's hard state lifetime; a longer local TTL
	// would advertise an authorization window the gateway cannot honour.
	MaxSessionTTL = 15 * time.Minute

	// DefaultUserAgent identifies this service to the gateway.
	DefaultUserAgent = "account-onboarding-workbuddy/1.0"

	// maxResponseBytes caps how much of a gateway body is read.
	maxResponseBytes = 1 << 20

	// maxRedirects is the hard ceiling for same-host redirects. Cross-host
	// redirects are refused outright by the CheckRedirect hook.
	maxRedirects = 3

	// terminalCacheTTL bounds how long a completed/expired session reference is
	// remembered so a duplicate poll never replays the state against the
	// gateway.
	terminalCacheTTL = time.Hour

	// terminalCacheLimit caps the in-memory terminal cache.
	terminalCacheLimit = 4096
)

const (
	pathLoginStart = "/panel/api/login/start"
	pathLoginPoll  = "/panel/api/login/poll"
)

// sessionRefPrefix is the opaque namespace for a stored authorization state.
// The reference never contains the state itself.
const sessionRefPrefix = provider.SecretRefPrefix + "oauth/workbuddy/"

// credentialRefPrefix is the opaque namespace of a gateway-owned credential.
// Recording it materializes only a pointer (uid/realm/base), never a token.
const credentialRefPrefix = provider.SecretRefPrefix + "workbuddy/credential/"

// Secret record kinds written through provider.SecretStore.
const (
	sessionSecretKind           = "workbuddy_authorization_session"
	credentialPointerSecretKind = "workbuddy_credential_pointer"
)

// allowedPaths is the closed set of gateway requests this adapter may issue.
var allowedPaths = map[string]struct{}{
	pathLoginStart: {},
	pathLoginPoll:  {},
}

// Config configures the WorkBuddy control-plane adapter.
//
// Enabled defaults to false. With Enabled false the adapter is inert: no base
// URL, no secret store access and no outbound request is attempted.
type Config struct {
	// Enabled opens the permission gate. Leave it false until verified written
	// permission from the platform exists.
	Enabled bool

	// RestrictionReason explains a closed gate. Defaults to
	// DefaultRestrictionReason.
	RestrictionReason string

	// BaseURL is the operator's own gateway control plane. It must be a
	// loopback or RFC1918 private address; this adapter never talks to the
	// public internet.
	BaseURL string

	// GatewayTokenRef is an opaque secret reference (secret://...) resolving to
	// the gateway panel API key. It is resolved per call and never logged.
	// Empty is allowed for a gateway configured without an api key.
	GatewayTokenRef string

	// Realm is the gateway login realm: "global" (default) or "cn".
	Realm string

	// Timeout bounds one control-plane call; defaults to DefaultTimeout.
	Timeout time.Duration

	// SessionTTL is the local authorization window; it is clamped to
	// MaxSessionTTL because the gateway expires its state after 15 minutes.
	SessionTTL time.Duration

	// AllowTestHTTP unlocks plaintext http for a loopback test fixture (httptest).
	// It is never enabled by production wiring.
	AllowTestHTTP bool

	// UserAgent defaults to DefaultUserAgent.
	UserAgent string
}

// withDefaults validates everything that does not need a transport and
// normalizes optional fields. Transport validation only runs when the gate is
// open, so a disabled adapter can always be constructed.
func (c Config) withDefaults() (Config, error) {
	out := c
	out.BaseURL = strings.TrimSpace(c.BaseURL)
	out.RestrictionReason = strings.TrimSpace(c.RestrictionReason)
	if out.RestrictionReason == "" {
		out.RestrictionReason = DefaultRestrictionReason
	}
	out.Realm = strings.ToLower(strings.TrimSpace(c.Realm))
	if out.Realm == "" {
		out.Realm = DefaultRealm
	}
	switch out.Realm {
	case "global", "cn":
	default:
		return Config{}, errs.New(errs.ClassValidation, "workbuddy.config",
			`realm must be "global" or "cn"`)
	}
	out.Timeout = c.Timeout
	if out.Timeout <= 0 {
		out.Timeout = DefaultTimeout
	}
	out.SessionTTL = c.SessionTTL
	if out.SessionTTL <= 0 {
		out.SessionTTL = DefaultSessionTTL
	}
	if out.SessionTTL > MaxSessionTTL {
		out.SessionTTL = MaxSessionTTL
	}
	out.UserAgent = strings.TrimSpace(c.UserAgent)
	if out.UserAgent == "" {
		out.UserAgent = DefaultUserAgent
	}
	out.GatewayTokenRef = strings.TrimSpace(c.GatewayTokenRef)
	if out.Enabled {
		if out.GatewayTokenRef != "" && !strings.HasPrefix(out.GatewayTokenRef, provider.SecretRefPrefix) {
			return Config{}, errs.New(errs.ClassValidation, "workbuddy.config",
				"gateway token reference must be an opaque "+provider.SecretRefPrefix+" reference, never a raw credential")
		}
	}
	return out, nil
}

// validateBaseURL enforces the private-address allowlist. No public host and no
// DNS name (other than localhost) is ever accepted, which keeps the adapter
// incapable of reaching the public WorkBuddy service even if misconfigured.
func validateBaseURL(raw string, allowTestHTTP bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errs.New(errs.ClassValidation, "workbuddy.config",
			"base URL is required when the adapter is enabled")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errs.New(errs.ClassValidation, "workbuddy.config", "base URL is not a valid URL")
	}
	if u.User != nil {
		return nil, errs.New(errs.ClassValidation, "workbuddy.config", "base URL must not embed credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errs.New(errs.ClassValidation, "workbuddy.config", "base URL must not carry a query or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errs.New(errs.ClassValidation, "workbuddy.config", "base URL must not include a path")
	}
	switch u.Scheme {
	case "https":
		if !isPrivateHost(u.Hostname()) {
			return nil, errs.New(errs.ClassValidation, "workbuddy.config",
				"base URL host must be loopback or a private RFC1918 address; public hosts are refused")
		}
	case "http":
		if !allowTestHTTP {
			return nil, errs.New(errs.ClassValidation, "workbuddy.config",
				"plaintext base URL requires the explicit loopback test flag (AllowTestHTTP)")
		}
		if !isLoopbackHost(u.Hostname()) {
			return nil, errs.New(errs.ClassValidation, "workbuddy.config",
				"plaintext base URL is only allowed for a loopback test fixture")
		}
	default:
		return nil, errs.New(errs.ClassValidation, "workbuddy.config", "base URL scheme must be https")
	}
	return u, nil
}

// isPrivateHost accepts loopback and RFC1918/ULA literals plus "localhost".
// DNS names are deliberately rejected: resolving a name here would make the
// allowlist bypassable through DNS rebinding.
func isPrivateHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
