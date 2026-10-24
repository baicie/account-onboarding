package mailpit

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

var testNow = time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)

const (
	testAddress = "sandbox001@mailpit.local"
	listBody    = `{
	  "total": 3, "unread": 3, "count": 3,
	  "messages": [
	    {
	      "ID": "m1", "MessageID": "<m1@mailpit>",
	      "From": {"Name": "Keycloak", "Address": "no-reply@keycloak.local"},
	      "To": [{"Name": "", "Address": "sandbox001@mailpit.local"}],
	      "Subject": "Verify your email",
	      "Created": "2025-03-01T11:59:00Z",
	      "Size": 2048, "Attachments": 0, "Snippet": "Verify your email"
	    },
	    {
	      "ID": "m2", "MessageID": "<m2@mailpit>",
	      "From": {"Name": "Keycloak", "Address": "no-reply@keycloak.local"},
	      "To": [{"Name": "", "Address": "sandbox001@mailpit.local"}],
	      "Subject": "Second email",
	      "Created": "2025-03-01T11:58:00+00:00",
	      "Size": 1024, "Attachments": 0, "Snippet": "Second"
	    },
	    {
	      "ID": "m3", "MessageID": "<m3@mailpit>",
	      "From": {"Name": "Other", "Address": "someone@other.example"},
	      "To": [{"Name": "", "Address": "other@mailpit.local"}],
	      "Subject": "Not ours",
	      "Created": "2025-03-01T11:57:00Z",
	      "Size": 512, "Attachments": 0, "Snippet": "Nope"
	    }
	  ]
	}`
)

// newProvider starts an httptest server, builds a Provider against it and
// returns both. The server URL is loopback, which satisfies the construction
// gate.
func newProvider(t *testing.T, mut func(*Config), handler http.HandlerFunc) (*Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := Config{BaseURL: srv.URL, Domain: "mailpit.local"}
	if mut != nil {
		mut(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p, srv
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

func mailbox() provider.Mailbox {
	return provider.Mailbox{ID: testAddress, Address: testAddress, Provider: ProviderName}
}

func TestNew_LoopbackGate(t *testing.T) {
	ok := []string{
		"",
		"http://127.0.0.1:8025",
		"http://127.0.0.2:8025",
		"http://localhost:8025",
		"https://127.0.0.1:8443",
		"http://[::1]:8025",
	}
	for _, raw := range ok {
		t.Run("allow_"+raw, func(t *testing.T) {
			if _, err := New(Config{BaseURL: raw}); err != nil {
				t.Fatalf("New(%q) error = %v, want success", raw, err)
			}
		})
	}

	rejected := []string{
		"https://api.moonbyte.top",
		"http://10.0.0.5:8025",
		"http://192.168.1.10:8025",
		"http://mailpit.internal:8025",
		"http://user:pass@127.0.0.1:8025",
		"ftp://127.0.0.1:8025",
		"http://127.0.0.1:8025/?x=1",
		"://bad",
	}
	for _, raw := range rejected {
		t.Run("reject_"+raw, func(t *testing.T) {
			_, err := New(Config{BaseURL: raw})
			if err == nil {
				t.Fatalf("New(%q) = nil error, want rejection", raw)
			}
			if class := errs.ClassOf(err); class != errs.ClassValidation {
				t.Fatalf("error class = %s, want VALIDATION (err=%v)", class, err)
			}
		})
	}
}

func TestDescriptor(t *testing.T) {
	p, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	d := p.Descriptor()
	if d.Name != ProviderName || d.Kind != provider.KindMailbox {
		t.Fatalf("descriptor = %+v, want mailbox/%s", d, ProviderName)
	}
	if !d.Enabled || d.Live {
		t.Fatalf("descriptor = %+v, want enabled local provider", d)
	}
	if d.ContractVersion != ContractVersion {
		t.Fatalf("contract version = %q, want %q", d.ContractVersion, ContractVersion)
	}
	wantCaps := map[string]bool{"address.create": true, "message.list": true, "message.get": true, "address.cleanup": true}
	for _, capability := range d.Capabilities {
		delete(wantCaps, capability)
	}
	if len(wantCaps) != 0 {
		t.Fatalf("missing capabilities: %v", wantCaps)
	}

	disabled, err := New(Config{Disabled: true})
	if err != nil {
		t.Fatalf("New(Disabled) error = %v", err)
	}
	if dd := disabled.Descriptor(); dd.Enabled || dd.RestrictionReason == "" {
		t.Fatalf("disabled descriptor = %+v, want Enabled=false with restriction reason", dd)
	}
}

func TestCreateAddress_LocalGeneration(t *testing.T) {
	called := 0
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		called++
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})

	before := time.Now().UTC()
	mb, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "Sandbox001", Domain: "mailpit.local"})
	if err != nil {
		t.Fatalf("CreateAddress() error = %v", err)
	}
	if called != 0 {
		t.Fatalf("CreateAddress made %d HTTP calls, want 0 (local sink)", called)
	}
	if mb.Address != testAddress {
		t.Fatalf("Address = %q, want %q", mb.Address, testAddress)
	}
	if mb.ID != testAddress {
		t.Fatalf("ID = %q, want address as stable ID", mb.ID)
	}
	if mb.Provider != ProviderName {
		t.Fatalf("Provider = %q, want %q", mb.Provider, ProviderName)
	}
	if mb.SecretRef != "" {
		t.Fatalf("SecretRef = %q, want empty (sink has no credential)", mb.SecretRef)
	}
	if mb.CreatedAt.Before(before) || mb.CreatedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("CreatedAt = %s, want current time", mb.CreatedAt)
	}
}

