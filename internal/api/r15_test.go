package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/alerts"
	"github.com/baicie/account-onboarding/internal/config"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/storage"
)

func TestAlertOutboxAdminReadAndExternalTransportDisabled(t *testing.T) {
	e := newEnv(t, nil)
	if status, _ := e.request(t, http.MethodPost, "/v1/ops/alerts/reconcile", nil, adminToken, nil); status != http.StatusOK {
		t.Fatalf("reconcile status=%d", status)
	}
	status, raw := e.request(t, http.MethodGet, "/v1/ops/alerts/outbox", nil, adminToken, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"durable":true`) {
		t.Fatalf("outbox status=%d %s", status, raw)
	}
	if status, _ := e.request(t, http.MethodPost, "/v1/ops/alerts/outbox/dispatch", nil, adminToken, nil); status != http.StatusInternalServerError {
		t.Fatalf("unconfigured dispatch status=%d, want 500 fail-closed", status)
	}
	viewer := "viewer-token-0123456789"
	e2 := newEnv(t, func(cfg *config.Config, _ *provider.Registry) { cfg.ReadOnlyToken = viewer })
	if status, _ := e2.request(t, http.MethodGet, "/v1/ops/alerts/outbox", nil, viewer, nil); status != http.StatusForbidden {
		t.Fatalf("viewer outbox status=%d", status)
	}
}

func TestAdminResolveClearsDurableOutbox(t *testing.T) {
	e := newEnv(t, nil)
	rule := "readyz_failing"
	e.server.AlertQueue.Enqueue([]alerts.Alert{{Rule: rule, Severity: "critical", Message: "readyz"}})
	if _, err := e.store.EnqueueAlertOutbox(t.Context(), []storage.AlertOutboxInput{{
		Fingerprint: alerts.Fingerprint(alerts.Alert{Rule: rule, Severity: "critical", Message: "readyz"}),
		Rule:        rule, Severity: "critical", Message: "readyz",
	}}, time.Now().UTC().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	status, raw := e.request(t, http.MethodPost, "/v1/ops/alerts/resolve", map[string]any{"rule": rule}, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("resolve status=%d %s", status, raw)
	}
	counts, err := e.store.AlertOutboxStatusCounts(t.Context())
	if err != nil || counts.Resolved != 1 || counts.Pending != 0 {
		t.Fatalf("durable outbox not resolved: %+v err=%v", counts, err)
	}
}
