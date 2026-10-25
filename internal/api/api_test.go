package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/api"
	"github.com/baicie/account-onboarding/internal/config"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/provider/fake"
	"github.com/baicie/account-onboarding/internal/secrets"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

const adminToken = "test-admin-token-value-0123456789"

type env struct {
	ts     *httptest.Server
	client *http.Client
	cfg    config.Config
	store  *storage.Store
	engine *workflow.Engine
	reg    *provider.Registry
	world  *fake.World
	sec    *secrets.Service
	log    *slog.Logger
	signer *security.SessionSigner
}

func newEnv(t *testing.T, mutate func(*config.Config, *provider.Registry)) *env {
	t.Helper()
	master := bytes.Repeat([]byte{3}, 32)
	ring, err := security.NewKeyRing(master)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), storage.Options{
		Path:     filepath.Join(t.TempDir(), "api.sqlite"),
		HMACKey:  security.DeriveKey(master, "storage-hmac"),
		Redactor: security.NewRedactor(adminToken),
		TransitionValidator: func(from, to string) error {
			return state.CanTransition(state.RunState(from), state.RunState(to))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sec := secrets.New(store, security.NewBox(ring))
	world := fake.NewWorld(sec)
	reg := provider.NewRegistry()
	reg.AddMailbox(fake.NewMailbox(world))
	reg.AddIdentity(fake.NewIdentity(world))
	reg.AddOAuth(fake.NewOAuth(world))
	reg.AddCredential(fake.NewCredential(world))
	reg.AddSandbox(fake.NewSandbox(world))
	reg.AddEmailManager(fake.NewEmailManager(world))

	cfg := config.Default()
	cfg.AdminToken = adminToken
	cfg.MasterKey = master
	cfg.FakeProviders = true
	cfg.Domains = []string{"moonbyte.test"}
	cfg.SenderAllowlist = []string{"moonbyte.test", "idp.fake.test", "github.com"}
	cfg.SessionTTL = time.Hour
	cfg.RateLimitPerMinute = 100000
	cfg.LoginRateLimitPerMinute = 1000
	if mutate != nil {
		mutate(&cfg, reg)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	engine := workflow.New(store, reg, sec, log, workflow.Config{
		MaxConcurrency:  2,
		TickInterval:    25 * time.Millisecond,
		LeaseTTL:        10 * time.Second,
		StepTimeout:     3 * time.Second,
		PollInterval:    100 * time.Millisecond,
		MaxAttempts:     2,
		RunDeadline:     time.Minute,
		Domains:         cfg.Domains,
		SenderAllowlist: cfg.SenderAllowlist,
		FingerprintKey:  security.DeriveKey(master, "fp"),
	})
	signer := security.NewSessionSigner(master, 3600)
	server, err := api.New(cfg, store, engine, reg, sec, log, signer, security.NewRedactor(adminToken))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	engine.Start(ctx)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	t.Cleanup(func() {
		ts.Close()
		cancel()
		engine.Stop()
		_ = store.Close()
	})
	return &env{ts: ts, client: client, cfg: cfg, store: store, engine: engine, reg: reg,
		world: world, sec: sec, log: log, signer: signer}
}

func (e *env) request(t *testing.T, method, path string, body any, token string, headers map[string]string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (e *env) createSandbox(t *testing.T, idem string) map[string]any {
	t.Helper()
	status, body := e.request(t, http.MethodPost, "/v1/runs", sandboxRequest(), adminToken,
		map[string]string{"Idempotency-Key": idem})
	if status != http.StatusAccepted {
		t.Fatalf("create run: status=%d body=%s", status, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sandboxRequest() map[string]any {
	return map[string]any{
		"template":          "sandbox-email-verify-oidc",
		"mode":              "sandbox-e2e",
		"mailbox_provider":  "fake-mailbox",
		"identity_provider": "fake-identity",
		"oauth_provider":    "fake-oauth",
		"sandbox_provider":  "fake-sandbox",
	}
}

func githubRequest() map[string]any {
	return map[string]any{
		"template":              "github-existing-email",
		"mode":                  "real-account",
		"mailbox_provider":      "fake-mailbox",
		"identity_provider":     "fake-identity",
		"oauth_provider":        "fake-oauth",
		"email_provider":        "fake-github",
		"existing_identity_ref": "operator-owned-account",
	}
}

func (e *env) waitStatus(t *testing.T, id, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, body := e.request(t, http.MethodGet, "/v1/runs/"+id, nil, adminToken, nil)
		if status != http.StatusOK {
			t.Fatalf("get run: %d %s", status, body)
		}
		var run map[string]any
		_ = json.Unmarshal(body, &run)
		if run["status"] == want {
			return
		}
		if st, ok := run["status"].(string); ok && state.RunState(st).IsTerminal() && st != want {
			t.Fatalf("run reached terminal %s while waiting for %s", st, want)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach %s", id, want)
}

func TestHealthAndReadiness(t *testing.T) {
	e := newEnv(t, nil)
	status, body := e.request(t, http.MethodGet, "/healthz", nil, "", nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("healthz: %d %s", status, body)
	}
	if status, _ := e.request(t, http.MethodGet, "/readyz", nil, "", nil); status != http.StatusUnauthorized {
		t.Fatalf("readyz without auth = %d", status)
	}
	status, body = e.request(t, http.MethodGet, "/readyz", nil, adminToken, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"ready":true`) {
		t.Fatalf("readyz with auth: %d %s", status, body)
	}
	status, body = e.request(t, http.MethodGet, "/v1/meta", nil, adminToken, nil)
	if status != http.StatusOK || !strings.Contains(string(body), "sandbox-email-verify-oidc") {
		t.Fatalf("meta: %d %s", status, body)
	}
}

func TestAuthenticationRequired(t *testing.T) {
	e := newEnv(t, nil)
	for _, token := range []string{"", "wrong-token"} {
		status, _ := e.request(t, http.MethodGet, "/v1/runs", nil, token, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d", token, status)
		}
	}
	if status, _ := e.request(t, http.MethodGet, "/v1/providers", nil, adminToken, nil); status != http.StatusOK {
		t.Fatalf("providers with token = %d", status)
	}
}

func TestCreateRunValidation(t *testing.T) {
	e := newEnv(t, nil)
	// Missing idempotency header.
	if status, _ := e.request(t, http.MethodPost, "/v1/runs", sandboxRequest(), adminToken, nil); status != http.StatusBadRequest {
		t.Fatalf("missing idempotency header = %d", status)
	}
	// Header/body alias mismatch.
	mismatchBody := sandboxRequest()
	mismatchBody["idempotency_key"] = "different-key-0001"
	if status, _ := e.request(t, http.MethodPost, "/v1/runs", mismatchBody, adminToken, map[string]string{
		"Idempotency-Key": "header-key-0001",
	}); status != http.StatusBadRequest {
		t.Fatalf("header/body mismatch = %d", status)
	}
	// Unknown field (account_count must never be accepted).
	bad := sandboxRequest()
	bad["account_count"] = 5
	if status, _ := e.request(t, http.MethodPost, "/v1/runs", bad, adminToken, map[string]string{
		"Idempotency-Key": "bad-field-0001",
	}); status != http.StatusBadRequest {
		t.Fatalf("unknown field accepted = %d", status)
	}
	// Template/mode mismatch.
	mismatch := sandboxRequest()
	mismatch["mode"] = "real-account"
	if status, _ := e.request(t, http.MethodPost, "/v1/runs", mismatch, adminToken, map[string]string{
		"Idempotency-Key": "mismatch-0001",
	}); status != http.StatusBadRequest {
		t.Fatalf("mode mismatch accepted = %d", status)
	}
	// real-account without existing identity reference.
	gh := githubRequest()
	delete(gh, "existing_identity_ref")
	if status, _ := e.request(t, http.MethodPost, "/v1/runs", gh, adminToken, map[string]string{
		"Idempotency-Key": "gh-noref-0001",
	}); status != http.StatusBadRequest {
		t.Fatalf("real-account without ref accepted = %d", status)
	}
	// Unknown provider.
	unknown := sandboxRequest()
	unknown["mailbox_provider"] = "no-such-mailbox"
	if status, _ := e.request(t, http.MethodPost, "/v1/runs", unknown, adminToken, map[string]string{
		"Idempotency-Key": "unknown-provider-0001",
	}); status != http.StatusBadRequest {
		t.Fatalf("unknown provider accepted = %d", status)
	}
}

type disabledOAuth struct{ provider.OAuthProvider }

func (d disabledOAuth) Descriptor() provider.Descriptor {
	desc := d.OAuthProvider.Descriptor()
	desc.Name = "workbuddy"
	desc.Enabled = false
	desc.RestrictionReason = "requires written platform permission"
	return desc
}

func TestDisabledProviderForbidden(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config, reg *provider.Registry) {
		reg.AddOAuth(disabledOAuth{OAuthProvider: fake.NewOAuth(fake.NewWorld(nil))})
	})
	req := map[string]any{
		"template":              "oauth-existing-account",
		"mode":                  "real-account",
		"mailbox_provider":      "fake-mailbox",
		"identity_provider":     "fake-identity",
		"oauth_provider":        "workbuddy",
		"existing_identity_ref": "existing-1",
	}
	status, body := e.request(t, http.MethodPost, "/v1/runs", req, adminToken, map[string]string{
		"Idempotency-Key": "disabled-provider-0001",
	})
	if status != http.StatusForbidden {
		t.Fatalf("disabled provider status = %d body=%s", status, body)
	}
	if !strings.Contains(string(body), "NOT_AUTHORIZED") {
		t.Fatalf("expected NOT_AUTHORIZED, got %s", body)
	}
}

func TestIdempotentCreateAndConflict(t *testing.T) {
	e := newEnv(t, nil)
	first := e.createSandbox(t, "idem-api-0001")
	second := e.createSandbox(t, "idem-api-0001")
	if first["id"] != second["id"] {
		t.Fatalf("idempotency broken: %v vs %v", first["id"], second["id"])
	}
	changed := sandboxRequest()
	changed["email_provider"] = "fake-github"
	status, body := e.request(t, http.MethodPost, "/v1/runs", changed, adminToken, map[string]string{
		"Idempotency-Key": "idem-api-0001",
	})
	if status != http.StatusConflict {
		t.Fatalf("conflicting idempotency status = %d body=%s", status, body)
	}
}

func TestRunLifecycleResumeCancel(t *testing.T) {
	e := newEnv(t, nil)
	status, body := e.request(t, http.MethodPost, "/v1/runs", githubRequest(), adminToken, map[string]string{
		"Idempotency-Key": "lifecycle-0001",
	})
	if status != http.StatusAccepted {
		t.Fatalf("create: %d %s", status, body)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	id := created["id"].(string)

	e.waitStatus(t, id, "WAITING_USER_ACTION", 8*time.Second)
	status, body = e.request(t, http.MethodGet, "/v1/runs/"+id, nil, adminToken, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"action_required":true`) {
		t.Fatalf("waiting run: %d %s", status, body)
	}
	// Resume is legal while waiting.
	if status, _ := e.request(t, http.MethodPost, "/v1/runs/"+id+"/resume", nil, adminToken, nil); status != http.StatusAccepted {
		t.Fatalf("resume status = %d", status)
	}
	// Cancel is legal while waiting.
	if status, _ := e.request(t, http.MethodPost, "/v1/runs/"+id+"/cancel", nil, adminToken, nil); status != http.StatusAccepted {
		t.Fatalf("cancel status = %d", status)
	}
	e.waitStatus(t, id, "CANCELLED", 8*time.Second)
	// Terminal runs reject both actions.
	if status, _ := e.request(t, http.MethodPost, "/v1/runs/"+id+"/cancel", nil, adminToken, nil); status != http.StatusConflict {
		t.Fatalf("cancel terminal status = %d", status)
	}
	if status, _ := e.request(t, http.MethodPost, "/v1/runs/"+id+"/resume", nil, adminToken, nil); status != http.StatusConflict {
		t.Fatalf("resume terminal status = %d", status)
	}
}

func TestCompletedRunRejectsActions(t *testing.T) {
	e := newEnv(t, nil)
	created := e.createSandbox(t, "complete-api-0001")
	id := created["id"].(string)
	e.waitStatus(t, id, "COMPLETED", 10*time.Second)
	if status, _ := e.request(t, http.MethodPost, "/v1/runs/"+id+"/cancel", nil, adminToken, nil); status != http.StatusConflict {
		t.Fatalf("cancel completed = %d", status)
	}
	if status, _ := e.request(t, http.MethodPost, "/v1/runs/"+id+"/resume", nil, adminToken, nil); status != http.StatusConflict {
		t.Fatalf("resume completed = %d", status)
	}
}

func TestRunDetailEventsAndList(t *testing.T) {
	e := newEnv(t, nil)
	created := e.createSandbox(t, "detail-api-0001")
	id := created["id"].(string)
	e.waitStatus(t, id, "COMPLETED", 10*time.Second)

	status, body := e.request(t, http.MethodGet, "/v1/runs/"+id, nil, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("detail: %d %s", status, body)
	}
	var detail map[string]any
	_ = json.Unmarshal(body, &detail)
	steps, ok := detail["steps"].([]any)
	if !ok || len(steps) != 6 {
		t.Fatalf("expected 6 steps, got %v", detail["steps"])
	}
	if strings.Contains(string(body), "secret://") || strings.Contains(string(body), "eyJ") {
		t.Fatal("run detail leaked a secret reference or JWT")
	}

	status, body = e.request(t, http.MethodGet, "/v1/runs/"+id+"/events", nil, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("events: %d %s", status, body)
	}
	var events struct {
		Items []struct {
			Seq     int64          `json:"seq"`
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatal(err)
	}
	if len(events.Items) == 0 {
		t.Fatal("no events returned")
	}
	for i, ev := range events.Items {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event sequence gap at %d: %d", i, ev.Seq)
		}
		for k, v := range ev.Payload {
			if !security.IsEventPayloadKeyAllowed(k) {
				t.Fatalf("event payload key %q is not allowlisted", k)
			}
			if s, ok := v.(string); ok && strings.Contains(s, "secret://") {
				t.Fatalf("event payload %q leaked a secret reference", k)
			}
		}
	}

	status, body = e.request(t, http.MethodGet, "/v1/runs?limit=1", nil, adminToken, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"next_cursor"`) {
		t.Fatalf("list with cursor: %d %s", status, body)
	}
	if status, _ := e.request(t, http.MethodGet, "/v1/runs/does-not-exist", nil, adminToken, nil); status != http.StatusNotFound {
		t.Fatalf("unknown run = %d", status)
	}
	if status, _ := e.request(t, http.MethodGet, "/v1/runs/x/events", nil, adminToken, nil); status != http.StatusNotFound {
		t.Fatalf("unknown run events = %d", status)
	}
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func TestUILoginAndCSRFProtection(t *testing.T) {
	e := newEnv(t, nil)
	// Wrong token is rejected.
	form := url.Values{"token": {"wrong"}}
	resp, err := e.client.PostForm(e.ts.URL+"/ui/login", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d", resp.StatusCode)
	}

	// Correct token with same-origin Origin header.
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/login", strings.NewReader(url.Values{"token": {adminToken}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, err = e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	csrf := ""
	for _, c := range resp.Cookies() {
		if c.Name == "onboard_session" {
			if !c.HttpOnly {
				t.Fatal("session cookie must be HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Fatal("session cookie must be SameSite=Strict")
			}
		}
	}

	// Dashboard is accessible and carries the CSRF token.
	resp, err = e.client.Get(e.ts.URL + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	dashboard, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d", resp.StatusCode)
	}
	m := csrfPattern.FindSubmatch(dashboard)
	if len(m) != 2 {
		t.Fatal("CSRF token not rendered")
	}
	csrf = string(m[1])

	// A mutation without the CSRF token is rejected.
	badReq, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/runs", strings.NewReader(url.Values{
		"template": {"sandbox-email-verify-oidc"}, "mailbox_provider": {"fake-mailbox"},
		"identity_provider": {"fake-identity"}, "oauth_provider": {"fake-oauth"},
	}.Encode()))
	badReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badReq.Header.Set("Origin", e.ts.URL)
	resp, err = e.client.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF = %d (want 403)", resp.StatusCode)
	}

	// With the CSRF token and a same-origin request it succeeds.
	goodReq, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/runs", strings.NewReader(url.Values{
		"csrf": {csrf}, "template": {"sandbox-email-verify-oidc"},
		"mailbox_provider": {"fake-mailbox"}, "identity_provider": {"fake-identity"},
		"oauth_provider": {"fake-oauth"}, "idempotency_key": {"ui-run-0001"},
	}.Encode()))
	goodReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	goodReq.Header.Set("Origin", e.ts.URL)
	resp, err = e.client.Do(goodReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST with CSRF = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/ui/runs/") {
		t.Fatalf("unexpected redirect %q", loc)
	}

	// A cookie-authenticated mutation without Origin or Referer is rejected.
	noOriginReq, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/runs", strings.NewReader(url.Values{
		"csrf": {csrf}, "template": {"sandbox-email-verify-oidc"},
	}.Encode()))
	noOriginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = e.client.Do(noOriginReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without Origin/Referer = %d (want 403)", resp.StatusCode)
	}

	// Cross-origin mutation is rejected even with a valid session.
	crossReq, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/runs", strings.NewReader(url.Values{
		"csrf": {csrf}, "template": {"sandbox-email-verify-oidc"},
	}.Encode()))
	crossReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	crossReq.Header.Set("Origin", "http://evil.example.test")
	resp, err = e.client.Do(crossReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d (want 403)", resp.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, nil)
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/ui/login", nil)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	checks := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, want := range checks {
		if got := resp.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("unexpected CSP: %q", csp)
	}
}

func TestConcurrentCreateSameKeyOnlyOneRun(t *testing.T) {
	e := newEnv(t, nil)
	const n = 6
	type result struct {
		status int
		id     string
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		go func() {
			status, body := func() (int, []byte) {
				req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/runs", bytes.NewReader(mustJSON(sandboxRequest())))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+adminToken)
				req.Header.Set("Idempotency-Key", "concurrent-api-0001")
				resp, err := e.client.Do(req)
				if err != nil {
					return 0, nil
				}
				defer resp.Body.Close()
				raw, _ := io.ReadAll(resp.Body)
				return resp.StatusCode, raw
			}()
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			id, _ := parsed["id"].(string)
			results <- result{status: status, id: id}
		}()
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		res := <-results
		if res.status != http.StatusAccepted {
			t.Fatalf("concurrent create status = %d", res.status)
		}
		if res.id == "" {
			t.Fatal("empty run id")
		}
		seen[res.id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("expected one run id, got %v", seen)
	}
}

func TestViewerRoleIsReadOnly(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config, reg *provider.Registry) {
		cfg.ReadOnlyToken = "viewer-token-0123456789"
	})
	viewer := "viewer-token-0123456789"
	if status, body := e.request(t, http.MethodGet, "/v1/runs", nil, viewer, nil); status != http.StatusOK {
		t.Fatalf("viewer GET /v1/runs = %d %s", status, body)
	}
	if status, _ := e.request(t, http.MethodGet, "/v1/providers", nil, viewer, nil); status != http.StatusOK {
		t.Fatalf("viewer GET /v1/providers != 200")
	}
	status, body := e.request(t, http.MethodPost, "/v1/runs", sandboxRequest(), viewer, map[string]string{
		"Idempotency-Key": "viewer-denied-0001",
	})
	if status != http.StatusForbidden || !strings.Contains(string(body), "read-only") {
		t.Fatalf("viewer POST /v1/runs = %d %s", status, body)
	}
	// The admin token still works for mutations.
	if status, _ := e.request(t, http.MethodPost, "/v1/runs", sandboxRequest(), adminToken, map[string]string{
		"Idempotency-Key": "viewer-denied-0002",
	}); status != http.StatusAccepted {
		t.Fatalf("admin POST /v1/runs = %d", status)
	}
	// An unknown token is rejected with 401.
	if status, _ := e.request(t, http.MethodGet, "/v1/runs", nil, "not-a-token", nil); status != http.StatusUnauthorized {
		t.Fatalf("unknown token = %d", status)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config, reg *provider.Registry) {
		cfg.LoginRateLimitPerMinute = 3
	})
	var lastStatus int
	var lastHeader http.Header
	for i := 0; i < 4; i++ {
		form := url.Values{"token": {"wrong-token"}}
		resp, err := e.client.PostForm(e.ts.URL+"/ui/login", form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		lastStatus = resp.StatusCode
		lastHeader = resp.Header
		if i < 3 && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d (want 401)", i+1, resp.StatusCode)
		}
	}
	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("4th login attempt = %d (want 429)", lastStatus)
	}
	if lastHeader.Get("Retry-After") == "" {
		t.Fatal("429 response missing Retry-After")
	}
}

func TestAPIRateLimit(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config, reg *provider.Registry) {
		cfg.RateLimitPerMinute = 3
	})
	for i := 0; i < 3; i++ {
		if status, _ := e.request(t, http.MethodGet, "/v1/providers", nil, adminToken, nil); status != http.StatusOK {
			t.Fatalf("request %d = %d", i+1, status)
		}
	}
	status, body := e.request(t, http.MethodGet, "/v1/providers", nil, adminToken, nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("4th request = %d %s (want 429)", status, body)
	}
	if !strings.Contains(string(body), "RATE_LIMITED") {
		t.Fatalf("expected RATE_LIMITED body, got %s", body)
	}
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal: %v", err))
	}
	return raw
}

func TestViewerCannotSeeAuthorizationURL(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config, reg *provider.Registry) {
		cfg.ReadOnlyToken = "viewer-token-0123456789"
	})
	e.world.AutoAuthorize = false
	req := map[string]any{
		"template": "oauth-existing-account", "mode": "real-account",
		"mailbox_provider": "fake-mailbox", "identity_provider": "fake-identity",
		"oauth_provider": "fake-oauth", "existing_identity_ref": "existing-1",
	}
	status, body := e.request(t, http.MethodPost, "/v1/runs", req, adminToken, map[string]string{
		"Idempotency-Key": "viewer-url-0001",
	})
	if status != http.StatusAccepted {
		t.Fatalf("create: %d %s", status, body)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	id := created["id"].(string)
	e.waitStatus(t, id, "WAITING_USER_ACTION", 8*time.Second)

	status, body = e.request(t, http.MethodGet, "/v1/runs/"+id, nil, adminToken, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"authorization_url"`) {
		t.Fatalf("admin should see the authorization URL: %d %s", status, body)
	}
	status, body = e.request(t, http.MethodGet, "/v1/runs/"+id, nil, "viewer-token-0123456789", nil)
	if status != http.StatusOK {
		t.Fatalf("viewer get: %d %s", status, body)
	}
	if strings.Contains(string(body), `"authorization_url"`) || strings.Contains(string(body), "idp.fake.test/authorize") {
		t.Fatalf("viewer must not see the authorization URL: %s", body)
	}
	status, body = e.request(t, http.MethodGet, "/v1/runs", nil, "viewer-token-0123456789", nil)
	if status != http.StatusOK || strings.Contains(string(body), `"authorization_url"`) {
		t.Fatalf("viewer list leaked authorization_url: %d %s", status, body)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	e := newEnv(t, nil)
	loginReq, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/login",
		strings.NewReader(url.Values{"token": {adminToken}}.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginReq.Header.Set("Origin", e.ts.URL)
	resp, err := e.client.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	var sessionValue string
	for _, c := range resp.Cookies() {
		if c.Name == "onboard_session" {
			sessionValue = c.Value
		}
	}
	if sessionValue == "" {
		t.Fatal("no session cookie issued")
	}
	// Sanity: the session works.
	if resp, err := e.client.Get(e.ts.URL + "/ui/"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard before logout: %v %v", err, resp)
	} else {
		resp.Body.Close()
	}
	// Logout (CSRF + Origin required for cookie mutations).
	dashResp, _ := e.client.Get(e.ts.URL + "/ui/")
	dashBody, _ := io.ReadAll(dashResp.Body)
	dashResp.Body.Close()
	m := csrfPattern.FindSubmatch(dashBody)
	if len(m) != 2 {
		t.Fatal("csrf token missing")
	}
	logoutReq, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/logout", strings.NewReader(url.Values{"csrf": {string(m[1])}}.Encode()))
	logoutReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	logoutReq.Header.Set("Origin", e.ts.URL)
	resp, err = e.client.Do(logoutReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	// Replaying the pre-logout cookie must be rejected server-side.
	replay, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/ui/", nil)
	replay.Header.Set("Cookie", "onboard_session="+sessionValue)
	resp, err = e.client.Do(replay)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/ui/login") {
		t.Fatalf("revoked session replay = %d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestTemplateFilterAppliedBeforePagination(t *testing.T) {
	e := newEnv(t, nil)
	// Create a GitHub (real-account) run first, then a newer sandbox run.
	if status, body := e.request(t, http.MethodPost, "/v1/runs", githubRequest(), adminToken, map[string]string{
		"Idempotency-Key": "filter-gh-0001",
	}); status != http.StatusAccepted {
		t.Fatalf("github create: %d %s", status, body)
	}
	e.createSandbox(t, "filter-sandbox-0001")
	// With limit=1 and template filter in SQL, the sandbox run must be returned
	// even though the newest run is also a sandbox; requesting github must also
	// match the older run (post-filtering would drop it).
	status, body := e.request(t, http.MethodGet, "/v1/runs?template=github-existing-email&limit=1", nil, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %s", status, body)
	}
	var page struct {
		Items []struct {
			Template string `json:"template"`
		} `json:"items"`
	}
	_ = json.Unmarshal(body, &page)
	if len(page.Items) != 1 || page.Items[0].Template != "github-existing-email" {
		t.Fatalf("template filter applied after pagination: %+v", page.Items)
	}
}

// rawRequest performs one request without calling testing.T from a goroutine.
func (e *env) rawRequest(method, path string, body any, token string, headers map[string]string) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// TestConcurrentHTTPReadsAndWrites exercises the API under concurrency: parallel
// authenticated reads must stay 200, and parallel run creations with distinct
// idempotency keys must all be accepted and persisted (SQLite single-writer
// queueing, rate-limit bucket configured high for the test env).
func TestConcurrentHTTPReadsAndWrites(t *testing.T) {
	e := newEnv(t, nil)
	const readers = 24
	var wg sync.WaitGroup
	type result struct {
		status int
		err    error
	}
	readResults := make([]result, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, err := e.rawRequest(http.MethodGet, "/v1/runs?limit=5", nil, adminToken, nil)
			readResults[i] = result{status, err}
		}(i)
	}
	wg.Wait()
	for i, r := range readResults {
		if r.err != nil || r.status != http.StatusOK {
			t.Fatalf("concurrent read %d = %d err=%v", i, r.status, r.err)
		}
	}
	const writers = 10
	createResults := make([]result, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := map[string]any{
				"template": "sandbox-email-verify-oidc", "mode": "sandbox-e2e",
				"mailbox_provider": "fake-mailbox", "identity_provider": "fake-identity",
				"oauth_provider": "fake-oauth", "sandbox_provider": "fake-sandbox",
			}
			status, err := e.rawRequest(http.MethodPost, "/v1/runs", req, adminToken, map[string]string{
				"Idempotency-Key": fmt.Sprintf("concurrent-%d", i),
			})
			createResults[i] = result{status, err}
		}(i)
	}
	wg.Wait()
	for i, r := range createResults {
		if r.err != nil || r.status != http.StatusAccepted {
			t.Fatalf("concurrent create %d = %d err=%v", i, r.status, r.err)
		}
	}
	page, err := e.store.ListRuns(context.Background(), storage.RunListFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) < writers {
		t.Fatalf("persisted runs = %d, want >= %d", len(page.Items), writers)
	}
}

func TestExternalOperationDiagnosticsAdminOnly(t *testing.T) {
	e := newEnv(t, nil)
	status, body := e.request(t, http.MethodGet, "/v1/ops/external", nil, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("admin GET /v1/ops/external = %d %s", status, body)
	}
	var resp struct {
		Items  []any          `json:"items"`
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if resp.Counts["total"] != 0 || resp.Counts["needs_reconciliation"] != 0 {
		t.Fatalf("unexpected counts: %+v", resp.Counts)
	}
	viewer := "viewer-token-0123456789"
	e.cfg.ReadOnlyToken = viewer
	// Rebuild the server so the viewer token is wired into auth.
	e2 := newEnv(t, func(cfg *config.Config, _ *provider.Registry) {
		cfg.ReadOnlyToken = viewer
	})
	if status, _ := e2.request(t, http.MethodGet, "/v1/ops/external", nil, viewer, nil); status != http.StatusForbidden {
		t.Fatalf("viewer GET /v1/ops/external = %d, want 403", status)
	}
}
