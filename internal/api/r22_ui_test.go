package api_test

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
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

func TestResolveDoesNotRewriteDeliveredAndRearmIsExplicit(t *testing.T) {
	e := newEnv(t, nil)
	csrf := loginUISession(t, e)
	now := time.Now().UnixMilli()
	if _, err := e.store.EnqueueAlertOutbox(t.Context(), []storage.AlertOutboxInput{{
		Fingerprint: "fp-delivered-r22", Rule: "delivered_rule", Severity: "warning", Message: "delivered",
	}}, now); err != nil {
		t.Fatal(err)
	}
	items, _, err := e.store.ClaimAlertOutbox(t.Context(), "test-owner", 1, time.Minute, now)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %v %d", err, len(items))
	}
	if err := e.store.CompleteAlertOutbox(t.Context(), items[0].DeliveryID, "test-owner", true, now+1, 0, 200, "", 3); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"csrf": {csrf}, "rule": {"delivered_rule"}}
	post := func(path string) {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.ts.URL)
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s = %d", path, resp.StatusCode)
		}
	}
	post("/ui/alerts/outbox/resolve")
	it, err := e.store.GetAlertOutbox(t.Context(), items[0].ID)
	if err != nil || it.Status != storage.AlertOutboxDelivered {
		t.Fatalf("resolve rewrote delivered: %+v err=%v", it, err)
	}
	post("/ui/alerts/outbox/rearm")
	it, err = e.store.GetAlertOutbox(t.Context(), items[0].ID)
	if err != nil || it.Status != storage.AlertOutboxResolved {
		t.Fatalf("rearm = %+v err=%v", it, err)
	}
	deliveries, err := e.store.ListAlertDeliveries(t.Context(), items[0].ID)
	if err != nil || len(deliveries) != 1 || deliveries[0].Status != storage.AlertOutboxDelivered {
		t.Fatalf("delivery history changed: %+v err=%v", deliveries, err)
	}
}

func TestRunIDLookupRedirectsToRunDetail(t *testing.T) {
	e := newEnv(t, nil)
	run, _, err := e.store.CreateRun(t.Context(), storage.CreateRunParams{
		ID: security.NewID(), OwnerID: "operator", Template: "sandbox-email-verify-oidc",
		Mode: "sandbox-e2e", IdempotencyKey: "r22-run-lookup", ProviderSnapshotJSN: `{}`,
		RequestFingerprint: "fp-r22", DeadlineMS: time.Now().Add(time.Hour).UnixMilli(),
		Steps: []storage.StepSeed{{Name: "validate"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loginUISession(t, e)
	client := &http.Client{Jar: e.client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(e.ts.URL + "/ui/?run=" + run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui/runs/"+run.ID {
		t.Fatalf("lookup = %d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
}
