package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

const (
	testToken = "ghp_SuperSecretTokenValue1234567890ABCDEF"
	testRef   = "secret://github/pat"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeResolver is an in-memory provider.SecretResolver. It can return a
// sequence of tokens to prove resolution happens per call, not at construction.
type fakeResolver struct {
	mu      sync.Mutex
	tokens  []string
	err     error
	calls   int
	refs    []string
	blocked bool // when true, Resolve must never be called (disabled gate)
}

func newResolver(tokens ...string) *fakeResolver {
	if len(tokens) == 0 {
		tokens = []string{testToken}
	}
	return &fakeResolver{tokens: tokens}
}

func (f *fakeResolver) Resolve(_ context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.refs = append(f.refs, ref)
	if f.blocked {
		panic("secret resolver must not be called by a disabled adapter")
	}
	if f.err != nil {
		return "", f.err
	}
	idx := f.calls - 1
	if idx >= len(f.tokens) {
		idx = len(f.tokens) - 1
	}
	return f.tokens[idx], nil
}

func (f *fakeResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeResolver) lastRef() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.refs) == 0 {
		return ""
	}
	return f.refs[len(f.refs)-1]
}

type recordedRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

// fakeAPI is a strict GitHub REST stand-in: it records every request, asserts
// that only allowlisted endpoint paths are used, and asserts the token never
// travels in a URL.
type fakeAPI struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recordedRequest
	// handler serves the request; tests set it before invoking the adapter.
	handler http.HandlerFunc
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recordedRequest{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			header: r.Header.Clone(),
			body:   body,
		})
		handler := f.handler
		f.mu.Unlock()

		if _, ok := allowedPaths[r.URL.Path]; !ok {
			t.Errorf("adapter requested a non-allowlisted path %q", r.URL.Path)
			http.Error(w, "not allowlisted", http.StatusTeapot)
			return
		}
		if strings.Contains(r.URL.String(), testToken) {
			t.Errorf("token leaked into the request URL: %s", r.URL)
		}
		if handler == nil {
			http.Error(w, "handler not configured", http.StatusInternalServerError)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) setHandler(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
}

func (f *fakeAPI) requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

func (f *fakeAPI) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// newTestAdapter builds an adapter against the fake API with the loopback test
// escape hatch enabled.
func newTestAdapter(t *testing.T, api *fakeAPI, resolver provider.SecretResolver, mutate ...func(*Config)) *Adapter {
	t.Helper()
	cfg := Config{
		BaseURL:           api.srv.URL,
		AllowLoopbackHTTP: true,
		TokenRef:          testRef,
		Timeout:           5 * time.Second,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := New(cfg, resolver)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// assertClass asserts the errs taxonomy class and that the summary is
// public-safe (no token, no upstream body echo).
func assertClass(t *testing.T, err error, want errs.Class, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s error, got nil", want)
	}
	if got := errs.ClassOf(err); got != want {
		t.Fatalf("class = %s, want %s (err: %v)", got, want, err)
	}
	assertNoLeak(t, err, secrets...)
}

func assertNoLeak(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		return
	}
	haystacks := []string{err.Error(), errs.SummaryOf(err), fmt.Sprintf("%v", err)}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, hay := range haystacks {
			if strings.Contains(hay, secret) {
				t.Fatalf("error output leaked a secret: %q", hay)
			}
		}
	}
}

// assertOnlyAllowlistedPaths verifies every recorded request stayed inside the
// two-endpoint compliance boundary.
func assertOnlyAllowlistedPaths(t *testing.T, reqs []recordedRequest) {
	t.Helper()
	for _, req := range reqs {
		if _, ok := allowedPaths[req.path]; !ok {
			t.Fatalf("request left the allowlist: %s %s", req.method, req.path)
		}
		if req.method != http.MethodGet && req.method != http.MethodPost {
			t.Fatalf("unexpected HTTP method %s", req.method)
		}
	}
}

// ---------------------------------------------------------------------------
// contract: ListEmails
// ---------------------------------------------------------------------------

