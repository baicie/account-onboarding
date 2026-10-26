package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
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
		AllowLoopback: true,
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

func TestWebhookEgressIPPolicy(t *testing.T) {
	cases := []struct {
		ip    string
		allow bool
		loop  bool
	}{
		{"127.0.0.1", false, false}, {"::1", false, false}, {"10.0.0.1", false, false},
		{"172.16.0.1", false, false}, {"192.168.1.1", false, false}, {"169.254.1.1", false, false},
		{"169.254.169.254", false, false}, {"::ffff:127.0.0.1", false, false},
		{"100.64.0.1", false, false}, {"1.1.1.1", true, false}, {"127.0.0.1", true, true},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		err := validateEgressIP(ip, c.loop)
		if (err == nil) != c.allow {
			t.Fatalf("ip %s loop=%v err=%v, want allow=%v", c.ip, c.loop, err, c.allow)
		}
	}
}

func TestWebhookRejectsAnyDisallowedResolvedRecord(t *testing.T) {
	n, err := NewWebhookNotifier(WebhookConfig{
		URL: "https://ssrf.test/hook", AllowedHosts: []string{"ssrf.test"}, Timeout: time.Second,
		AllowLoopback: true,
		ResolveIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.1")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), []Alert{{Rule: "r", Severity: "warning", Message: "m"}}); err == nil {
		t.Fatal("mixed public/private DNS answer accepted")
	}
}

func TestWebhookDNSFailureAndCertificateMismatch(t *testing.T) {
	dnsFail, err := NewWebhookNotifier(WebhookConfig{
		URL: "https://dnsfail.test/hook", AllowedHosts: []string{"dnsfail.test"}, Timeout: time.Second,
		ResolveIP: func(context.Context, string) ([]net.IP, error) { return nil, errors.New("dns down") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dnsFail.Notify(context.Background(), []Alert{{Rule: "r", Severity: "warning", Message: "m"}}); err == nil {
		t.Fatal("DNS failure accepted")
	}
	n, srv := webhookFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }, "")
	_ = srv
	// Remove the private CA to force a certificate verification failure.
	unsigned, err := NewWebhookNotifier(WebhookConfig{
		URL: n.cfg.URL, AllowedHosts: n.cfg.AllowedHosts, Timeout: 2 * time.Second, AllowLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := unsigned.Notify(context.Background(), []Alert{{Rule: "r", Severity: "warning", Message: "m"}}); err == nil {
		t.Fatal("self-signed certificate accepted without root CA")
	}
}

func TestWebhookIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	n, _ := webhookFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }, "")
	if err := n.Notify(context.Background(), []Alert{{Rule: "r", Severity: "warning", Message: "m"}}); err != nil {
		t.Fatalf("proxy env affected direct pinned dial: %v", err)
	}
}

func TestWebhookDeliveryIdentityIsStableLogicalID(t *testing.T) {
	var got webhookPayload
	var idem, delivery, logical string
	n, _ := webhookFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		idem = r.Header.Get("X-Alert-Idempotency-Key")
		delivery = r.Header.Get("X-Alert-Delivery-ID")
		logical = r.Header.Get("X-Alert-Logical-ID")
		w.WriteHeader(204)
	}, "")
	err := n.NotifyDelivery(context.Background(), DeliveryEnvelope{
		DeliveryID: "delivery-1", AlertID: "alert-stable", Alerts: []Alert{{Rule: "r", Severity: "warning", Message: "m"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.DeliveryID != "delivery-1" || got.AlertID != "alert-stable" || idem != "alert-stable" || delivery != "delivery-1" || logical != "alert-stable" {
		t.Fatalf("identity mismatch payload=%+v headers idem=%s delivery=%s logical=%s", got, idem, delivery, logical)
	}
}
