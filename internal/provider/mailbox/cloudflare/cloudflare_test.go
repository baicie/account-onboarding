package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/mailx"
	"github.com/baicie/account-onboarding/internal/provider"
)

const (
	testAdminPassword = "admin-password-must-never-leak"
	testSitePassword  = "site-password-must-never-leak"
	testJWT           = "eyJhbGciOiJIUzI1NiJ9.jwt-payload-must-never-leak.signature"
	testAddress       = "sandbox001@moonbyte.top"
	testAddressID     = "12345"
	testSecretRef     = "secret://mailbox/" + testAddressID
)

// fakeSecrets is an in-memory provider.SecretStore for tests.
type fakeSecrets struct {
	mu     sync.Mutex
	values map[string]string
	kinds  map[string]string
	putErr error
	getErr error
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{
		values: map[string]string{},
		kinds:  map[string]string{},
	}
}

func (f *fakeSecrets) Put(_ context.Context, ref, kind, plaintext string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.values[ref] = plaintext
	f.kinds[ref] = kind
	return nil
}

func (f *fakeSecrets) Resolve(_ context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return "", f.getErr
	}
	value, ok := f.values[ref]
	if !ok {
		return "", errors.New("secret not found")
	}
	return value, nil
}

func (f *fakeSecrets) Delete(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.values, ref)
	delete(f.kinds, ref)
	return nil
}

func (f *fakeSecrets) get(ref string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.values[ref]
	return value, ok
}

func (f *fakeSecrets) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.values)
}

func testMailbox() provider.Mailbox {
	return provider.Mailbox{
		ID:        testAddressID,
		Address:   testAddress,
		Provider:  ProviderName,
		SecretRef: testSecretRef,
	}
}

func testConfig(srvURL string, secrets *fakeSecrets) Config {
	return Config{
		BaseURL:       srvURL,
		AllowLoopback: true,
		AdminAuth:     testAdminPassword,
		SitePassword:  testSitePassword,
		Domain:        "moonbyte.top",
		Timeout:       2 * time.Second,
		Secrets:       secrets,
		Resolver:      secrets,
	}
}

func newProvider(t *testing.T, handler http.HandlerFunc, mut func(*Config)) (*Provider, *fakeSecrets, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	secrets := newFakeSecrets()
	cfg := testConfig(srv.URL, secrets)
	if mut != nil {
		mut(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p, secrets, srv
}

func loadJWT(t *testing.T, secrets *fakeSecrets) {
	t.Helper()
	if err := secrets.Put(context.Background(), testSecretRef, SecretRefKind, testJWT); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
}

func mustClass(t *testing.T, err error, want errs.Class) *errs.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want class %s", want)
	}
	if got := errs.ClassOf(err); got != want {
		t.Fatalf("error class = %s, want %s (err=%v)", got, want, err)
	}
	var classified *errs.Error
	if !errors.As(err, &classified) {
		t.Fatalf("error %v is not an *errs.Error", err)
	}
	return classified
}

// assertNoSecrets fails when any credential appears in the error's public or
// diagnostic rendering.
func assertNoSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	rendered := err.Error() + " | " + errs.SummaryOf(err) + " | " + fmt.Sprintf("%#v", err)
	for _, secret := range []string{testAdminPassword, testSitePassword, testJWT, "Bearer "} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("error leaked secret %q: %s", secret, rendered)
		}
	}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNew_Validation(t *testing.T) {
	secrets := newFakeSecrets()

	valid := func() Config { return testConfig("https://api.moonbyte.top", secrets) }

	cases := []struct {
		name string
		cfg  func() Config
	}{
		{"missing admin credential", func() Config { c := valid(); c.AdminAuth = ""; return c }},
		{"admin credential with newline", func() Config { c := valid(); c.AdminAuth = "line\nbreak"; return c }},
		{"site password with newline", func() Config { c := valid(); c.SitePassword = "line\rbreak"; return c }},
		{"missing secret writer", func() Config { c := valid(); c.Secrets = nil; return c }},
		{"missing secret resolver", func() Config { c := valid(); c.Resolver = nil; return c }},
		{"host not allowlisted", func() Config { c := valid(); c.BaseURL = "https://evil.example"; return c }},
		{"allowlisted host but http", func() Config { c := valid(); c.BaseURL = "http://api.moonbyte.top"; return c }},
		{"loopback without override", func() Config { c := valid(); c.BaseURL = "http://127.0.0.1:8025"; c.AllowLoopback = false; return c }},
		{"userinfo in base url", func() Config { c := valid(); c.BaseURL = "https://admin:secret@api.moonbyte.top"; return c }},
		{"query in base url", func() Config { c := valid(); c.BaseURL = "https://api.moonbyte.top/?x=1"; return c }},
		{"fragment in base url", func() Config { c := valid(); c.BaseURL = "https://api.moonbyte.top/#frag"; return c }},
		{"invalid base url", func() Config { c := valid(); c.BaseURL = "://nope"; return c }},
		{"invalid domain", func() Config { c := valid(); c.Domain = "not a domain"; return c }},
		{"invalid allowed domain", func() Config { c := valid(); c.AllowedDomains = []string{"bad domain"}; return c }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg())
			if err == nil {
				t.Fatal("New() = nil error, want validation error")
			}
			if class := errs.ClassOf(err); class != errs.ClassValidation {
				t.Fatalf("error class = %s, want VALIDATION (err=%v)", class, err)
			}
			assertNoSecrets(t, err)
		})
	}

	accepted := []struct {
		name string
		cfg  func() Config
	}{
		{"live default endpoint", func() Config {
			c := valid()
			c.BaseURL = ""
			return c
		}},
		{"explicit live endpoint", valid},
		{"custom allowlisted host", func() Config {
			c := valid()
			c.BaseURL = "https://worker.example.test"
			c.AllowedHosts = []string{"worker.example.test"}
			return c
		}},
		{"loopback override", func() Config {
			c := valid()
			c.BaseURL = "http://127.0.0.1:8787"
			c.AllowLoopback = true
			return c
		}},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg()); err != nil {
				t.Fatalf("New() error = %v, want success", err)
			}
		})
	}
}

