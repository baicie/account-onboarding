package storage_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

func testStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), storage.Options{
		Path:     filepath.Join(t.TempDir(), "state.sqlite"),
		HMACKey:  []byte("test-hmac-key"),
		Redactor: security.NewRedactor("test-admin-token"),
		TransitionValidator: func(from, to string) error {
			return state.CanTransition(state.RunState(from), state.RunState(to))
		},
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func createRun(t *testing.T, s *storage.Store, idem string, steps ...string) storage.Run {
	t.Helper()
	seeds := make([]storage.StepSeed, 0, len(steps))
	for _, name := range steps {
		seeds = append(seeds, storage.StepSeed{Name: name})
	}
	run, created, err := s.CreateRun(context.Background(), storage.CreateRunParams{
		ID:                  security.NewID(),
		OwnerID:             "operator",
		Template:            "test-template",
		Mode:                "sandbox-e2e",
		IdempotencyKey:      idem,
		ProviderSnapshotJSN: `{"mailbox":{"name":"fake"}}`,
		RequestFingerprint:  "fp-" + idem,
		DeadlineMS:          time.Now().Add(time.Hour).UnixMilli(),
		Steps:               seeds,
	})
	if err != nil || !created {
		t.Fatalf("create run: %v created=%v", err, created)
	}
	return run
}

func claimReq(owner string, ttl time.Duration) storage.ClaimRequest {
	return storage.ClaimRequest{
		Owner:            owner,
		LeaseTTL:         ttl,
		RunnableStatuses: []string{"CREATED", "VALIDATING", "CANCEL_REQUESTED", "WAITING_USER_ACTION", "RETRY_WAIT", "REAUTHORIZE_REQUIRED"},
		ActiveState: func(template, step string) (string, bool) {
			if step == "validate" {
				return string(state.Validating), true
			}
			return "", false
		},
	}
}

func TestMigrateHealthAndWAL(t *testing.T) {
	s := testStore(t)
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if health.SchemaVersion != 3 {
		t.Fatalf("schema version = %d", health.SchemaVersion)
	}
	if health.WALMode != "wal" {
		t.Fatalf("journal mode = %q", health.WALMode)
	}
	if !health.Writable {
		t.Fatal("store not writable")
	}
}

func TestCreateRunIdempotency(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	params := storage.CreateRunParams{
		ID: security.NewID(), OwnerID: "operator", Template: "t", Mode: "sandbox-e2e",
		IdempotencyKey: "key-0001", ProviderSnapshotJSN: `{}`, RequestFingerprint: "fp1",
		DeadlineMS: time.Now().Add(time.Hour).UnixMilli(), Steps: []storage.StepSeed{{Name: "validate"}},
	}
	first, created, err := s.CreateRun(ctx, params)
	if err != nil || !created {
		t.Fatalf("first create: %v %v", err, created)
	}
	params.ID = security.NewID()
	second, created, err := s.CreateRun(ctx, params)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("expected idempotent replay, got created=%v id=%s", created, second.ID)
	}
	params.RequestFingerprint = "different"
	if _, _, err := s.CreateRun(ctx, params); !errors.Is(err, storage.ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
}

func TestClaimLeaseAndFencing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	createRun(t, s, "lease-key", "validate")

	claim1, err := s.ClaimNext(ctx, claimReq("worker-1", 40*time.Millisecond))
	if err != nil || claim1 == nil {
		t.Fatalf("first claim: %v %v", err, claim1)
	}
	if claim1.Generation != 1 || claim1.StateAfter != "VALIDATING" {
		t.Fatalf("unexpected claim: %+v", claim1)
	}
	// A second worker must not be able to claim while the lease is held.
	if other, err := s.ClaimNext(ctx, claimReq("worker-2", time.Minute)); err != nil || other != nil {
		t.Fatalf("expected no claim while leased, got %+v err=%v", other, err)
	}
	// Wait for the lease to expire, then a second worker may re-claim with a
	// higher fencing generation.
	time.Sleep(60 * time.Millisecond)
	claim2, err := s.ClaimNext(ctx, claimReq("worker-2", time.Minute))
	if err != nil || claim2 == nil {
		t.Fatalf("second claim after expiry: %v %v", err, claim2)
	}
	if claim2.Generation != claim1.Generation+1 {
		t.Fatalf("generation did not increase: %d -> %d", claim1.Generation, claim2.Generation)
	}
	// The stale worker's commit must be rejected.
	stale := storage.StepResult{NextState: "FAILED_TERMINAL", StepStatus: "FAILED_TERMINAL"}
	if err := s.CommitStepResult(ctx, claim1, stale); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost, got %v", err)
	}
	// The current lease holder can commit.
	if err := s.CommitStepResult(ctx, claim2, stale); err != nil {
		t.Fatalf("current lease commit failed: %v", err)
	}
	run, _ := s.GetRun(ctx, claim2.Run.ID)
	if run.Status != "FAILED_TERMINAL" {
		t.Fatalf("run status = %s", run.Status)
	}
	if run.FinishedAtMS == nil {
		t.Fatal("finished_at not set for terminal run")
	}
}

