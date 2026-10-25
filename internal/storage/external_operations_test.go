package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
)

func claimedRun(t *testing.T, s *Store, idem string) Claim {
	t.Helper()
	ctx := context.Background()
	run, created, err := s.CreateRun(ctx, CreateRunParams{
		ID: security.NewID(), OwnerID: "operator", Template: "test-template",
		Mode: "sandbox-e2e", IdempotencyKey: idem,
		ProviderSnapshotJSN: `{}`, RequestFingerprint: "fp-" + idem,
		DeadlineMS: time.Now().Add(time.Hour).UnixMilli(),
		Steps:      []StepSeed{{Name: "validate"}},
	})
	if err != nil || !created {
		t.Fatalf("create run: %v created=%v", err, created)
	}
	claim, err := s.ClaimNext(ctx, ClaimRequest{
		Owner: "worker-1", LeaseTTL: time.Minute,
		RunnableStatuses: []string{"CREATED", "VALIDATING"},
		ActiveState: func(template, step string) (string, bool) {
			return "VALIDATING", true
		},
	})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", err, claim)
	}
	_ = run
	return *claim
}

func beginParams(claim Claim) BeginExternalOperationParams {
	return BeginExternalOperationParams{
		RunID: claim.Run.ID, StepName: claim.StepName, Provider: "fake-mailbox",
		OperationType: "mailbox.create", Fingerprint: "0123456789abcdef",
		LeaseOwner: claim.Owner, FencingGeneration: claim.Generation,
	}
}

func completeExternalTest(t *testing.T, s *Store, claim Claim, key, status, class string) (ExternalOperation, error) {
	t.Helper()
	op, err := s.GetExternalOperation(context.Background(), key)
	if err != nil {
		return ExternalOperation{}, err
	}
	return s.CompleteExternalOperation(context.Background(), CompleteExternalOperationParams{
		OperationKey: key, Status: status, ErrorClass: class,
		LeaseOwner: claim.Owner, FencingGeneration: claim.Generation, OperationVersion: op.Version,
	})
}

func TestExternalOperationGateBlocksAfterAmbiguousOutcome(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "gate.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "gate-1")
	op, err := s.BeginExternalOperation(ctx, beginParams(claim))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != ExternalPrepared || op.AttemptCount != 1 {
		t.Fatalf("op = %+v", op)
	}
	// Both workers may see PREPARED; only one MarkInFlight CAS may win.
	if _, err := s.BeginExternalOperation(ctx, beginParams(claim)); err != nil {
		t.Fatalf("duplicate PREPARED begin should return a candidate, got %v", err)
	}
	if _, err := s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation); !errors.Is(err, ErrExternalOperationBlocked) {
		t.Fatalf("second mark = %v, want blocked", err)
	}
	if _, err := completeExternalTest(t, s, claim, op.OperationKey, ExternalOutcomeUnknown, "TIMEOUT_UNCERTAIN"); err != nil {
		t.Fatal(err)
	}
	// No automatic retry after an unknown outcome.
	if _, err := s.BeginExternalOperation(ctx, beginParams(claim)); !errors.Is(err, ErrExternalOperationBlocked) {
		t.Fatalf("begin after unknown = %v, want blocked", err)
	}
	// A later completion cannot overwrite the unknown outcome.
	if _, err := completeExternalTest(t, s, claim, op.OperationKey, ExternalFailedDefinite, "VALIDATION"); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrite unknown = %v, want conflict", err)
	}
	got, err := s.GetExternalOperation(ctx, op.OperationKey)
	if err != nil || got.Status != ExternalOutcomeUnknown {
		t.Fatalf("status after failed overwrite = %+v err=%v", got, err)
	}
}

func TestExternalOperationFailedDefiniteAllowsRetry(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "retry.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "gate-2")
	op, err := s.BeginExternalOperation(ctx, beginParams(claim))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := completeExternalTest(t, s, claim, op.OperationKey, ExternalFailedDefinite, "VALIDATION"); err != nil {
		t.Fatal(err)
	}
	second, err := s.BeginExternalOperation(ctx, beginParams(claim))
	if err != nil || second.AttemptCount != 2 || second.Status != ExternalPrepared {
		t.Fatalf("retry after definite failure = %+v err=%v", second, err)
	}
	if _, err := s.MarkExternalOperationInFlight(ctx, second.OperationKey, claim.Owner, claim.Generation); err != nil {
		t.Fatalf("retry mark = %v", err)
	}
}