func TestDescriptor(t *testing.T) {
	p, _, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {}, nil)
	d := p.Descriptor()
	if d.Name != ProviderName || d.Kind != provider.KindMailbox {
		t.Fatalf("descriptor = %+v, want mailbox/%s", d, ProviderName)
	}
	if !d.Enabled || !d.Live {
		t.Fatalf("descriptor = %+v, want enabled live provider", d)
	}
	if d.ContractVersion != ContractVersion {
		t.Fatalf("contract version = %q, want %q", d.ContractVersion, ContractVersion)
	}
	want := map[string]bool{"address.create": true, "message.list": true, "message.get": true}
	for _, capability := range d.Capabilities {
		delete(want, capability)
	}
	if len(want) != 0 {
		t.Fatalf("missing capabilities: %v", want)
	}

	secrets := newFakeSecrets()
	cfg := testConfig("http://127.0.0.1:1", secrets)
	cfg.Disabled = true
	disabled, err := New(cfg)
	if err != nil {
		t.Fatalf("New(Disabled) error = %v", err)
	}
	if dd := disabled.Descriptor(); dd.Enabled || dd.RestrictionReason == "" {
		t.Fatalf("disabled descriptor = %+v, want Enabled=false with restriction reason", dd)
	}
}

func TestProviderDoesNotCleanRealAddresses(t *testing.T) {
	p, _, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {}, nil)
	if _, ok := any(p).(provider.MailboxCleaner); ok {
		t.Fatal("cloudflare provider must not implement provider.MailboxCleaner")
	}
}

func TestDisabledProviderRejectsWithoutNetwork(t *testing.T) {
	requests := 0
	secrets := newFakeSecrets()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL, secrets)
	cfg.Disabled = true
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
	mustClass(t, err, errs.ClassNotAuthorized)
	_, err = p.ListMessages(context.Background(), testMailbox(), time.Time{})
	mustClass(t, err, errs.ClassNotAuthorized)
	_, err = p.GetMessage(context.Background(), testMailbox(), "m1")
	mustClass(t, err, errs.ClassNotAuthorized)
	if requests != 0 {
		t.Fatalf("disabled provider made %d network calls, want 0", requests)
	}
}

// ---------------------------------------------------------------------------
// CreateAddress
// ---------------------------------------------------------------------------

func TestCreateAddress_Success(t *testing.T) {
	requests := 0
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/admin/new_address" {
			t.Errorf("request = %s %s, want POST /admin/new_address", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("x-admin-auth"); got != testAdminPassword {
			t.Errorf("x-admin-auth = %q, want admin credential", got)
		}
		if got := r.Header.Get("x-custom-auth"); got != testSitePassword {
			t.Errorf("x-custom-auth = %q, want site password", got)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		var body struct {
			EnablePrefix bool   `json:"enablePrefix"`
			Name         string `json:"name"`
			Domain       string `json:"domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.EnablePrefix || body.Name != "sandbox001" || body.Domain != "moonbyte.top" {
			t.Errorf("body = %+v, want enablePrefix=false name=sandbox001 domain=moonbyte.top", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jwt":"` + testJWT + `","address":"` + testAddress + `","address_id":12345}`))
	}, nil)

	before := time.Now().UTC()
	mb, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001", Domain: "moonbyte.top"})
	if err != nil {
		t.Fatalf("CreateAddress() error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if mb.ID != testAddressID || mb.Address != testAddress || mb.Provider != ProviderName {
		t.Fatalf("mailbox = %+v, want id=%s address=%s provider=%s", mb.ID, mb.Address, testAddress, ProviderName)
	}
	if mb.SecretRef != testSecretRef {
		t.Fatalf("SecretRef = %q, want %q", mb.SecretRef, testSecretRef)
	}
	if mb.CreatedAt.Before(before) || mb.CreatedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("CreatedAt = %s, want current time", mb.CreatedAt)
	}

	stored, ok := secrets.get(testSecretRef)
	if !ok || stored != testJWT {
		t.Fatalf("secrets[%q] = %q, %v; want the address JWT", testSecretRef, stored, ok)
	}
	if secrets.kinds[testSecretRef] != SecretRefKind {
		t.Fatalf("secret kind = %q, want %q", secrets.kinds[testSecretRef], SecretRefKind)
	}

	raw, err := json.Marshal(mb)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(raw), testJWT) {
		t.Fatalf("serialized mailbox leaked the JWT: %s", raw)
	}
	if strings.Contains(string(raw), "secret://") {
		t.Fatalf("serialized mailbox exposed the secret reference: %s", raw)
	}
}

func TestCreateAddress_AddressIDVariants(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantID   string
		wantRef  string
		wantAddr string
	}{
		{
			name:     "numeric id",
			body:     `{"jwt":"` + testJWT + `","address":"sandbox001@moonbyte.top","address_id":98765}`,
			wantID:   "98765",
			wantRef:  "secret://mailbox/98765",
			wantAddr: testAddress,
		},
		{
			name:     "string id",
			body:     `{"jwt":"` + testJWT + `","address":"sandbox001@moonbyte.top","address_id":"addr-abc"}`,
			wantID:   "addr-abc",
			wantRef:  "secret://mailbox/addr-abc",
			wantAddr: testAddress,
		},
		{
			name:     "object id",
			body:     `{"jwt":"` + testJWT + `","address":"sandbox001@moonbyte.top","address_id":{"id":"777"}}`,
			wantID:   "777",
			wantRef:  "secret://mailbox/777",
			wantAddr: testAddress,
		},
		{
			name:     "missing id falls back to address",
			body:     `{"jwt":"` + testJWT + `","address":"sandbox001@moonbyte.top"}`,
			wantID:   testAddress,
			wantRef:  "secret://mailbox/sandbox001@moonbyte.top",
			wantAddr: testAddress,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}, nil)
			mb, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
			if err != nil {
				t.Fatalf("CreateAddress() error = %v", err)
			}
			if mb.ID != tc.wantID || mb.SecretRef != tc.wantRef || mb.Address != tc.wantAddr {
				t.Fatalf("mailbox = %+v, want id=%q ref=%q address=%q", mb, tc.wantID, tc.wantRef, tc.wantAddr)
			}
			if stored, ok := secrets.get(tc.wantRef); !ok || stored != testJWT {
				t.Fatalf("stored JWT = %q, %v", stored, ok)
			}
		})
	}
}

