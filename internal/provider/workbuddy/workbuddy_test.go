package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

const (
	gwTokenRef  = "secret://workbuddy/gateway"
	gwToken     = "gateway-panel-secret-value"
	testState   = "STATE-abc123"
	testAuthURL = "https://www.workbuddy.ai/oauth/authorize?state=" + testState
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// memStore is an in-memory provider.SecretStore with call accounting, so tests
// can assert that disabled adapters never touch secrets and that sessions are
// consumed exactly once.
type memStore struct {
	mu         sync.Mutex
	data       map[string]string
	kinds      map[string]string
	puts       int
	deletes    int
	resolves   int
	failPut    func(ref string) error
	resolveErr error
}

func newMemStore() *memStore {
	return &memStore{data: map[string]string{}, kinds: map[string]string{}}
}

func newTestStore() *memStore {
	s := newMemStore()
	s.data[gwTokenRef] = gwToken
	return s
}

func (m *memStore) Put(_ context.Context, ref, kind, plaintext string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if m.failPut != nil {
		if err := m.failPut(ref); err != nil {
			return err
		}
	}
	m.data[ref] = plaintext
	m.kinds[ref] = kind
	return nil
}

func (m *memStore) Resolve(_ context.Context, ref string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resolves++
	if m.resolveErr != nil {
		return "", m.resolveErr
	}
	value, ok := m.data[ref]
	if !ok {
		return "", errors.New("secret not found")
	}
	return value, nil
}

func (m *memStore) Delete(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes++
	delete(m.data, ref)
	delete(m.kinds, ref)
	return nil
}

func (m *memStore) get(ref string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.data[ref]
	return value, ok
}

func (m *memStore) kind(ref string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kinds[ref]
}

func (m *memStore) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.data)
}

// sessionCount counts only stored authorization sessions (the seeded gateway
// credential is also a SecretStore entry).
func (m *memStore) sessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for ref := range m.data {
		if strings.HasPrefix(ref, sessionRefPrefix) {
			n++
		}
	}
	return n
}

func (m *memStore) putCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts
}

func (m *memStore) resolveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolves
}

func (m *memStore) setFailPut(fn func(ref string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failPut = fn
}

// gwRequest records one control-plane call.
type gwRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

// fakeGateway is a strict stand-in for the operator's gateway control plane. It
// refuses unknown paths so tests prove only /panel/api/login/{start,poll} are
// ever called.
type fakeGateway struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []gwRequest
	start http.HandlerFunc
	poll  http.HandlerFunc
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	f := &fakeGateway{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, gwRequest{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			header: r.Header.Clone(),
			body:   body,
		})
		var handler http.HandlerFunc
		switch r.URL.Path {
		case pathLoginStart:
			handler = f.start
		case pathLoginPoll:
			handler = f.poll
		default:
			f.mu.Unlock()
			t.Errorf("adapter called a non-allowlisted gateway path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		f.mu.Unlock()
		if handler == nil {
			http.Error(w, "handler not configured", http.StatusInternalServerError)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGateway) setStart(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.start = h
}

func (f *fakeGateway) setPoll(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.poll = h
}

func (f *fakeGateway) requests() []gwRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]gwRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

func (f *fakeGateway) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.reqs {
		if req.path == path {
			n++
		}
	}
	return n
}

func (f *fakeGateway) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func writeJSONBody(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func startSuccessHandler(state, realm string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, http.StatusOK, fmt.Sprintf(
			`{"ok":true,"url":"https://www.workbuddy.ai/oauth/authorize?state=%s","state":%q,"realm":%q}`,
			state, state, realm))
	}
}

func pollPendingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, http.StatusOK, `{"done":false,"message":"正在写入凭据…"}`)
	}
}

func pollDoneHandler(uid string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, http.StatusOK, fmt.Sprintf(
			`{"done":true,"uid":%q,"nickname":"operator","realm":"global","credits":0}`, uid))
	}
}

func statusHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, status, body)
	}
}

// newAdapter builds an enabled adapter pointed at the fake gateway.
func newAdapter(t *testing.T, gw *fakeGateway, store *memStore, mutate ...func(*Config)) *Adapter {
	t.Helper()
	if store == nil {
		store = newTestStore()
	}
	cfg := Config{
		Enabled:         true,
		BaseURL:         gw.srv.URL,
		AllowTestHTTP:   true,
		GatewayTokenRef: gwTokenRef,
		Timeout:         5 * time.Second,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := New(cfg, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func mustStart(t *testing.T, a *Adapter, subject string) provider.AuthorizationSession {
	t.Helper()
	session, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: subject})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return session
}

// ---------------------------------------------------------------------------
// assertions
// ---------------------------------------------------------------------------

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
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, hay := range []string{err.Error(), errs.SummaryOf(err), fmt.Sprintf("%v", err)} {
			if strings.Contains(hay, secret) {
				t.Fatalf("error output leaked %q: %s", secret, hay)
			}
		}
	}
}

func assertSessionStored(t *testing.T, store *memStore, ref string) {
	t.Helper()
	if _, ok := store.get(ref); !ok {
		t.Fatalf("session %q should still be stored", ref)
	}
}

func assertSessionGone(t *testing.T, store *memStore, ref string) {
	t.Helper()
	if _, ok := store.get(ref); ok {
		t.Fatalf("session %q should have been consumed", ref)
	}
}

// ---------------------------------------------------------------------------
// permission gate and configuration
// ---------------------------------------------------------------------------