func TestConcurrentClaimsOnlyOneWins(t *testing.T) {
	s := testStore(t)
	createRun(t, s, "concurrent-key", "validate")
	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claim, err := s.ClaimNext(context.Background(), claimReq("worker", time.Minute))
			if err != nil {
				return
			}
			if claim != nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly one claim, got %d", wins)
	}
}

func TestTransitionValidationRejectsIllegalEdge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	createRun(t, s, "transition-key", "validate")
	claim, err := s.ClaimNext(ctx, claimReq("worker", time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	// VALIDATING -> COMPLETED is not a legal transition.
	err = s.CommitStepResult(ctx, claim, storage.StepResult{NextState: "COMPLETED", StepStatus: "SUCCEEDED"})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("expected conflict for illegal transition, got %v", err)
	}
}

func TestCancelFlow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	run := createRun(t, s, "cancel-key", "validate")
	cancelled, err := s.RequestCancel(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "CANCEL_REQUESTED" {
		t.Fatalf("status = %s", cancelled.Status)
	}
	claim, err := s.ClaimNext(ctx, claimReq("worker", time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("claim cancel: %v %v", err, claim)
	}
	if !claim.CancelRequested {
		t.Fatal("claim should be marked cancel requested")
	}
	if err := s.CommitStepResult(ctx, claim, storage.StepResult{NextState: "CANCELLED", StepStatus: "CANCELLED"}); err != nil {
		t.Fatal(err)
	}
	final, _ := s.GetRun(ctx, run.ID)
	if final.Status != "CANCELLED" {
		t.Fatalf("final status = %s", final.Status)
	}
	if _, err := s.RequestCancel(ctx, run.ID); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("expected conflict cancelling terminal run, got %v", err)
	}
}

