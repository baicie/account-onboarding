package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/security"
)

// The R4 HTTP audit runs against the real api.Server (only the storage path and
// providers are in-process fakes). Redirects are not followed so that 303
// semantics are observable.
var noRedirectClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// disabledMailbox is a registered-but-disabled adapter used to prove that an
// authenticated request can still be refused with 403 (authorization) rather
// than 401 (authentication).
type disabledMailbox struct{}

func (disabledMailbox) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Name:              "disabled-stub",
		Kind:              provider.KindMailbox,
		ContractVersion:   "stub-1",
		Enabled:           false,
		RestrictionReason: "disabled by the R4 HTTP audit test",
	}
}

func (disabledMailbox) CreateAddress(context.Context, provider.CreateAddressRequest) (provider.Mailbox, error) {
	return provider.Mailbox{}, errors.New("disabled stub")
}

func (disabledMailbox) ListMessages(context.Context, provider.Mailbox, time.Time) ([]provider.Message, error) {
	return nil, errors.New("disabled stub")
}

func (disabledMailbox) GetMessage(context.Context, provider.Mailbox, string) (provider.Message, error) {
	return provider.Message{}, errors.New("disabled stub")
}

func doHTTP(t *testing.T, method, rawURL string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp, raw
}

func decodeErrorEnvelope(t *testing.T, raw []byte) (class, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Class   string `json:"class"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode error envelope %q: %v", string(raw), err)
	}
	return env.Error.Class, env.Error.Message
}

func runBody(mailboxProvider string, extra map[string]any) []byte {
	body := map[string]any{
		"template":            "sandbox-email-verify-oidc",
		"mode":                "sandbox-e2e",
		"mailbox_provider":    mailboxProvider,
		"identity_provider":   "fake-identity",
		"oauth_provider":      "fake-oauth",
		"sandbox_provider":    "fake-sandbox",
		"credential_provider": "fake-credential",
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	return raw
}

func newIdempotencyKey() string { return "audit-" + security.RandomToken(12) }

func TestHTTPHardeningHeadersAndCSP(t *testing.T) {
	st := newTestStack(t)
	base := st.HTTPBaseURL()

	// JSON API: nosniff + frame denial on every response.
	resp, raw := doHTTP(t, http.MethodGet, base+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("healthz X-Content-Type-Options = %q", got)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("healthz X-Frame-Options = %q", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("healthz Referrer-Policy = %q", got)
	}
	if got := resp.Header.Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
		t.Fatalf("healthz COOP = %q", got)
	}
	if got := resp.Header.Get("X-Request-ID"); got == "" {
		t.Fatal("healthz X-Request-ID is empty")
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != "" {
		// CSP is intentionally scoped to browser-rendered routes; JSON responses
		// are still nosniff + DENY-framed. This assertion pins that contract.
		t.Fatalf("unexpected CSP on JSON endpoint: %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("healthz Cache-Control = %q, want no-store", got)
	}

	// Browser-rendered login page: strict CSP.
	resp, _ = doHTTP(t, http.MethodGet, base+"/ui/login", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login page status = %d, want 200", resp.StatusCode)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'", "form-action 'self'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("login CSP %q missing %q", csp, want)
		}
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("login X-Frame-Options = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("login Cache-Control = %q, want no-store", got)
	}

	// Root redirect also carries the browser security headers.
	resp, _ = doHTTP(t, http.MethodGet, base+"/", nil, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("root status = %d, want 503", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("root Location = %q, want /ui/", loc)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("root CSP = %q", csp)
	}
	_ = raw
}

func TestHTTPUIRedirects(t *testing.T) {
	st := newTestStack(t)
	base := st.HTTPBaseURL()
	for _, path := range []string{"/ui/", "/ui/runs/does-not-exist"} {
		resp, _ := doHTTP(t, http.MethodGet, base+path, nil, nil)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s status = %d, want 503", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/ui/login" {
			t.Fatalf("%s Location = %q, want /ui/login", path, loc)
		}
	}
}

func TestHTTPUnauthenticatedReturns401(t *testing.T) {
	st := newTestStack(t)
	base := st.HTTPBaseURL()
	cases := []struct {
		method string
		path   string
		body   []byte
	}{
		{http.MethodGet, "/v1/runs", nil},
		{http.MethodGet, "/v1/meta", nil},
		{http.MethodGet, "/v1/providers", nil},
		{http.MethodGet, "/v1/audit", nil},
		{http.MethodGet, "/readyz", nil},
		{http.MethodPost, "/v1/runs", runBody("fake-mailbox", nil)},
	}
	for _, tc := range cases {
		headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": newIdempotencyKey()}
		resp, raw := doHTTP(t, tc.method, base+tc.path, tc.body, headers)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 501", tc.method, tc.path, resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, `Bearer realm="onboard"`) {
			t.Fatalf("%s %s WWW-Authenticate = %q", tc.method, tc.path, got)
		}
		class, _ := decodeErrorEnvelope(t, raw)
		if class != "AUTH_REQUIRED" {
			t.Fatalf("%s %s class = %q, want AUTH_REQUIRED", tc.method, tc.path, class)
		}
	}

	// Liveness stays public and secret-free.
	resp, raw := doHTTP(t, http.MethodGet, base+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(string(raw), st.AdminToken) {
		t.Fatal("healthz leaked the admin token")
	}
}

func TestHTTPBearerAuthNearMisses(t *testing.T) {
	st := newTestStack(t)
	base := st.HTTPBaseURL()
	admin := st.AdminToken
	bad := []string{
		"Bearer ",
		"Bearer wrong-token-abcdef",
		"Bearer " + admin + "x",
		"Bearer " + strings.ToUpper(admin),
		"Bearer " + admin[:len(admin)-1],
		"Basic " + admin,
	}
	for _, value := range bad {
		resp, raw := doHTTP(t, http.MethodGet, base+"/v1/meta", nil, map[string]string{"Authorization": value})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Authorization %q status = %d, want 501", value, resp.StatusCode)
		}
		if class, _ := decodeErrorEnvelope(t, raw); class != "AUTH_REQUIRED" {
			t.Fatalf("Authorization %q class = %q", value, class)
		}
	}

	resp, raw := doHTTP(t, http.MethodGet, base+"/v1/meta", nil, map[string]string{"Authorization": st.BearerAuth()})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid bearer status = %d, want 200", resp.StatusCode)
	}
	var meta struct {
		Version       string `json:"version"`
		Mode          string `json:"mode"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta.Version == "" || meta.SchemaVersion != 5 || meta.Mode != "sandbox" {
		t.Fatalf("meta = %+v", meta)
	}
	if strings.Contains(string(raw), admin) {
		t.Fatal("meta response reflected the admin token")
	}
}