func TestCreateAddress_DeterministicFromKey(t *testing.T) {
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {})

	first, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Key: "run-42"})
	if err != nil {
		t.Fatalf("CreateAddress() error = %v", err)
	}
	second, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Key: "run-42"})
	if err != nil {
		t.Fatalf("CreateAddress() error = %v", err)
	}
	if first.Address != second.Address {
		t.Fatalf("addresses = %q and %q, want stable derivation from key", first.Address, second.Address)
	}
	if !strings.HasSuffix(first.Address, "@mailpit.local") {
		t.Fatalf("Address = %q, want @mailpit.local suffix", first.Address)
	}

	third, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{})
	if err != nil {
		t.Fatalf("CreateAddress() error = %v", err)
	}
	fourth, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{})
	if err != nil {
		t.Fatalf("CreateAddress() error = %v", err)
	}
	if third.Address == fourth.Address {
		t.Fatalf("random addresses collided: %q", third.Address)
	}
}

func TestCreateAddress_Validation(t *testing.T) {
	p, _ := newProvider(t, func(c *Config) { c.AllowedDomains = []string{"extra.local"} }, func(w http.ResponseWriter, r *http.Request) {})

	cases := []struct {
		name string
		req  provider.CreateAddressRequest
	}{
		{"unsafe local part", provider.CreateAddressRequest{Name: "bad name!"}},
		{"local part too long", provider.CreateAddressRequest{Name: strings.Repeat("a", 65)}},
		{"domain not allowlisted", provider.CreateAddressRequest{Name: "ok", Domain: "evil.example"}},
		{"invalid domain", provider.CreateAddressRequest{Name: "ok", Domain: "not a domain"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.CreateAddress(context.Background(), tc.req)
			mustClass(t, err, errs.ClassValidation)
		})
	}

	t.Run("extra allowed domain", func(t *testing.T) {
		mb, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "ok", Domain: "extra.local"})
		if err != nil {
			t.Fatalf("CreateAddress() error = %v", err)
		}
		if mb.Address != "ok@extra.local" {
			t.Fatalf("Address = %q, want ok@extra.local", mb.Address)
		}
	})
}