func TestDescriptorDisabledByDefault(t *testing.T) {
	store := newMemStore()
	a, err := New(Config{}, store)
	if err != nil {
		t.Fatalf("New(Config{}): %v", err)
	}
	d := a.Descriptor()
	if d.Enabled || d.Live {
		t.Fatalf("default adapter must be disabled: %+v", d)
	}
	if d.Name != ProviderName || d.Kind != provider.KindOAuth || d.ContractVersion != ContractVersion {
		t.Fatalf("unexpected descriptor: %+v", d)
	}
	if !strings.Contains(d.RestrictionReason, "written permission") {
		t.Fatalf("restriction reason must cite the permission gate, got %q", d.RestrictionReason)
	}

	ctx := context.Background()
	_, startErr := a.Start(ctx, provider.StartAuthRequest{SubjectID: "subject-1"})
	assertClass(t, startErr, errs.ClassNotAuthorized)
	_, pollErr := a.Poll(ctx, "secret://oauth/workbuddy/whatever")
	assertClass(t, pollErr, errs.ClassNotAuthorized)
	if store.putCount() != 0 || store.resolveCount() != 0 {
		t.Fatalf("disabled adapter touched the secret store: puts=%d resolves=%d",
			store.putCount(), store.resolveCount())
	}
}

func TestDisabledAdapterPerformsZeroOutboundCalls(t *testing.T) {
	gw := newFakeGateway(t)
	store := newTestStore()
	a, err := New(Config{
		Enabled:         false,
		BaseURL:         gw.srv.URL,
		AllowTestHTTP:   true,
		GatewayTokenRef: gwTokenRef,
	}, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Descriptor().Enabled {
		t.Fatalf("adapter must stay disabled")
	}
	_, startErr := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
	assertClass(t, startErr, errs.ClassNotAuthorized)
	_, pollErr := a.Poll(context.Background(), "secret://oauth/workbuddy/x")
	assertClass(t, pollErr, errs.ClassNotAuthorized)
	if n := gw.total(); n != 0 {
		t.Fatalf("disabled adapter issued %d gateway calls", n)
	}
	if store.putCount() != 0 || store.resolveCount() != 0 {
		t.Fatalf("disabled adapter touched secrets")
	}
}

func TestNewDisabledCarriesCustomReason(t *testing.T) {
	a := NewDisabled("awaiting legal review")
	d := a.Descriptor()
	if d.Enabled {
		t.Fatalf("NewDisabled must be disabled")
	}
	if d.RestrictionReason != "awaiting legal review" {
		t.Fatalf("reason = %q", d.RestrictionReason)
	}
	if err := a.requireEnabled(); err == nil || !strings.Contains(err.Error(), "awaiting legal review") {
		t.Fatalf("gate error must cite the reason, got %v", err)
	}
}

func TestDescriptorEnabledWhenGateOpen(t *testing.T) {
	gw := newFakeGateway(t)
	a := newAdapter(t, gw, nil)
	d := a.Descriptor()
	if !d.Enabled || !d.Live {
		t.Fatalf("enabled adapter descriptor: %+v", d)
	}
	if d.Kind != provider.KindOAuth || d.Name != ProviderName {
		t.Fatalf("unexpected descriptor: %+v", d)
	}
	caps := map[string]bool{}
	for _, c := range d.Capabilities {
		caps[c] = true
	}
	for _, want := range []string{"oauth.device.start", "oauth.device.poll", "credential.ref"} {
		if !caps[want] {
			t.Fatalf("capability %q missing", want)
		}
	}
}

func TestNilAdapterFailsClosed(t *testing.T) {
	var a *Adapter
	if a.Descriptor().Enabled {
		t.Fatalf("nil adapter must not be enabled")
	}
	_, err := a.Poll(context.Background(), "secret://oauth/workbuddy/x")
	assertClass(t, err, errs.ClassNotAuthorized)
}

func TestNewRequiresSecretStoreWhenEnabled(t *testing.T) {
	_, err := New(Config{Enabled: true, BaseURL: "https://127.0.0.1:8443"}, nil)
	assertClass(t, err, errs.ClassValidation)
}

func TestNewRejectsRawGatewayCredential(t *testing.T) {
	gw := newFakeGateway(t)
	_, err := New(Config{
		Enabled:         true,
		BaseURL:         gw.srv.URL,
		AllowTestHTTP:   true,
		GatewayTokenRef: "raw-panel-api-key",
	}, newTestStore())
	assertClass(t, err, errs.ClassValidation)
}

func TestBaseURLAllowlist(t *testing.T) {
	cases := []struct {
		name      string
		base      string
		allowTest bool
		wantErr   bool
	}{
		{name: "missing base url", base: "", wantErr: true},
		{name: "public api host", base: "https://www.workbuddy.ai", wantErr: true},
		{name: "public ip", base: "https://8.8.8.8", wantErr: true},
		{name: "dns name", base: "https://gateway.internal", wantErr: true},
		{name: "loopback http without test flag", base: "http://127.0.0.1:8080", wantErr: true},
		{name: "loopback http with test flag", base: "http://127.0.0.1:8080", allowTest: true},
		{name: "loopback https", base: "https://127.0.0.1:8443"},
		{name: "ipv6 loopback https", base: "https://[::1]:8443"},
		{name: "localhost http with test flag", base: "http://localhost:9000", allowTest: true},
		{name: "private rfc1918 10/8 https", base: "https://10.1.2.3:8443"},
		{name: "private rfc1918 172.16/12 https", base: "https://172.20.0.5:8443"},
		{name: "private rfc1918 192.168/16 https", base: "https://192.168.1.10:8443"},
		{name: "plaintext private without flag", base: "http://192.168.1.10:8443", wantErr: true},
		{name: "plaintext private with test flag", base: "http://192.168.1.10:8443", allowTest: true, wantErr: true},
		{name: "scheme ftp", base: "ftp://127.0.0.1", wantErr: true},
		{name: "userinfo", base: "https://user:pass@127.0.0.1", wantErr: true},
		{name: "path", base: "https://127.0.0.1:8443/panel", wantErr: true},
		{name: "query", base: "https://127.0.0.1:8443?x=1", wantErr: true},
		{name: "fragment", base: "https://127.0.0.1:8443#f", wantErr: true},
	}
	store := newTestStore()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{
				Enabled:         true,
				BaseURL:         tc.base,
				AllowTestHTTP:   tc.allowTest,
				GatewayTokenRef: gwTokenRef,
			}, store)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected a configuration error")
				}
				assertClass(t, err, errs.ClassValidation)
				return
			}
			if err != nil {
				t.Fatalf("New(%q): %v", tc.base, err)
			}
		})
	}
}