func TestListEmailsVerifiedAndUnverified(t *testing.T) {
	api := newFakeAPI(t)
	const body = `[
		{"email":"primary@moonbyte.top","primary":true,"verified":true,"visibility":"public"},
		{"email":"alias@moonbyte.top","primary":false,"verified":false,"visibility":"private"}
	]`
	api.setHandler(jsonHandler(http.StatusOK, body))

	resolver := newResolver()
	a := newTestAdapter(t, api, resolver)

	got, err := a.ListEmails(context.Background())
	if err != nil {
		t.Fatalf("ListEmails: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d addresses, want 2", len(got))
	}
	if got[0].Email != "primary@moonbyte.top" || !got[0].Verified || !got[0].Primary || got[0].Visibility != "public" {
		t.Fatalf("unexpected first address: %+v", got[0])
	}
	if got[1].Email != "alias@moonbyte.top" || got[1].Verified || got[1].Primary || got[1].Visibility != "private" {
		t.Fatalf("unexpected second address: %+v", got[1])
	}

	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.method != http.MethodGet || req.path != "/user/emails" {
		t.Fatalf("unexpected request: %s %s", req.method, req.path)
	}
	if !strings.Contains(req.query, "per_page=100") {
		t.Fatalf("expected per_page=100, got query %q", req.query)
	}
	if got := req.header.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.header.Get("Accept"); got != "application/vnd.github+json" {
		t.Fatalf("Accept = %q", got)
	}
	if got := req.header.Get("X-GitHub-Api-Version"); got != DefaultAPIVersion {
		t.Fatalf("X-GitHub-Api-Version = %q", got)
	}
	if got := req.header.Get("User-Agent"); got != DefaultUserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	assertOnlyAllowlistedPaths(t, reqs)
}

func TestListEmailsEmpty(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusOK, `[]`))

	got, err := newTestAdapter(t, api, newResolver()).ListEmails(context.Background())
	if err != nil {
		t.Fatalf("ListEmails: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d addresses, want 0", len(got))
	}
}

func TestListEmailsSchemaViolationsAreProviderBug(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"object instead of array", `{"email":"a@b.co"}`},
		{"not json", `<html>nope</html>`},
		{"entry without email", `[{"verified":true}]`},
		{"empty body", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.setHandler(jsonHandler(http.StatusOK, tc.body))
			_, err := newTestAdapter(t, api, newResolver()).ListEmails(context.Background())
			assertClass(t, err, errs.ClassProviderBug, testToken)
		})
	}
}

// ---------------------------------------------------------------------------
// contract: HTTP status mapping
// ---------------------------------------------------------------------------

func TestStatusMapping(t *testing.T) {
	const githubError = `{"message":"resource error","documentation_url":"https://docs.github.com"}`
	cases := []struct {
		name       string
		status     int
		headers    map[string]string
		body       string
		wantClass  errs.Class
		wantHTTP   int
		wantRetry  time.Duration
		checkRetry bool
	}{
		{name: "401 bad credentials", status: 401, body: githubError, wantClass: errs.ClassNotAuthorized, wantHTTP: 401},
		{name: "403 forbidden", status: 403, body: githubError, wantClass: errs.ClassNotAuthorized, wantHTTP: 403},
		{
			name: "403 exhausted primary rate limit", status: 403,
			headers:   map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1700000000", "Retry-After": "60"},
			body:      githubError,
			wantClass: errs.ClassRateLimited, wantHTTP: 403, wantRetry: 60 * time.Second, checkRetry: true,
		},
		{name: "404 missing", status: 404, body: githubError, wantClass: errs.ClassValidation, wantHTTP: 404},
		{name: "409 conflict", status: 409, body: githubError, wantClass: errs.ClassValidation, wantHTTP: 409},
		{name: "422 unprocessable", status: 422, body: githubError, wantClass: errs.ClassValidation, wantHTTP: 422},
		{name: "429 rate limited", status: 429, headers: map[string]string{"Retry-After": "120"}, body: githubError, wantClass: errs.ClassRateLimited, wantHTTP: 429, wantRetry: 120 * time.Second, checkRetry: true},
		{name: "500 server error", status: 500, body: githubError, wantClass: errs.ClassUpstreamTransient, wantHTTP: 500},
		{name: "502 bad gateway", status: 502, body: githubError, wantClass: errs.ClassUpstreamTransient, wantHTTP: 502},
		{name: "503 unavailable", status: 503, body: githubError, wantClass: errs.ClassUpstreamTransient, wantHTTP: 503},
		{name: "400 bad request", status: 400, body: githubError, wantClass: errs.ClassValidation, wantHTTP: 400},
		{name: "408 request timeout", status: 408, body: githubError, wantClass: errs.ClassTimeoutUncertain, wantHTTP: 408},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.setHandler(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := newTestAdapter(t, api, newResolver()).ListEmails(context.Background())
			assertClass(t, err, tc.wantClass, testToken)
			if got := errs.HTTPStatusOf(err); got != tc.wantHTTP {
				t.Fatalf("HTTPStatus = %d, want %d", got, tc.wantHTTP)
			}
			if tc.checkRetry {
				if got := errs.RetryAfterOf(err); got != tc.wantRetry {
					t.Fatalf("RetryAfter = %v, want %v", got, tc.wantRetry)
				}
			}
		})
	}
}

