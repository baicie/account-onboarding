package api_test

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/storage"
)

func csrfFrom(t *testing.T, body []byte) string {
	t.Helper()
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(string(body))
	if len(m) != 2 {
		t.Fatalf("no csrf token in page: %s", body)
	}
	return m[1]
}

func TestAlertsPageUsesDurableOutboxAndResolve(t *testing.T) {
	e := newEnv(t, nil)
	csrf := loginUISession(t, e)
	if _, err := e.store.EnqueueAlertOutbox(t.Context(), []storage.AlertOutboxInput{{
		Fingerprint: "fp-r22", Rule: "backup_stale", Severity: "warning", Message: "test alert message",
	}}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.Get(e.ts.URL + "/ui/alerts/outbox")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "backup_stale") {
		t.Fatalf("alerts page = %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "External alert transport is not configured") {
		t.Fatal("alerts page must explain fail-closed transport")
	}
	if !strings.Contains(string(body), "Reconcile") || !strings.Contains(string(body), "Dispatch") {
		t.Fatal("alerts page must separate reconcile and dispatch")
	}
	form := url.Values{"csrf": {csrf}, "rule": {"backup_stale"}}
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/alerts/outbox/resolve", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, err = e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("resolve = %d", resp.StatusCode)
	}
	resp, err = e.client.Get(e.ts.URL + "/ui/alerts/outbox?status=RESOLVED")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "backup_stale") {
		t.Fatalf("resolved alert not visible: %s", body)
	}
}

func TestRunsFilterAndRunIDRedirect(t *testing.T) {
	e := newEnv(t, nil)
	csrf := loginUISession(t, e)
	resp, err := e.client.Get(e.ts.URL + "/ui/?status=FAILED&template=no-such-template")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "No runs yet") {
		t.Fatalf("filtered empty page = %d %s", resp.StatusCode, body)
	}
	if csrf == "" {
		t.Fatal("missing csrf")
	}
}

func TestAlertDispatchFailsClosedWithoutTransport(t *testing.T) {
	e := newEnv(t, nil)
	csrf := loginUISession(t, e)
	form := url.Values{"csrf": {csrf}}
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/alerts/outbox/dispatch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("dispatch = %d", resp.StatusCode)
	}
	resp, err = e.client.Get(e.ts.URL + "/ui/alerts/outbox?error=no+transport")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "External alert transport is not configured") {
		t.Fatal("fail-closed transport notice missing")
	}
}