func TestRealmValidation(t *testing.T) {
	gw := newFakeGateway(t)
	store := newTestStore()
	if _, err := New(Config{Enabled: true, BaseURL: gw.srv.URL, AllowTestHTTP: true, Realm: "eu"}, store); err == nil {
		t.Fatalf("unsupported realm must be rejected")
	} else {
		assertClass(t, err, errs.ClassValidation)
	}
	a := newAdapter(t, gw, store, func(c *Config) { c.Realm = "cn" })
	if got := a.cfg.Realm; got != "cn" {
		t.Fatalf("realm = %q, want cn", got)
	}
}

// ---------------------------------------------------------------------------
// Start
// ---------------------------------------------------------------------------

func TestStartSuccess(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	store := newTestStore()
	a := newAdapter(t, gw, store)

	before := time.Now()
	session, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "subject-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(session.Ref, "secret://oauth/workbuddy/") {
		t.Fatalf("session ref %q is not an opaque oauth secret reference", session.Ref)
	}
	if strings.Contains(session.Ref, testState) {
		t.Fatalf("session ref must not embed the gateway state")
	}
	if session.AuthorizationURL != testAuthURL {
		t.Fatalf("authorization URL = %q", session.AuthorizationURL)
	}
	earliest := before.Add(DefaultSessionTTL - time.Minute)
	latest := before.Add(DefaultSessionTTL + time.Minute)
	if session.ExpiresAt.Before(earliest) || session.ExpiresAt.After(latest) {
		t.Fatalf("ExpiresAt = %v, want about 15 minutes from now", session.ExpiresAt)
	}

	raw, ok := store.get(session.Ref)
	if !ok {
		t.Fatalf("session state was not persisted")
	}
	if kind := store.kind(session.Ref); kind != sessionSecretKind {
		t.Fatalf("stored kind = %q", kind)
	}
	var record sessionRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		t.Fatalf("decode stored session: %v", err)
	}
	if record.State != testState || record.Realm != "global" || record.SubjectID != "subject-1" {
		t.Fatalf("unexpected stored session: %+v", record)
	}

	reqs := gw.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d gateway calls, want 1", len(reqs))
	}
	req := reqs[0]
	if req.method != http.MethodPost || req.path != pathLoginStart {
		t.Fatalf("unexpected control-plane call: %s %s", req.method, req.path)
	}
	if got := req.header.Get("Authorization"); got != "Bearer "+gwToken {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := req.header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
	var payload struct {
		Realm string `json:"realm"`
	}
	if err := json.Unmarshal(req.body, &payload); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if payload.Realm != "global" {
		t.Fatalf("realm = %q, want global", payload.Realm)
	}
	if strings.Contains(req.query, testState) {
		t.Fatalf("start request must have no query")
	}
}

func TestStartUsesConfiguredRealm(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "cn"))
	a := newAdapter(t, gw, newTestStore(), func(c *Config) { c.Realm = "cn" })

	if _, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	req := gw.requests()[0]
	var payload struct {
		Realm string `json:"realm"`
	}
	if err := json.Unmarshal(req.body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Realm != "cn" {
		t.Fatalf("realm = %q, want cn", payload.Realm)
	}
}

func TestStartClampsSessionTTLToGatewayLimit(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	a := newAdapter(t, gw, newTestStore(), func(c *Config) { c.SessionTTL = time.Hour })

	before := time.Now()
	session, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if session.ExpiresAt.After(before.Add(MaxSessionTTL + 2*time.Second)) {
		t.Fatalf("ExpiresAt = %v, must not exceed the gateway's 15 minute TTL", session.ExpiresAt)
	}
	if session.ExpiresAt.Before(before.Add(MaxSessionTTL - time.Minute)) {
		t.Fatalf("ExpiresAt = %v, unexpectedly short", session.ExpiresAt)
	}
}

func TestStartStatusMapping(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		headers   map[string]string
		wantClass errs.Class
		wantRetry time.Duration
	}{
		{name: "401", status: http.StatusUnauthorized, wantClass: errs.ClassNotAuthorized},
		{name: "403", status: http.StatusForbidden, wantClass: errs.ClassNotAuthorized},
		{name: "400", status: http.StatusBadRequest, wantClass: errs.ClassValidation},
		{name: "404", status: http.StatusNotFound, wantClass: errs.ClassValidation},
		{name: "429", status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "45"}, wantClass: errs.ClassRateLimited, wantRetry: 45 * time.Second},
		{name: "500", status: http.StatusInternalServerError, wantClass: errs.ClassUpstreamTransient},
		{name: "502", status: http.StatusBadGateway, wantClass: errs.ClassUpstreamTransient},
		{name: "503", status: http.StatusServiceUnavailable, wantClass: errs.ClassUpstreamTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newFakeGateway(t)
			gw.setStart(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				writeJSONBody(w, tc.status, `{"ok":false,"error":"gateway rejected"}`)
			})
			store := newTestStore()
			a := newAdapter(t, gw, store)
			_, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
			assertClass(t, err, tc.wantClass, gwToken)
			if got := errs.HTTPStatusOf(err); got != tc.status {
				t.Fatalf("HTTPStatus = %d, want %d", got, tc.status)
			}
			if tc.wantRetry > 0 {
				if got := errs.RetryAfterOf(err); got != tc.wantRetry {
					t.Fatalf("RetryAfter = %v, want %v", got, tc.wantRetry)
				}
			}
			if n := store.putCount(); n != 0 {
				t.Fatalf("failed start persisted %d session records", n)
			}
		})
	}
}