func TestRateLimitRetryAfterHTTPDate(t *testing.T) {
	api := newFakeAPI(t)
	retryAt := time.Now().Add(90 * time.Second).UTC()
	api.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", retryAt.Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"slow down"}`)
	})
	_, err := newTestAdapter(t, api, newResolver()).ListEmails(context.Background())
	assertClass(t, err, errs.ClassRateLimited, testToken)
	retry := errs.RetryAfterOf(err)
	if retry <= 0 || retry > 90*time.Second {
		t.Fatalf("RetryAfter = %v, want in (0s, 90s]", retry)
	}
}

func TestTimeoutClassifiedAsTimedOutUncertain(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `[]`)
	})
	a := newTestAdapter(t, api, newResolver(), func(c *Config) { c.Timeout = 40 * time.Millisecond })
	_, err := a.ListEmails(context.Background())
	assertClass(t, err, errs.ClassTimeoutUncertain, testToken)
}

func TestContextCancelledClassifiedAsCancelled(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusOK, `[]`))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newTestAdapter(t, api, newResolver()).ListEmails(ctx)
	assertClass(t, err, errs.ClassCancelled, testToken)
}

func TestCrossHostRedirectRefused(t *testing.T) {
	var otherHits int
	var otherAuth []string
	var otherMu sync.Mutex
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherMu.Lock()
		otherHits++
		otherAuth = append(otherAuth, r.Header.Get("Authorization"))
		otherMu.Unlock()
		_, _ = io.WriteString(w, `[]`)
	}))
	defer other.Close()

	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/user/emails", http.StatusTemporaryRedirect)
	})

	_, err := newTestAdapter(t, api, newResolver()).ListEmails(context.Background())
	assertClass(t, err, errs.ClassProviderBug, testToken)
	if !strings.Contains(errs.SummaryOf(err), "redirect") {
		t.Fatalf("summary should explain the refused redirect, got %q", errs.SummaryOf(err))
	}
	otherMu.Lock()
	defer otherMu.Unlock()
	if otherHits != 0 {
		t.Fatalf("secondary host received %d requests, want 0", otherHits)
	}
	for _, auth := range otherAuth {
		if auth != "" {
			t.Fatalf("credential was forwarded to the redirect target: %q", auth)
		}
	}
}

// ---------------------------------------------------------------------------
// contract: AddEmail
// ---------------------------------------------------------------------------

func TestAddEmailCreated(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusCreated, `[
		{"email":"github@moonbyte.top","primary":false,"verified":false,"visibility":"private"}
	]`))

	resolver := newResolver()
	a := newTestAdapter(t, api, resolver)
	got, err := a.AddEmail(context.Background(), "github@moonbyte.top")
	if err != nil {
		t.Fatalf("AddEmail: %v", err)
	}
	if got.Email != "github@moonbyte.top" || got.Verified {
		t.Fatalf("unexpected result: %+v", got)
	}
	reqs := api.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.method != http.MethodPost || req.path != "/user/emails" {
		t.Fatalf("unexpected request: %s %s", req.method, req.path)
	}
	if ct := req.header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var payload struct {
		Emails []string `json:"emails"`
	}
	if err := json.Unmarshal(req.body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload.Emails) != 1 || payload.Emails[0] != "github@moonbyte.top" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if resolver.callCount() != 1 || resolver.lastRef() != testRef {
		t.Fatalf("token resolution: calls=%d ref=%q", resolver.callCount(), resolver.lastRef())
	}
	assertOnlyAllowlistedPaths(t, reqs)
}

