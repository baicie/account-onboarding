package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
)

func newSweepStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{
		Path:     path,
		HMACKey:  []byte("sweep-race-hmac-key"),
		Redactor: security.NewRedactor(),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func createExpiredRun(t *testing.T, s *Store, idem string) Run {
	t.Helper()
	return createRunWithDeadline(t, s, idem, time.Now().Add(-time.Minute))
}

func createRunWithDeadline(t *testing.T, s *Store, idem string, deadline time.Time) Run {
	t.Helper()
	run, created, err := s.CreateRun(context.Background(), CreateRunParams{
		ID:                  security.NewID(),
		OwnerID:             "operator",
		Template:            "test-template",
		Mode:                "sandbox-e2e",
		IdempotencyKey:      idem,
		ProviderSnapshotJSN: `{}`,
		RequestFingerprint:  "fp-" + idem,
		DeadlineMS:          deadline.UnixMilli(),
		Steps:               []StepSeed{{Name: "validate"}},
	})
	if err != nil || !created {
		t.Fatalf("create expired run: %v created=%v", err, created)
	}
	return run
}

func eventsOfType(t *testing.T, s *Store, runID, eventType string) int {
	t.Helper()
	events, err := s.ListEvents(context.Background(), runID, 0, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Type == eventType {
			n++
		}
	}
	return n
}

// TestSweepDeadlinesDoesNotOverrideCancel deterministically cancels a run
// between the sweeper's SELECT and its write transaction. The timeout sweep must
// leave CANCEL_REQUESTED intact and must not emit a RUN_FAILED event.
func TestSweepDeadlinesDoesNotOverrideCancel(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "race.sqlite"))
	ctx := context.Background()
	run := createExpiredRun(t, s, "sweep-cancel-race")

	reached := make(chan struct{})
	release := make(chan struct{})
	s.sweepHook = func() { close(reached); <-release }
	t.Cleanup(func() { s.sweepHook = nil })

	type result struct {
		ids []string
		err error
	}
	done := make(chan result, 1)
	go func() {
		ids, err := s.SweepDeadlines(ctx, 10)
		done <- result{ids, err}
	}()
	<-reached
	if _, err := s.RequestCancel(ctx, run.ID); err != nil {
		close(release)
		t.Fatalf("cancel between select and tx: %v", err)
	}
	close(release)
	res := <-done
	if res.err != nil {
		t.Fatalf("sweep: %v", res.err)
	}
	if len(res.ids) != 0 {
		t.Fatalf("sweeper claimed a cancelled run: %v", res.ids)
	}
	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "CANCEL_REQUESTED" {
		t.Fatalf("status = %s, want CANCEL_REQUESTED", got.Status)
	}
	if n := eventsOfType(t, s, run.ID, "RUN_FAILED"); n != 0 {
		t.Fatalf("RUN_FAILED events = %d, want 0", n)
	}
	if n := eventsOfType(t, s, run.ID, "RUN_CANCEL_REQUESTED"); n != 1 {
		t.Fatalf("RUN_CANCEL_REQUESTED events = %d, want 1", n)
	}
}

// TestSweepDeadlinesDoesNotDoubleFailWorkerTerminal commits a worker terminal
// result between SELECT and the sweep transaction; the sweep must not overwrite
// it or append a second RUN_FAILED event.
func TestSweepDeadlinesDoesNotDoubleFailWorkerTerminal(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "race2.sqlite"))
	ctx := context.Background()
	run := createRunWithDeadline(t, s, "sweep-worker-race", time.Now().Add(time.Minute))
	claim, err := s.ClaimNext(ctx, ClaimRequest{
		Owner: "worker-1", LeaseTTL: time.Minute,
		RunnableStatuses: []string{"CREATED"},
		ActiveState: func(template, step string) (string, bool) {
			return "VALIDATING", true
		},
	})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", err, claim)
	}
	// Expire the deadline only after the claim, so the sweeper sees the run as
	// expired while a live worker holds the lease (the race under test).
	if _, err := s.writeDB.ExecContext(ctx, `UPDATE runs SET deadline_ms=? WHERE id=?`,
		time.Now().Add(-time.Minute).UnixMilli(), run.ID); err != nil {
		t.Fatalf("expire deadline: %v", err)
	}

	reached := make(chan struct{})
	release := make(chan struct{})
	s.sweepHook = func() { close(reached); <-release }
	t.Cleanup(func() { s.sweepHook = nil })

	done := make(chan error, 1)
	go func() {
		_, err := s.SweepDeadlines(ctx, 10)
		done <- err
	}()
	<-reached
	if err := s.CommitStepResult(ctx, claim, StepResult{
		NextState: "FAILED_TERMINAL", StepStatus: "FAILED_TERMINAL",
		ErrorClass: "UPSTREAM_PERMANENT", ErrorSummary: "worker failed",
	}); err != nil {
		close(release)
		t.Fatalf("worker terminal commit: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, _ := s.GetRun(ctx, run.ID)
	if got.Status != "FAILED_TERMINAL" {
		t.Fatalf("status = %s", got.Status)
	}
	if n := eventsOfType(t, s, run.ID, "RUN_FAILED"); n != 1 {
		t.Fatalf("RUN_FAILED events = %d, want exactly 1", n)
	}
}

// TestSweepDeadlinesIsIdempotent ensures a second sweep cannot append another
// terminal event for the same run.
func TestSweepDeadlinesIsIdempotent(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "idem.sqlite"))
	ctx := context.Background()
	run := createExpiredRun(t, s, "sweep-idem")
	first, err := s.SweepDeadlines(ctx, 10)
	if err != nil || len(first) != 1 || first[0] != run.ID {
		t.Fatalf("first sweep = %v err=%v", first, err)
	}
	second, err := s.SweepDeadlines(ctx, 10)
	if err != nil || len(second) != 0 {
		t.Fatalf("second sweep = %v err=%v, want empty", second, err)
	}
	if n := eventsOfType(t, s, run.ID, "RUN_FAILED"); n != 1 {
		t.Fatalf("RUN_FAILED events = %d, want 1", n)
	}
}

// TestSweepDeadlinesAfterRestart proves expired runs are still cleaned after a
// process restart (durable deadline state).
func TestSweepDeadlinesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.sqlite")
	ctx := context.Background()
	s := newSweepStore(t, path)
	run := createExpiredRun(t, s, "sweep-restart")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newSweepStore(t, path)
	ids, err := reopened.SweepDeadlines(ctx, 10)
	if err != nil || len(ids) != 1 || ids[0] != run.ID {
		t.Fatalf("sweep after restart = %v err=%v", ids, err)
	}
	got, _ := reopened.GetRun(ctx, run.ID)
	if got.Status != "FAILED_TERMINAL" {
		t.Fatalf("status = %s", got.Status)
	}
}

// TestSweepDeadlinesSkipsAlreadyCancelled covers the case where the run was
// cancelled before the sweeper SELECT even ran.
func TestSweepDeadlinesSkipsAlreadyCancelled(t *testing.T) {
	s := newSweepStore(t, filepath.Join(t.TempDir(), "cancelled.sqlite"))
	ctx := context.Background()
	run := createExpiredRun(t, s, "sweep-cancelled")
	if _, err := s.RequestCancel(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := s.SweepDeadlines(ctx, 10)
	if err != nil || len(ids) != 0 {
		t.Fatalf("sweep cancelled run = %v err=%v", ids, err)
	}
	got, _ := s.GetRun(ctx, run.ID)
	if got.Status != "CANCEL_REQUESTED" {
		t.Fatalf("status = %s", got.Status)
	}
}