func TestStartTimeout(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		writeJSONBody(w, http.StatusOK, `{"ok":true,"url":"https://x/y","state":"s"}`)
	})
	a := newAdapter(t, gw, newTestStore(), func(c *Config) { c.Timeout = 40 * time.Millisecond })
	_, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
	assertClass(t, err, errs.ClassTimeoutUncertain, gwToken)
}

func TestStartContextCancelled(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	a := newAdapter(t, gw, newTestStore())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Start(ctx, provider.StartAuthRequest{SubjectID: "s"})
	assertClass(t, err, errs.ClassCancelled, gwToken)
}

func TestStartMalformedResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "not json", body: `<html>gateway</html>`},
		{name: "ok false", body: `{"ok":false,"error":"refused"}`},
		{name: "missing url", body: `{"ok":true,"state":"state-1"}`},
		{name: "missing state", body: `{"ok":true,"url":"https://x/y"}`},
		{name: "plaintext authorization url", body: `{"ok":true,"url":"http://insecure/x","state":"state-1"}`},
		{name: "relative authorization url", body: `{"ok":true,"url":"/oauth","state":"state-1"}`},
		{name: "state with space", body: `{"ok":true,"url":"https://x/y","state":"has space"}`},
		{name: "realm mismatch", body: `{"ok":true,"url":"https://x/y","state":"state-1","realm":"cn"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newFakeGateway(t)
			gw.setStart(statusHandler(http.StatusOK, tc.body))
			store := newTestStore()
			a := newAdapter(t, gw, store)
			_, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
			assertClass(t, err, errs.ClassProviderBug, gwToken)
			if n := store.putCount(); n != 0 {
				t.Fatalf("malformed start persisted %d session records", n)
			}
		})
	}
}

func TestStartStoreFailureIsTransient(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	store := newTestStore()
	store.setFailPut(func(string) error { return errors.New("disk full") })
	a := newAdapter(t, gw, store)
	_, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
	assertClass(t, err, errs.ClassUpstreamTransient, gwToken)
}

func TestStartGatewayCredentialFailure(t *testing.T) {
	t.Run("resolver error", func(t *testing.T) {
		gw := newFakeGateway(t)
		gw.setStart(startSuccessHandler(testState, "global"))
		store := newTestStore()
		store.resolveErr = errors.New("vault unavailable")
		a := newAdapter(t, gw, store)
		_, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
		assertClass(t, err, errs.ClassNotAuthorized, gwToken)
		if n := gw.total(); n != 0 {
			t.Fatalf("gateway called despite an unavailable credential")
		}
	})
	t.Run("empty secret", func(t *testing.T) {
		gw := newFakeGateway(t)
		gw.setStart(startSuccessHandler(testState, "global"))
		store := newTestStore()
		store.data[gwTokenRef] = "   "
		a := newAdapter(t, gw, store)
		_, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
		assertClass(t, err, errs.ClassNotAuthorized, gwToken)
	})
}

func TestStartWithoutGatewayCredentialSendsNoAuthHeader(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	a := newAdapter(t, gw, newTestStore(), func(c *Config) { c.GatewayTokenRef = "" })

	if _, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	req := gw.requests()[0]
	if got := req.header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization sent without a configured credential: %q", got)
	}
}

func TestGatewayCredentialResolvedPerCall(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	store := newTestStore()
	a := newAdapter(t, gw, store)

	mustStart(t, a, "s1")
	store.mu.Lock()
	store.data[gwTokenRef] = "rotated-gateway-secret"
	store.mu.Unlock()
	mustStart(t, a, "s2")

	reqs := gw.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d gateway calls, want 2", len(reqs))
	}
	if got := reqs[0].header.Get("Authorization"); got != "Bearer "+gwToken {
		t.Fatalf("first Authorization = %q", got)
	}
	if got := reqs[1].header.Get("Authorization"); got != "Bearer rotated-gateway-secret" {
		t.Fatalf("rotated Authorization = %q", got)
	}
}

func TestSessionRefsAreUniqueAndOpaque(t *testing.T) {
	gw := newFakeGateway(t)
	var mu sync.Mutex
	states := []string{"state-one", "state-two"}
	call := 0
	gw.setStart(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		state := states[call]
		call++
		mu.Unlock()
		startSuccessHandler(state, "global")(w, nil)
	})
	store := newTestStore()
	a := newAdapter(t, gw, store)

	first := mustStart(t, a, "s1")
	second := mustStart(t, a, "s2")
	if first.Ref == second.Ref {
		t.Fatalf("duplicate session reference %q", first.Ref)
	}
	for _, session := range []provider.AuthorizationSession{first, second} {
		for _, state := range states {
			if strings.Contains(session.Ref, state) {
				t.Fatalf("session ref %q embeds state %q", session.Ref, state)
			}
		}
	}
	if store.sessionCount() != 2 {
		t.Fatalf("stored sessions = %d, want 2", store.sessionCount())
	}
}

// ---------------------------------------------------------------------------
// Poll
// ---------------------------------------------------------------------------

func TestPollPending(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	gw.setPoll(pollPendingHandler())
	store := newTestStore()
	a := newAdapter(t, gw, store)

	session := mustStart(t, a, "subject-1")
	status, err := a.Poll(context.Background(), session.Ref)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if status.State != provider.AuthPending {
		t.Fatalf("state = %s, want PENDING", status.State)
	}
	if status.SubjectID != "subject-1" {
		t.Fatalf("SubjectID = %q", status.SubjectID)
	}
	assertSessionStored(t, store, session.Ref)

	reqs := gw.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d gateway calls, want 2 (start + poll)", len(reqs))
	}
	pollReq := reqs[1]
	if pollReq.method != http.MethodGet || pollReq.path != pathLoginPoll {
		t.Fatalf("unexpected poll call: %s %s", pollReq.method, pollReq.path)
	}
	if got := pollReq.query; got != "state="+testState {
		t.Fatalf("poll query = %q, want state=%s", got, testState)
	}
}

func TestPollAuthorizedRecordsOnlyAPointer(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	gw.setPoll(func(w http.ResponseWriter, _ *http.Request) {
		// A hostile/verbose gateway body must not leak into our storage or DTOs.
		writeJSONBody(w, http.StatusOK, `{"done":true,"uid":"u-42","nickname":"operator","realm":"global",
			"access_token":"SUPERSECRET-ACCESS","refresh_token":"SUPERSECRET-REFRESH"}`)
	})
	store := newTestStore()
	a := newAdapter(t, gw, store)

	session := mustStart(t, a, "subject-1")
	status, err := a.Poll(context.Background(), session.Ref)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if status.State != provider.AuthAuthorized {
		t.Fatalf("state = %s, want AUTHORIZED", status.State)
	}
	wantRef := "secret://workbuddy/credential/u-42"
	if status.CredentialRef != wantRef {
		t.Fatalf("CredentialRef = %q, want %q", status.CredentialRef, wantRef)
	}
	if status.SubjectID != "subject-1" {
		t.Fatalf("SubjectID = %q", status.SubjectID)
	}
	assertSessionGone(t, store, session.Ref)

	raw, ok := store.get(wantRef)
	if !ok {
		t.Fatalf("credential pointer was not stored")
	}
	if kind := store.kind(wantRef); kind != credentialPointerSecretKind {
		t.Fatalf("credential pointer kind = %q", kind)
	}
	for _, forbidden := range []string{"SUPERSECRET", "access_token", "refresh_token"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("credential pointer leaked %q: %s", forbidden, raw)
		}
	}
	var pointer credentialPointer
	if err := json.Unmarshal([]byte(raw), &pointer); err != nil {
		t.Fatalf("decode pointer: %v", err)
	}
	if pointer.UID != "u-42" || pointer.Realm != "global" || pointer.Provider != ProviderName {
		t.Fatalf("unexpected pointer: %+v", pointer)
	}
	serialized, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	for _, forbidden := range []string{"SUPERSECRET", "u-42", wantRef} {
		if strings.Contains(string(serialized), forbidden) {
			t.Fatalf("authorization status DTO leaked %q: %s", forbidden, serialized)
		}
	}

	// A duplicate poll must be served from the terminal cache: the single-use
	// gateway state is never replayed.
	again, err := a.Poll(context.Background(), session.Ref)
	if err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if again.State != provider.AuthAuthorized || again.CredentialRef != wantRef {
		t.Fatalf("cached poll result = %+v", again)
	}
	if n := gw.count(pathLoginPoll); n != 1 {
		t.Fatalf("duplicate poll reached the gateway %d times, want 1", n)
	}
}

func TestPollGoneMapsToAuthExpiredAndIsNeverReplayed(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	gw.setPoll(statusHandler(http.StatusGone, `{"ok":false,"error":"login session 已过期"}`))
	store := newTestStore()
	a := newAdapter(t, gw, store)

	session := mustStart(t, a, "subject-1")
	_, err := a.Poll(context.Background(), session.Ref)
	assertClass(t, err, errs.ClassAuthExpired, gwToken, testState)
	if !strings.Contains(strings.ToLower(errs.SummaryOf(err)), "start a new authorization") {
		t.Fatalf("summary should tell the operator to restart, got %q", errs.SummaryOf(err))
	}
	assertSessionGone(t, store, session.Ref)

	_, secondErr := a.Poll(context.Background(), session.Ref)
	assertClass(t, secondErr, errs.ClassAuthExpired, gwToken, testState)
	if n := gw.count(pathLoginPoll); n != 1 {
		t.Fatalf("expired session was replayed %d times, want 1", n)
	}
}

func TestPollStatusMappingKeepsSession(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		headers   map[string]string
		wantClass errs.Class
		wantRetry time.Duration
	}{
		{name: "401", status: http.StatusUnauthorized, wantClass: errs.ClassNotAuthorized},
		{name: "403", status: http.StatusForbidden, wantClass: errs.ClassNotAuthorized},
		{name: "400", status: http.StatusBadRequest, wantClass: errs.ClassValidation},
		{name: "404", status: http.StatusNotFound, wantClass: errs.ClassValidation},
		{name: "429", status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "90"}, wantClass: errs.ClassRateLimited, wantRetry: 90 * time.Second},
		{name: "500", status: http.StatusInternalServerError, wantClass: errs.ClassUpstreamTransient},
		{name: "502", status: http.StatusBadGateway, wantClass: errs.ClassUpstreamTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newFakeGateway(t)
			gw.setStart(startSuccessHandler(testState, "global"))
			gw.setPoll(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				writeJSONBody(w, tc.status, `{"ok":false,"error":"upstream failure"}`)
			})
			store := newTestStore()
			a := newAdapter(t, gw, store)
			session := mustStart(t, a, "subject-1")
			_, err := a.Poll(context.Background(), session.Ref)
			assertClass(t, err, tc.wantClass, gwToken, testState)
			if got := errs.HTTPStatusOf(err); got != tc.status {
				t.Fatalf("HTTPStatus = %d, want %d", got, tc.status)
			}
			if tc.wantRetry > 0 {
				if got := errs.RetryAfterOf(err); got != tc.wantRetry {
					t.Fatalf("RetryAfter = %v, want %v", got, tc.wantRetry)
				}
			}
			assertSessionStored(t, store, session.Ref)
		})
	}
}

func TestPollMalformedResponsesAreProviderBug(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "not json", body: `done=true`},
		{name: "missing done", body: `{"message":"waiting"}`},
		{name: "done wrong type", body: `{"done":"yes"}`},
		{name: "done true without uid", body: `{"done":true}`},
		{name: "done true with path traversal uid", body: `{"done":true,"uid":"../../etc/passwd"}`},
		{name: "done true with long uid", body: `{"done":true,"uid":"` + strings.Repeat("a", 65) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newFakeGateway(t)
			gw.setStart(startSuccessHandler(testState, "global"))
			gw.setPoll(statusHandler(http.StatusOK, tc.body))
			store := newTestStore()
			a := newAdapter(t, gw, store)
			session := mustStart(t, a, "subject-1")
			_, err := a.Poll(context.Background(), session.Ref)
			assertClass(t, err, errs.ClassProviderBug, gwToken, testState)
			assertSessionStored(t, store, session.Ref)
			for ref := range store.data {
				if strings.HasPrefix(ref, credentialRefPrefix) {
					t.Fatalf("malformed completion stored a credential pointer: %s", ref)
				}
			}
		})
	}
}