func TestAddEmailDuplicate422ReconciledAsSuccess(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"Validation Failed","errors":[{"resource":"Email","code":"already_exists","field":"email"}]}`)
			return
		}
		jsonHandler(http.StatusOK, `[{"email":"github@moonbyte.top","primary":true,"verified":true,"visibility":"public"}]`)(w, r)
	})

	got, err := newTestAdapter(t, api, newResolver()).AddEmail(context.Background(), "github@moonbyte.top")
	if err != nil {
		t.Fatalf("AddEmail must reconcile a duplicate as success, got %v", err)
	}
	if !got.Verified || !got.Primary {
		t.Fatalf("expected the reconciled verified record, got %+v", got)
	}
	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want POST then GET", len(reqs))
	}
	if reqs[1].method != http.MethodGet || reqs[1].path != "/user/emails" {
		t.Fatalf("reconcile request = %s %s", reqs[1].method, reqs[1].path)
	}
}

func TestAddEmailConflict409ReconciledAsSuccess(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"message":"conflict"}`)
			return
		}
		jsonHandler(http.StatusOK, `[{"email":"github@moonbyte.top","verified":false}]`)(w, r)
	})

	got, err := newTestAdapter(t, api, newResolver()).AddEmail(context.Background(), "github@moonbyte.top")
	if err != nil {
		t.Fatalf("AddEmail: %v", err)
	}
	if got.Email != "github@moonbyte.top" || got.Verified {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestAddEmailDuplicateNotVisibleIsValidationFailure(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"message":"conflict"}`)
			return
		}
		jsonHandler(http.StatusOK, `[{"email":"someone-else@moonbyte.top","verified":true}]`)(w, r)
	})

	_, err := newTestAdapter(t, api, newResolver()).AddEmail(context.Background(), "github@moonbyte.top")
	assertClass(t, err, errs.ClassValidation, testToken)
	if !strings.Contains(strings.ToLower(errs.SummaryOf(err)), "reconcile") {
		t.Fatalf("summary should require manual reconciliation, got %q", errs.SummaryOf(err))
	}
}

func TestAddEmail422InvalidIsTerminalValidation(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusUnprocessableEntity,
		`{"message":"Validation Failed","errors":[{"resource":"Email","code":"invalid","field":"email"}]}`))

	_, err := newTestAdapter(t, api, newResolver()).AddEmail(context.Background(), "github@moonbyte.top")
	assertClass(t, err, errs.ClassValidation, testToken)
	if n := api.requestCount(); n != 1 {
		t.Fatalf("invalid 422 must not be reconciled, got %d requests", n)
	}
}

func TestAddEmailCreatedButNotEchoedReconciles(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, ``)
			return
		}
		jsonHandler(http.StatusOK, `[{"email":"github@moonbyte.top","verified":false}]`)(w, r)
	})

	got, err := newTestAdapter(t, api, newResolver()).AddEmail(context.Background(), "github@moonbyte.top")
	if err != nil {
		t.Fatalf("AddEmail: %v", err)
	}
	if got.Email != "github@moonbyte.top" || got.Verified {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestAddEmailCreatedUnexpectedBodyIsProviderBug(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			jsonHandler(http.StatusCreated, `{"message":"not an array"}`)(w, r)
			return
		}
		jsonHandler(http.StatusOK, `[]`)(w, r)
	})

	_, err := newTestAdapter(t, api, newResolver()).AddEmail(context.Background(), "github@moonbyte.top")
	assertClass(t, err, errs.ClassProviderBug, testToken)
}

func TestAddEmailRejectsInvalidAddressWithoutNetwork(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"not-an-email",
		"user@",
		"@domain.top",
		"Display Name <ops@moonbyte.top>",
		"a@b",
		"two@@moonbyte.top",
		"ops@moonbyte.top extra",
		strings.Repeat("a", 250) + "@moonbyte.top",
	}
	for _, email := range cases {
		t.Run(fmt.Sprintf("%q", email), func(t *testing.T) {
			api := newFakeAPI(t)
			api.setHandler(jsonHandler(http.StatusCreated, `[]`))
			_, err := newTestAdapter(t, api, newResolver()).AddEmail(context.Background(), email)
			assertClass(t, err, errs.ClassValidation, testToken)
			if n := api.requestCount(); n != 0 {
				t.Fatalf("invalid address reached the network (%d requests)", n)
			}
		})
	}
}

func TestAddEmailDomainAllowlist(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusCreated, `[{"email":"github@moonbyte.top","verified":false}]`))
	a := newTestAdapter(t, api, newResolver(), func(c *Config) {
		c.AllowedDomains = []string{" MoonByte.TOP ", "@moonbyte.top"}
	})

	if _, err := a.AddEmail(context.Background(), "github@moonbyte.top"); err != nil {
		t.Fatalf("allowed domain rejected: %v", err)
	}
	before := api.requestCount()
	_, err := a.AddEmail(context.Background(), "github@evil.example")
	assertClass(t, err, errs.ClassValidation, testToken)
	if n := api.requestCount(); n != before {
		t.Fatalf("disallowed domain reached the network")
	}
}

// ---------------------------------------------------------------------------
// contract: token handling
// ---------------------------------------------------------------------------

func TestTokenResolvedPerCall(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusOK, `[]`))
	resolver := newResolver("token-one", "token-two")
	a := newTestAdapter(t, api, resolver)
	if resolver.callCount() != 0 {
		t.Fatalf("constructor must not resolve the token (calls=%d)", resolver.callCount())
	}

	if _, err := a.ListEmails(context.Background()); err != nil {
		t.Fatalf("ListEmails: %v", err)
	}
	if _, err := a.ListEmails(context.Background()); err != nil {
		t.Fatalf("ListEmails: %v", err)
	}
	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(reqs))
	}
	if got := reqs[0].header.Get("Authorization"); got != "Bearer token-one" {
		t.Fatalf("first Authorization = %q", got)
	}
	if got := reqs[1].header.Get("Authorization"); got != "Bearer token-two" {
		t.Fatalf("rotated Authorization = %q", got)
	}
}

func TestTokenResolutionFailureIsNotAuthorizedWithoutNetwork(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusOK, `[]`))
	resolver := newResolver()
	resolver.err = errors.New("vault unavailable: ghp_should_not_surface")
	a := newTestAdapter(t, api, resolver)

	_, listErr := a.ListEmails(context.Background())
	assertClass(t, listErr, errs.ClassNotAuthorized, testToken, "ghp_should_not_surface")
	_, addErr := a.AddEmail(context.Background(), "github@moonbyte.top")
	assertClass(t, addErr, errs.ClassNotAuthorized, testToken)
	if n := api.requestCount(); n != 0 {
		t.Fatalf("token resolution failure still triggered %d requests", n)
	}
}

func TestEmptyResolvedTokenIsNotAuthorized(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusOK, `[]`))
	_, err := newTestAdapter(t, api, newResolver("   ")).ListEmails(context.Background())
	assertClass(t, err, errs.ClassNotAuthorized, testToken)
	if n := api.requestCount(); n != 0 {
		t.Fatalf("empty token still triggered %d requests", n)
	}
}

func TestAdapterStructHoldsNoTokenMaterial(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusOK, `[]`))
	a := newTestAdapter(t, api, newResolver())

	// Walk the adapter's own fields (never the injected resolver, which is the
	// only holder of the plaintext) and fail if the secret is reachable.
	var walk func(v reflect.Value, path string, depth int)
	walk = func(v reflect.Value, path string, depth int) {
		if depth > 4 {
			return
		}
		switch v.Kind() {
		case reflect.String:
			if v.String() == testToken {
				t.Fatalf("adapter stores the raw token at %s", path)
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if !v.Type().Field(i).IsExported() {
					walk(v.Field(i), path+"."+v.Type().Field(i).Name, depth+1)
				}
			}
		case reflect.Interface, reflect.Pointer:
			// Interfaces and pointers are the resolver / client boundary: the
			// adapter must not dereference them to cache a token.
		}
	}
	walk(reflect.ValueOf(a).Elem(), "Adapter", 0)
}

func TestErrorsAndDTOsNeverContainToken(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusOK, `[{"email":"a@moonbyte.top","verified":true}]`))
	a := newTestAdapter(t, api, newResolver())
	list, err := a.ListEmails(context.Background())
	if err != nil {
		t.Fatalf("ListEmails: %v", err)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), testToken) {
		t.Fatalf("email DTO leaked the token: %s", raw)
	}
	desc, err := json.Marshal(a.Descriptor())
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	if strings.Contains(string(desc), testToken) {
		t.Fatalf("descriptor leaked the token: %s", desc)
	}

	// A failure that includes the token reference must stay clean too.
	api.setHandler(jsonHandler(http.StatusUnauthorized, `{"message":"Bad credentials"}`))
	_, err = a.ListEmails(context.Background())
	assertClass(t, err, errs.ClassNotAuthorized, testToken)
	var classErr *errs.Error
	if !errors.As(err, &classErr) {
		t.Fatalf("expected *errs.Error, got %T", err)
	}
	if strings.Contains(classErr.Op, testToken) || strings.Contains(classErr.Summary, testToken) {
		t.Fatalf("structured error leaked the token")
	}
}

// ---------------------------------------------------------------------------
// contract: activation gate and configuration
// ---------------------------------------------------------------------------

func TestDisabledAdapterFailsBeforeNetwork(t *testing.T) {
	resolver := newResolver()
	resolver.blocked = true
	a := NewDisabled("")

	d := a.Descriptor()
	if d.Enabled {
		t.Fatalf("disabled adapter reported Enabled=true")
	}
	if d.RestrictionReason == "" {
		t.Fatalf("disabled adapter must carry an explicit restriction reason")
	}
	if d.Name != ProviderName || d.Kind != provider.KindEmail || d.ContractVersion != ContractVersion {
		t.Fatalf("unexpected descriptor: %+v", d)
	}

	_, addErr := a.AddEmail(context.Background(), "github@moonbyte.top")
	assertClass(t, addErr, errs.ClassNotAuthorized)
	_, listErr := a.ListEmails(context.Background())
	assertClass(t, listErr, errs.ClassNotAuthorized)
	_, verifyErr := a.Verify(context.Background(), testRef)
	assertClass(t, verifyErr, errs.ClassNotAuthorized)
	if resolver.callCount() != 0 {
		t.Fatalf("disabled adapter resolved a secret")
	}
}

func TestNilAdapterFailsClosed(t *testing.T) {
	var a *Adapter
	if a.Descriptor().Enabled {
		t.Fatalf("nil adapter must not be enabled")
	}
	_, err := a.ListEmails(context.Background())
	assertClass(t, err, errs.ClassNotAuthorized)
}

func TestNewRejectsUnsafeConfig(t *testing.T) {
	api := newFakeAPI(t)
	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "missing token ref", cfg: Config{BaseURL: api.srv.URL, AllowLoopbackHTTP: true}},
		{name: "raw token instead of ref", cfg: Config{BaseURL: api.srv.URL, AllowLoopbackHTTP: true, TokenRef: testToken}},
		{name: "public api host via http", cfg: Config{BaseURL: "http://api.github.com", TokenRef: testRef}},
		{name: "arbitrary https host", cfg: Config{BaseURL: "https://evil.example", TokenRef: testRef}},
		{name: "lookalike host", cfg: Config{BaseURL: "https://api.github.com.evil.example", TokenRef: testRef}},
		{name: "base url with path", cfg: Config{BaseURL: "https://api.github.com/v3", TokenRef: testRef}},
		{name: "base url with query", cfg: Config{BaseURL: "https://api.github.com?x=1", TokenRef: testRef}},
		{name: "base url with userinfo", cfg: Config{BaseURL: "https://user:pass@api.github.com", TokenRef: testRef}},
		{name: "unsupported scheme", cfg: Config{BaseURL: "ftp://api.github.com", TokenRef: testRef}},
		{name: "loopback without explicit test flag", cfg: Config{BaseURL: "http://127.0.0.1:9", TokenRef: testRef}},
		{name: "non-loopback private host with test flag", cfg: Config{BaseURL: "http://192.168.1.10:8080", AllowLoopbackHTTP: true, TokenRef: testRef}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg, newResolver()); err == nil {
				t.Fatalf("expected a configuration error")
			} else {
				assertClass(t, err, errs.ClassValidation, testToken)
			}
		})
	}
}

func TestNewRejectsNilResolver(t *testing.T) {
	_, err := New(Config{TokenRef: testRef}, nil)
	assertClass(t, err, errs.ClassValidation)
}

func TestNewAcceptsDocumentedBaseURLs(t *testing.T) {
	cases := []struct {
		name       string
		base       string
		allowLocal bool
	}{
		{name: "canonical", base: "https://api.github.com"},
		{name: "canonical with trailing slash", base: "https://api.github.com/"},
		{name: "loopback http with flag", base: "http://127.0.0.1:8080", allowLocal: true},
		{name: "loopback https with flag", base: "https://127.0.0.1:8443", allowLocal: true},
		{name: "localhost with flag", base: "http://localhost:9999/", allowLocal: true},
		{name: "ipv6 loopback with flag", base: "http://[::1]:8080", allowLocal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := New(Config{BaseURL: tc.base, AllowLoopbackHTTP: tc.allowLocal, TokenRef: testRef}, newResolver())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if a.Descriptor().Enabled != true || a.Descriptor().Live != true {
				t.Fatalf("configured adapter must be enabled and live: %+v", a.Descriptor())
			}
		})
	}
}

func TestDescriptorCapabilities(t *testing.T) {
	api := newFakeAPI(t)
	a := newTestAdapter(t, api, newResolver())
	d := a.Descriptor()
	if d.Name != "github" || d.Kind != provider.KindEmail {
		t.Fatalf("unexpected descriptor: %+v", d)
	}
	want := map[string]bool{"email.add": false, "email.list": false, "credential.verify": false}
	for _, c := range d.Capabilities {
		if _, ok := want[c]; ok {
			want[c] = true
		}
	}
	for cap, seen := range want {
		if !seen {
			t.Fatalf("capability %q missing from descriptor", cap)
		}
	}
	if !d.Enabled || !d.Live {
		t.Fatalf("live adapter descriptor: %+v", d)
	}
}

// ---------------------------------------------------------------------------
// contract: credential verifier
// ---------------------------------------------------------------------------

func TestVerifyValidCredential(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusOK, `{"id":42,"login":"operator"}`))
	res, err := newTestAdapter(t, api, newResolver()).Verify(context.Background(), "")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.State != provider.CredentialValid || res.SubjectID != "42" || res.Issuer != "github.com" {
		t.Fatalf("unexpected result: %+v", res)
	}
	reqs := api.requests()
	if len(reqs) != 1 || reqs[0].method != http.MethodGet || reqs[0].path != "/user" {
		t.Fatalf("unexpected requests: %+v", reqs)
	}
}

func TestVerifyRejectedCredentialIsAResultNotAnError(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusUnauthorized, `{"message":"Bad credentials"}`))
	res, err := newTestAdapter(t, api, newResolver()).Verify(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.State != provider.CredentialInvalid {
		t.Fatalf("state = %s, want INVALID", res.State)
	}
}

func TestVerifyTransientFailureReturnsError(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusBadGateway, `{}`))
	_, err := newTestAdapter(t, api, newResolver()).Verify(context.Background(), testRef)
	assertClass(t, err, errs.ClassUpstreamTransient, testToken)
}

// ---------------------------------------------------------------------------
// compliance: no account-creation or bypass code paths
// ---------------------------------------------------------------------------

func TestEndpointAllowlistIsExactlyTheReadWriteEmailEndpoints(t *testing.T) {
	want := map[string]struct{}{
		"/user/emails": {},
		"/user":        {},
	}
	if len(allowedPaths) != len(want) {
		t.Fatalf("allowedPaths = %v, want %v", allowedPaths, want)
	}
	for path := range allowedPaths {
		if _, ok := want[path]; !ok {
			t.Fatalf("unexpected endpoint in allowlist: %q", path)
		}
	}
}

func TestNoAccountCreationOrBypassCode(t *testing.T) {
	// Forbidden concepts from docs/09-integration-github.md. Scanning the
	// non-test sources makes "we only call the official email API" enforceable
	// by CI rather than by review memory.
	forbidden := []string{
		"signup", "sign_up", "createaccount", "create_account", "registeraccount",
		"captcha", "hcaptcha", "recaptcha",
		"playwright", "puppeteer", "chromedp", "selenium", "devtools",
		"proxy rotation", "proxypool", "proxy_pool",
		"user-agent rotation", "fingerprint",
		"/users", // account creation endpoint; email endpoints are /user/emails
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// doc.go states the prohibition itself (it must name CAPTCHA and account
		// creation to document the boundary). Executable sources are the
		// surface that can actually implement a bypass.
		if name == "doc.go" {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		lower := strings.ToLower(string(raw))
		for _, bad := range forbidden {
			if strings.Contains(lower, bad) {
				t.Fatalf("%s contains forbidden account-creation/bypass concept %q", name, bad)
			}
		}
		scanned++
	}
	if scanned == 0 {
		t.Fatalf("compliance scan found no source files")
	}
}

// ---------------------------------------------------------------------------
// transport and defensive branches
// ---------------------------------------------------------------------------

func TestUpstreamUnreachableIsTransient(t *testing.T) {
	api := newFakeAPI(t)
	baseURL := api.srv.URL
	api.srv.Close()

	a, err := New(Config{
		BaseURL:           baseURL,
		AllowLoopbackHTTP: true,
		TokenRef:          testRef,
		Timeout:           time.Second,
	}, newResolver())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = a.ListEmails(context.Background())
	assertClass(t, err, errs.ClassUpstreamTransient, testToken)
}

func TestRateLimitHintFromResetHeader(t *testing.T) {
	api := newFakeAPI(t)
	resetAt := time.Now().Add(120 * time.Second).Unix()
	api.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	})
	_, err := newTestAdapter(t, api, newResolver()).ListEmails(context.Background())
	assertClass(t, err, errs.ClassRateLimited, testToken)
	retry := errs.RetryAfterOf(err)
	if retry <= 0 || retry > 120*time.Second {
		t.Fatalf("RetryAfter = %v, want in (0s, 120s]", retry)
	}
}

func TestUnusableRetryAfterHeadersAreIgnored(t *testing.T) {
	cases := []struct {
		name       string
		retryAfter string
	}{
		{name: "garbage", retryAfter: "soon"},
		{name: "date in the past", retryAfter: time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.setHandler(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", tc.retryAfter)
				w.WriteHeader(http.StatusTooManyRequests)
			})
			_, err := newTestAdapter(t, api, newResolver()).ListEmails(context.Background())
			assertClass(t, err, errs.ClassRateLimited, testToken)
			if got := errs.RetryAfterOf(err); got != 0 {
				t.Fatalf("RetryAfter = %v, want 0 for an unusable header", got)
			}
		})
	}
}

func TestAddEmailUnauthorizedDoesNotReconcile(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusUnauthorized, `{"message":"Bad credentials"}`))
	_, err := newTestAdapter(t, api, newResolver()).AddEmail(context.Background(), "github@moonbyte.top")
	assertClass(t, err, errs.ClassNotAuthorized, testToken)
	if n := api.requestCount(); n != 1 {
		t.Fatalf("got %d requests, want 1", n)
	}
}

func TestAddEmailDuplicateReconcileTransportFailurePropagates(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"message":"conflict"}`)
			return
		}
		jsonHandler(http.StatusBadGateway, `{}`)(w, r)
	})
	_, err := newTestAdapter(t, api, newResolver()).AddEmail(context.Background(), "github@moonbyte.top")
	assertClass(t, err, errs.ClassUpstreamTransient, testToken)
}

