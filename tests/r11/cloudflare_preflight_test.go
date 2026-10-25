package r11test

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runPreflight(t *testing.T, url, caPath, tokenPath, report string, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"scripts/cloudflare_preflight.sh", "--mock-live",
		"--mock-url", url, "--mock-ca", caPath, "--test-address", "r11-test@example.test",
		"--token-file", tokenPath, "--max-requests", "3", "--max-seconds", "30", "--report", report}, extra...)
	cmd := exec.Command("bash", args...)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "ONBOARD_CF_TEST_ADDRESSES=r11-test@example.test")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeTLSFixture(t *testing.T, srv *httptest.Server) (caPath, tokenPath string) {
	t.Helper()
	dir := t.TempDir()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("no server certificate")
	}
	caPath = filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = x509.NewCertPool()
	tokenPath = filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("test-address-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	return caPath, tokenPath
}

func TestCloudflareMockLiveReadOnly(t *testing.T) {
	var gotAuth bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "write not allowed", http.StatusMethodNotAllowed)
			return
		}
		gotAuth = strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.URL.Path != "/api/parsed_mails" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("offset") != "0" {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"messages":[]}`))
	}))
	defer srv.Close()
	ca, token := writeTLSFixture(t, srv)
	report := filepath.Join(t.TempDir(), "report.json")
	out, err := runPreflight(t, srv.URL, ca, token, report)
	if err != nil {
		t.Fatalf("mock-live failed: %v\n%s", err, out)
	}
	if !gotAuth {
		t.Fatal("mock did not receive bearer auth")
	}
	raw, err := os.ReadFile(report)
	if err != nil || !strings.Contains(string(raw), `"stop_reason": "COMPLETED"`) ||
		!strings.Contains(string(raw), `"verification_status": "MOCK_PASS"`) ||
		!strings.Contains(string(raw), `"response_schema_valid": "PASS"`) ||
		!strings.Contains(string(raw), `"tls_result": "PASS"`) {
		t.Fatalf("report does not contain truthful contract/TLS evidence: %s err=%v", raw, err)
	}
	if strings.Contains(out, "test-address-jwt") || strings.Contains(string(raw), "test-address-jwt") {
		t.Fatal("bearer token leaked into output or report")
	}
}

func TestCloudflareMockLiveStopsOn429AndRedirect(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		want string
	}{
		{"rate-limit", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }, "RATE_LIMITED"},
		{"html-200", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html>ok</html>"))
		}, "RESPONSE_CONTENT_TYPE_INVALID"},
		{"invalid-json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{invalid"))
		}, "RESPONSE_INVALID_JSON"},
		{"wrong-schema", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}, "RESPONSE_SCHEMA_INVALID"},
		{"wrong-item-type", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"messages":[42]}`))
		}, "RESPONSE_ITEM_TYPE_INVALID"},
		{"empty-204", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNoContent)
		}, "RESPONSE_EMPTY_BODY"},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://evil.example/", http.StatusFound)
		}, "REDIRECT_REFUSED"},
		{"forbidden", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }, "AUTH_DENIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(tc.h)
			defer srv.Close()
			ca, token := writeTLSFixture(t, srv)
			report := filepath.Join(t.TempDir(), "report.json")
			out, err := runPreflight(t, srv.URL, ca, token, report)
			if err == nil {
				t.Fatalf("expected failure, got success: %s", out)
			}
			raw, _ := os.ReadFile(report)
			if !strings.Contains(string(raw), `"stop_reason": "`+tc.want+`"`) {
				t.Fatalf("report = %s, want stop %s; out=%s", raw, tc.want, out)
			}
		})
	}
}