func TestCreateAddress_EnablePrefixPassthrough(t *testing.T) {
	var gotPrefix *bool
	p, _, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			EnablePrefix bool `json:"enablePrefix"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotPrefix = &body.EnablePrefix
		_, _ = w.Write([]byte(`{"jwt":"` + testJWT + `","address":"sandbox001@moonbyte.top","address_id":1}`))
	}, nil)
	if _, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001", EnablePrefix: true}); err != nil {
		t.Fatalf("CreateAddress() error = %v", err)
	}
	if gotPrefix == nil || !*gotPrefix {
		t.Fatalf("enablePrefix = %v, want true", gotPrefix)
	}
}

func TestCreateAddress_DuplicateFailsClosed(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"address already exists"}`))
	}, nil)

	_, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
	mustClass(t, err, errs.ClassConflict)
	mu.Lock()
	gotAttempts := attempts
	mu.Unlock()
	if gotAttempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (no second-address recovery)", gotAttempts)
	}
	if secrets.count() != 0 {
		t.Fatalf("secrets stored for a failed create: %d", secrets.count())
	}
	assertNoSecrets(t, err)
}

func TestCreateAddress_DuplicateMarkerIn400BodyFailsClosed(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"该地址已存在"}`))
	}, nil)

	_, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
	mustClass(t, err, errs.ClassConflict)
	mu.Lock()
	gotAttempts := attempts
	mu.Unlock()
	if gotAttempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1", gotAttempts)
	}
	if secrets.count() != 0 {
		t.Fatalf("secrets stored for a failed create: %d", secrets.count())
	}
}

func TestCreateAddress_DuplicateTwiceIsConflict(t *testing.T) {
	requests := 0
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"duplicate address"}`))
	}, nil)

	_, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
	mustClass(t, err, errs.ClassConflict)
	if requests != 1 {
		t.Fatalf("requests = %d, want exactly 1 (no second-address retry)", requests)
	}
	if secrets.count() != 0 {
		t.Fatalf("secrets stored for a failed create: %d", secrets.count())
	}
	assertNoSecrets(t, err)
}

func TestCreateAddress_DuplicateWithPrefixEnabledIsConflict(t *testing.T) {
	requests := 0
	p, _, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"address already exists"}`))
	}, nil)
	_, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001", EnablePrefix: true})
	mustClass(t, err, errs.ClassConflict)
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 (no retry when prefix already enabled)", requests)
	}
}

func TestCreateAddress_ErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		header     map[string]string
		body       string
		wantClass  errs.Class
		retryAfter time.Duration
	}{
		{name: "400 validation", status: 400, body: `{"error":"invalid domain"}`, wantClass: errs.ClassValidation},
		{name: "422 validation", status: 422, body: `{}`, wantClass: errs.ClassValidation},
		{name: "401 not authorized", status: 401, body: `{}`, wantClass: errs.ClassNotAuthorized},
		{name: "403 not authorized", status: 403, body: `{}`, wantClass: errs.ClassNotAuthorized},
		{name: "404 endpoint missing", status: 404, body: `{}`, wantClass: errs.ClassProviderBug},
		{name: "410 auth expired", status: 410, body: `{}`, wantClass: errs.ClassAuthExpired},
		{name: "429 rate limited honors retry-after", status: 429, header: map[string]string{"Retry-After": "5"}, body: `{}`, wantClass: errs.ClassRateLimited, retryAfter: 5 * time.Second},
		{name: "500 transient", status: 500, body: `{"error":"boom"}`, wantClass: errs.ClassUpstreamTransient},
		{name: "502 transient", status: 502, body: "bad gateway", wantClass: errs.ClassUpstreamTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}, nil)
			_, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
			classified := mustClass(t, err, tc.wantClass)
			if classified.HTTPStatus != tc.status {
				t.Fatalf("HTTPStatus = %d, want %d", classified.HTTPStatus, tc.status)
			}
			if errs.RetryAfterOf(err) != tc.retryAfter {
				t.Fatalf("RetryAfter = %s, want %s", errs.RetryAfterOf(err), tc.retryAfter)
			}
			if secrets.count() != 0 {
				t.Fatalf("secrets stored for failed create: %d", secrets.count())
			}
			assertNoSecrets(t, err)
		})
	}
}

func TestCreateAddress_TimeoutIsUncertain(t *testing.T) {
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}, func(c *Config) { c.Timeout = 30 * time.Millisecond })

	start := time.Now()
	_, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
	mustClass(t, err, errs.ClassTimeoutUncertain)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("CreateAddress took %s, timeout not applied", elapsed)
	}
	if secrets.count() != 0 {
		t.Fatalf("secrets stored for timed out create: %d", secrets.count())
	}
	assertNoSecrets(t, err)
}

func TestCreateAddress_NonJSONAndSchemaMismatch(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"non-json", "<html>not json</html>"},
		{"empty object", `{}`},
		{"missing jwt", `{"address":"sandbox001@moonbyte.top","address_id":1}`},
		{"invalid address", `{"jwt":"` + testJWT + `","address":"not-an-address","address_id":1}`},
		{"empty address", `{"jwt":"` + testJWT + `","address_id":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}, nil)
			_, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
			mustClass(t, err, errs.ClassProviderBug)
			if secrets.count() != 0 {
				t.Fatalf("secrets stored for invalid response: %d", secrets.count())
			}
			assertNoSecrets(t, err)
		})
	}
}

