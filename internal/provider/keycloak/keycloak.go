// Package keycloak adapts a LOCAL, self-controlled Keycloak sandbox realm to the
// provider contracts. It is the identity/sandbox/credential fixture used by the
// `sandbox-email-verify-oidc` workflow: it creates controlled test users through
// the Admin REST API, triggers the VERIFY_EMAIL action email (delivered to
// Mailpit over the realm SMTP), follows the verification link (including
// Keycloak's "click here to proceed" interstitial), drives a full Authorization
// Code + PKCE S256 browser login, and validates the resulting opaque credential.
//
// Boundaries (see docs/07-security-and-compliance.md):
//
//   - The base URL is pinned at construction time to a loopback host. This
//     adapter can never be pointed at a public IdP, and redirects never cross
//     origins, so cookies and bearer tokens cannot leak to a third party.
//   - Tokens and action links are transient. The only persistent artifacts are
//     OIDC access/id tokens written through provider.SecretStore under
//     secret://keycloak_oidc/<random>; callers only ever see the opaque
//     reference plus non-secret claims.
//   - The package never logs anything. Error summaries carry HTTP status and
//     operation names only, never bodies, URLs, codes, links or credentials.
//   - Admin credentials (master realm password grant or a service-account
//     client) are held in memory for the admin token only.
//
// All three registry roles are served by one shared http.Client/config and
// exposed as per-kind wrappers, because the registry keys adapters by
// (kind, name) and each descriptor reports exactly one kind.
package keycloak

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

const (
	// ProviderName is the registry name shared by every wrapper descriptor.
	ProviderName = "keycloak-test"
	// ContractVersion is locked into runs at creation time.
	ContractVersion = "v1"
	// DefaultTimeout bounds a single upstream HTTP request.
	DefaultTimeout = 20 * time.Second
	// DefaultAdminRealm is the realm that owns the admin account.
	DefaultAdminRealm = "master"
	// DefaultAdminClientID is the built-in client used for the password grant.
	DefaultAdminClientID = "admin-cli"

	// secretKindOIDC is the SecretStore kind for stored OIDC token envelopes.
	secretKindOIDC = "keycloak_oidc"

	adminTokenSafety    = 30 * time.Second
	actionLifespanSecs  = 3600
	maxResponseBytes    = 8 << 20
	maxLoginPageBytes   = 4 << 20
	maxRedirects        = 5
	maxVerificationHops = 2
)

var (
	errRedirectBlocked  = errors.New("keycloak: redirect to a different origin refused")
	errTooManyRedirects = errors.New("keycloak: too many redirects")
	errHTTPRequired     = errors.New("keycloak: plain http base url requires AllowLoopbackHTTP")
)

// Config configures the sandbox Keycloak adapter.
//
// BaseURL/Realm identify the sandbox realm. AdminUser/AdminPassword drive the
// master-realm password grant; AdminClientID/AdminClientSecret enable the
// client_credentials service-account path instead (preferred when present).
// ClientID/ClientSecret/RedirectURI describe the OIDC client used for the
// authorization code flow.
type Config struct {
	BaseURL           string
	Realm             string
	AdminUser         string
	AdminPassword     string
	AdminClientID     string
	AdminClientSecret string
	ClientID          string
	ClientSecret      string
	RedirectURI       string
	Timeout           time.Duration
	// AllowLoopbackHTTP permits an http:// base URL for loopback hosts. The
	// local sandbox serves plain HTTP on 127.0.0.1; anything else must use TLS.
	AllowLoopbackHTTP bool
	// HTTPClient optionally supplies a transport. Its timeout and redirect
	// policy are replaced by the adapter's own so the loopback pin and the
	// callback guard cannot be bypassed.
	HTTPClient *http.Client
	// Disabled gates the adapter: every call fails with NOT_AUTHORIZED before
	// any network work and the descriptor reports Enabled=false.
	Disabled       bool
	DisabledReason string
}

