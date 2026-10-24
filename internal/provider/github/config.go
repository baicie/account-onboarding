package github

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
	ProviderName = "github"

	// ContractVersion changes only when the externally observable behaviour of
	// the adapter changes; runs pin it through provider.ProviderSnapshot.
	ContractVersion = "1.0.0"

	// DefaultBaseURL is the only production API base. GitHub Enterprise Server
	// hosts are intentionally not supported: an operator must never be able to
	// point this adapter at an arbitrary host by configuration.
	DefaultBaseURL = "https://api.github.com"

	// DefaultTimeout bounds one whole API call (connect + headers + body).
	DefaultTimeout = 10 * time.Second

	// DefaultUserAgent identifies this service to GitHub.
	DefaultUserAgent = "account-onboarding-github/1.0"

	// DefaultAPIVersion pins the GitHub REST API version header.
	DefaultAPIVersion = "2022-11-28"

	// maxResponseBytes caps how much of an upstream body is read. GitHub email
	// listings are tiny; anything larger is a schema/protocol anomaly.
	maxResponseBytes = 1 << 20

	// maxRedirects is the hard ceiling for same-host redirects. Cross-host
	// redirects are refused outright by the CheckRedirect hook.
	maxRedirects = 3
)

const (
	pathUserEmails = "/user/emails"
	pathUser       = "/user"
)

// allowedPaths is the closed set of request paths this adapter may issue. do()
// re-validates every call against it so a future edit cannot silently add an
// account-creation or other endpoint.
var allowedPaths = map[string]struct{}{
	pathUserEmails: {},
	pathUser:       {},
}

// Config configures the GitHub email manager. Every field has a safe default
// except TokenRef, which the operator must supply to construct a live adapter.
type Config struct {
	// BaseURL defaults to DefaultBaseURL. Overrides are honoured only for a
	// loopback test endpoint and only when AllowLoopbackHTTP is set.
	BaseURL string

	// TokenRef is an opaque secret reference (secret://...) whose resolved
	// plaintext is a GitHub PAT or App installation token with email access.
	// The plaintext is resolved per call and never stored in this struct.
	TokenRef string

	// AllowLoopbackHTTP explicitly enables http://127.0.0.1:<port>,
	// http://[::1]:<port> or http://localhost:<port> test endpoints. Never set
	// it in a production configuration: it exists for httptest in the contract
	// suite and for an operator-run local fixture.
	AllowLoopbackHTTP bool

	// AllowedDomains, when non-empty, is an additional local guard: only
	// addresses whose domain (case-insensitive, exact match) appears here are
	// sent to GitHub. The workflow engine validates domains too; this is
	// defence in depth so a caller bug cannot register an arbitrary address.
	AllowedDomains []string

	// Timeout bounds one API call; defaults to DefaultTimeout.
	Timeout time.Duration

	// UserAgent defaults to DefaultUserAgent.
	UserAgent string

	// APIVersion defaults to DefaultAPIVersion.
	APIVersion string
}

// withDefaults validates cfg and returns the effective configuration.
func (c Config) withDefaults() (Config, error) {
	out := c
	out.BaseURL = strings.TrimSpace(c.BaseURL)
	if out.BaseURL == "" {
		out.BaseURL = DefaultBaseURL
	}
	out.BaseURL = strings.TrimRight(out.BaseURL, "/")
	out.TokenRef = strings.TrimSpace(c.TokenRef)
	out.Timeout = c.Timeout
	if out.Timeout <= 0 {
		out.Timeout = DefaultTimeout
	}
	out.UserAgent = strings.TrimSpace(c.UserAgent)
	if out.UserAgent == "" {
		out.UserAgent = DefaultUserAgent
	}
	out.APIVersion = strings.TrimSpace(c.APIVersion)
	if out.APIVersion == "" {
		out.APIVersion = DefaultAPIVersion
	}
	if out.TokenRef == "" {
		return Config{}, errs.New(errs.ClassValidation, "github.config",
			"token reference is required (an existing operator-authorized account must be configured)")
	}
	if !strings.HasPrefix(out.TokenRef, provider.SecretRefPrefix) {
		return Config{}, errs.New(errs.ClassValidation, "github.config",
			"token reference must be an opaque "+provider.SecretRefPrefix+" reference, never a raw token")
	}
	if err := validateBaseURL(out.BaseURL, out.AllowLoopbackHTTP); err != nil {
		return Config{}, err
	}
	out.AllowedDomains = normalizeDomains(c.AllowedDomains)
	return out, nil
}

// validateBaseURL enforces the allowlist: the canonical public API base, or a
// loopback test endpoint explicitly unlocked by the caller.
func validateBaseURL(raw string, allowLoopback bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errs.New(errs.ClassValidation, "github.config", "base URL is not a valid URL")
	}
	if u.User != nil {
		return errs.New(errs.ClassValidation, "github.config", "base URL must not embed credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errs.New(errs.ClassValidation, "github.config", "base URL must not carry a query or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return errs.New(errs.ClassValidation, "github.config", "base URL must not include a path")
	}
	switch u.Scheme {
	case "https":
		// The only accepted public host. An operator cannot redirect the adapter
		// (and its Authorization header) to an arbitrary host by configuration.
		if strings.EqualFold(u.Host, "api.github.com") {
			return nil
		}
		if allowLoopback && isLoopbackHost(u.Host) {
			return nil
		}
		return errs.New(errs.ClassValidation, "github.config",
			"base URL must be "+DefaultBaseURL+" (loopback https requires AllowLoopbackHTTP)")
	case "http":
		if allowLoopback && isLoopbackHost(u.Host) {
			return nil
		}
		return errs.New(errs.ClassValidation, "github.config",
			"plaintext base URL is only allowed for an explicit loopback test endpoint (AllowLoopbackHTTP)")
	default:
		return errs.New(errs.ClassValidation, "github.config", "base URL scheme must be https")
	}
}

// isLoopbackHost matches the tiny set of test-only hosts used by httptest and
// by local fixtures. Deliberately excludes every public name and every
// non-loopback IP.
func isLoopbackHost(host string) bool {
	h := host
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		h = hostOnly
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// normalizeDomains lower-cases, trims and de-duplicates the optional allowlist.
func normalizeDomains(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@")))
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}