func TestCreateAddress_SecretStoreFailure(t *testing.T) {
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jwt":"` + testJWT + `","address":"` + testAddress + `","address_id":1}`))
	}, nil)
	secrets.putErr = errors.New("store unavailable")

	_, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
	mustClass(t, err, errs.ClassInternal)
	assertNoSecrets(t, err)
}

func TestCreateAddress_ValidationGates(t *testing.T) {
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be reached for invalid requests")
	}, func(c *Config) { c.AllowedDomains = []string{"extra.moonbyte.top"} })

	cases := []struct {
		name string
		req  provider.CreateAddressRequest
	}{
		{"unsafe name", provider.CreateAddressRequest{Name: "bad name!"}},
		{"name too long", provider.CreateAddressRequest{Name: strings.Repeat("a", 65)}},
		{"domain not allowlisted", provider.CreateAddressRequest{Name: "ok", Domain: "evil.example"}},
		{"invalid domain", provider.CreateAddressRequest{Name: "ok", Domain: "not a domain"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.CreateAddress(context.Background(), tc.req)
			mustClass(t, err, errs.ClassValidation)
			assertNoSecrets(t, err)
		})
	}
	if secrets.count() != 0 {
		t.Fatalf("secrets stored for invalid requests")
	}
}

// ---------------------------------------------------------------------------
// ListMessages
// ---------------------------------------------------------------------------

const rawMail = "Subject: Verify your email\r\n" +
	"From: Keycloak <no-reply@keycloak.local>\r\n" +
	"To: sandbox001@moonbyte.top\r\n" +
	"Date: Sat, 01 Mar 2025 11:59:00 +0000\r\n" +
	"Message-ID: <m1@moonbyte.top>\r\n" +
	"\r\n" +
	"Your code is 123456 and link https://id.example/verify?token=abc"

const rawMailJSON = `"Subject: Verify your email\r\nFrom: Keycloak <no-reply@keycloak.local>\r\nTo: sandbox001@moonbyte.top\r\nDate: Sat, 01 Mar 2025 11:59:00 +0000\r\nMessage-ID: <m1@moonbyte.top>\r\n\r\nYour code is 123456 and link https://id.example/verify?token=abc"`

func TestListMessages_BareArrayRawString(t *testing.T) {
	var gotAuth, gotCustom, gotQuery, gotPath string
	requests := 0
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotAuth = r.Header.Get("Authorization")
		gotCustom = r.Header.Get("x-custom-auth")
		gotQuery = r.URL.RawQuery
		gotPath = r.URL.Path
		if r.Header.Get("x-admin-auth") != "" {
			t.Error("user API must not receive the admin credential")
		}
		_, _ = w.Write([]byte(`[{"id":"m1","raw":` + rawMailJSON + `}]`))
	}, nil)
	loadJWT(t, secrets)

	msgs, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if gotPath != "/api/parsed_mails" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer "+testJWT {
		t.Fatalf("Authorization = %q, want bearer address credential", gotAuth)
	}
	if gotCustom != testSitePassword {
		t.Fatalf("x-custom-auth = %q, want site password", gotCustom)
	}
	if !strings.Contains(gotQuery, "limit=20") || !strings.Contains(gotQuery, "offset=0") {
		t.Fatalf("query = %q, want limit=20&offset=0", gotQuery)
	}

	if len(msgs) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(msgs))
	}
	msg := msgs[0]
	if msg.ID != "m1" || msg.MailboxID != testAddressID {
		t.Fatalf("message = %+v, want id m1 and mailbox id", msg)
	}
	if msg.Sender != "Keycloak <no-reply@keycloak.local>" {
		t.Fatalf("Sender = %q", msg.Sender)
	}
	if msg.Recipient != testAddress {
		t.Fatalf("Recipient = %q, want %q", msg.Recipient, testAddress)
	}
	if msg.Subject != "Verify your email" {
		t.Fatalf("Subject = %q", msg.Subject)
	}
	if !msg.ReceivedAt.Equal(time.Date(2025, 3, 1, 11, 59, 0, 0, time.UTC)) {
		t.Fatalf("ReceivedAt = %s, want 2025-03-01T11:59:00Z", msg.ReceivedAt)
	}
	if !strings.Contains(msg.Text, "Your code is 123456") {
		t.Fatalf("Text = %q, want raw body", msg.Text)
	}
}

