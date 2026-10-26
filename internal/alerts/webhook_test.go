package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func webhookFixture(t *testing.T, h http.HandlerFunc, secret string) (*WebhookNotifier, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	n, err := NewWebhookNotifier(WebhookConfig{
		URL: "https://" + strings.TrimPrefix(srv.URL, "https://"), AllowedHosts: []string{strings.TrimPrefix(srv.URL, "https://")},
		Secret: []byte(secret), Timeout: 2 * time.Second, MaxResponseByte: 1024, RootCAs: pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	return n, srv
}

func TestWebhookNotifierSignsAndBounds(t *testing.T) {
	var got webhookPayload
	var sig string
	var signedBody []byte
	n, _ := webhookFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		signedBody = body
		sig = r.Header.Get("X-Onboard-Signature")
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("payload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}, "top-secret")
	if err := n.Notify(context.Background(), []Alert{{Rule: "r", Severity: "critical", Message: "m"}}); err != nil {
		t.Fatal(err)
	}
	if len(got.Alerts) != 1 || got.IdempotencyKey == "" || !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("payload/signature missing: %+v %s", got, sig)
	}
	mac := hmac.New(sha256.New, []byte("top-secret"))
	_, _ = mac.Write(signedBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		t.Fatalf("signature mismatch: got %q want %q", sig, want)
	}
}

func TestWebhookNotifierRejectsDisallowedHostAndRedirect(t *testing.T) {
	if _, err := NewWebhookNotifier(WebhookConfig{URL: "https://evil.example/hook", AllowedHosts: []string{"127.0.0.1:1"}}); err == nil {
		t.Fatal("disallowed host accepted")
	}
	n, _ := webhookFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/", http.StatusFound)
	}, "")
	if err := n.Notify(context.Background(), []Alert{{Rule: "r", Severity: "warning", Message: "m"}}); err == nil {
		t.Fatal("redirect followed/accepted")
	}
}

func TestWebhookNotifierClassifiesAndOpensCircuit(t *testing.T) {
	fail := true
	hits := 0
	n, _ := webhookFixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}, "")
	n.cfg.Now = func() time.Time { return time.Unix(1000, 0) }
	for i := 0; i < 3; i++ {
		if err := n.Notify(context.Background(), []Alert{{Rule: "r", Severity: "warning", Message: "m"}}); err == nil {
			t.Fatal("500 accepted")
		}
	}
	if err := n.Notify(context.Background(), []Alert{{Rule: "r", Severity: "warning", Message: "m"}}); err != ErrWebhookCircuitOpen {
		t.Fatalf("circuit not open: %v", err)
	}
	fail = false
	n.cfg.Now = func() time.Time { return time.Unix(2000, 0) }
	if err := n.Notify(context.Background(), []Alert{{Rule: "r", Severity: "warning", Message: "m"}}); err != nil {
		t.Fatalf("circuit recovery: %v", err)
	}
	if hits != 4 {
		t.Fatalf("hits=%d", hits)
	}
}

func TestWebhookNotifierPermanent4xx(t *testing.T) {
	n, _ := webhookFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) }, "")
	if err := n.Notify(context.Background(), []Alert{{Rule: "r", Severity: "warning", Message: "m"}}); err == nil || !strings.Contains(err.Error(), "permanent") {
		t.Fatalf("4xx classification: %v", err)
	}
}