func TestRetryThenResume(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	run := createRun(t, s, "resume-key", "validate")
	claim, err := s.ClaimNext(ctx, claimReq("worker", time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	future := time.Now().Add(time.Hour).UnixMilli()
	res := storage.StepResult{
		NextState: "RETRY_WAIT", StepStatus: "RETRY_WAIT",
		ErrorClass: "UPSTREAM_TRANSIENT", ErrorSummary: "temporary failure",
		RetryAtMS: &future, ResumeState: "VALIDATING", CountFailure: true,
	}
	if err := s.CommitStepResult(ctx, claim, res); err != nil {
		t.Fatal(err)
	}
	waiting, _ := s.GetRun(ctx, run.ID)
	if waiting.Status != "RETRY_WAIT" {
		t.Fatalf("run not in retry wait: %+v", waiting)
	}
	// A retry with a future schedule is not claimable before it is due.
	if next, _ := s.ClaimNext(ctx, claimReq("worker", time.Minute)); next != nil {
		t.Fatalf("future retry should not be claimable: %+v", next)
	}
	if _, err := s.RequestResume(ctx, run.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	next, err := s.ClaimNext(ctx, claimReq("worker", time.Minute))
	if err != nil || next == nil {
		t.Fatalf("claim after resume: %v %v", err, next)
	}
	if next.StateAfter != "VALIDATING" {
		t.Fatalf("state after resume = %s", next.StateAfter)
	}
	if next.Failures != 1 {
		t.Fatalf("failure counter not persisted: %d", next.Failures)
	}
	if err := s.CommitStepResult(ctx, next, storage.StepResult{
		NextState: "FAILED_TERMINAL", StepStatus: "FAILED_TERMINAL",
		ErrorClass: "VALIDATION", ErrorSummary: "invalid request",
	}); err != nil {
		t.Fatalf("fail after resume: %v", err)
	}
	final, _ := s.GetRun(ctx, run.ID)
	if final.Status != "FAILED_TERMINAL" {
		t.Fatalf("final status = %s", final.Status)
	}
	if _, err := s.RequestResume(ctx, run.ID); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("expected conflict resuming terminal run, got %v", err)
	}
}

func TestEventsAppendOnlyAndRedacted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	run := createRun(t, s, "events-key", "validate")
	if err := s.AppendEvent(ctx, run.ID, "STEP_STARTED", "validate", map[string]any{
		"attempt": 1, "access_token": "must-not-persist", "reason": "Bearer abcdefghijklmnop",
	}); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListEvents(ctx, run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("expected RUN_CREATED + appended event, got %d", len(events))
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event seq out of order: %d at %d", ev.Seq, i)
		}
		if _, ok := ev.Payload["access_token"]; ok {
			t.Fatal("disallowed payload key persisted")
		}
		if reason, ok := ev.Payload["reason"].(string); ok && reason == "Bearer abcdefghijklmnop" {
			t.Fatal("unredacted bearer token persisted")
		}
	}
}

func TestMailReceiptsDedupe(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	run := createRun(t, s, "mail-key", "validate")
	first, err := s.ConsumeMessage(ctx, run.ID, "fake-mailbox", "message-1")
	if err != nil || !first {
		t.Fatalf("first consume: %v %v", err, first)
	}
	second, err := s.ConsumeMessage(ctx, run.ID, "fake-mailbox", "message-1")
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("message consumed twice")
	}
	other, err := s.ConsumeMessage(ctx, run.ID, "other-provider", "message-1")
	if err != nil || !other {
		t.Fatalf("different provider should be independent: %v %v", err, other)
	}
}

func TestSecretsCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.PutSecret(ctx, "id1", "mailbox", []byte("ct"), []byte("nonce"), 1); err != nil {
		t.Fatal(err)
	}
	rec, err := s.GetSecret(ctx, "id1")
	if err != nil || string(rec.Ciphertext) != "ct" || rec.KeyVersion != 1 {
		t.Fatalf("get secret: %+v %v", rec, err)
	}
	if err := s.PutSecret(ctx, "id1", "mailbox", []byte("ct2"), []byte("nonce2"), 2); err != nil {
		t.Fatal(err)
	}
	rec, _ = s.GetSecret(ctx, "id1")
	if string(rec.Ciphertext) != "ct2" || rec.KeyVersion != 2 {
		t.Fatal("secret upsert failed")
	}
	if err := s.DeleteSecret(ctx, "id1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSecret(ctx, "id1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestDeadlineSweepFailsRun(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	run, _, err := s.CreateRun(ctx, storage.CreateRunParams{
		ID: security.NewID(), OwnerID: "operator", Template: "t", Mode: "sandbox-e2e",
		IdempotencyKey: "deadline-key", ProviderSnapshotJSN: `{}`, RequestFingerprint: "fp",
		DeadlineMS: time.Now().Add(-time.Minute).UnixMilli(),
		Steps:      []storage.StepSeed{{Name: "validate"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.SweepDeadlines(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		if id == run.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("deadline sweep did not include run: %v", ids)
	}
	got, _ := s.GetRun(ctx, run.ID)
	if got.Status != "FAILED_TERMINAL" || got.LastErrorClass != "TIMEOUT_UNCERTAIN" {
		t.Fatalf("run not failed by deadline: %+v", got)
	}
}

func TestListRunsPagination(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		createRun(t, s, "page-key-"+string(rune('a'+i)), "validate")
	}
	page1, err := s.ListRuns(ctx, storage.RunListFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Items) != 2 || page1.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", page1)
	}
	page2, err := s.ListRuns(ctx, storage.RunListFilter{Limit: 2, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Items) != 2 {
		t.Fatalf("unexpected second page: %+v", page2)
	}
	for _, a := range page1.Items {
		for _, b := range page2.Items {
			if a.ID == b.ID {
				t.Fatal("pagination returned duplicate run")
			}
		}
	}
	// Verify ordering is newest first.
	if page1.Items[0].CreatedAtMS < page1.Items[1].CreatedAtMS {
		t.Fatal("runs not ordered newest first")
	}
}

func TestBackupIntegrityAndSecretsList(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	run := createRun(t, s, "backup-key", "validate")
	if err := s.AppendEvent(ctx, run.ID, "STEP_STARTED", "validate", map[string]any{"attempt": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "sec-1", "mailbox", []byte("ct"), []byte("nonce"), 1); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := s.Backup(ctx, dest); err == nil {
		t.Fatal("backup must refuse to overwrite an existing destination")
	}
	check, err := s.IntegrityCheck(ctx)
	if err != nil || check != "ok" {
		t.Fatalf("integrity check: %q %v", check, err)
	}
	// The snapshot must be openable and contain the data.
	snap, err := storage.Open(ctx, storage.Options{Path: dest, HMACKey: []byte("test-hmac-key")})
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer snap.Close()
	health, err := snap.Health(ctx)
	if err != nil || health.SchemaVersion != 3 {
		t.Fatalf("snapshot health: %+v %v", health, err)
	}
	if _, err := snap.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("snapshot missing run: %v", err)
	}
	if _, err := snap.GetSecret(ctx, "sec-1"); err != nil {
		t.Fatalf("snapshot missing secret: %v", err)
	}
	secrets, err := s.ListSecrets(ctx)
	if err != nil || len(secrets) != 1 || secrets[0].ID != "sec-1" {
		t.Fatalf("list secrets: %+v %v", secrets, err)
	}
}

func TestMailReceiptsAreAtomicWithStepCommit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	run := createRun(t, s, "receipt-key", "validate")
	claim, err := s.ClaimNext(ctx, claimReq("worker", time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", err, claim)
	}
	// A failed commit must not consume the message.
	if err := s.CommitStepResult(ctx, claim, storage.StepResult{
		NextState: "FAILED_TERMINAL", StepStatus: "FAILED_TERMINAL",
		ErrorClass: "VALIDATION", ErrorSummary: "failed before consumption",
		MailReceipts: []storage.MailReceiptDraft{{Provider: "fake-mailbox", MessageKey: "msg-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	consumed, err := s.MessageConsumed(ctx, run.ID, "fake-mailbox", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if !consumed {
		t.Fatal("receipt was lost: consume must be committed atomically with the step")
	}
	// A second commit for another message records exactly once.
	run2 := createRun(t, s, "receipt-key-2", "validate")
	claim2, err := s.ClaimNext(ctx, claimReq("worker", time.Minute))
	if err != nil || claim2 == nil {
		t.Fatalf("claim2: %v %v", err, claim2)
	}
	if err := s.CommitStepResult(ctx, claim2, storage.StepResult{
		NextState: "FAILED_TERMINAL", StepStatus: "FAILED_TERMINAL",
		ErrorClass: "VALIDATION", ErrorSummary: "done",
		MailReceipts: []storage.MailReceiptDraft{{Provider: "fake-mailbox", MessageKey: "msg-2"}},
	}); err != nil {
		t.Fatal(err)
	}
	if consumed, _ := s.MessageConsumed(ctx, run2.ID, "fake-mailbox", "msg-2"); !consumed {
		t.Fatal("second receipt not persisted")
	}
	if consumed, _ := s.MessageConsumed(ctx, run.ID, "fake-mailbox", "msg-2"); consumed {
		t.Fatal("receipt leaked across runs")
	}
}