func TestListMessages_SchemaVariants(t *testing.T) {
	itemFields := `{"id":"m1","subject":"Verify your email","from":"Keycloak <no-reply@keycloak.local>","to":"sandbox001@moonbyte.top","date":"2025-03-01T11:59:00Z","text":"Your code is 123456"}`
	itemObject := `{"id":"m2","subject":"Verify your email","from":{"Name":"Keycloak","Address":"no-reply@keycloak.local"},"to":[{"Address":"sandbox001@moonbyte.top"}],"timestamp":1740830340,"html":"<p>code 654321</p>"}`
	itemRawObject := `{"id":"m3","raw":{"headers":{"Subject":"Verify your email","From":"no-reply@keycloak.local","To":"sandbox001@moonbyte.top","Date":"Sat, 01 Mar 2025 11:59:00 +0000"},"text":"body code 111111"}}`

	cases := []struct {
		name     string
		body     string
		wantIDs  []string
		wantCode string
	}{
		{name: "results wrapper", body: `{"results":[` + itemFields + `],"count":1}`, wantIDs: []string{"m1"}, wantCode: "Your code is 123456"},
		{name: "parsed_mails wrapper", body: `{"parsed_mails":[` + itemFields + `]}`, wantIDs: []string{"m1"}},
		{name: "mails wrapper", body: `{"mails":[` + itemFields + `]}`, wantIDs: []string{"m1"}},
		{name: "object fields and epoch timestamp", body: `{"results":[` + itemObject + `]}`, wantIDs: []string{"m2"}},
		{name: "raw object with headers", body: `{"results":[` + itemRawObject + `]}`, wantIDs: []string{"m3"}},
		{name: "null results is empty", body: `{"results":null,"count":0}`, wantIDs: nil},
		{name: "bare array", body: `[` + itemFields + `]`, wantIDs: []string{"m1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}, nil)
			loadJWT(t, secrets)
			msgs, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
			if err != nil {
				t.Fatalf("ListMessages() error = %v", err)
			}
			var gotIDs []string
			for _, msg := range msgs {
				gotIDs = append(gotIDs, msg.ID)
			}
			if strings.Join(gotIDs, ",") != strings.Join(tc.wantIDs, ",") {
				t.Fatalf("IDs = %v, want %v", gotIDs, tc.wantIDs)
			}
			if len(msgs) > 0 && tc.wantCode != "" && !strings.Contains(msgs[0].Text, tc.wantCode) {
				t.Fatalf("Text = %q, want %q", msgs[0].Text, tc.wantCode)
			}
		})
	}

	t.Run("unknown envelope is a provider bug", func(t *testing.T) {
		p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"rows":[]}`))
		}, nil)
		loadJWT(t, secrets)
		_, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
		mustClass(t, err, errs.ClassProviderBug)
	})

	t.Run("unrecognizable items are skipped", func(t *testing.T) {
		p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"results":[42,null,{"unrelated":"x"},` + itemFields + `]}`))
		}, nil)
		loadJWT(t, secrets)
		msgs, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
		if err != nil {
			t.Fatalf("ListMessages() error = %v", err)
		}
		if len(msgs) != 1 || msgs[0].ID != "m1" {
			t.Fatalf("messages = %+v, want only m1", msgs)
		}
	})
}

func TestListMessages_SinceFilter(t *testing.T) {
	body := `{"results":[` +
		`{"id":"m1","subject":"a","from":"a@b","to":"sandbox001@moonbyte.top","date":"2025-03-01T11:59:00Z"},` +
		`{"id":"m2","subject":"b","from":"a@b","to":"sandbox001@moonbyte.top","date":"2025-03-01T11:58:00Z"}]}`
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}, nil)
	loadJWT(t, secrets)

	msgs, err := p.ListMessages(context.Background(), testMailbox(), time.Date(2025, 3, 1, 11, 58, 30, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "m1" {
		t.Fatalf("messages = %+v, want only m1 after since", msgs)
	}
}

func TestListMessages_Pagination(t *testing.T) {
	var mu sync.Mutex
	var offsets []string
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		offsets = append(offsets, r.URL.Query().Get("offset"))
		mu.Unlock()
		switch r.URL.Query().Get("offset") {
		case "0":
			_, _ = w.Write([]byte(`{"results":[{"id":"m1","subject":"a","from":"a@b","to":"sandbox001@moonbyte.top","date":"2025-03-01T11:59:00Z"},{"id":"m2","subject":"b","from":"a@b","to":"sandbox001@moonbyte.top","date":"2025-03-01T11:58:00Z"}]}`))
		case "2":
			_, _ = w.Write([]byte(`{"results":[{"id":"m3","subject":"c","from":"a@b","to":"sandbox001@moonbyte.top","date":"2025-03-01T11:57:00Z"}]}`))
		default:
			t.Errorf("unexpected offset %q", r.URL.Query().Get("offset"))
		}
	}, func(c *Config) {
		c.ListLimit = 2
		c.MaxPages = 3
	})
	loadJWT(t, secrets)

	msgs, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len(messages) = %d, want 3 across two pages", len(msgs))
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(offsets, ",") != "0,2" {
		t.Fatalf("offsets = %v, want [0 2]", offsets)
	}
}

func TestListMessages_MaxMessagesCap(t *testing.T) {
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":"m1","subject":"a","from":"a@b","to":"sandbox001@moonbyte.top","date":"2025-03-01T11:59:00Z"},{"id":"m2","subject":"b","from":"a@b","to":"sandbox001@moonbyte.top","date":"2025-03-01T11:58:00Z"}]}`))
	}, func(c *Config) {
		c.ListLimit = 2
		c.MaxMessages = 1
	})
	loadJWT(t, secrets)
	msgs, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(messages) = %d, want 1 (MaxMessages cap)", len(msgs))
	}
}