// Adapter is the shared Keycloak sandbox client. Use Identity, Sandbox and
// Credential to obtain the per-kind views the registry accepts.
type Adapter struct {
	cfg      Config
	base     *url.URL
	realm    string
	client   string // OIDC client id
	secret   string // OIDC client secret (usually empty: public PKCE client)
	redir    *url.URL
	timeout  time.Duration
	api      *http.Client
	secrets  provider.SecretStore
	admin    *adminSession
	disabled bool
	now      func() time.Time
}

// New validates the configuration and builds the shared adapter. It performs no
// network I/O.
func New(cfg Config, secrets provider.SecretStore) (*Adapter, error) {
	if cfg.Disabled {
		return &Adapter{disabled: true, cfg: cfg, now: time.Now, secrets: secrets}, nil
	}
	if secrets == nil {
		return nil, configErr("a secret store is required to persist OIDC credentials")
	}
	base, err := parseBaseURL(cfg.BaseURL, cfg.AllowLoopbackHTTP)
	if err != nil {
		return nil, err
	}
	realm, err := parseRealm(cfg.Realm)
	if err != nil {
		return nil, err
	}
	clientID := strings.TrimSpace(cfg.ClientID)
	if clientID == "" {
		return nil, configErr("client id is required")
	}
	if len(clientID) > 200 || strings.ContainsAny(clientID, " \t\r\n\"'<>") {
		return nil, configErr("client id contains invalid characters")
	}
	redir, err := parseRedirectURI(cfg.RedirectURI, cfg.AllowLoopbackHTTP)
	if err != nil && strings.TrimSpace(cfg.RedirectURI) != "" {
		return nil, err
	}
	// Admin credentials: a service account is preferred, the master-realm
	// password grant from the sandbox fixture is the fallback.
	hasServiceAccount := strings.TrimSpace(cfg.AdminClientID) != "" && strings.TrimSpace(cfg.AdminClientSecret) != ""
	hasPasswordGrant := strings.TrimSpace(cfg.AdminUser) != "" && strings.TrimSpace(cfg.AdminPassword) != ""
	if !hasServiceAccount && !hasPasswordGrant {
		return nil, configErr("admin credentials are required (service account client or admin user/password)")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	a := &Adapter{
		cfg:     cfg,
		base:    base,
		realm:   realm,
		client:  clientID,
		secret:  strings.TrimSpace(cfg.ClientSecret),
		redir:   redir,
		timeout: timeout,
		secrets: secrets,
		now:     time.Now,
	}
	baseClient := http.Client{}
	if cfg.HTTPClient != nil {
		baseClient.Transport = cfg.HTTPClient.Transport
	}
	baseClient.Timeout = timeout
	baseClient.CheckRedirect = a.redirectGuard(a.redir)
	a.api = &baseClient
	a.admin = &adminSession{adapter: a}
	return a, nil
}

// NewDisabled builds an inert adapter whose descriptors explain the activation
// gate. It backs GET /v1/providers when ONBOARD_KEYCLOAK_ENABLED=false.
func NewDisabled(reason string) *Adapter {
	if strings.TrimSpace(reason) == "" {
		reason = "Keycloak sandbox adapter is disabled by configuration"
	}
	return &Adapter{disabled: true, now: time.Now, cfg: Config{DisabledReason: reason}}
}

// Disabled reports whether the activation gate is engaged.
func (a *Adapter) Disabled() bool { return a == nil || a.disabled }

// ---------------------------------------------------------------------------
// Per-kind wrappers
// ---------------------------------------------------------------------------

// Identity returns the identity-provider view (request/check verification).
func (a *Adapter) Identity() provider.IdentityProvider { return identityView{a} }

// Sandbox returns the sandbox-driver view (register/confirm/OIDC/validate).
func (a *Adapter) Sandbox() provider.SandboxDriver { return sandboxView{a} }

// Credential returns the credential-verifier view.
func (a *Adapter) Credential() provider.CredentialVerifier { return credentialView{a} }

// Issuer is the canonical OIDC issuer of the configured realm.
func (a *Adapter) Issuer() string {
	if a == nil || a.base == nil {
		return ""
	}
	return a.realmURL("")
}

func (a *Adapter) descriptor(kind provider.Kind, capabilities []string) provider.Descriptor {
	d := provider.Descriptor{
		Name:            ProviderName,
		Kind:            kind,
		ContractVersion: ContractVersion,
		Enabled:         !a.Disabled(),
		// This adapter only ever automates a self-controlled local sandbox IdP.
		Live:         false,
		Capabilities: capabilities,
	}
	if a.Disabled() {
		reason := a.cfg.DisabledReason
		if strings.TrimSpace(reason) == "" {
			reason = "Keycloak sandbox adapter is disabled by configuration"
		}
		d.RestrictionReason = reason
	}
	return d
}

// ensureEnabled fails closed before any network call, like the registry gate.
func (a *Adapter) ensureEnabled(kind provider.Kind) error {
	if a.Disabled() {
		reason := a.cfg.DisabledReason
		if strings.TrimSpace(reason) == "" {
			reason = "Keycloak sandbox adapter is disabled by configuration"
		}
		return provider.NewDisallowedError(kind, ProviderName, reason)
	}
	return nil
}

// ---------------------------------------------------------------------------
// URL construction
// ---------------------------------------------------------------------------

func (a *Adapter) path(suffix string) string {
	p := strings.TrimSuffix(a.base.Path, "/")
	return p + suffix
}

// realmURL builds an OIDC endpoint URL for the configured realm.
func (a *Adapter) realmURL(suffix string) string {
	u := *a.base
	u.Path = a.path("/realms/" + a.realm + suffix)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// adminURL builds an Admin REST API URL for the configured realm.
func (a *Adapter) adminURL(suffix string) string {
	u := *a.base
	u.Path = a.path("/admin/realms/" + a.realm + suffix)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (a *Adapter) tokenURL() string    { return a.realmURL("/protocol/openid-connect/token") }
func (a *Adapter) authURL() string     { return a.realmURL("/protocol/openid-connect/auth") }
func (a *Adapter) userinfoURL() string { return a.realmURL("/protocol/openid-connect/userinfo") }

func (a *Adapter) adminTokenURL() string {
	u := *a.base
	u.Path = a.path("/realms/" + DefaultAdminRealm + "/protocol/openid-connect/token")
	u.RawQuery = ""
	return u.String()
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

// newBrowserClient returns a client with its own cookie jar and the adapter's
// redirect policy. Every browser-like flow gets a fresh jar so concurrent runs
// never share an AUTH_SESSION_ID. When callback is non-nil the client stops at
// that application callback instead of fetching it, so the authorization code
// can be read from the Location header.
func (a *Adapter) newBrowserClient(callback *url.URL) (*http.Client, error) {
	jar, err := newJar()
	if err != nil {
		return nil, errs.Wrap(errs.ClassInternal, "keycloak.cookiejar", "could not create the sandbox cookie jar", err)
	}
	client := &http.Client{Transport: a.api.Transport, Timeout: a.timeout, CheckRedirect: a.redirectGuard(callback), Jar: jar}
	return client, nil
}

// redirectGuard keeps every redirect on the configured origin (cookies and
// bearer headers can never bounce to a third party) and optionally stops at the
// application callback.
func (a *Adapter) redirectGuard(callback *url.URL) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		// The application callback lives on a different origin by design: stop
		// there so the authorization code can be read from the Location header.
		if callbackMatches(req.URL, callback) {
			return http.ErrUseLastResponse
		}
		origin := via[0].URL
		if !sameOrigin(req.URL, origin) {
			return errRedirectBlocked
		}
		if len(via) >= maxRedirects {
			return errTooManyRedirects
		}
		return nil
	}
}

// callbackMatches compares a redirect target with the application callback URI.
func callbackMatches(u, callback *url.URL) bool {
	if u == nil || callback == nil {
		return false
	}
	return strings.EqualFold(u.Scheme, callback.Scheme) &&
		strings.EqualFold(u.Host, callback.Host) &&
		strings.TrimSuffix(u.Path, "/") == strings.TrimSuffix(callback.Path, "/")
}

// doJSON performs one JSON request and decodes the response into out. An empty
// successful body is allowed when out is nil.
func (a *Adapter) doJSON(ctx context.Context, client *http.Client, op, method, rawURL string, header http.Header, body []byte, out any) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, ctxErr(op, err)
	}
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, errs.New(errs.ClassInternal, op, "could not build the keycloak request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		for _, vv := range v {
			req.Header.Add(k, vv)
		}
	}
	if client == nil {
		client = a.api
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, a.transportError(op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return resp, a.transportError(op, err)
	}
	if len(payload) > maxResponseBytes {
		return resp, errs.New(errs.ClassProviderBug, op, "keycloak response exceeded the size limit").WithHTTPStatus(resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp, statusError(op, resp, payload)
	}
	if out == nil {
		return resp, nil
	}
	if len(strings.TrimSpace(string(payload))) == 0 {
		return resp, errs.New(errs.ClassProviderBug, op, "keycloak returned an empty response body").WithHTTPStatus(resp.StatusCode)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return resp, errs.New(errs.ClassProviderBug, op, "keycloak returned a non-JSON body").WithHTTPStatus(resp.StatusCode)
	}
	return resp, nil
}

// postForm submits an application/x-www-form-urlencoded request.
func (a *Adapter) postForm(ctx context.Context, client *http.Client, op, rawURL string, form url.Values, header http.Header, out any) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, ctxErr(op, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errs.New(errs.ClassInternal, op, "could not build the keycloak request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, v := range header {
		for _, vv := range v {
			req.Header.Add(k, vv)
		}
	}
	if client == nil {
		client = a.api
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, a.transportError(op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return resp, a.transportError(op, err)
	}
	if len(payload) > maxResponseBytes {
		return resp, errs.New(errs.ClassProviderBug, op, "keycloak response exceeded the size limit").WithHTTPStatus(resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp, statusError(op, resp, payload)
	}
	if out == nil {
		return resp, nil
	}
	if len(strings.TrimSpace(string(payload))) == 0 {
		return resp, errs.New(errs.ClassProviderBug, op, "keycloak returned an empty response body").WithHTTPStatus(resp.StatusCode)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return resp, errs.New(errs.ClassProviderBug, op, "keycloak returned a non-JSON body").WithHTTPStatus(resp.StatusCode)
	}
	return resp, nil
}

// getHTML fetches a browser page and returns the body without JSON decoding.
func (a *Adapter) getHTML(ctx context.Context, client *http.Client, op, rawURL string, limit int64) (string, *http.Response, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, ctxErr(op, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", nil, errs.New(errs.ClassInternal, op, "could not build the keycloak request")
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	if client == nil {
		client = a.api
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, a.transportError(op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", resp, a.transportError(op, err)
	}
	if int64(len(payload)) > limit {
		return "", resp, errs.New(errs.ClassProviderBug, op, "keycloak page exceeded the size limit").WithHTTPStatus(resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return string(payload), resp, statusError(op, resp, payload)
	}
	return string(payload), resp, nil
}

// transportError classifies transport failures. Keycloak sandbox calls are not
// safely retried when the outcome is unknown, so a timeout is TIMEOUT_UNCERTAIN
// (the engine reconciles) rather than a plain transient error.
func (a *Adapter) transportError(op string, err error) error {
	if errors.Is(err, context.Canceled) {
		return errs.Wrap(errs.ClassCancelled, op, "keycloak request cancelled", err)
	}
	if errors.Is(err, errRedirectBlocked) {
		return errs.Wrap(errs.ClassProviderBug, op, "keycloak attempted a cross-origin redirect", err)
	}
	if errors.Is(err, errTooManyRedirects) {
		return errs.Wrap(errs.ClassProviderBug, op, "keycloak redirect loop", err)
	}
	if isTimeout(err) {
		return errs.Wrap(errs.ClassTimeoutUncertain, op, "keycloak request timed out", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.ClassTimeoutUncertain, op, "keycloak request timed out", err)
	}
	return errs.Wrap(errs.ClassUpstreamTransient, op, "keycloak request failed", err)
}

// statusError maps an upstream HTTP status onto the domain taxonomy. Error text
// never includes the response body or the request URL (both may carry secrets).
func statusError(op string, resp *http.Response, body []byte) error {
	status := resp.StatusCode
	class := errs.FromHTTPStatus(status)
	switch status {
	case http.StatusBadRequest:
		class = errs.ClassValidation
		if looksLikeOAuthRejection(body) {
			class = errs.ClassAuthExpired
		}
	case http.StatusNotFound:
		class = errs.ClassNotFound
	case http.StatusConflict:
		class = errs.ClassConflict
	case http.StatusGone:
		class = errs.ClassAuthExpired
	}
	e := errs.New(class, op, fmt.Sprintf("keycloak returned HTTP %d", status)).WithHTTPStatus(status)
	if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); retryAfter > 0 {
		e = e.WithRetryAfter(retryAfter)
	}
	return e
}

// looksLikeOAuthRejection inspects an OAuth error code without echoing it.
func looksLikeOAuthRejection(body []byte) bool {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	switch payload.Error {
	case "invalid_grant", "invalid_token", "expired_token", "access_denied", "invalid_request":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func configErr(summary string) error {
	return errs.New(errs.ClassValidation, "keycloak.config", summary)
}

func ctxErr(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.ClassTimeoutUncertain, op, "keycloak request timed out", err)
	}
	return errs.New(errs.ClassCancelled, op, "request cancelled")
}

func parseBaseURL(raw string, allowHTTP bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, configErr("base url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, configErr("base url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, configErr("base url must use http or https")
	}
	if u.User != nil {
		return nil, configErr("base url must not embed credentials")
	}
	if u.Host == "" {
		return nil, configErr("base url must include a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, configErr("base url must not include a query or fragment")
	}
	if !isLoopbackHost(u.Hostname()) {
		return nil, configErr("base url must point at a loopback host: this adapter is for the local sandbox IdP only")
	}
	if u.Scheme == "http" && !allowHTTP {
		return nil, configErr("plain http base url requires AllowLoopbackHTTP")
	}
	return u, nil
}

func parseRealm(raw string) (string, error) {
	realm := strings.TrimSpace(raw)
	if realm == "" {
		return "", configErr("realm is required")
	}
	if len(realm) > 64 {
		return "", configErr("realm is too long")
	}
	for _, r := range realm {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return "", configErr("realm contains invalid characters")
		}
	}
	return realm, nil
}

func parseRedirectURI(raw string, allowHTTP bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 2048 {
		return nil, configErr("redirect uri is too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, configErr("redirect uri is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, configErr("redirect uri must use http or https")
	}
	if u.User != nil {
		return nil, configErr("redirect uri must not embed credentials")
	}
	if u.Host == "" {
		return nil, configErr("redirect uri must include a host")
	}
	if u.Fragment != "" || u.RawQuery != "" {
		return nil, configErr("redirect uri must not include a query or fragment")
	}
	if !isLoopbackHost(u.Hostname()) {
		return nil, configErr("redirect uri must point at a loopback host for the sandbox flow")
	}
	if u.Scheme == "http" && !allowHTTP {
		return nil, configErr("plain http redirect uri requires AllowLoopbackHTTP")
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delta := when.Sub(now); delta > 0 {
			return delta
		}
	}
	return 0
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// randomToken returns a URL-safe random string with n bytes of entropy.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// constantTimeEqual is used for the OAuth state comparison.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// pkcePair derives an S256 code challenge from a verifier.
func pkcePair() (verifier, challenge string, err error) {
	verifier, err = randomToken(48)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// newJar creates a private cookie jar for one browser-like flow.
func newJar() (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	// The local Keycloak fixture serves plain HTTP on loopback but marks its
	// session cookies Secure. Go's cookiejar refuses to send Secure cookies to
	// http:// URLs, which breaks the two-step verification link and the login
	// form flow. Treat the loopback sandbox as a secure context: this wrapper is
	// only reachable through the adapter's loopback-only base URL guard.
	return loopbackSecureJar{base: jar}, nil
}

// loopbackSecureJar lets Secure cookies be sent to the loopback sandbox over
// plain HTTP. It never weakens the origin policy: the adapter only talks to
// 127.0.0.1/localhost with the configured port.
type loopbackSecureJar struct {
	base http.CookieJar
}

func (j loopbackSecureJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.base.SetCookies(u, cookies)
}

func (j loopbackSecureJar) Cookies(u *url.URL) []*http.Cookie {
	if u == nil {
		return j.base.Cookies(u)
	}
	if u.Scheme == "http" {
		httpsURL := *u
		httpsURL.Scheme = "https"
		if cookies := j.base.Cookies(&httpsURL); len(cookies) > 0 {
			return cookies
		}
	}
	return j.base.Cookies(u)
}

// bearerAuthorized builds the Authorization header for an access token.
func bearerAuthorized(token string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	return h
}

// userAgent identifies the sandbox automation without leaking versions.
const userAgent = "account-onboarding-sandbox-e2e/1 (local keycloak fixture)"

// withUserAgent stamps the automation user agent on an outgoing request header.
// It is applied through header maps set on every request builder.
func withUserAgent(h http.Header) http.Header {
	if h == nil {
		h = http.Header{}
	}
	h.Set("User-Agent", userAgent)
	return h
}

// ---------------------------------------------------------------------------
// Admin token session
// ---------------------------------------------------------------------------

type adminSession struct {
	adapter *Adapter
	mu      sync.Mutex
	token   string
	expires time.Time
	// fetched counts successful token acquisitions; tests use it to assert the
	// cache behaviour without inspecting internals of the HTTP layer.
	fetched int64
}

// adminToken returns a cached admin access token, refreshing it 30s before the
// provider expiry. Concurrent callers serialize on the mutex so a cold cache
// issues exactly one token request.
func (s *adminSession) adminToken(ctx context.Context) (string, error) {
	a := s.adapter
	s.mu.Lock()
	defer s.mu.Unlock()
	now := a.now()
	if s.token != "" && now.Add(time.Second).Before(s.expires) {
		return s.token, nil
	}
	token, ttl, err := s.fetch(ctx)
	if err != nil {
		return "", err
	}
	s.token = token
	s.expires = now.Add(ttl)
	s.fetched++
	return token, nil
}

// invalidate drops the cached token after an unexpected 401.
func (s *adminSession) invalidate() {
	s.mu.Lock()
	s.token = ""
	s.expires = time.Time{}
	s.mu.Unlock()
}

func (s *adminSession) fetch(ctx context.Context) (string, time.Duration, error) {
	const op = "keycloak.admin_token"
	a := s.adapter
	form := url.Values{}
	tokenURL := a.adminTokenURL()
	if strings.TrimSpace(a.cfg.AdminClientID) != "" && strings.TrimSpace(a.cfg.AdminClientSecret) != "" {
		// A service-account client belongs to the sandbox realm, so its
		// client_credentials grant must be verified by that realm's token
		// endpoint (the master realm only knows admin-cli).
		form.Set("grant_type", "client_credentials")
		form.Set("client_id", strings.TrimSpace(a.cfg.AdminClientID))
		form.Set("client_secret", strings.TrimSpace(a.cfg.AdminClientSecret))
		tokenURL = a.realmURL("/protocol/openid-connect/token")
	} else {
		form.Set("grant_type", "password")
		form.Set("client_id", DefaultAdminClientID)
		form.Set("username", strings.TrimSpace(a.cfg.AdminUser))
		form.Set("password", a.cfg.AdminPassword)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if _, err := a.postForm(ctx, a.api, op, tokenURL, form, withUserAgent(nil), &payload); err != nil {
		return "", 0, err
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", 0, errs.New(errs.ClassProviderBug, op, "keycloak admin token response is missing an access token")
	}
	ttl := time.Duration(payload.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	ttl -= adminTokenSafety
	if ttl < 5*time.Second {
		// Very short-lived tokens: refresh half-way instead of thrashing.
		ttl = time.Duration(payload.ExpiresIn) * time.Second / 2
		if ttl < 5*time.Second {
			ttl = 5 * time.Second
		}
	}
	return payload.AccessToken, ttl, nil
}