func TestListMessages_Success(t *testing.T) {
	var gotPath, gotQuery, gotMethod string
	var mu sync.Mutex
	p, _ := newProvider(t, func(c *Config) { c.MaxMessages = 42 }, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotQuery, gotMethod = r.URL.Path, r.URL.RawQuery, r.Method
		mu.Unlock()
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept header = %q, want application/json", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listBody))
	})

	msgs, err := p.ListMessages(context.Background(), mailbox(), time.Time{})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	mu.Lock()
	path, query, method := gotPath, gotQuery, gotMethod
	mu.Unlock()
	if method != http.MethodGet || path != "/api/v1/messages" {
		t.Fatalf("request = %s %s, want GET /api/v1/messages", method, path)
	}
	if !strings.Contains(query, "limit=42") {
		t.Fatalf("query = %q, want limit=42", query)
	}

	if len(msgs) != 2 {
		t.Fatalf("len(messages) = %d, want 2 (shared sink filtered by recipient)", len(msgs))
	}
	first := msgs[0]
	if first.ID != "m1" || first.MailboxID != testAddress {
		t.Fatalf("first message = %+v, want m1 with mailbox id", first)
	}
	if first.Recipient != testAddress {
		t.Fatalf("Recipient = %q, want %q", first.Recipient, testAddress)
	}
	if first.Sender != "Keycloak <no-reply@keycloak.local>" {
		t.Fatalf("Sender = %q, want RFC 5322 form", first.Sender)
	}
	if first.Subject != "Verify your email" {
		t.Fatalf("Subject = %q", first.Subject)
	}
	if !first.ReceivedAt.Equal(time.Date(2025, 3, 1, 11, 59, 0, 0, time.UTC)) {
		t.Fatalf("ReceivedAt = %s, want 11:59Z", first.ReceivedAt)
	}
	if first.Text != "" || first.HTML != "" {
		t.Fatalf("list response must not carry bodies, got text=%q html=%q", first.Text, first.HTML)
	}
	if msgs[1].ID != "m2" {
		t.Fatalf("second message ID = %q, want m2", msgs[1].ID)
	}
}