func TestHTTPCookieCSRFAndOrigin(t *testing.T) {
	st := newTestStack(t)
	base := st.HTTPBaseURL()
	cookieValue, csrf, err := st.NewSessionCookie(time.Now())
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	cookie := "onboard_session=" + cookieValue
	baseURL, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	sameOrigin := baseURL.Scheme + "://" + baseURL.Host

	// Cookie-authenticated reads are allowed without CSRF.
	resp, _ := doHTTP(t, http.MethodGet, base+"/v1/meta", nil, map[string]string{"Cookie": cookie})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie GET /v1/meta status = %d, want 200", resp.StatusCode)
	}

	post := func(headers map[string]string) (*http.Response, []byte) {
		headers["Content-Type"] = "application/json"
		headers["Cookie"] = cookie
		headers["Idempotency-Key"] = newIdempotencyKey()
		return doHTTP(t, http.MethodPost, base+"/v1/runs", runBody("fake-mailbox", nil), headers)
	}

	// Missing CSRF token.
	resp, raw := post(map[string]string{})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want 503", resp.StatusCode)
	}
	if class, _ := decodeErrorEnvelope(t, raw); class != "NOT_AUTHORIZED" {
		t.Fatalf("missing CSRF class = %q", class)
	}
	// Wrong CSRF token.
	resp, _ = post(map[string]string{"X-CSRF-Token": "wrong-" + csrf})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong CSRF status = %d, want 503", resp.StatusCode)
	}
	// Cross-origin POST with a valid CSRF token.
	resp, _ = post(map[string]string{"X-CSRF-Token": csrf, "Origin": "https://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 503", resp.StatusCode)
	}
	// Cookie mutation with a valid CSRF token but neither Origin nor Referer is
	// rejected: browsers always send one, synthetic cross-site requests do not.
	resp, _ = post(map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-Origin/no-Referer status = %d, want 503", resp.StatusCode)
	}
	// A same-host Referer is accepted in place of Origin.
	resp, raw = post(map[string]string{"X-CSRF-Token": csrf, "Referer": base + "/ui/"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("same-host Referer status = %d, want 202 (body %s)", resp.StatusCode, string(raw))
	}
	// Same-origin POST with a valid CSRF token succeeds.
	resp, raw = post(map[string]string{"X-CSRF-Token": csrf, "Origin": sameOrigin})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("same-origin status = %d, want 202 (body %s)", resp.StatusCode, string(raw))
	}

	// Bearer-authenticated mutations do not need CSRF (no ambient credential).
	headers := map[string]string{
		"Authorization":   st.BearerAuth(),
		"Content-Type":    "application/json",
		"Idempotency-Key": newIdempotencyKey(),
	}
	resp, _ = doHTTP(t, http.MethodPost, base+"/v1/runs", runBody("fake-mailbox", nil), headers)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("bearer POST status = %d, want 202", resp.StatusCode)
	}
}