func TestListMessages_ErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		header     map[string]string
		body       string
		wantClass  errs.Class
		retryAfter time.Duration
	}{
		{name: "400 validation", status: 400, body: `{}`, wantClass: errs.ClassValidation},
		{name: "401 not authorized", status: 401, body: `{}`, wantClass: errs.ClassNotAuthorized},
		{name: "403 not authorized", status: 403, body: `{}`, wantClass: errs.ClassNotAuthorized},
		{name: "404 endpoint missing", status: 404, body: `{}`, wantClass: errs.ClassProviderBug},
		{name: "410 auth expired", status: 410, body: `{}`, wantClass: errs.ClassAuthExpired},
		{name: "429 honors retry-after", status: 429, header: map[string]string{"Retry-After": "7"}, body: `{}`, wantClass: errs.ClassRateLimited, retryAfter: 7 * time.Second},
		{name: "500 transient", status: 500, body: `{"error":"boom"}`, wantClass: errs.ClassUpstreamTransient},
		{name: "502 transient", status: 502, body: "bad gateway", wantClass: errs.ClassUpstreamTransient},
		{name: "503 transient", status: 503, body: "unavailable", wantClass: errs.ClassUpstreamTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}, nil)
			loadJWT(t, secrets)

			_, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
			classified := mustClass(t, err, tc.wantClass)
			if classified.HTTPStatus != tc.status {
				t.Fatalf("HTTPStatus = %d, want %d", classified.HTTPStatus, tc.status)
			}
			if errs.RetryAfterOf(err) != tc.retryAfter {
				t.Fatalf("RetryAfter = %s, want %s", errs.RetryAfterOf(err), tc.retryAfter)
			}
			assertNoSecrets(t, err)
		})
	}

	t.Run("malformed json", func(t *testing.T) {
		p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"results":[`))
		}, nil)
		loadJWT(t, secrets)
		_, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
		mustClass(t, err, errs.ClassProviderBug)
		assertNoSecrets(t, err)
	})

	t.Run("non json body", func(t *testing.T) {
		p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json at all"))
		}, nil)
		loadJWT(t, secrets)
		_, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
		mustClass(t, err, errs.ClassProviderBug)
		assertNoSecrets(t, err)
	})

	t.Run("timeout is transient for reads", func(t *testing.T) {
		p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}, func(c *Config) { c.Timeout = 30 * time.Millisecond })
		loadJWT(t, secrets)
		_, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
		mustClass(t, err, errs.ClassUpstreamTransient)
		assertNoSecrets(t, err)
	})

	t.Run("cancelled context", func(t *testing.T) {
		p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler must not be reached with a cancelled context")
		}, nil)
		loadJWT(t, secrets)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := p.ListMessages(ctx, testMailbox(), time.Time{})
		mustClass(t, err, errs.ClassCancelled)
	})
}

func TestListMessages_CredentialResolution(t *testing.T) {
	cases := []struct {
		name       string
		secretRef  string
		resolveErr error
		resolved   string
	}{
		{name: "missing reference", secretRef: "", resolved: testJWT},
		{name: "non opaque reference", secretRef: "raw-jwt", resolved: testJWT},
		{name: "resolver failure", secretRef: testSecretRef, resolveErr: errors.New("vault down")},
		{name: "resolver returns empty", secretRef: testSecretRef, resolved: ""},
		{name: "resolver returns control characters", secretRef: testSecretRef, resolved: "bad\ncredential"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				t.Error("handler must not be reached without a usable credential")
			}, nil)
			secrets.getErr = tc.resolveErr
			if tc.resolved != "" {
				_ = secrets.Put(context.Background(), testSecretRef, SecretRefKind, tc.resolved)
			}
			mb := testMailbox()
			mb.SecretRef = tc.secretRef

			_, err := p.ListMessages(context.Background(), mb, time.Time{})
			mustClass(t, err, errs.ClassNotAuthorized)
			if requests != 0 {
				t.Fatalf("requests = %d, want 0", requests)
			}
			assertNoSecrets(t, err)
		})
	}
}

// ---------------------------------------------------------------------------
// GetMessage
// ---------------------------------------------------------------------------

const detailBody = `{"id":"m1","raw":` + rawMailJSON + `}`

func TestGetMessage_DetailSuccess(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/api/parsed_mail/m1" {
			t.Errorf("path = %q, want /api/parsed_mail/m1", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+testJWT {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(detailBody))
	}, nil)
	loadJWT(t, secrets)

	msg, err := p.GetMessage(context.Background(), testMailbox(), "m1")
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if msg.ID != "m1" || msg.MailboxID != testAddressID || msg.Subject != "Verify your email" {
		t.Fatalf("message = %+v", msg)
	}
	if !strings.Contains(msg.Text, "Your code is 123456") {
		t.Fatalf("Text = %q", msg.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 {
		t.Fatalf("paths = %v, want only the detail endpoint", paths)
	}
}

func TestGetMessage_Detail404FallsBackToList(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/parsed_mail/m1":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		case "/api/parsed_mails":
			_, _ = w.Write([]byte(`{"results":[{"id":"m1","raw":` + rawMailJSON + `}]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}, nil)
	loadJWT(t, secrets)

	msg, err := p.GetMessage(context.Background(), testMailbox(), "m1")
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if msg.ID != "m1" || !strings.Contains(msg.Text, "Your code is 123456") {
		t.Fatalf("message = %+v, want list-scan fallback", msg)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || paths[0] != "/api/parsed_mail/m1" || paths[1] != "/api/parsed_mails" {
		t.Fatalf("paths = %v, want detail then list fallback", paths)
	}
}

func TestGetMessage_Detail405FallsBackToList(t *testing.T) {
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/parsed_mail/m1":
			w.WriteHeader(http.StatusMethodNotAllowed)
		case "/api/parsed_mails":
			_, _ = w.Write([]byte(`{"results":[{"id":"m1","raw":` + rawMailJSON + `}]}`))
		}
	}, nil)
	loadJWT(t, secrets)
	if _, err := p.GetMessage(context.Background(), testMailbox(), "m1"); err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
}

func TestGetMessage_DisableDetailEndpoint(t *testing.T) {
	detailCalls := 0
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/parsed_mail/") {
			detailCalls++
		}
		_, _ = w.Write([]byte(`{"results":[{"id":"m1","raw":` + rawMailJSON + `}]}`))
	}, func(c *Config) { c.DisableDetailEndpoint = true })
	loadJWT(t, secrets)

	if _, err := p.GetMessage(context.Background(), testMailbox(), "m1"); err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if detailCalls != 0 {
		t.Fatalf("detail calls = %d, want 0 when disabled", detailCalls)
	}
}

func TestGetMessage_NotFound(t *testing.T) {
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/parsed_mail/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/api/parsed_mails":
			_, _ = w.Write([]byte(`{"results":[]}`))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}, nil)
	loadJWT(t, secrets)

	_, err := p.GetMessage(context.Background(), testMailbox(), "missing")
	mustClass(t, err, errs.ClassNotFound)
}