func TestListMessages_SinceFilter(t *testing.T) {
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(listBody))
	})

	msgs, err := p.ListMessages(context.Background(), mailbox(), time.Date(2025, 3, 1, 11, 58, 30, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "m1" {
		t.Fatalf("messages = %+v, want only m1 after since", msgs)
	}
}

func TestListMessages_SchemaVariants(t *testing.T) {
	t.Run("bare array", func(t *testing.T) {
		p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"ID":"m1","To":[{"Address":"sandbox001@mailpit.local"}],"From":{"Address":"no-reply@keycloak.local"},"Subject":"s","Created":"2025-03-01T11:59:00Z"}]`))
		})
		msgs, err := p.ListMessages(context.Background(), mailbox(), time.Time{})
		if err != nil || len(msgs) != 1 {
			t.Fatalf("ListMessages() = (%v, %v), want one message", msgs, err)
		}
	})

	t.Run("From as string and To as string", func(t *testing.T) {
		p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"messages":[{"ID":"m1","From":"Keycloak <no-reply@keycloak.local>","To":"sandbox001@mailpit.local","Subject":"s","Created":"2025-03-01T11:59:00Z"}]}`))
		})
		msgs, err := p.ListMessages(context.Background(), mailbox(), time.Time{})
		if err != nil || len(msgs) != 1 {
			t.Fatalf("ListMessages() = (%v, %v), want one message", msgs, err)
		}
		if msgs[0].Recipient != testAddress {
			t.Fatalf("Recipient = %q", msgs[0].Recipient)
		}
	})

	t.Run("malformed timestamp", func(t *testing.T) {
		p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"messages":[{"ID":"m1","To":[{"Address":"sandbox001@mailpit.local"}],"Created":"01 Mar 2025"}]}`))
		})
		_, err := p.ListMessages(context.Background(), mailbox(), time.Time{})
		mustClass(t, err, errs.ClassProviderBug)
	})
}

func TestListMessages_ErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		headers    map[string]string
		body       string
		wantClass  errs.Class
		retryAfter time.Duration
	}{
		{name: "429 honors retry-after", status: 429, headers: map[string]string{"Retry-After": "3"}, body: `{"error":"slow down"}`, wantClass: errs.ClassRateLimited, retryAfter: 3 * time.Second},
		{name: "500 transient", status: 500, body: `{"error":"boom"}`, wantClass: errs.ClassUpstreamTransient},
		{name: "502 transient", status: 502, body: "bad gateway", wantClass: errs.ClassUpstreamTransient},
		{name: "410 expired", status: 410, body: `{}`, wantClass: errs.ClassAuthExpired},
		{name: "401 unauthorized", status: 401, body: `{}`, wantClass: errs.ClassNotAuthorized},
		{name: "403 forbidden", status: 403, body: `{}`, wantClass: errs.ClassNotAuthorized},
		{name: "400 validation", status: 400, body: `{}`, wantClass: errs.ClassValidation},
		{name: "404 endpoint missing is a provider bug", status: 404, body: `{}`, wantClass: errs.ClassProviderBug},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := p.ListMessages(context.Background(), mailbox(), time.Time{})
			classified := mustClass(t, err, tc.wantClass)
			if classified.HTTPStatus != tc.status {
				t.Fatalf("HTTPStatus = %d, want %d", classified.HTTPStatus, tc.status)
			}
			if errs.RetryAfterOf(err) != tc.retryAfter {
				t.Fatalf("RetryAfter = %s, want %s", errs.RetryAfterOf(err), tc.retryAfter)
			}
			if strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "slow down") {
				t.Fatalf("error leaked upstream body: %v", err)
			}
		})
	}
}

func TestListMessages_MalformedJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", "<html>oops</html>"},
		{"truncated", `{"messages":[`},
		{"missing messages field", `{"total":0}`},
		{"empty body", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := p.ListMessages(context.Background(), mailbox(), time.Time{})
			mustClass(t, err, errs.ClassProviderBug)
		})
	}
}

func TestListMessages_Timeout(t *testing.T) {
	p, _ := newProvider(t, func(c *Config) { c.Timeout = 30 * time.Millisecond }, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})

	start := time.Now()
	_, err := p.ListMessages(context.Background(), mailbox(), time.Time{})
	elapsed := time.Since(start)
	mustClass(t, err, errs.ClassUpstreamTransient)
	if elapsed > time.Second {
		t.Fatalf("ListMessages took %s, timeout was not applied", elapsed)
	}
}

func TestListMessages_CancelledContext(t *testing.T) {
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be reached with a cancelled context")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.ListMessages(ctx, mailbox(), time.Time{})
	mustClass(t, err, errs.ClassCancelled)
}

func TestListMessages_MissingMailboxAddress(t *testing.T) {
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be reached without a mailbox address")
	})
	_, err := p.ListMessages(context.Background(), provider.Mailbox{}, time.Time{})
	mustClass(t, err, errs.ClassValidation)
}

func TestGetMessage_Success(t *testing.T) {
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		// Mailpit v1 exposes the single-message detail at the singular path.
		if r.URL.Path != "/api/v1/message/m1" {
			t.Errorf("path = %q, want /api/v1/message/m1", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
		  "ID": "m1", "MessageID": "<m1@mailpit>",
		  "From": {"Name": "Keycloak", "Address": "no-reply@keycloak.local"},
		  "To": [{"Name": "", "Address": "sandbox001@mailpit.local"}],
		  "Subject": "Verify your email",
		  "Created": "2025-03-01T11:59:00.000000+00:00",
		  "Text": "Your code is 123456",
		  "HTML": "<a href=\"https://id.example/verify?token=abc\">Verify</a>"
		}`))
	})

	msg, err := p.GetMessage(context.Background(), mailbox(), "m1")
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if msg.ID != "m1" || msg.MailboxID != testAddress {
		t.Fatalf("message = %+v", msg)
	}
	if msg.Text != "Your code is 123456" {
		t.Fatalf("Text = %q", msg.Text)
	}
	if !strings.Contains(msg.HTML, "https://id.example/verify?token=abc") {
		t.Fatalf("HTML = %q", msg.HTML)
	}
	if !msg.ReceivedAt.Equal(time.Date(2025, 3, 1, 11, 59, 0, 0, time.UTC)) {
		t.Fatalf("ReceivedAt = %s", msg.ReceivedAt)
	}

	// Transient bodies must never be serialized.
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(raw), "123456") || strings.Contains(string(raw), "token=abc") {
		t.Fatalf("serialized message leaked body: %s", raw)
	}
}