func TestOversizedResponseIsProviderBug(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `["`+strings.Repeat("a", maxResponseBytes+16)+`"]`)
	})
	_, err := newTestAdapter(t, api, newResolver()).ListEmails(context.Background())
	assertClass(t, err, errs.ClassProviderBug, testToken)
}

func TestVerifyMalformedUserSchema(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusOK, `{"login":"operator"}`))
	_, err := newTestAdapter(t, api, newResolver()).Verify(context.Background(), "")
	assertClass(t, err, errs.ClassProviderBug, testToken)
}

func TestVerifyResolverFailureDoesNotReachNetwork(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusOK, `{"id":1}`))
	resolver := newResolver()
	resolver.err = errors.New("vault sealed")
	a := newTestAdapter(t, api, resolver)
	_, err := a.Verify(context.Background(), "secret://github/other")
	assertClass(t, err, errs.ClassNotAuthorized, testToken, "vault sealed")
	if n := api.requestCount(); n != 0 {
		t.Fatalf("verification reached the network without a credential")
	}
}

func TestNormalizeDomains(t *testing.T) {
	got := normalizeDomains([]string{"", "  ", "MoonByte.TOP", "@moonbyte.top", "moonbyte.top "})
	want := []string{"moonbyte.top"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeDomains = %v, want %v", got, want)
	}
	if normalizeDomains(nil) != nil {
		t.Fatalf("nil allowlist must stay nil")
	}
}