func TestGetMessage_ErrorPassthrough(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		header     map[string]string
		wantClass  errs.Class
		retryAfter time.Duration
	}{
		{name: "401 not authorized", status: 401, wantClass: errs.ClassNotAuthorized},
		{name: "403 not authorized", status: 403, wantClass: errs.ClassNotAuthorized},
		{name: "410 auth expired", status: 410, wantClass: errs.ClassAuthExpired},
		{name: "429 rate limited", status: 429, header: map[string]string{"Retry-After": "4"}, wantClass: errs.ClassRateLimited, retryAfter: 4 * time.Second},
		{name: "500 transient", status: 500, wantClass: errs.ClassUpstreamTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			}, nil)
			loadJWT(t, secrets)
			_, err := p.GetMessage(context.Background(), testMailbox(), "m1")
			mustClass(t, err, tc.wantClass)
			if errs.RetryAfterOf(err) != tc.retryAfter {
				t.Fatalf("RetryAfter = %s, want %s", errs.RetryAfterOf(err), tc.retryAfter)
			}
			assertNoSecrets(t, err)
		})
	}

	t.Run("unrecognized detail schema", func(t *testing.T) {
		p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"weird":true}`))
		}, nil)
		loadJWT(t, secrets)
		_, err := p.GetMessage(context.Background(), testMailbox(), "m1")
		mustClass(t, err, errs.ClassProviderBug)
		assertNoSecrets(t, err)
	})

	t.Run("empty id", func(t *testing.T) {
		p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler must not be reached for an empty id")
		}, nil)
		loadJWT(t, secrets)
		_, err := p.GetMessage(context.Background(), testMailbox(), "  ")
		mustClass(t, err, errs.ClassValidation)
	})
}

// ---------------------------------------------------------------------------
// Security: redirects and secret hygiene
// ---------------------------------------------------------------------------

func TestCrossOriginRedirectNeverForwardsCredentials(t *testing.T) {
	var mu sync.Mutex
	targetRequests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targetRequests++
		mu.Unlock()
		if r.Header.Get("Authorization") != "" || r.Header.Get("x-admin-auth") != "" {
			t.Errorf("redirect target received credentials: %v", r.Header)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	originRequests := 0
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		originRequests++
		http.Redirect(w, r, target.URL+"/api/parsed_mails", http.StatusFound)
	}, nil)
	loadJWT(t, secrets)

	_, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
	mustClass(t, err, errs.ClassProviderBug)
	if originRequests != 1 {
		t.Fatalf("origin requests = %d, want 1", originRequests)
	}
	mu.Lock()
	defer mu.Unlock()
	if targetRequests != 0 {
		t.Fatalf("redirect target requests = %d, want 0", targetRequests)
	}
	assertNoSecrets(t, err)
}

func TestSameOriginRedirectIsFollowed(t *testing.T) {
	requests := 0
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("redirected") == "" {
			http.Redirect(w, r, "/api/parsed_mails?redirected=1", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}, nil)
	loadJWT(t, secrets)

	if _, err := p.ListMessages(context.Background(), testMailbox(), time.Time{}); err != nil {
		t.Fatalf("same-origin redirect failed: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestCreateRedirectNeverForwardsAdminCredential(t *testing.T) {
	var mu sync.Mutex
	targetRequests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targetRequests++
		mu.Unlock()
	}))
	defer target.Close()

	p, _, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-admin-auth") != testAdminPassword {
			t.Errorf("origin did not receive the admin credential")
		}
		http.Redirect(w, r, target.URL+"/admin/new_address", http.StatusTemporaryRedirect)
	}, nil)

	_, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
	mustClass(t, err, errs.ClassProviderBug)
	mu.Lock()
	defer mu.Unlock()
	if targetRequests != 0 {
		t.Fatalf("redirect target requests = %d, want 0", targetRequests)
	}
	assertNoSecrets(t, err)
}

func TestSecretsNeverAppearInErrors(t *testing.T) {
	scenarios := []struct {
		name  string
		cfg   func(*Config)
		setup func(*fakeSecrets)
		call  func(*Provider) error
	}{
		{
			name: "unauthorized list",
			cfg:  nil,
			call: func(p *Provider) error {
				_, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
				return err
			},
		},
		{
			name: "create 500",
			cfg:  func(c *Config) {},
			call: func(p *Provider) error {
				_, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
				return err
			},
		},
		{
			name:  "resolver error",
			setup: func(s *fakeSecrets) { s.getErr = errors.New("vault: " + testJWT) },
			call: func(p *Provider) error {
				_, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
				return err
			},
		},
	}
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"` + testJWT + `"}`))
			}, tc.cfg)
			if tc.setup != nil {
				tc.setup(secrets)
			}
			loadJWT(t, secrets)
			err := tc.call(p)
			if err == nil {
				t.Fatal("error = nil")
			}
			assertNoSecrets(t, err)
		})
	}
}

// ---------------------------------------------------------------------------
// Normalizer details
// ---------------------------------------------------------------------------

func TestDerivedMessageIDIsStable(t *testing.T) {
	item := `{"results":[{"subject":"Verify your email","from":"no-reply@keycloak.local","to":"sandbox001@moonbyte.top","date":"2025-03-01T11:59:00Z","text":"code 123456"}]}`
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(item))
	}, nil)
	loadJWT(t, secrets)

	first, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	second, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("messages = %d and %d, want 1 each", len(first), len(second))
	}
	if first[0].ID == "" || first[0].ID != second[0].ID {
		t.Fatalf("derived IDs = %q and %q, want stable non-empty ID", first[0].ID, second[0].ID)
	}
	if !strings.HasPrefix(first[0].ID, "cf-auto-") {
		t.Fatalf("derived ID = %q, want cf-auto- prefix", first[0].ID)
	}
}