func TestExternalOperationRejectsStaleFence(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "fence.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "gate-3")
	p := beginParams(claim)
	p.LeaseOwner = "stale-worker"
	if _, err := s.BeginExternalOperation(ctx, p); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale owner = %v, want ErrLeaseLost", err)
	}
	p = beginParams(claim)
	p.FencingGeneration = claim.Generation + 1
	if _, err := s.BeginExternalOperation(ctx, p); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale generation = %v, want ErrLeaseLost", err)
	}
}

func TestExternalOperationResolveCAS(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "resolve.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "gate-4")
	op, err := s.BeginExternalOperation(ctx, beginParams(claim))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := completeExternalTest(t, s, claim, op.OperationKey, ExternalOutcomeUnknown, "TIMEOUT_UNCERTAIN"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetExternalOperation(ctx, op.OperationKey)
	resolved, err := s.ResolveExternalOperation(ctx, op.ID, ExternalResolution{
		Resolution: "RESOURCE_CONFIRMED", Reason: "operator verified mailbox exists",
		EvidenceReference: "ticket-123", ExpectedVersion: before.Version,
		Actor: "test-admin", RequestedAtMS: time.Now().UnixMilli(),
	})
	if err != nil || resolved.Status != ExternalResolved {
		t.Fatalf("resolve = %+v err=%v", resolved, err)
	}
	// Repeated resolution with a stale version must not overwrite.
	if _, err := s.ResolveExternalOperation(ctx, op.ID, ExternalResolution{
		Resolution: "NO_RESOURCE_CREATED", Reason: "second operator",
		ExpectedVersion: before.Version,
		Actor:           "test-admin", RequestedAtMS: time.Now().UnixMilli(),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second resolve = %v, want conflict", err)
	}
	// Resolve is not a retry permit.
	if _, err := s.BeginExternalOperation(ctx, beginParams(claim)); !errors.Is(err, ErrExternalOperationBlocked) {
		t.Fatalf("begin after resolve = %v, want blocked", err)
	}
}

func TestExternalOperationsPaginationAndGlobalCounts(t *testing.T) {
	for _, n := range []int{201, 1000, 10000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			s := newSweepStore(t, filepath.Join(t.TempDir(), "page.sqlite"))
			ctx := context.Background()
			claim := claimedRun(t, s, "page-run")
			runID := claim.Run.ID
			// Seed rows directly for deterministic pagination/query-plan tests.
			tx, err := s.writeDB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			stmt, err := tx.PrepareContext(ctx, `INSERT INTO external_operations
				(id, run_id, step_name, operation_key, provider, operation_type, request_fingerprint,
				 status, attempt_count, version, prepared_at_ms, created_at_ms, updated_at_ms)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < n; i++ {
				status := ExternalOutcomeUnknown
				if i%2 == 0 {
					status = ExternalConfirmed
				}
				// Deliberately repeat timestamps to prove deterministic id tie-break.
				created := int64(1000 + i/10)
				if _, err := stmt.ExecContext(ctx, fmt.Sprintf("op-%05d", i), runID, "prepare_mailbox",
					fmt.Sprintf("seed-key-%05d", i), "fake-mailbox", "mailbox.create", "fp",
					status, 1, 1, created, created, created); err != nil {
					t.Fatal(err)
				}
			}
			stmt.Close()
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			cursor := ""
			pages := 0
			for {
				page, err := s.ListExternalOperations(ctx, ExternalOperationFilter{Limit: 37, Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				if pages == 0 {
					wantNeeds := int64(n / 2)
					if page.TotalNeedsReconciliation != wantNeeds {
						t.Fatalf("global needs = %d, want %d", page.TotalNeedsReconciliation, wantNeeds)
					}
				}
				pages++
				for _, item := range page.Items {
					if seen[item.ID] {
						t.Fatalf("duplicate item in pagination: %s", item.ID)
					}
					seen[item.ID] = true
				}
				if page.NextCursor == "" {
					break
				}
				cursor = page.NextCursor
				if pages > n+10 {
					t.Fatal("pagination did not terminate")
				}
			}
			if len(seen) != n {
				t.Fatalf("paginated %d rows, want %d", len(seen), n)
			}
		})
	}
}

func TestExternalOperationsQueryPlanUsesIndex(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "plan.sqlite"))
	rows, err := s.readDB.Query(`EXPLAIN QUERY PLAN SELECT id FROM external_operations
		WHERE status=? ORDER BY created_at_ms DESC, id DESC LIMIT 10`, ExternalOutcomeUnknown)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + " "
	}
	if !contains(plan, "idx_external_ops_status_created") {
		t.Fatalf("query plan does not use status index: %s", plan)
	}
}

func TestExternalOperationConcurrentMarkOnlyOneWinner(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "concurrent.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "gate-concurrent")
	if _, err := s.BeginExternalOperation(ctx, beginParams(claim)); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			_, err := s.MarkExternalOperationInFlight(ctx, claim.Run.ID+":"+claim.StepName+":v1", claim.Owner, claim.Generation)
			results <- err
		}()
	}
	winners := 0
	for i := 0; i < workers; i++ {
		if err := <-results; err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent MarkInFlight winners = %d, want exactly 1", winners)
	}
}

func TestExternalOperationSurvivesRestartAndAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.sqlite")
	ctx := context.Background()
	s := newSweepStore(t, path)
	claim := claimedRun(t, s, "gate-restart")
	op, err := s.BeginExternalOperation(ctx, beginParams(claim))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := completeExternalTest(t, s, claim, op.OperationKey, ExternalOutcomeUnknown, "TIMEOUT_UNCERTAIN"); err != nil {
		t.Fatal(err)
	}
	op, _ = s.GetExternalOperation(ctx, op.OperationKey)
	resolved, err := s.ResolveExternalOperation(ctx, op.ID, ExternalResolution{
		Resolution: "NO_RESOURCE_CREATED", Reason: "verified absent", ExpectedVersion: op.Version,
		Actor: "test-admin", RequestedAtMS: time.Now().UnixMilli(),
	})
	if err != nil || resolved.Status != ExternalResolved {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	audits, err := s.ListAudit(ctx, 20)
	if err != nil || len(audits) == 0 || audits[0].Action != "external_operation.resolve" {
		t.Fatalf("audit row missing: %+v err=%v", audits, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newSweepStore(t, path)
	got, err := reopened.GetExternalOperation(ctx, op.OperationKey)
	if err != nil || got.Status != ExternalResolved || got.Resolution != "NO_RESOURCE_CREATED" {
		t.Fatalf("after restart: %+v err=%v", got, err)
	}
}

func TestBeginExternalOperationFailsClosedWithoutTable(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "notable.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "gate-notable")
	if _, err := s.writeDB.ExecContext(ctx, `DROP TABLE external_operations`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginExternalOperation(ctx, beginParams(claim)); err == nil {
		t.Fatal("BeginExternalOperation succeeded without the durable gate table")
	}
}

func TestResolveRejectsLiveInFlightAndSupportsExpiredLease(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "live.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "gate-live")
	op, err := s.BeginExternalOperation(ctx, beginParams(claim))
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveExternalOperation(ctx, op.ID, ExternalResolution{
		Resolution: "NO_RESOURCE_CREATED", Reason: "operator race", ExpectedVersion: op.Version,
		Actor: "test-admin", RequestedAtMS: time.Now().UnixMilli(),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("resolve live IN_FLIGHT = %v, want conflict", err)
	}
	// Lease expiry alone is never enough to infer the worker stopped. The
	// single-process lifecycle barrier must clear IN_FLIGHT first.
	if n, err := s.RecoverExternalOperations(ctx, "TEST_WORKER_STOPPED"); err != nil || n != 1 {
		t.Fatalf("lifecycle recovery n=%d err=%v", n, err)
	}
	op, err = s.GetExternalOperation(ctx, op.OperationKey)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveExternalOperation(ctx, op.ID, ExternalResolution{
		Resolution: "NO_RESOURCE_CREATED", Reason: "worker expired; provider console shows no resource",
		ExpectedVersion: op.Version,
		Actor:           "test-admin", RequestedAtMS: time.Now().UnixMilli(),
	})
	if err != nil || resolved.Status != ExternalResolved {
		t.Fatalf("expired-lease resolve = %+v err=%v", resolved, err)
	}
}

func TestResolveConfirmedRecoveryPath(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "confirmed.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "gate-confirmed")
	op, err := s.BeginExternalOperation(ctx, beginParams(claim))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation); err != nil {
		t.Fatal(err)
	}
	op, err = completeExternalTest(t, s, claim, op.OperationKey, ExternalConfirmed, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveExternalOperation(ctx, op.ID, ExternalResolution{
		Resolution: "NO_RESOURCE_CREATED", Reason: "invalid", ExpectedVersion: op.Version,
		Actor: "test-admin", RequestedAtMS: time.Now().UnixMilli(),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("CONFIRMED no-resource = %v, want conflict", err)
	}
	resolved, err := s.ResolveExternalOperation(ctx, op.ID, ExternalResolution{
		Resolution: "RESOURCE_CONFIRMED", Reason: "step commit failed after remote success",
		EvidenceReference: "ticket-confirmed-1", ExpectedVersion: op.Version,
		Actor: "test-admin", RequestedAtMS: time.Now().UnixMilli(),
	})
	if err != nil || resolved.Status != ExternalResolved {
		t.Fatalf("CONFIRMED recovery = %+v err=%v", resolved, err)
	}
	audits, _ := s.ListAudit(ctx, 5)
	if len(audits) == 0 || audits[0].Detail["resolution"] != "RESOURCE_CONFIRMED" ||
		audits[0].Detail["evidence_reference"] != "ticket-confirmed-1" {
		t.Fatalf("resolution audit detail incomplete: %+v", audits)
	}
}

func TestExternalOperationsUnfilteredQueryPlanUsesCreatedIndex(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "plan2.sqlite"))
	rows, err := s.readDB.Query(`EXPLAIN QUERY PLAN SELECT id FROM external_operations
		ORDER BY created_at_ms DESC, id DESC LIMIT 10`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + " "
	}
	if !contains(plan, "idx_external_ops_created") {
		t.Fatalf("unfiltered query plan does not use created index: %s", plan)
	}
}

func TestCompleteRejectsStaleLeaseAndPreservesReconciliation(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "stale-complete.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "complete-stale")
	op, err := s.BeginExternalOperation(ctx, beginParams(claim))
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation)
	if err != nil {
		t.Fatal(err)
	}
	// A newer worker/lease generation owns the run now.
	if _, err := s.writeDB.ExecContext(ctx, `UPDATE runs SET lease_owner='new-worker', fencing_generation=fencing_generation+1 WHERE id=?`, claim.Run.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.CompleteExternalOperation(ctx, CompleteExternalOperationParams{
		OperationKey: op.OperationKey, Status: ExternalConfirmed,
		LeaseOwner: claim.Owner, FencingGeneration: claim.Generation, OperationVersion: op.Version,
	})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale completion = %v, want ErrLeaseLost", err)
	}
	got, err := s.GetExternalOperation(ctx, op.OperationKey)
	if err != nil || got.Status != ExternalReconciliationReq || got.LastErrorClass != "LEASE_LOST_AFTER_REMOTE_CALL_MAYBE_SENT" {
		t.Fatalf("stale completion state = %+v err=%v", got, err)
	}
}

func TestAdminResolveWinsAgainstLateWorkerCompletion(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "resolve-race.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "resolve-race")
	op, err := s.BeginExternalOperation(ctx, beginParams(claim))
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverExternalOperations(ctx, "CRASH_SIMULATED"); err != nil {
		t.Fatal(err)
	}
	op, err = s.GetExternalOperation(ctx, op.OperationKey)
	if err != nil || op.Status != ExternalReconciliationReq {
		t.Fatalf("recovered op = %+v err=%v", op, err)
	}
	resolved, err := s.ResolveExternalOperation(ctx, op.ID, ExternalResolution{
		Resolution: "RESOURCE_CONFIRMED", Reason: "operator checked provider console",
		EvidenceReference: "ticket-late-worker", ExpectedVersion: op.Version,
		Actor: "bearer:admin:test", RequestedAtMS: time.Now().UnixMilli(),
	})
	if err != nil || resolved.Status != ExternalResolved {
		t.Fatalf("resolve = %+v err=%v", resolved, err)
	}
	if _, err := s.CompleteExternalOperation(ctx, CompleteExternalOperationParams{
		OperationKey: op.OperationKey, Status: ExternalConfirmed,
		LeaseOwner: claim.Owner, FencingGeneration: claim.Generation, OperationVersion: resolved.Version,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("late completion after resolve = %v, want conflict", err)
	}
	got, _ := s.GetExternalOperation(ctx, op.OperationKey)
	if got.Status != ExternalResolved {
		t.Fatalf("late completion overwrote resolve: %+v", got)
	}
	audits, _ := s.ListAudit(ctx, 10)
	if len(audits) == 0 || audits[0].Actor != "bearer:admin:test" || audits[0].RunID != op.RunID {
		t.Fatalf("audit identity/run binding: %+v", audits)
	}
	if audits[0].Detail["operation_id"] != op.ID || audits[0].Detail["actor"] != "bearer:admin:test" {
		t.Fatalf("audit detail binding: %+v", audits[0].Detail)
	}
}

func TestRecoverySeparatesPreparedAndInflight(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "recovery.sqlite"))
	ctx := context.Background()
	c1 := claimedRun(t, s, "recovery-prepared")
	// Begin leaves PREPARED (provider call not started).
	if _, err := s.BeginExternalOperation(ctx, beginParams(c1)); err != nil {
		t.Fatal(err)
	}
	// A second run is IN_FLIGHT (may have reached the provider).
	c2 := claimedRun(t, s, "recovery-inflight")
	if _, err := s.BeginExternalOperation(ctx, beginParams(c2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkExternalOperationInFlight(ctx, c2.Run.ID+":"+c2.StepName+":v1", c2.Owner, c2.Generation); err != nil {
		t.Fatal(err)
	}
	n, err := s.RecoverExternalOperations(ctx, "STARTUP_PREVIOUS_PROCESS")
	if err != nil || n != 2 {
		t.Fatalf("recovery n=%d err=%v", n, err)
	}
	p, _ := s.GetExternalOperation(ctx, c1.Run.ID+":"+c1.StepName+":v1")
	f, _ := s.GetExternalOperation(ctx, c2.Run.ID+":"+c2.StepName+":v1")
	if p.Status != ExternalFailedDefinite {
		t.Fatalf("PREPARED recovery = %+v, want FAILED_DEFINITE", p)
	}
	if f.Status != ExternalReconciliationReq {
		t.Fatalf("IN_FLIGHT recovery = %+v, want RECONCILIATION_REQUIRED", f)
	}
}

func TestCompleteRejectsOperationVersionMismatch(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "version.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "version-check")
	op, err := s.BeginExternalOperation(ctx, beginParams(claim))
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteExternalOperation(ctx, CompleteExternalOperationParams{
		OperationKey: op.OperationKey, Status: ExternalConfirmed,
		LeaseOwner: claim.Owner, FencingGeneration: claim.Generation, OperationVersion: op.Version + 1,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("version mismatch = %v, want conflict", err)
	}
}

func TestResolveAuditFailureRollsBack(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "audit-fail.sqlite"))
	ctx := context.Background()
	claim := claimedRun(t, s, "audit-fail")
	op, err := s.BeginExternalOperation(ctx, beginParams(claim))
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation)
	if err != nil {
		t.Fatal(err)
	}
	op, err = completeExternalTest(t, s, claim, op.OperationKey, ExternalOutcomeUnknown, "TIMEOUT")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.ExecContext(ctx, `DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveExternalOperation(ctx, op.ID, ExternalResolution{
		Resolution: "RESOURCE_CONFIRMED", Reason: "audit should fail", ExpectedVersion: op.Version,
		Actor: "test-admin", RequestedAtMS: time.Now().UnixMilli(),
	}); err == nil {
		t.Fatal("resolve succeeded without audit table")
	}
	got, err := s.GetExternalOperation(ctx, op.OperationKey)
	if err != nil || got.Status != ExternalOutcomeUnknown {
		t.Fatalf("audit failure changed status: %+v err=%v", got, err)
	}
}