func TestPollTimeoutKeepsSessionForRetry(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	gw.setPoll(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		writeJSONBody(w, http.StatusOK, `{"done":false}`)
	})
	store := newTestStore()
	a := newAdapter(t, gw, store, func(c *Config) { c.Timeout = 40 * time.Millisecond })
	session := mustStart(t, a, "subject-1")
	_, err := a.Poll(context.Background(), session.Ref)
	assertClass(t, err, errs.ClassTimeoutUncertain, gwToken, testState)
	assertSessionStored(t, store, session.Ref)
}

func TestPollLocalDeadlineExpiresWithoutNetwork(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	gw.setPoll(pollPendingHandler())
	store := newTestStore()
	a := newAdapter(t, gw, store, func(c *Config) { c.SessionTTL = 50 * time.Millisecond })
	session := mustStart(t, a, "subject-1")

	// Advance the adapter's clock past the local deadline; the gateway is never
	// asked about a state we already know is stale.
	fixed := time.Now().Add(2 * time.Minute)
	a.now = func() time.Time { return fixed }

	_, err := a.Poll(context.Background(), session.Ref)
	assertClass(t, err, errs.ClassAuthExpired, gwToken, testState)
	if n := gw.count(pathLoginPoll); n != 0 {
		t.Fatalf("expired session reached the gateway %d times, want 0", n)
	}
	assertSessionGone(t, store, session.Ref)
}