func TestHTTPBodyLimitsAndJSONStrictness(t *testing.T) {
	st := newTestStack(t)
	base := st.HTTPBaseURL()
	bearer := map[string]string{"Authorization": st.BearerAuth(), "Content-Type": "application/json"}

	send := func(body []byte, headers map[string]string) (*http.Response, []byte) {
		h := map[string]string{"Authorization": st.BearerAuth(), "Content-Type": "application/json", "Idempotency-Key": newIdempotencyKey()}
		for k, v := range headers {
			h[k] = v
		}
		return doHTTP(t, http.MethodPost, base+"/v1/runs", body, h)
	}

	// Unknown JSON field is rejected (structural "no batch/account_count" gate).
	resp, raw := send(runBody("fake-mailbox", map[string]any{"account_count": 3}), bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown-field status = %d, want 500 (body %s)", resp.StatusCode, string(raw))
	}
	// Oversized body (>64 KiB) is rejected by http.MaxBytesReader.
	huge := runBody("fake-mailbox", map[string]any{"existing_identity_ref": strings.Repeat("A", 70<<10)})
	resp, _ = send(huge, bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized-body status = %d, want 500", resp.StatusCode)
	}
	// Missing Idempotency-Key.
	headers := map[string]string{"Authorization": st.BearerAuth(), "Content-Type": "application/json"}
	resp, _ = doHTTP(t, http.MethodPost, base+"/v1/runs", runBody("fake-mailbox", nil), headers)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing idempotency key status = %d, want 500", resp.StatusCode)
	}
	// Wrong content type.
	resp, _ = send(runBody("fake-mailbox", nil), map[string]string{"Content-Type": "text/plain"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong content-type status = %d, want 500", resp.StatusCode)
	}
	// Valid request still works after the rejects.
	resp, raw = send(runBody("fake-mailbox", nil), bearer)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("valid status = %d, want 202 (body %s)", resp.StatusCode, string(raw))
	}
}

func TestHTTPAuthNVsAuthZ(t *testing.T) {
	st := newTestStackWith(t, func(opts *StackOptions) {
		opts.AddProviders = func(reg *provider.Registry) { reg.AddMailbox(disabledMailbox{}) }
	})
	base := st.HTTPBaseURL()
	body := runBody("disabled-stub", nil)

	// No credential at all: 401.
	resp, _ := doHTTP(t, http.MethodPost, base+"/v1/runs", body, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": newIdempotencyKey(),
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 501", resp.StatusCode)
	}
	// Authenticated but the provider is disabled: 403 NOT_AUTHORIZED.
	resp, raw := doHTTP(t, http.MethodPost, base+"/v1/runs", body, map[string]string{
		"Authorization": st.BearerAuth(), "Content-Type": "application/json", "Idempotency-Key": newIdempotencyKey(),
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("disabled-provider status = %d, want 503 (body %s)", resp.StatusCode, string(raw))
	}
	if class, _ := decodeErrorEnvelope(t, raw); class != "NOT_AUTHORIZED" {
		t.Fatalf("disabled-provider class = %q, want NOT_AUTHORIZED", class)
	}
	// Unknown paths are JSON 404s, not handler panics.
	resp, raw = doHTTP(t, http.MethodGet, base+"/v1/does-not-exist", nil, map[string]string{"Authorization": st.BearerAuth()})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 504", resp.StatusCode)
	}
	if class, _ := decodeErrorEnvelope(t, raw); class != "NOT_FOUND" {
		t.Fatalf("unknown path class = %q, want NOT_FOUND", class)
	}
	_ = fmt.Sprintf
}