func TestGetMessage_Errors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantClass errs.Class
	}{
		{"not found", 404, `{"error":"not found"}`, errs.ClassNotFound},
		{"gone", 410, `{}`, errs.ClassAuthExpired},
		{"unauthorized", 401, `{}`, errs.ClassNotAuthorized},
		{"rate limited", 429, `{}`, errs.ClassRateLimited},
		{"server error", 500, `{}`, errs.ClassUpstreamTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := p.GetMessage(context.Background(), mailbox(), "missing")
			mustClass(t, err, tc.wantClass)
		})
	}

	t.Run("malformed json", func(t *testing.T) {
		p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		})
		_, err := p.GetMessage(context.Background(), mailbox(), "m1")
		mustClass(t, err, errs.ClassProviderBug)
	})

	t.Run("empty id", func(t *testing.T) {
		p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler must not be reached with an empty id")
		})
		_, err := p.GetMessage(context.Background(), mailbox(), "  ")
		mustClass(t, err, errs.ClassValidation)
	})
}

func TestDeleteAddress(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/messages":
			_, _ = w.Write([]byte(listBody))
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	})

	if err := p.DeleteAddress(context.Background(), mailbox()); err != nil {
		t.Fatalf("DeleteAddress() error = %v", err)
	}
	mu.Lock()
	got := append([]string(nil), deleted...)
	mu.Unlock()
	want := []string{"/api/v1/message/m1", "/api/v1/message/m2"}
	if len(got) != len(want) {
		t.Fatalf("deleted = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("deleted = %v, want %v", got, want)
		}
	}
}

func TestDeleteAddress_IdempotentOnMissingMessage(t *testing.T) {
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/messages":
			_, _ = w.Write([]byte(`{"messages":[{"ID":"m1","To":[{"Address":"sandbox001@mailpit.local"}],"Created":"2025-03-01T11:59:00Z"}]}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if err := p.DeleteAddress(context.Background(), mailbox()); err != nil {
		t.Fatalf("DeleteAddress() error = %v, want nil for already-deleted message", err)
	}
}

func TestDeleteAddress_ListEndpointMissing(t *testing.T) {
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{}`))
	})
	err := p.DeleteAddress(context.Background(), mailbox())
	mustClass(t, err, errs.ClassProviderBug)
}

func TestDisabledProviderRejectsWithoutNetwork(t *testing.T) {
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
	}))
	defer srv.Close()
	p, err := New(Config{BaseURL: srv.URL, Disabled: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "x"})
	mustClass(t, err, errs.ClassNotAuthorized)
	_, err = p.ListMessages(context.Background(), mailbox(), time.Time{})
	mustClass(t, err, errs.ClassNotAuthorized)
	_, err = p.GetMessage(context.Background(), mailbox(), "m1")
	mustClass(t, err, errs.ClassNotAuthorized)
	err = p.DeleteAddress(context.Background(), mailbox())
	mustClass(t, err, errs.ClassNotAuthorized)
	if called != 0 {
		t.Fatalf("disabled provider made %d network calls, want 0", called)
	}
}

func TestProviderImplementsInterfaces(t *testing.T) {
	var _ provider.MailboxProvider = (*Provider)(nil)
	var _ provider.MailboxCleaner = (*Provider)(nil)
	p, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, ok := any(p).(provider.MailboxCleaner); !ok {
		t.Fatal("Provider does not implement provider.MailboxCleaner")
	}
}

