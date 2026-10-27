package api_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

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
	resp, err = e.client.Get(e.ts.URL + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(string(raw))
	if len(m) != 2 {
		t.Fatalf("dashboard has no csrf token: %s", raw)
	}
	return m[1]
}

func TestReconciliationActionStatesAndAudit404(t *testing.T) {
	for _, state := range []string{"PREPARED", "IN_FLIGHT", "CONFIRMED", "OUTCOME_UNKNOWN", "RECONCILIATION_REQUIRED"} {
		t.Run(state, func(t *testing.T) {
			e := newEnv(t, nil)
			op := seedExternalAt(t, e, state)
			csrf := loginUISession(t, e)
			resp, err := e.client.Get(e.ts.URL + "/ui/ops/external/" + op.ID)
			if err != nil {
				t.Fatal(err)
			}
			page, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			hasForm := strings.Contains(string(page), `action="/ui/ops/external/`+op.ID+`/resolve"`)
			wantForm := state == "CONFIRMED" || state == "OUTCOME_UNKNOWN" || state == "RECONCILIATION_REQUIRED"
			if hasForm != wantForm {
				t.Fatalf("status %s form=%v want=%v page=%s", state, hasForm, wantForm, page)
			}
			if state == "CONFIRMED" && strings.Contains(string(page), `value="NO_RESOURCE_CREATED"`) {
				t.Fatal("CONFIRMED UI must not offer NO_RESOURCE_CREATED")
			}
			// Hidden buttons are not authorization: direct UI POST must still be fenced.
			body := url.Values{"csrf": {csrf}, "expected_version": {"1"}, "resolution": {"UNRESOLVED"}, "reason": {"review"}}
			req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/ops/external/"+op.ID+"/resolve", strings.NewReader(body.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", e.ts.URL)
			resp, err = e.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("direct POST status = %d", resp.StatusCode)
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
				bad := url.Values{"csrf": {csrf}, "expected_version": {strconv.FormatInt(got.Version, 10)},
					"resolution": {"NO_RESOURCE_CREATED"}, "reason": {"claim no resource"}}
				req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/ui/ops/external/"+op.ID+"/resolve", strings.NewReader(bad.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("Origin", e.ts.URL)
				resp, err := e.client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				after, _ := e.store.GetExternalOperationByID(context.Background(), op.ID)
				if after.Status != storage.ExternalConfirmed || after.Version != got.Version {
					t.Fatalf("CONFIRMED NO_RESOURCE_CREATED mutated operation: %+v", after)
				}
				good := url.Values{"csrf": {csrf}, "expected_version": {strconv.FormatInt(got.Version, 10)},
					"resolution": {"MANUAL_COMPENSATION_COMPLETED"}, "reason": {"manual cleanup complete"}}
				req, _ = http.NewRequest(http.MethodPost, e.ts.URL+"/ui/ops/external/"+op.ID+"/resolve", strings.NewReader(good.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("Origin", e.ts.URL)
				resp, err = e.client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
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
	resp, err = client.Get(e.ts.URL + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(string(raw))
	if len(m) != 2 {
		t.Fatalf("dashboard has no csrf token: %s", raw)
	}
	return m[1]
}
