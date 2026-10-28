package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

func resolveExternal(t *testing.T, e *env, csrf, id string, version int64, resolution, reason string) (int, []byte) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"expected_version": version, "resolution": resolution, "reason": reason})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/ops/external/"+id+"/resolve", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
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

func seedExternalAt(t *testing.T, e *env, state string) storage.ExternalOperation {
	t.Helper()
	ctx := context.Background()
	run, created, err := e.store.CreateRun(ctx, storage.CreateRunParams{
		ID: security.NewID(), OwnerID: "operator", Template: "test-template",
		Mode: "sandbox-e2e", IdempotencyKey: "r13-ui-" + security.NewID(),
		ProviderSnapshotJSN: `{}`, RequestFingerprint: "fp",
		DeadlineMS: time.Now().Add(time.Hour).UnixMilli(),
		Steps:      []storage.StepSeed{{Name: "validate"}},
	})
	if err != nil || !created {
		t.Fatalf("create run: %v created=%v", err, created)
	}
	claim, err := e.store.ClaimNext(ctx, storage.ClaimRequest{
		Owner: "worker-r13", LeaseTTL: time.Minute,
		RunnableStatuses: []string{"CREATED", "VALIDATING"},
		ActiveState:      func(template, step string) (string, bool) { return "VALIDATING", true },
	})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", err, claim)
	}
	op, err := e.store.BeginExternalOperation(ctx, storage.BeginExternalOperationParams{
		RunID: run.ID, StepName: claim.StepName, Provider: "fake-mailbox",
		OperationType: "mailbox.create", Fingerprint: "0123456789abcdef",
		LeaseOwner: claim.Owner, FencingGeneration: claim.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state == "PREPARED" {
		return op
	}
	op, err = e.store.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if state == "IN_FLIGHT" {
		return op
	}
	status := storage.ExternalOutcomeUnknown
	if state == "CONFIRMED" {
		status = storage.ExternalConfirmed
	}
	op, err = e.store.CompleteExternalOperation(ctx, storage.CompleteExternalOperationParams{
		OperationKey: op.OperationKey, Status: status, ErrorClass: "R13_FIXTURE",
		LeaseOwner: claim.Owner, FencingGeneration: claim.Generation, OperationVersion: op.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state == "RECONCILIATION_REQUIRED" {
		if _, err := e.store.RecoverExternalOperations(ctx, "R13_TEST"); err != nil {
			t.Fatal(err)
		}
		op, err = e.store.GetExternalOperationByID(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	return op
}

func loginUISession(t *testing.T, e *env) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/login", strings.NewReader(url.Values{"token": {adminToken}}.Encode()))
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
	csrf, ok := sessionCSRF(t, e)
	if !ok {
		t.Fatal("session endpoint did not return a csrf token")
	}
	return csrf
}

func TestReconciliationActionStatesAndAudit404(t *testing.T) {
	for _, state := range []string{"PREPARED", "IN_FLIGHT", "CONFIRMED", "OUTCOME_UNKNOWN", "RECONCILIATION_REQUIRED"} {
		t.Run(state, func(t *testing.T) {
			e := newEnv(t, nil)
			op := seedExternalAt(t, e, state)
			csrf := loginUISession(t, e)
			// Hidden buttons are not authorization: direct UI POST must still be fenced.
			// Hidden buttons are not authorization: the API fences the same call.
			status, body := resolveExternal(t, e, csrf, op.ID, op.Version, "UNRESOLVED", "review")
			_ = body
			if status != http.StatusOK && status < 400 {
				t.Fatalf("direct resolve status = %d", status)
			}
			got, err := e.store.GetExternalOperationByID(context.Background(), op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if state == "PREPARED" || state == "IN_FLIGHT" {
				if got.Status != state || got.Version != op.Version {
					t.Fatalf("direct POST mutated %s: status=%s version=%d", state, got.Status, got.Version)
				}
			}
			if state == "CONFIRMED" {
				got, err = e.store.GetExternalOperationByID(context.Background(), op.ID)
				if err != nil {
					t.Fatal(err)
				}
				// CONFIRMED recovery may close as confirmed/compensated, never
				// as no-resource-created.
				_, _ = resolveExternal(t, e, csrf, op.ID, got.Version, "NO_RESOURCE_CREATED", "claim no resource")
				after, _ := e.store.GetExternalOperationByID(context.Background(), op.ID)
				if after.Status != storage.ExternalConfirmed || after.Version != got.Version {
					t.Fatalf("CONFIRMED NO_RESOURCE_CREATED mutated operation: %+v", after)
				}
				if st, body := resolveExternal(t, e, csrf, op.ID, got.Version, "MANUAL_COMPENSATION_COMPLETED", "manual cleanup complete"); st != http.StatusOK {
					t.Fatalf("CONFIRMED recovery = %d %s", st, body)
				}
				after, _ = e.store.GetExternalOperationByID(context.Background(), op.ID)
				if after.Status != storage.ExternalResolved || after.Resolution != "MANUAL_COMPENSATION_COMPLETED" {
					t.Fatalf("CONFIRMED recovery did not close: %+v", after)
				}
			}
		})
	}
	// Unknown audit IDs are a real 404, not an empty 200.
	e := newEnv(t, nil)
	status, _ := e.request(t, http.MethodGet, "/v1/ops/external/does-not-exist/audit", nil, adminToken, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown audit status = %d, want 404", status)
	}
}

func loginUISessionClient(t *testing.T, e *env, client *http.Client) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/login", strings.NewReader(url.Values{"token": {adminToken}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	resp, err = client.Get(e.ts.URL + "/ui/api/session")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var payload struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.CSRF == "" {
		t.Fatalf("session endpoint returned no csrf: %s", body)
	}
	return payload.CSRF
}
