package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

// These tests used to drive the server-rendered pages. The console is a single-page app now,
// so they assert the JSON API and the session contract the console depends on.

func cookieJSON(t *testing.T, e *env, csrf, method, path string, payload any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Origin", e.ts.URL)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestAlertsOutboxAndResolve(t *testing.T) {
	e := newEnv(t, nil)
	csrf := loginUISession(t, e)
	if _, err := e.store.EnqueueAlertOutbox(t.Context(), []storage.AlertOutboxInput{{
		Fingerprint: "fp-r22", Rule: "backup_stale", Severity: "warning", Message: "test alert message",
	}}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	status, body := e.request(t, http.MethodGet, "/v1/ops/alerts/outbox", nil, adminToken, nil)
	if status != http.StatusOK || !strings.Contains(string(body), "backup_stale") {
		t.Fatalf("outbox = %d %s", status, body)
	}
	status, body = cookieJSON(t, e, csrf, http.MethodPost, "/v1/ops/alerts/outbox/resolve", map[string]string{"rule": "backup_stale"})
	if status != http.StatusOK {
		t.Fatalf("resolve = %d %s", status, body)
	}
	status, body = e.request(t, http.MethodGet, "/v1/ops/alerts/outbox", nil, adminToken, nil)
	if status != http.StatusOK || !strings.Contains(string(body), "RESOLVED") {
		t.Fatalf("resolved alert not visible: %d %s", status, body)
	}
}

func TestRunsFilterUsesAPI(t *testing.T) {
	e := newEnv(t, nil)
	csrf := loginUISession(t, e)
	if csrf == "" {
		t.Fatal("missing csrf")
	}
	status, body := e.request(t, http.MethodGet, "/v1/runs?status=FAILED_TERMINAL&template=sandbox-email-verify-oidc", nil, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("filtered runs = %d %s", status, body)
	}
	var page struct {
		Items []struct {
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.Status != "FAILED_TERMINAL" {
			t.Fatalf("filter leaked %q", item.Status)
		}
	}
}

func TestAlertDispatchFailsClosedWithoutTransport(t *testing.T) {
	e := newEnv(t, nil)
	csrf := loginUISession(t, e)
	// Fail-closed: with no transport configured the dispatcher refuses instead of silently
	// dropping alerts, and nothing is reported as sent.
	status, body := cookieJSON(t, e, csrf, http.MethodPost, "/v1/ops/alerts/outbox/dispatch", map[string]any{})
	if status != http.StatusInternalServerError || !strings.Contains(string(body), "transport is not configured") {
		t.Fatalf("dispatch = %d %s", status, body)
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
	post := func(path string) {
		status, body := cookieJSON(t, e, csrf, http.MethodPost, path, map[string]string{"rule": "delivered_rule"})
		if status != http.StatusOK {
			t.Fatalf("POST %s = %d %s", path, status, body)
		}
	}
	post("/v1/ops/alerts/outbox/resolve")
	it, err := e.store.GetAlertOutbox(t.Context(), items[0].ID)
	if err != nil || it.Status != storage.AlertOutboxDelivered {
		t.Fatalf("resolve rewrote delivered: %+v err=%v", it, err)
	}
	post("/v1/ops/alerts/outbox/rearm")
	it, err = e.store.GetAlertOutbox(t.Context(), items[0].ID)
	if err != nil || it.Status != storage.AlertOutboxResolved {
		t.Fatalf("rearm = %+v err=%v", it, err)
	}
	deliveries, err := e.store.ListAlertDeliveries(t.Context(), items[0].ID)
	if err != nil || len(deliveries) != 1 || deliveries[0].Status != storage.AlertOutboxDelivered {
		t.Fatalf("delivery history changed: %+v err=%v", deliveries, err)
	}
}

func TestRunLookupByAPI(t *testing.T) {
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
	status, body := e.request(t, http.MethodGet, "/v1/runs/"+run.ID, nil, adminToken, nil)
	if status != http.StatusOK || !strings.Contains(string(body), run.ID) {
		t.Fatalf("run lookup = %d %s", status, body)
	}
	status, _ = e.request(t, http.MethodGet, "/v1/runs/"+security.NewID(), nil, adminToken, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown run = %d, want 404", status)
	}
}

func TestInvalidRunFilterAndCursorAre400(t *testing.T) {
	e := newEnv(t, nil)
	loginUISession(t, e)
	for _, path := range []string{
		"/v1/runs?status=NOPE",
		"/v1/runs?cursor=" + strings.Repeat("x", 600),
		"/v1/runs?cursor=not-base64",
	} {
		status, body := e.request(t, http.MethodGet, path, nil, adminToken, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400 (%s)", path, status, body)
		}
	}
}

func TestRunsCursorPaginationNoDuplicates(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < 55; i++ {
		if _, _, err := e.store.CreateRun(t.Context(), storage.CreateRunParams{
			ID: security.NewID(), OwnerID: "operator", Template: "sandbox-email-verify-oidc",
			Mode: "sandbox-e2e", IdempotencyKey: "r22-page-" + string(rune('a'+i%26)) + time.Now().Format("150405.000000000") + string(rune('0'+i%10)) + time.Now().Format("000"),
			ProviderSnapshotJSN: `{}`, RequestFingerprint: "fp-page",
			DeadlineMS: time.Now().Add(time.Hour).UnixMilli(),
			Steps:      []storage.StepSeed{{Name: "validate"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/v1/runs?limit=10"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		status, body := e.request(t, http.MethodGet, path, nil, adminToken, nil)
		if status != http.StatusOK {
			t.Fatalf("page = %d %s", status, body)
		}
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatalf("duplicate run %s across pages", item.ID)
			}
			seen[item.ID] = true
		}
		pages++
		if page.NextCursor == "" || pages > 20 {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) < 50 {
		t.Fatalf("pagination returned %d runs, want at least 50", len(seen))
	}
}