func TestNormalizerRejectsUnknownEnvelopeOnly(t *testing.T) {
	// A 200 response that is valid JSON but has no list is a schema drift and
	// must surface as PROVIDER_BUG instead of an empty mailbox.
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"count":0}`))
	}, nil)
	loadJWT(t, secrets)
	_, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
	mustClass(t, err, errs.ClassProviderBug)
}

func TestMessageBodiesAreTransient(t *testing.T) {
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"m1","raw":` + rawMailJSON + `}]`))
	}, nil)
	loadJWT(t, secrets)

	msgs, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(messages) = %d", len(msgs))
	}
	raw, err := json.Marshal(msgs[0])
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(raw), "123456") || strings.Contains(string(raw), "verify?token=abc") {
		t.Fatalf("serialized message leaked transient body: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// Cross-layer integration: adapter output feeds the mailx matcher
// ---------------------------------------------------------------------------

// TestNormalizedMessagesMatchMailxPolicy proves the two layers agree: the
// Cloudflare normalizer output is accepted by the mailx policy only for the
// genuinely addressed, allowlisted message, and the extracted link/code match
// the raw body.
func TestNormalizedMessagesMatchMailxPolicy(t *testing.T) {
	spoofed := "Subject: Verify your email\r\n" +
		"From: \"no-reply@keycloak.local\" <attacker@evil.example>\r\n" +
		"To: sandbox001@moonbyte.top\r\n" +
		"Date: Sat, 01 Mar 2025 11:59:00 +0000\r\n" +
		"\r\ncode 999999"
	otherRecipient := "Subject: Verify your email\r\n" +
		"From: no-reply@keycloak.local\r\n" +
		"To: someone-else@moonbyte.top\r\n" +
		"Date: Sat, 01 Mar 2025 11:59:00 +0000\r\n" +
		"\r\ncode 888888"

	body := `{"results":[` +
		marshalRawItem("m-spoof", spoofed) + `,` +
		marshalRawItem("m-other", otherRecipient) + `,` +
		marshalRawItem("m1", rawMail) + `]}`
	p, secrets, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}, nil)
	loadJWT(t, secrets)

	msgs, err := p.ListMessages(context.Background(), testMailbox(), time.Time{})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(msgs))
	}

	received := time.Date(2025, 3, 1, 11, 59, 0, 0, time.UTC)
	matcher, err := mailx.NewMatcher(mailx.Config{
		ExpectedRecipient: testAddress,
		SenderAllowlist:   []string{"keycloak.local"},
		SubjectPatterns:   []string{"verify your email"},
		WindowStart:       received.Add(-time.Minute),
		WindowEnd:         received.Add(time.Minute),
		ClockSkew:         time.Minute,
	})
	if err != nil {
		t.Fatalf("mailx.NewMatcher() error = %v", err)
	}
	res, ok := matcher.Match(msgs)
	if !ok {
		t.Fatal("mailx.Match() = no match, want the genuinely addressed message")
	}
	if res.MessageID != "m1" {
		t.Fatalf("MessageID = %q, want m1 (spoofed sender and other recipient must be rejected)", res.MessageID)
	}
	if res.Code != "123456" {
		t.Fatalf("Code = %q, want 123456", res.Code)
	}
	if !strings.Contains(res.Link, "https://id.example/verify?token=abc") {
		t.Fatalf("Link = %q, want the verification URL", res.Link)
	}
}

func marshalRawItem(id, raw string) string {
	encoded, err := json.Marshal(map[string]string{"id": id, "raw": raw})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// kindEnforcingSecrets mimics internal/secrets.Service.Put, which rejects a
// secret kind that differs from the reference kind. This regression test exists
// because "secret://<kind>/<id>" fixes the kind to the reference namespace.
type kindEnforcingSecrets struct {
	*fakeSecrets

	mu       sync.Mutex
	lastKind string
}

func (e *kindEnforcingSecrets) Put(ctx context.Context, ref, kind, plaintext string) error {
	refKindValue, ok := refKind(ref)
	if !ok {
		return errors.New("invalid secret reference")
	}
	if refKindValue != kind {
		return fmt.Errorf("kind mismatch (%s != %s)", refKindValue, kind)
	}
	e.mu.Lock()
	e.lastKind = kind
	e.mu.Unlock()
	return e.fakeSecrets.Put(ctx, ref, kind, plaintext)
}

func TestSecretStoreKindContract(t *testing.T) {
	secrets := &kindEnforcingSecrets{fakeSecrets: newFakeSecrets()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/new_address":
			_, _ = w.Write([]byte(`{"jwt":"` + testJWT + `","address":"` + testAddress + `","address_id":12345}`))
		case "/api/parsed_mails":
			if r.Header.Get("Authorization") != "Bearer "+testJWT {
				t.Errorf("Authorization = %q, want the resolved address credential", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"results":[]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := Config{
		BaseURL:       srv.URL,
		AllowLoopback: true,
		AdminAuth:     testAdminPassword,
		SitePassword:  testSitePassword,
		Domain:        "moonbyte.top",
		Timeout:       2 * time.Second,
		Secrets:       secrets,
		Resolver:      secrets,
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	mb, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
	if err != nil {
		t.Fatalf("CreateAddress() error = %v", err)
	}
	if mb.SecretRef != testSecretRef {
		t.Fatalf("SecretRef = %q, want %q", mb.SecretRef, testSecretRef)
	}
	secrets.mu.Lock()
	kind := secrets.lastKind
	secrets.mu.Unlock()
	if kind != "mailbox" {
		t.Fatalf("Put kind = %q, want mailbox (derived from the secret reference)", kind)
	}
	if _, err := p.ListMessages(context.Background(), mb, time.Time{}); err != nil {
		t.Fatalf("ListMessages() through the resolving store error = %v", err)
	}
}
