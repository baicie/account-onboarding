package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/config"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/storage"
)

func TestConcurrentAdminResolutionsUseVersionCAS(t *testing.T) {
	e := newEnv(t, nil)
	op, _, _ := seedPendingExternalOperation(t, e)
	body := map[string]any{"resolution": "RESOURCE_CONFIRMED", "reason": "admin one",
		"evidence_reference": "ticket", "expected_version": op.Version}
	const callers = 2
	results := make(chan int, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _ := e.request(t, http.MethodPost, "/v1/ops/external/"+op.ID+"/resolve", body, adminToken, nil)
			results <- status
		}()
	}
	wg.Wait()
	close(results)
	ok, conflict := 0, 0
	for status := range results {
		switch status {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("CAS results ok=%d conflict=%d", ok, conflict)
	}
}

func TestConcurrentDifferentResolutionsCannotBothWin(t *testing.T) {
	e := newEnv(t, nil)
	op, _, _ := seedPendingExternalOperation(t, e)
	bodies := []map[string]any{
		{"resolution": "RESOURCE_CONFIRMED", "reason": "confirmed", "expected_version": op.Version},
		{"resolution": "MANUAL_COMPENSATION_COMPLETED", "reason": "compensated", "expected_version": op.Version},
	}
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for _, b := range bodies {
		wg.Add(1)
		go func(body map[string]any) {
			defer wg.Done()
			status, _ := e.request(t, http.MethodPost, "/v1/ops/external/"+op.ID+"/resolve", body, adminToken, nil)
			results <- status
		}(b)
	}
	wg.Wait()
	close(results)
	ok := 0
	for status := range results {
		if status == http.StatusOK {
			ok++
		} else if status != http.StatusConflict {
			t.Fatalf("unexpected status %d", status)
		}
	}
	if ok != 1 {
		t.Fatalf("both different resolutions won: %d", ok)
	}
}

func TestAuditReasonRedactsSensitiveMaterial(t *testing.T) {
	e := newEnv(t, nil)
	op, _, _ := seedPendingExternalOperation(t, e)
	body := map[string]any{"resolution": "UNRESOLVED", "reason": "token=super-secret-value-123456",
		"evidence_reference": "ticket", "expected_version": op.Version}
	if status, _ := e.request(t, http.MethodPost, "/v1/ops/external/"+op.ID+"/resolve", body, adminToken, nil); status != http.StatusOK {
		t.Fatalf("resolve status=%d", status)
	}
	status, raw := e.request(t, http.MethodGet, "/v1/ops/external/"+op.ID+"/audit", nil, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("audit status=%d", status)
	}
	if strings.Contains(string(raw), "super-secret-value-123456") {
		t.Fatal("audit API leaked sensitive reason material")
	}
}

func TestAuditLimitAndMissingOperation(t *testing.T) {
	e := newEnv(t, nil)
	op, _, _ := seedPendingExternalOperation(t, e)
	for i := 0; i < 3; i++ {
		if _, err := e.store.ResolveExternalOperation(t.Context(), op.ID, storageResolution(op.Version+int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	status, raw := e.request(t, http.MethodGet, "/v1/ops/external/"+op.ID+"/audit?limit=2", nil, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("audit limit status=%d", status)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Items) > 2 {
		t.Fatalf("audit limit items=%d err=%v", len(page.Items), err)
	}
	if status, _ := e.request(t, http.MethodGet, "/v1/ops/external/missing/audit", nil, adminToken, nil); status != http.StatusNotFound {
		t.Fatalf("missing audit status=%d", status)
	}
}

func storageResolution(version int64) storage.ExternalResolution {
	return storage.ExternalResolution{Resolution: "UNRESOLVED", Reason: "review", ExpectedVersion: version,
		Actor: "test:admin", RequestedAtMS: time.Now().UnixMilli()}
}

func TestAlertQueueStatusAdminOnly(t *testing.T) {
	e := newEnv(t, nil)
	status, raw := e.request(t, http.MethodGet, "/v1/ops/alerts/queue", nil, adminToken, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"pending"`) {
		t.Fatalf("admin queue status=%d %s", status, raw)
	}
	viewer := "viewer-token-0123456789"
	e2 := newEnv(t, func(cfg *config.Config, _ *provider.Registry) { cfg.ReadOnlyToken = viewer })
	if status, _ := e2.request(t, http.MethodGet, "/v1/ops/alerts/queue", nil, viewer, nil); status != http.StatusForbidden {
		t.Fatalf("viewer queue status=%d", status)
	}
}

func TestAlertResolveAPIRequiresActiveRule(t *testing.T) {
	e := newEnv(t, nil)
	status, _ := e.request(t, http.MethodPost, "/v1/ops/alerts/resolve", map[string]any{"rule": "does-not-exist"}, adminToken, nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing alert rule status=%d", status)
	}
}

func TestAlertReconcileEndpointIsExplicitAdminControl(t *testing.T) {
	e := newEnv(t, nil)
	if status, _ := e.request(t, http.MethodPost, "/v1/ops/alerts/reconcile", nil, adminToken, nil); status != http.StatusOK {
		t.Fatalf("reconcile status=%d", status)
	}
	if status, _ := e.request(t, http.MethodGet, "/v1/ops/alerts", nil, adminToken, nil); status != http.StatusOK {
		t.Fatalf("alerts status=%d", status)
	}
	if status, _ := e.request(t, http.MethodGet, "/v1/ops/alerts/queue", nil, adminToken, nil); status != http.StatusOK {
		t.Fatalf("queue status=%d", status)
	}
}

func TestTwoAdminsUIResolutionRace(t *testing.T) {
	e := newEnv(t, nil)
	op := seedExternalAt(t, e, "OUTCOME_UNKNOWN")
	jarA, _ := cookiejar.New(nil)
	jarB, _ := cookiejar.New(nil)
	clientA := &http.Client{Jar: jarA, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	clientB := &http.Client{Jar: jarB, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	csrfA := loginUISessionClient(t, e, clientA)
	csrfB := loginUISessionClient(t, e, clientB)
	payload := func(resolution string) []byte {
		raw, err := json.Marshal(map[string]any{"expected_version": op.Version, "resolution": resolution, "reason": "ui race"})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for i, res := range []string{"RESOURCE_CONFIRMED", "MANUAL_COMPENSATION_COMPLETED"} {
		wg.Add(1)
		go func(i int, resolution string) {
			defer wg.Done()
			csrf, client := csrfA, clientA
			if i == 1 {
				csrf, client = csrfB, clientB
			}
			req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/ops/external/"+op.ID+"/resolve",
				bytes.NewReader(payload(resolution)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-CSRF-Token", csrf)
			req.Header.Set("Origin", e.ts.URL)
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				results <- 0
				return
			}
			resp.Body.Close()
			results <- resp.StatusCode
		}(i, res)
	}
	wg.Wait()
	close(results)
	winners, conflicts := 0, 0
	for status := range results {
		switch status {
		case http.StatusOK:
			winners++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("UI race status=%d", status)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("race winners=%d conflicts=%d, want exactly one of each", winners, conflicts)
	}
	after, err := e.store.GetExternalOperationByID(t.Context(), op.ID)
	if err != nil || after.Status != storage.ExternalResolved {
		t.Fatalf("operation not resolved exactly once: %+v err=%v", after, err)
	}
	audits, err := e.store.ListExternalOperationAudit(t.Context(), op.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, a := range audits {
		if a.Action == "external_operation.resolve" {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("resolution audit count=%d, want 1", resolved)
	}
}