func TestPollUnknownSessionReference(t *testing.T) {
	gw := newFakeGateway(t)
	a := newAdapter(t, gw, newTestStore())
	_, err := a.Poll(context.Background(), "secret://oauth/workbuddy/does-not-exist")
	assertClass(t, err, errs.ClassAuthExpired, gwToken)
	if n := gw.total(); n != 0 {
		t.Fatalf("unknown session reference reached the gateway")
	}
}

func TestPollEmptyReferenceIsValidation(t *testing.T) {
	gw := newFakeGateway(t)
	a := newAdapter(t, gw, newTestStore())
	_, err := a.Poll(context.Background(), "   ")
	assertClass(t, err, errs.ClassValidation, gwToken)
	if n := gw.total(); n != 0 {
		t.Fatalf("empty session reference reached the gateway")
	}
}

func TestPollCorruptSessionRecordIsProviderBug(t *testing.T) {
	gw := newFakeGateway(t)
	store := newTestStore()
	ref := "secret://oauth/workbuddy/corrupt"
	if err := store.Put(context.Background(), ref, sessionSecretKind, "not-json"); err != nil {
		t.Fatalf("seed corrupt record: %v", err)
	}
	a := newAdapter(t, gw, store)
	_, err := a.Poll(context.Background(), ref)
	assertClass(t, err, errs.ClassProviderBug, gwToken)
	if n := gw.total(); n != 0 {
		t.Fatalf("corrupt session record reached the gateway")
	}
}

func TestPollCredentialPointerStoreFailureIsTransient(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	gw.setPoll(pollDoneHandler("u-7"))
	store := newTestStore()
	a := newAdapter(t, gw, store)
	session := mustStart(t, a, "subject-1")
	store.setFailPut(func(ref string) error {
		if strings.HasPrefix(ref, credentialRefPrefix) {
			return errors.New("vault write failed")
		}
		return nil
	})
	_, err := a.Poll(context.Background(), session.Ref)
	assertClass(t, err, errs.ClassUpstreamTransient, gwToken, testState)
	if _, ok := store.get(session.Ref); !ok {
		t.Fatalf("session should be preserved when the pointer cannot be recorded")
	}
}

