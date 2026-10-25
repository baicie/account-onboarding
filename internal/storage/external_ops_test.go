package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
)

// TestExternalOperationReviewIsSticky covers the reconciliation invariant: once
// an attempt is ambiguous (OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED), a later
// deterministic FAILED event must not make the operation look resolved.
func TestExternalOperationReviewIsSticky(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "external.sqlite"))
	ctx := context.Background()
	run, created, err := s.CreateRun(ctx, CreateRunParams{
		ID: security.NewID(), OwnerID: "operator", Template: "test-template",
		Mode: "sandbox-e2e", IdempotencyKey: "external-sticky",
		ProviderSnapshotJSN: `{}`, RequestFingerprint: "fp",
		DeadlineMS: time.Now().Add(time.Hour).UnixMilli(),
		Steps:      []StepSeed{{Name: "validate"}},
	})
	if err != nil || !created {
		t.Fatalf("create run: %v created=%v", err, created)
	}
	payload := func(status, class string) map[string]any {
		return map[string]any{
			"operation_key":       run.ID + ":prepare_mailbox:v1",
			"provider":            "fake-mailbox",
			"operation_type":      "mailbox.create",
			"request_fingerprint": "0123456789abcdef",
			"status":              status,
			"error_class":         class,
			"attempt":             int64(1),
		}
	}
	if err := s.AppendEvent(ctx, run.ID, "EXTERNAL_OPERATION_PREPARED", "prepare_mailbox", payload("PREPARED", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, run.ID, "EXTERNAL_OPERATION_OUTCOME_UNKNOWN", "prepare_mailbox", payload("OUTCOME_UNKNOWN", "TIMEOUT_UNCERTAIN")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, run.ID, "EXTERNAL_OPERATION_FAILED", "prepare_mailbox", payload("FAILED", "UPSTREAM_TRANSIENT")); err != nil {
		t.Fatal(err)
	}
	ops, err := s.ListExternalOperations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].Status != "FAILED" || !ops[0].NeedsReview || ops[0].ReviewReason == "" {
		t.Fatalf("ambiguous history must stay in review: %+v", ops[0])
	}
}