// ---------------------------------------------------------------------------
// registry integration: the control plane resolves this adapter through the
// EmailManager slot added to the frozen contracts, gated on Descriptor().Enabled.
// ---------------------------------------------------------------------------

func TestRegistryEmailManagerGate(t *testing.T) {
	api := newFakeAPI(t)
	api.setHandler(jsonHandler(http.StatusOK, `[{"email":"ops@moonbyte.top","verified":true}]`))

	reg := provider.NewRegistry()
	reg.AddEmailManager(newTestAdapter(t, api, newResolver()))

	resolved, err := reg.EmailManager(ProviderName)
	if err != nil {
		t.Fatalf("EmailManager lookup: %v", err)
	}
	if d := resolved.Descriptor(); d.Kind != provider.KindEmail || !d.Enabled {
		t.Fatalf("unexpected catalog entry: %+v", d)
	}
	list, err := resolved.ListEmails(context.Background())
	if err != nil {
		t.Fatalf("resolved ListEmails: %v", err)
	}
	if len(list) != 1 || !list[0].Verified {
		t.Fatalf("resolved ListEmails = %+v", list)
	}

	disabledReg := provider.NewRegistry()
	disabledReg.AddEmailManager(NewDisabled("operator has not authorized a github account"))
	if _, err := disabledReg.EmailManager(ProviderName); err == nil {
		t.Fatalf("disabled email manager must be rejected by the registry")
	} else {
		assertClass(t, err, errs.ClassNotAuthorized)
	}
}
