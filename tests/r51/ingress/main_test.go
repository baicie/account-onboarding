package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	if !strings.Contains("ok", "ok") {
		t.Fatal("unreachable")
	}
}
