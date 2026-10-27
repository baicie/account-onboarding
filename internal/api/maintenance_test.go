package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func decodeMaintenance(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, string(body))
	}
	return out
}

func TestMaintenanceWindowRejectsWritesAndDrains(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()

	status, body := e.request(t, http.MethodPost, "/v1/maintenance/enter",
		map[string]string{"reason": "planned restore drill"}, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("enter status=%d body=%s", status, body)
	}
	entered := decodeMaintenance(t, body)
	if entered["active"] != true || entered["drained"] != true || entered["workers_paused"] != true {
		t.Fatalf("enter response does not describe a drained window: %s", body)
	}
	if !e.engine.Paused() {
		t.Fatal("workflow engine was not paused")
	}

	// Reads keep working: only mutations are rejected.
	if status, body = e.request(t, http.MethodGet, "/v1/runs", nil, adminToken, nil); status != http.StatusOK {
		t.Fatalf("read during maintenance status=%d body=%s", status, body)
	}

	status, body = e.request(t, http.MethodPost, "/v1/runs", sandboxRequest(), adminToken,
		map[string]string{"Idempotency-Key": "maintenance-write-0001"})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("write during maintenance status=%d body=%s", status, body)
	}
	if cls := decodeMaintenance(t, body)["error"].(map[string]any)["class"]; cls != "MAINTENANCE" {
		t.Fatalf("maintenance rejection class = %v", cls)
	}

	// Readiness reports the window so orchestrators stop routing traffic.
	req, err := http.NewRequest(http.MethodGet, e.ts.URL+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz during maintenance status=%d", resp.StatusCode)
	}
	var ready map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&ready); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if ready["maintenance"] != true || ready["ready"] != false {
		t.Fatalf("readyz body = %v", ready)
	}

	// Paused means no new claims, even when work exists.
	if did, err := e.engine.Tick(ctx); err != nil || did {
		t.Fatalf("paused engine claimed work: did=%v err=%v", did, err)
	}

	status, body = e.request(t, http.MethodPost, "/v1/maintenance/exit", nil, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("exit status=%d body=%s", status, body)
	}
	exited := decodeMaintenance(t, body)
	if exited["active"] != false || exited["resumed"] != true || exited["ready"] != true {
		t.Fatalf("exit response = %s", body)
	}
	if e.engine.Paused() {
		t.Fatal("workflow engine still paused after exit")
	}
	if status, body = e.request(t, http.MethodPost, "/v1/runs", sandboxRequest(), adminToken,
		map[string]string{"Idempotency-Key": "maintenance-write-0002"}); status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("write after exit status=%d body=%s", status, body)
	}
	if status, body = e.request(t, http.MethodGet, "/readyz", nil, adminToken, nil); status != http.StatusOK {
		t.Fatalf("readyz after exit status=%d body=%s", status, body)
	}
}

func TestMaintenanceWindowIsIdempotentAndAudited(t *testing.T) {
	e := newEnv(t, nil)
	if status, body := e.request(t, http.MethodPost, "/v1/maintenance/enter", map[string]string{"reason": "first"}, adminToken, nil); status != http.StatusOK {
		t.Fatalf("enter status=%d body=%s", status, body)
	}
	status, body := e.request(t, http.MethodPost, "/v1/maintenance/enter", map[string]string{"reason": "second"}, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("second enter status=%d", status)
	}
	if decodeMaintenance(t, body)["active"] != true {
		t.Fatal("second enter closed the window")
	}
	if status, body = e.request(t, http.MethodGet, "/v1/maintenance", nil, adminToken, nil); status != http.StatusOK {
		t.Fatalf("state status=%d", status)
	}
	state := decodeMaintenance(t, body)
	if state["active"] != true || state["reason"] != "first" {
		t.Fatalf("re-entering must not rewrite the open window: %s", body)
	}

	auditStatus, auditBody := e.request(t, http.MethodGet, "/v1/audit", nil, adminToken, nil)
	if auditStatus != http.StatusOK {
		t.Fatalf("audit status=%d", auditStatus)
	}
	if !strings.Contains(string(auditBody), "maintenance.enter") {
		t.Fatalf("maintenance.enter was not audited: %s", auditBody)
	}
	if _, body = e.request(t, http.MethodPost, "/v1/maintenance/exit", nil, adminToken, nil); !strings.Contains(string(auditBody), "maintenance.enter") {
		t.Fatal("audit query changed during the test")
	}
	_, auditBody = e.request(t, http.MethodGet, "/v1/audit", nil, adminToken, nil)
	if !strings.Contains(string(auditBody), "maintenance.exit") {
		t.Fatalf("maintenance.exit was not audited: %s", auditBody)
	}
}

func TestMaintenanceRequiresAuthentication(t *testing.T) {
	e := newEnv(t, nil)
	if status, _ := e.request(t, http.MethodPost, "/v1/maintenance/enter", nil, "", nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated enter status=%d, want 401", status)
	}
	if status, _ := e.request(t, http.MethodGet, "/v1/maintenance", nil, "", nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated state status=%d, want 401", status)
	}
}