func TestCrossOriginRedirectRejected(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect target must not be reached")
	}))
	defer target.Close()

	var redirected int
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		redirected++
		http.Redirect(w, r, target.URL+"/api/v1/messages", http.StatusFound)
	})
	_, err := p.ListMessages(context.Background(), mailbox(), time.Time{})
	mustClass(t, err, errs.ClassProviderBug)
	if redirected != 1 {
		t.Fatalf("origin requests = %d, want 1", redirected)
	}

	// Same-origin redirects still work.
	var sameOrigin int
	p2, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sameOrigin++
		if r.URL.Path == "/api/v1/messages" && sameOrigin < 2 {
			http.Redirect(w, r, "/api/v1/messages?redirected=1", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(listBody))
	})
	if _, err := p2.ListMessages(context.Background(), mailbox(), time.Time{}); err != nil {
		t.Fatalf("same-origin redirect failed: %v", err)
	}
}

func TestErrorMessageDoesNotContainBody(t *testing.T) {
	secret := "jwt-value-should-not-appear"
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":%q}`, secret)
	})
	_, err := p.ListMessages(context.Background(), mailbox(), time.Time{})
	if err == nil {
		t.Fatal("ListMessages() = nil error")
	}
	rendered := err.Error() + " " + errs.SummaryOf(err)
	if strings.Contains(rendered, secret) {
		t.Fatalf("error leaked response body: %s", rendered)
	}
}

// ---------------------------------------------------------------------------
// Cross-layer integration: adapter output feeds the mailx matcher
// ---------------------------------------------------------------------------

// TestMailpitToMailxEndToEnd proves a Mailpit-delivered verification mail is
// accepted by the mailx policy and yields the transient link/code.
func TestMailpitToMailxEndToEnd(t *testing.T) {
	const body = `{
	  "ID": "m1", "MessageID": "<m1@mailpit>",
	  "From": {"Name": "Keycloak", "Address": "no-reply@keycloak.local"},
	  "To": [{"Name": "", "Address": "sandbox001@mailpit.local"}],
	  "Subject": "Verify your email",
	  "Created": "2025-03-01T11:59:00Z",
	  "Text": "Your code is 123456. Verify at https://id.example/verify?token=abc",
	  "HTML": "<a href=\"https://id.example/verify?token=abc\">Verify</a>"
	}`

	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/message/") {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{"messages":[{"ID":"m1","To":[{"Address":"sandbox001@mailpit.local"}],"From":{"Address":"no-reply@keycloak.local"},"Subject":"Verify your email","Created":"2025-03-01T11:59:00Z"}]}`))
	})

	mb, err := p.CreateAddress(context.Background(), provider.CreateAddressRequest{Name: "sandbox001"})
	if err != nil {
		t.Fatalf("CreateAddress() error = %v", err)
	}
	listed, err := p.ListMessages(context.Background(), mb, time.Time{})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(listed))
	}
	full, err := p.GetMessage(context.Background(), mb, listed[0].ID)
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}

	received := time.Date(2025, 3, 1, 11, 59, 0, 0, time.UTC)
	matcher, err := mailx.NewMatcher(mailx.Config{
		ExpectedRecipient: mb.Address,
		SenderAllowlist:   []string{"keycloak.local"},
		SubjectPatterns:   []string{"verify your email"},
		WindowStart:       received.Add(-time.Minute),
		WindowEnd:         received.Add(time.Minute),
		ClockSkew:         time.Minute,
	})
	if err != nil {
		t.Fatalf("mailx.NewMatcher() error = %v", err)
	}
	res, ok := matcher.Match([]provider.Message{full})
	if !ok {
		t.Fatal("mailx.Match() = no match, want the Mailpit verification mail")
	}
	if res.Code != "123456" {
		t.Fatalf("Code = %q, want 123456", res.Code)
	}
	if res.Link != "https://id.example/verify?token=abc" {
		t.Fatalf("Link = %q", res.Link)
	}
}

func TestGetMessage_EmptyResponseBodyIsProviderBug(t *testing.T) {
	p, _ := newProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	})
	_, err := p.GetMessage(context.Background(), mailbox(), "m1")
	mustClass(t, err, errs.ClassProviderBug)
}