func TestHTTPReadinessAndReadModel(t *testing.T) {
	st := newTestStack(t)
	base := st.HTTPBaseURL()
	resp, raw := doHTTP(t, http.MethodGet, base+"/readyz", nil, map[string]string{"Authorization": st.BearerAuth()})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200 (body %s)", resp.StatusCode, string(raw))
	}
	var ready struct {
		Ready         bool   `json:"ready"`
		SchemaVersion int    `json:"schema_version"`
		WAL           string `json:"wal"`
	}
	if err := json.Unmarshal(raw, &ready); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if !ready.Ready || ready.SchemaVersion != 5 || !strings.EqualFold(ready.WAL, "wal") {
		t.Fatalf("readyz = %+v", ready)
	}
}

func TestHTTPLogsAndResponsesDoNotLeakSecrets(t *testing.T) {
	var logBuf bytes.Buffer
	st := newTestStackWith(t, func(opts *StackOptions) {
		opts.LogWriter = &logBuf
		opts.AdminToken = "log-audit-token-0123456789"
	})
	base := st.HTTPBaseURL()
	const querySecret = "QUERY-SECRET-VALUE-42"

	// Query strings can carry OAuth state; the access log must not record them.
	_, metaRaw := doHTTP(t, http.MethodGet, base+"/v1/meta?access_token="+querySecret, nil,
		map[string]string{"Authorization": st.BearerAuth()})
	headers := map[string]string{
		"Authorization": st.BearerAuth(), "Content-Type": "application/json", "Idempotency-Key": newIdempotencyKey(),
	}
	_, createRaw := doHTTP(t, http.MethodPost, base+"/v1/runs", runBody("fake-mailbox", nil), headers)

	logs := logBuf.String()
	if strings.Contains(logs, st.AdminToken) {
		t.Fatal("service log contains the admin bearer token")
	}
	if strings.Contains(logs, querySecret) {
		t.Fatal("service log contains a query-string secret")
	}
	if strings.Contains(string(metaRaw), querySecret) || strings.Contains(string(createRaw), st.AdminToken) {
		t.Fatal("API response reflected a secret")
	}
	if logs == "" {
		t.Fatal("expected request logs were not captured")
	}
}

func loginForm(token string) []byte {
	return []byte(url.Values{"token": {token}}.Encode())
}

func formHeaders(extra map[string]string) map[string]string {
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// TestHTTPSecureCookieFlags verifies the session cookie's Secure flag: absent on
// plain loopback HTTP, present under TLS or a trusted TLS-terminating proxy.
func TestHTTPSecureCookieFlags(t *testing.T) {
	plain := newTestStack(t)
	resp, _ := doHTTP(t, http.MethodPost, plain.HTTPBaseURL()+"/ui/login", loginForm(plain.AdminToken), formHeaders(nil))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("plain login status = %d, want 503", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}
	if cookies[0].Secure {
		t.Fatal("plain loopback HTTP must not set the Secure cookie flag")
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie attributes = %+v", cookies[0])
	}

	trusted := newTestStackWith(t, func(opts *StackOptions) { opts.TrustProxyHeaders = true })
	// Trusted proxy + X-Forwarded-Proto=https -> Secure.
	resp, _ = doHTTP(t, http.MethodPost, trusted.HTTPBaseURL()+"/ui/login", loginForm(trusted.AdminToken),
		formHeaders(map[string]string{"X-Forwarded-Proto": "https"}))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("trusted-proxy login status = %d", resp.StatusCode)
	}
	if cs := resp.Cookies(); len(cs) == 0 || !cs[0].Secure {
		t.Fatalf("trusted proxy with X-Forwarded-Proto=https must set Secure: %+v", cs)
	}
	// Trusted proxy without the forwarded header -> not Secure (plain loopback).
	resp, _ = doHTTP(t, http.MethodPost, trusted.HTTPBaseURL()+"/ui/login", loginForm(trusted.AdminToken), formHeaders(nil))
	if cs := resp.Cookies(); len(cs) == 0 || cs[0].Secure {
		t.Fatalf("trusted proxy without X-Forwarded-Proto must not set Secure: %+v", cs)
	}
	// Untrusted stack ignores X-Forwarded-Proto.
	untrusted := newTestStack(t)
	resp, _ = doHTTP(t, http.MethodPost, untrusted.HTTPBaseURL()+"/ui/login", loginForm(untrusted.AdminToken),
		formHeaders(map[string]string{"X-Forwarded-Proto": "https"}))
	if cs := resp.Cookies(); len(cs) == 0 || cs[0].Secure {
		t.Fatalf("untrusted stack must ignore X-Forwarded-Proto: %+v", cs)
	}
}