func TestPollIsSingleFlightUnderConcurrency(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var handlerCalls int
	var mu sync.Mutex
	gw.setPoll(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		handlerCalls++
		mu.Unlock()
		entered <- struct{}{}
		<-release
		writeJSONBody(w, http.StatusOK, `{"done":true,"uid":"u-9"}`)
	})
	store := newTestStore()
	a := newAdapter(t, gw, store)
	session := mustStart(t, a, "subject-1")

	type result struct {
		status provider.AuthorizationStatus
		err    error
	}
	results := make(chan result, 2)
	run := func() {
		status, err := a.Poll(context.Background(), session.Ref)
		results <- result{status: status, err: err}
	}
	go run()
	<-entered
	go run()
	// Give the second poll a chance to block on the flight lock.
	time.Sleep(20 * time.Millisecond)
	close(release)

	first := <-results
	second := <-results
	for i, res := range []result{first, second} {
		if res.err != nil {
			t.Fatalf("poll %d: %v", i, res.err)
		}
		if res.status.State != provider.AuthAuthorized || res.status.CredentialRef != "secret://workbuddy/credential/u-9" {
			t.Fatalf("poll %d result = %+v", i, res.status)
		}
	}
	mu.Lock()
	calls := handlerCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("gateway poll handler ran %d times, want 1", calls)
	}
	if n := gw.count(pathLoginPoll); n != 1 {
		t.Fatalf("gateway saw %d polls, want 1", n)
	}
}

func TestPollRetriesPendingThenAuthorizes(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	var mu sync.Mutex
	polls := 0
	gw.setPoll(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		polls++
		current := polls
		mu.Unlock()
		if current == 1 {
			writeJSONBody(w, http.StatusOK, `{"done":false}`)
			return
		}
		writeJSONBody(w, http.StatusOK, `{"done":true,"uid":"u-11"}`)
	})
	store := newTestStore()
	a := newAdapter(t, gw, store)
	session := mustStart(t, a, "subject-1")

	first, err := a.Poll(context.Background(), session.Ref)
	if err != nil || first.State != provider.AuthPending {
		t.Fatalf("first poll = %+v, err %v", first, err)
	}
	second, err := a.Poll(context.Background(), session.Ref)
	if err != nil || second.State != provider.AuthAuthorized {
		t.Fatalf("second poll = %+v, err %v", second, err)
	}
	if n := gw.count(pathLoginPoll); n != 2 {
		t.Fatalf("gateway polls = %d, want 2", n)
	}
	assertSessionGone(t, store, session.Ref)
}

// ---------------------------------------------------------------------------
// redirect safety and compliance
// ---------------------------------------------------------------------------

func TestCrossHostRedirectRefused(t *testing.T) {
	var otherHits int
	var otherAuth []string
	var mu sync.Mutex
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		otherHits++
		otherAuth = append(otherAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		writeJSONBody(w, http.StatusOK, `{"ok":true}`)
	}))
	defer other.Close()

	gw := newFakeGateway(t)
	gw.setStart(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+pathLoginStart, http.StatusTemporaryRedirect)
	})
	a := newAdapter(t, gw, newTestStore())
	_, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
	assertClass(t, err, errs.ClassProviderBug, gwToken)
	if !strings.Contains(errs.SummaryOf(err), "redirect") {
		t.Fatalf("summary should explain the refused redirect, got %q", errs.SummaryOf(err))
	}
	mu.Lock()
	defer mu.Unlock()
	if otherHits != 0 {
		t.Fatalf("redirect target received %d requests, want 0", otherHits)
	}
	for _, auth := range otherAuth {
		if auth != "" {
			t.Fatalf("gateway credential forwarded to the redirect target")
		}
	}
}

func TestEndpointAllowlistIsTheDocumentedControlPlaneOnly(t *testing.T) {
	want := map[string]struct{}{
		"/panel/api/login/start": {},
		"/panel/api/login/poll":  {},
	}
	if len(allowedPaths) != len(want) {
		t.Fatalf("allowedPaths = %v", allowedPaths)
	}
	for path := range allowedPaths {
		if _, ok := want[path]; !ok {
			t.Fatalf("unexpected endpoint in allowlist: %q", path)
		}
	}
	if _, ok := allowedPaths["/panel/api/login/cancel"]; ok {
		t.Fatalf("cancel must not be part of the control-plane surface")
	}
}

func TestNoScrapingOrTokenExtractionCode(t *testing.T) {
	// The real adapter must not contain any path that reaches the platform's
	// web app, extracts a token or automates a browser. doc.go names these
	// prohibitions to document the boundary; executable sources must be clean.
	forbidden := []string{
		"copilot.tencent.com",
		"workbuddy.ai",
		"/v2/plugin",
		"auth/token",
		"login/account",
		"playwright", "puppeteer", "chromedp", "selenium",
		"cookie", "localstorage", "document.cookie", "devtools",
		"access_token", "refresh_token", "accesstoken", "refreshtoken",
		"scrape", "captcha",
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
				t.Fatalf("%s contains forbidden scraping/token-extraction concept %q", name, bad)
			}
		}
		scanned++
	}
	if scanned == 0 {
		t.Fatalf("compliance scan found no source files")
	}
}

// ---------------------------------------------------------------------------
// transport, fallbacks and cache hygiene
// ---------------------------------------------------------------------------

func TestUpstreamUnreachableIsTransient(t *testing.T) {
	gw := newFakeGateway(t)
	baseURL := gw.srv.URL
	gw.srv.Close()

	a, err := New(Config{
		Enabled:         true,
		BaseURL:         baseURL,
		AllowTestHTTP:   true,
		GatewayTokenRef: gwTokenRef,
		Timeout:         time.Second,
	}, newTestStore())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
	assertClass(t, err, errs.ClassUpstreamTransient, gwToken)
}

