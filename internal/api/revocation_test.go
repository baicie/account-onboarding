package api_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/api"
	"github.com/baicie/account-onboarding/internal/security"
)

// login issues a real UI session and returns the cookie value plus CSRF token.
func login(t *testing.T, e *env) (cookieValue, csrf string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/login",
		strings.NewReader(url.Values{"token": {adminToken}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "onboard_session" {
			cookieValue = c.Value
		}
	}
	if cookieValue == "" {
		t.Fatal("no session cookie issued")
	}
	dash, err := e.client.Get(e.ts.URL + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := dash.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	dash.Body.Close()
	m := csrfPattern.FindSubmatch(body)
	if len(m) != 2 {
		t.Fatal("csrf token missing from dashboard")
	}
	return cookieValue, string(m[1])
}

func postLogout(t *testing.T, client *http.Client, base, cookieValue, csrf string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/ui/logout",
		strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", base)
	if cookieValue != "" {
		req.Header.Set("Cookie", "onboard_session="+cookieValue)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func getDashboardWithCookie(t *testing.T, client *http.Client, base, cookieValue string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/ui/", nil)
	req.Header.Set("Cookie", "onboard_session="+cookieValue)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

// TestLogoutRevocationPersistsAcrossRestart is the R5-A core gate: after a
// successful logout the revocation is in SQLite, so a fresh Server instance
// (simulated restart) must reject the old cookie.
func TestLogoutRevocationPersistsAcrossRestart(t *testing.T) {
	e := newEnv(t, nil)
	cookieValue, csrf := login(t, e)
	if status := postLogout(t, e.client, e.ts.URL, cookieValue, csrf); status != http.StatusSeeOther {
		t.Fatalf("logout = %d", status)
	}
	n, err := e.store.CountSessionRevocations(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("revocation rows = %d, err=%v", n, err)
	}
	// Simulate a process restart: a brand-new Server over the same store and
	// the same signing key, without any in-memory revocation state.
	srv2, err := api.New(e.cfg, e.store, e.engine, e.reg, e.sec, e.log, e.signer, security.NewRedactor(adminToken))
	if err != nil {
		t.Fatal(err)
	}
	ts2 := newTestServer(t, srv2)
	status, location := getDashboardWithCookie(t, e.client, ts2, cookieValue)
	if status != http.StatusSeeOther || !strings.Contains(location, "/ui/login") {
		t.Fatalf("old cookie after restart = %d location=%q", status, location)
	}
	// Sanity: a fresh login on the restarted server works (revocation is scoped
	// to the session id, not the operator).
	if status := postLoginRaw(t, e.client, ts2); status != http.StatusSeeOther {
		t.Fatalf("login after restart = %d", status)
	}
}

// TestConcurrentAndRepeatedLogout asserts idempotency under concurrency: eight
// simultaneous logout requests all succeed, produce exactly one durable row,
// and the old cookie stays rejected afterwards.
func TestConcurrentAndRepeatedLogout(t *testing.T) {
	e := newEnv(t, nil)
	cookieValue, csrf := login(t, e)
	var wg sync.WaitGroup
	statuses := make([]int, 8)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = postLogout(t, e.client, e.ts.URL, cookieValue, csrf)
		}(i)
	}
	wg.Wait()
	for i, status := range statuses {
		if status != http.StatusSeeOther {
			t.Fatalf("concurrent logout %d = %d", i, status)
		}
	}
	n, err := e.store.CountSessionRevocations(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("revocation rows = %d, err=%v (duplicate or lost rows)", n, err)
	}
	// Repeated logout with the same (already revoked) cookie is also idempotent.
	if status := postLogout(t, e.client, e.ts.URL, cookieValue, csrf); status != http.StatusSeeOther {
		t.Fatalf("repeated logout = %d", status)
	}
	status, location := getDashboardWithCookie(t, e.client, e.ts.URL, cookieValue)
	if status != http.StatusSeeOther || !strings.Contains(location, "/ui/login") {
		t.Fatalf("revoked cookie still accepted: %d %q", status, location)
	}
	if n, _ := e.store.CountSessionRevocations(context.Background()); n != 1 {
		t.Fatalf("repeated logout created %d rows", n)
	}
}

// TestLogoutWithoutSessionIsRefused documents that an unauthenticated logout is
// refused by the UI auth wrapper (redirect to login) and never reaches the
// revocation handler.
func TestLogoutWithoutSessionIsRefused(t *testing.T) {
	e := newEnv(t, nil)
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/logout", strings.NewReader("csrf=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/ui/login") {
		t.Fatalf("logout without session = %d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n, _ := e.store.CountSessionRevocations(context.Background()); n != 0 {
		t.Fatalf("unauthenticated logout wrote %d revocation rows", n)
	}
}

// TestSessionRevocationExpiryAndPrune covers the expiry contract at the store
// level: an expired revocation no longer blocks, and prune removes it.
func TestSessionRevocationExpiryAndPrune(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	now := time.Now()
	sid := "session-expiring-" + time.Now().Format("150405.000000")
	if err := e.store.RevokeSession(ctx, sid, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	revoked, err := e.store.SessionRevoked(ctx, sid, now)
	if err != nil || !revoked {
		t.Fatalf("active revocation not reported: %v %v", revoked, err)
	}
	// Move the clock forward: the same row is expired and must not block.
	revoked, err = e.store.SessionRevoked(ctx, sid, now.Add(2*time.Minute))
	if err != nil || revoked {
		t.Fatalf("expired revocation still blocks: %v %v", revoked, err)
	}
	removed, err := e.store.PruneSessionRevocations(ctx, now.Add(2*time.Minute))
	if err != nil || removed != 1 {
		t.Fatalf("prune removed=%d err=%v", removed, err)
	}
	if n, _ := e.store.CountSessionRevocations(ctx); n != 0 {
		t.Fatalf("rows after prune = %d", n)
	}
}
