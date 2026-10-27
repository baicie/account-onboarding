package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestProxyStripsClientForwardingHeadersAndInjectsTrustedOnes(t *testing.T) {
	type captured struct {
		host string
		hdr  http.Header
	}
	got := make(chan captured, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- captured{host: r.Host, hdr: r.Header.Clone()}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	proxy := newProxyHandler(u)

	req := httptest.NewRequest(http.MethodGet, "http://localhost:18443/healthz", nil)
	req.Host = "localhost:18443"
	req.RemoteAddr = "127.0.0.1:51515"
	req.Header.Set("Forwarded", "for=203.0.113.9;proto=http;host=evil.example")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Real-IP", "203.0.113.9")
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	c := <-got
	if c.host != "localhost:18443" {
		t.Fatalf("upstream Host = %q, want original browser-visible host", c.host)
	}
	if got := c.hdr.Get("X-Forwarded-Proto"); got != "https" {
		t.Fatalf("X-Forwarded-Proto = %q", got)
	}
	if got := c.hdr.Get("X-Forwarded-For"); got != "127.0.0.1" {
		t.Fatalf("X-Forwarded-For = %q (client-supplied IP must be stripped)", got)
	}
	if got := c.hdr.Get("X-Real-Ip"); got != "127.0.0.1" {
		t.Fatalf("X-Real-Ip = %q", got)
	}
	if got := c.hdr.Get("Forwarded"); got != "" {
		t.Fatalf("Forwarded header leaked: %q", got)
	}
	if got := c.hdr.Get("X-Forwarded-Host"); got != "localhost:18443" {
		t.Fatalf("X-Forwarded-Host = %q", got)
	}
}

func TestRedirectHandlerRedirectsToHTTPSOnTLSListenPort(t *testing.T) {
	h := newRedirectHandler("18443")
	req := httptest.NewRequest(http.MethodGet, "http://localhost:18080/ui/?x=1", nil)
	req.Host = "localhost:18080"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "https://localhost:18443/ui/?x=1" {
		t.Fatalf("location = %q", loc)
	}
	// A forged Host header must not turn the redirect into an open redirect.
	evil := httptest.NewRequest(http.MethodGet, "http://evil.example:18080/healthz", nil)
	evil.Host = "evil.example"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, evil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("forged Host status = %d, want 400", rr.Code)
	}
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]", "localhost:18080"} {
		if !redirectHostAllowed(host) {
			t.Fatalf("redirectHostAllowed(%q) = false", host)
		}
	}
	for _, host := range []string{"evil.example", "0.0.0.0", ""} {
		if redirectHostAllowed(host) {
			t.Fatalf("redirectHostAllowed(%q) = true", host)
		}
	}
}

func TestRequireLoopbackRejectsWildcardAndNonLoopback(t *testing.T) {
	bad := []string{"0.0.0.0:18443", ":18443", "192.0.2.10:18443", "example.com:18443"}
	for _, addr := range bad {
		if err := requireLoopback(addr); err == nil {
			t.Fatalf("requireLoopback(%q) accepted a non-loopback address", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:18443", "localhost:18443", "[::1]:18443"} {
		if err := requireLoopback(addr); err != nil {
			t.Fatalf("requireLoopback(%q) = %v", addr, err)
		}
	}
}

func TestCheckAddressStagingOptIn(t *testing.T) {
	if err := checkAddress("0.0.0.0:8443", false); err == nil {
		t.Fatal("0.0.0.0 must be rejected without the staging opt-in")
	}
	if err := checkAddress("app:8788", false); err == nil {
		t.Fatal("service names must be rejected without the staging opt-in")
	}
	if err := checkAddress("0.0.0.0:8443", true); err != nil {
		t.Fatalf("staging opt-in should accept 0.0.0.0:8443: %v", err)
	}
	if err := checkAddress("app:8788", true); err != nil {
		t.Fatalf("staging opt-in should accept app:8788: %v", err)
	}
	if err := checkAddress("not-an-address", true); err == nil {
		t.Fatal("staging opt-in must still require host:port")
	}
}

func TestRequireAllowedHostRejectsUnknownHost(t *testing.T) {
	h := requireAllowedHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), []string{"localhost", "127.0.0.1"})
	for _, host := range []string{"localhost:8443", "127.0.0.1:8443"} {
		req := httptest.NewRequest(http.MethodGet, "https://"+host+"/healthz", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("host %s = %d, want 200", host, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "https://evil.example/healthz", nil)
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("evil host = %d, want 421", rec.Code)
	}
}