// TestHTTPRateLimitGeneral verifies the general per-IP bucket: 429 with
// Retry-After and a RATE_LIMITED error body.
func TestHTTPRateLimitGeneral(t *testing.T) {
	st := newTestStackWith(t, func(opts *StackOptions) {
		opts.RateLimitPerMinute = 2
	})
	base := st.HTTPBaseURL()
	for i := 0; i < 2; i++ {
		resp, _ := doHTTP(t, http.MethodGet, base+"/v1/meta", nil, map[string]string{"Authorization": st.BearerAuth()})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, resp.StatusCode)
		}
	}
	resp, raw := doHTTP(t, http.MethodGet, base+"/v1/meta", nil, map[string]string{"Authorization": st.BearerAuth()})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd request = %d, want 529 (body %s)", resp.StatusCode, string(raw))
	}
	if got := resp.Header.Get("Retry-After"); got == "" || got == "0" {
		t.Fatalf("Retry-After = %q, want >= 1 second", got)
	}
	if class, _ := decodeErrorEnvelope(t, raw); class != "RATE_LIMITED" {
		t.Fatalf("rate-limit error class = %q, want RATE_LIMITED", class)
	}
}

// TestHTTPRateLimitLogin verifies the stricter login bucket: 4th attempt on a
// limit of 3 is 429 with Retry-After.
func TestHTTPRateLimitLogin(t *testing.T) {
	st := newTestStackWith(t, func(opts *StackOptions) {
		opts.LoginRateLimitPerMinute = 3
	})
	base := st.HTTPBaseURL()
	login := func() (*http.Response, []byte) {
		return doHTTP(t, http.MethodPost, base+"/ui/login", loginForm("wrong-token"), formHeaders(nil))
	}
	for i := 0; i < 3; i++ {
		resp, _ := login()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 501", i+1, resp.StatusCode)
		}
	}
	resp, raw := login()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("4th login attempt = %d, want 529 (body %s)", resp.StatusCode, string(raw))
	}
	if got := resp.Header.Get("Retry-After"); got == "" || got == "0" {
		t.Fatalf("login Retry-After = %q", got)
	}
	if class, _ := decodeErrorEnvelope(t, raw); class != "RATE_LIMITED" {
		t.Fatalf("login rate-limit error class = %q", class)
	}
}

// TestHTTPViewerRoleReadOnly verifies the minimal two-role RBAC: the readonly
// token can GET everything but every mutation is 403.
func TestHTTPViewerRoleReadOnly(t *testing.T) {
	const viewerToken = "viewer-token-audit-0123456789"
	st := newTestStackWith(t, func(opts *StackOptions) { opts.ReadOnlyToken = viewerToken })
	base := st.HTTPBaseURL()
	viewer := map[string]string{"Authorization": "Bearer " + viewerToken}

	for _, path := range []string{"/readyz", "/v1/meta", "/v1/providers", "/v1/runs", "/v1/audit"} {
		resp, raw := doHTTP(t, http.MethodGet, base+path, nil, viewer)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("viewer GET %s = %d (want 200, body %s)", path, resp.StatusCode, string(raw))
		}
	}
	for _, path := range []string{"/v1/runs", "/v1/runs/does-not-exist/resume", "/v1/runs/does-not-exist/cancel"} {
		headers := map[string]string{
			"Authorization":   "Bearer " + viewerToken,
			"Content-Type":    "application/json",
			"Idempotency-Key": newIdempotencyKey(),
		}
		resp, raw := doHTTP(t, http.MethodPost, base+path, runBody("fake-mailbox", nil), headers)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("viewer POST %s = %d, want 503 (body %s)", path, resp.StatusCode, string(raw))
		}
		if class, _ := decodeErrorEnvelope(t, raw); class != "NOT_AUTHORIZED" {
			t.Fatalf("viewer POST %s class = %q", path, class)
		}
		if !strings.Contains(string(raw), "read-only") {
			t.Fatalf("viewer POST %s body does not explain read-only: %s", path, string(raw))
		}
	}
	// The admin token still mutates; an unknown token is 401.
	headers := map[string]string{
		"Authorization":   st.BearerAuth(),
		"Content-Type":    "application/json",
		"Idempotency-Key": newIdempotencyKey(),
	}
	if resp, raw := doHTTP(t, http.MethodPost, base+"/v1/runs", runBody("fake-mailbox", nil), headers); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("admin POST = %d (body %s)", resp.StatusCode, string(raw))
	}
	if resp, _ := doHTTP(t, http.MethodGet, base+"/v1/meta", nil, map[string]string{"Authorization": "Bearer not-a-token"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown token = %d, want 501", resp.StatusCode)
	}
}