func TestRetryAfterHeaderVariants(t *testing.T) {
	t.Run("http date", func(t *testing.T) {
		gw := newFakeGateway(t)
		retryAt := time.Now().Add(60 * time.Second).UTC()
		gw.setStart(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", retryAt.Format(http.TimeFormat))
			writeJSONBody(w, http.StatusTooManyRequests, `{"ok":false}`)
		})
		_, err := newAdapter(t, gw, newTestStore()).Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
		assertClass(t, err, errs.ClassRateLimited, gwToken)
		if got := errs.RetryAfterOf(err); got <= 0 || got > 60*time.Second {
			t.Fatalf("RetryAfter = %v, want in (0s, 60s]", got)
		}
	})
	t.Run("unusable header", func(t *testing.T) {
		gw := newFakeGateway(t)
		gw.setStart(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "later")
			writeJSONBody(w, http.StatusTooManyRequests, `{"ok":false}`)
		})
		_, err := newAdapter(t, gw, newTestStore()).Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
		assertClass(t, err, errs.ClassRateLimited, gwToken)
		if got := errs.RetryAfterOf(err); got != 0 {
			t.Fatalf("RetryAfter = %v, want 0 for an unusable header", got)
		}
	})
}

func TestOversizedGatewayResponseIsProviderBug(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"url":"https://example.invalid/x","state":"state-1","pad":"`+
			strings.Repeat("a", maxResponseBytes+16)+`"}`)
	})
	_, err := newAdapter(t, gw, newTestStore()).Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
	assertClass(t, err, errs.ClassProviderBug, gwToken)
}

func TestUnusableAuthorizationURLsAreProviderBug(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{name: "oversized", url: "https://example.invalid/" + strings.Repeat("a", 4200)},
		{name: "control character", url: "https://example.invalid/\tpath"},
		{name: "userinfo", url: "https://user:pass@example.invalid/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newFakeGateway(t)
			payload, err := json.Marshal(map[string]any{"ok": true, "url": tc.url, "state": "state-1"})
			if err != nil {
				t.Fatalf("encode fake response: %v", err)
			}
			gw.setStart(statusHandler(http.StatusOK, string(payload)))
			_, err = newAdapter(t, gw, newTestStore()).Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"})
			assertClass(t, err, errs.ClassProviderBug, gwToken)
		})
	}
}

func TestSessionRecordFallbacks(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setPoll(pollPendingHandler())
	store := newTestStore()
	ref := "secret://oauth/workbuddy/legacy"
	// A legacy record carries neither realm nor expiry: the adapter must fall
	// back to its configuration and to created_at + SessionTTL.
	record := fmt.Sprintf(`{"state":%q,"subject_id":"subject-9","created_at":%q}`,
		testState, time.Now().UTC().Format(time.RFC3339))
	if err := store.Put(context.Background(), ref, sessionSecretKind, record); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	a := newAdapter(t, gw, store)
	status, err := a.Poll(context.Background(), ref)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if status.State != provider.AuthPending || status.SubjectID != "subject-9" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if n := gw.count(pathLoginPoll); n != 1 {
		t.Fatalf("gateway polls = %d, want 1", n)
	}
}

func TestTerminalCacheExpiresAndIsBounded(t *testing.T) {
	gw := newFakeGateway(t)
	store := newTestStore()
	a := newAdapter(t, gw, store)
	base := time.Now()
	a.now = func() time.Time { return base }

	a.markAuthorized("ref-done", provider.AuthorizationStatus{State: provider.AuthAuthorized})
	a.markExpired("ref-expired")
	if len(a.terminals) != 2 {
		t.Fatalf("terminal entries = %d, want 2", len(a.terminals))
	}

	later := base.Add(terminalCacheTTL + time.Minute)
	a.now = func() time.Time { return later }
	if _, ok := a.cachedAuthorized("ref-done"); ok {
		t.Fatalf("expired terminal cache entry was reused")
	}
	if a.cachedExpired("ref-expired") {
		t.Fatalf("expired terminal cache entry was reused")
	}
	a.pruneTerminals()
	if len(a.terminals) != 0 {
		t.Fatalf("prune left %d terminal entries", len(a.terminals))
	}

	for i := 0; i < terminalCacheLimit+2; i++ {
		a.markAuthorized(fmt.Sprintf("bulk-%d", i), provider.AuthorizationStatus{State: provider.AuthAuthorized})
	}
	a.pruneTerminals()
	if len(a.terminals) != terminalCacheLimit {
		t.Fatalf("cache size = %d, want %d", len(a.terminals), terminalCacheLimit)
	}
}

// ---------------------------------------------------------------------------
// registry integration: OAuth lookups are gated before any network call.
// ---------------------------------------------------------------------------

func TestRegistryOAuthGate(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setStart(startSuccessHandler(testState, "global"))
	store := newTestStore()

	reg := provider.NewRegistry()
	reg.AddOAuth(newAdapter(t, gw, store))

	resolved, err := reg.OAuth(ProviderName)
	if err != nil {
		t.Fatalf("OAuth lookup: %v", err)
	}
	if d := resolved.Descriptor(); d.Kind != provider.KindOAuth || !d.Enabled {
		t.Fatalf("unexpected catalog entry: %+v", d)
	}
	if _, err := resolved.Start(context.Background(), provider.StartAuthRequest{SubjectID: "s"}); err != nil {
		t.Fatalf("resolved Start: %v", err)
	}

	before := gw.total()
	disabledReg := provider.NewRegistry()
	disabledReg.AddOAuth(NewDisabled(""))
	if _, err := disabledReg.OAuth(ProviderName); err == nil {
		t.Fatalf("disabled oauth provider must be rejected by the registry")
	} else {
		assertClass(t, err, errs.ClassNotAuthorized)
	}
	if after := gw.total(); after != before {
		t.Fatalf("disabled registry lookup performed %d gateway calls", after-before)
	}
}
