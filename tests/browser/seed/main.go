// Command browser-seed creates deterministic SQLite fixtures for the Chromium
// matrix. It is invoked only by tests/browser/run.sh and is not part of onboardd.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

const template = "sandbox-email-verify-oidc"

func main() {
	db, master, out := os.Getenv("ONBOARD_DB_PATH"), os.Getenv("ONBOARD_MASTER_KEY_FILE"), os.Getenv("ONBOARD_BROWSER_FIXTURES")
	if db == "" || master == "" || out == "" {
		panic("ONBOARD_* fixture env required")
	}
	raw, err := os.ReadFile(master)
	if err != nil {
		panic(err)
	}
	key := sha256.Sum256(raw)
	ctx := context.Background()
	st, err := storage.Open(ctx, storage.Options{Path: db, HMACKey: key[:], Redactor: security.NewRedactor()})
	if err != nil {
		panic(err)
	}
	defer st.Close()

	opRun, _, err := st.CreateRun(ctx, storage.CreateRunParams{
		ID: "browser-op-run", OwnerID: "browser-fixture", Template: template, Mode: "sandbox-e2e",
		IdempotencyKey: "browser-op-run", ProviderSnapshotJSN: `{"fixture":true}`,
		RequestFingerprint: "browser-op-fp", DeadlineMS: time.Now().Add(24 * time.Hour).UnixMilli(),
		Steps: []storage.StepSeed{{Name: "external_write"}},
	})
	if err != nil {
		panic(err)
	}
	claim, err := st.ClaimNext(ctx, storage.ClaimRequest{Owner: "browser-seed", LeaseTTL: time.Hour,
		RunnableStatuses: []string{"CREATED"},
		ActiveState:      func(string, string) (string, bool) { return "VALIDATING", true }})
	if err != nil || claim == nil || claim.Run.ID != opRun.ID {
		panic("claim external-op fixture run")
	}
	op, err := st.BeginExternalOperation(ctx, storage.BeginExternalOperationParams{
		RunID: opRun.ID, StepName: claim.StepName, Provider: "cycletls", OperationType: "mailbox.create",
		Fingerprint: "browser-fixture-operation", LeaseOwner: claim.Owner, FencingGeneration: claim.Generation})
	if err != nil {
		panic(err)
	}
	if op, err = st.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation); err != nil {
		panic(err)
	}
	if op, err = st.CompleteExternalOperation(ctx, storage.CompleteExternalOperationParams{
		OperationKey: op.OperationKey, Status: storage.ExternalOutcomeUnknown, ErrorClass: "BROWSER_FIXTURE",
		LeaseOwner: claim.Owner, FencingGeneration: claim.Generation, OperationVersion: op.Version}); err != nil {
		panic(err)
	}

	for i := 0; i < 55; i++ {
		if _, _, err := st.CreateRun(ctx, storage.CreateRunParams{
			ID: fmt.Sprintf("browser-run-%04d", i), OwnerID: "browser-fixture", Template: template,
			Mode: "sandbox-e2e", IdempotencyKey: fmt.Sprintf("browser-run-%04d", i),
			ProviderSnapshotJSN: `{"fixture":true}`, RequestFingerprint: "browser-fixture-fp",
			DeadlineMS: time.Now().Add(24 * time.Hour).UnixMilli(),
			Steps:      []storage.StepSeed{{Name: "validate"}}}); err != nil {
			panic(err)
		}
	}

	// Terminal fixture runs are not claimable by the running engine, keeping
	// list/filter/pagination deterministic while the real onboardd is alive.
	rawDB, err := sql.Open("sqlite3", db+"?_busy_timeout=5000")
	if err != nil {
		panic(err)
	}
	if _, err := rawDB.ExecContext(ctx, `UPDATE runs SET status='FAILED_TERMINAL' WHERE id LIKE 'browser-run-%'`); err != nil {
		panic(err)
	}
	rawDB.Close()

	deliveredID := enqueueAlert(st, "browser_delivered", "delivered_fixture")
	items, _, err := st.ClaimAlertOutbox(ctx, "browser-seed", 1, time.Minute, time.Now().UnixMilli())
	if err != nil || len(items) != 1 || items[0].Rule != "browser_delivered" {
		panic("claim delivered fixture")
	}
	if err := st.CompleteAlertOutbox(ctx, items[0].DeliveryID, "browser-seed", true, time.Now().UnixMilli(), 0, 200, "", 3); err != nil {
		panic(err)
	}
	deadID := enqueueAlert(st, "browser_dead", "dead_fixture")
	items, _, err = st.ClaimAlertOutbox(ctx, "browser-seed", 1, time.Minute, time.Now().UnixMilli())
	if err != nil || len(items) != 1 || items[0].Rule != "browser_dead" {
		panic("claim dead fixture")
	}
	if err := st.CompleteAlertOutbox(ctx, items[0].DeliveryID, "browser-seed", false, time.Now().UnixMilli(), 0, 500, "fixture failure", 1); err != nil {
		panic(err)
	}
	pendingID := enqueueAlert(st, "browser_pending", "pending_fixture")

	fixtures := map[string]any{
		"run_ids": []string{"browser-run-0000", "browser-run-0054"}, "run_count": 55,
		"external_op_id": op.ID, "external_op_version": op.Version, "external_op_run_id": opRun.ID,
		"pending_alert_id": pendingID, "delivered_alert_id": deliveredID, "dead_letter_alert_id": deadID,
		"pending_rule": "browser_pending", "delivered_rule": "browser_delivered", "dead_rule": "browser_dead",
	}
	rawJSON, _ := json.MarshalIndent(fixtures, "", "  ")
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		panic(err)
	}
	if err := os.WriteFile(out, rawJSON, 0o600); err != nil {
		panic(err)
	}
}

func enqueueAlert(st *storage.Store, rule, fingerprint string) string {
	if _, err := st.EnqueueAlertOutbox(context.Background(), []storage.AlertOutboxInput{{
		Fingerprint: fingerprint, Rule: rule, Severity: "warning", Message: "browser fixture " + rule,
	}}, time.Now().UnixMilli()); err != nil {
		panic(err)
	}
	items, err := st.ListAlertOutboxFilter(context.Background(), storage.AlertOutboxPending, 10, 0)
	if err != nil {
		panic(err)
	}
	for _, it := range items {
		if it.Rule == rule {
			return it.ID
		}
	}
	panic("alert not found: " + rule)
}
