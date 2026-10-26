package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func insertBulkOutbox(t *testing.T, s *Store, prefix, status string, n int, updated int64) {
	t.Helper()
	tx, err := s.writeDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%05d", prefix, i)
		rule := fmt.Sprintf("%s-rule-%05d", prefix, i%7)
		_, err := tx.Exec(`INSERT INTO alert_outbox
			(id,fingerprint,rule,severity,message,status,attempts,next_attempt_ms,created_at_ms,updated_at_ms)
			VALUES(?,?,?,?,?,?,0,?,?,?)`, id, id, rule, "warning", "bulk", status, updated, updated, updated)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveMissingActiveBeyond500History(t *testing.T) {
	s := outboxStore(t)
	ctx := context.Background()
	// Old active rows deliberately have LOW updated_at; newest 600 are resolved
	// history, so ListAlertOutbox(500) would hide every active row.
	insertBulkOutbox(t, s, "active", AlertOutboxPending, 100, 1)
	insertBulkOutbox(t, s, "hist", AlertOutboxResolved, 600, 10000)
	n, err := s.ResolveAlertOutboxMissing(ctx, nil, time.Now().UnixMilli())
	if err != nil || n != 100 {
		t.Fatalf("resolved %d err=%v, want 100", n, err)
	}
	st, _ := s.AlertOutboxStatusCounts(ctx)
	if st.Pending != 0 || st.Resolved != 700 {
		t.Fatalf("counts after bulk resolve: %+v", st)
	}
}

func TestResolveMissingDoesNotTouchDeliveredHistory(t *testing.T) {
	s := outboxStore(t)
	ctx := context.Background()
	insertBulkOutbox(t, s, "active", AlertOutboxPending, 1, 1)
	insertBulkOutbox(t, s, "deliv", AlertOutboxDelivered, 1000, 10000)
	n, err := s.ResolveAlertOutboxMissing(ctx, nil, time.Now().UnixMilli())
	if err != nil || n != 1 {
		t.Fatalf("resolved %d err=%v", n, err)
	}
	st, _ := s.AlertOutboxStatusCounts(ctx)
	if st.Delivered != 1000 || st.Pending != 0 || st.Resolved != 1 {
		t.Fatalf("delivered history changed: %+v", st)
	}
}

func TestResolveMissingLargeActiveAndKeepCurrentRules(t *testing.T) {
	s := outboxStore(t)
	ctx := context.Background()
	insertBulkOutbox(t, s, "active", AlertOutboxRetryPending, 1000, 1)
	// 1000 rows are spread over 7 rules; keeping rule 0 must preserve its 143
	// rows and resolve the other 857.
	n, err := s.ResolveAlertOutboxMissing(ctx, []string{"active-rule-00000"}, time.Now().UnixMilli())
	st, _ := s.AlertOutboxStatusCounts(ctx)
	if err != nil || n != 857 || st.Resolved != 857 || st.RetryPending != 143 {
		t.Fatalf("resolved=%d status=%+v err=%v, want 857 resolved + 143 retry", n, st, err)
	}
}

func TestResolveMissingClosesInFlightDeliveryHistory(t *testing.T) {
	s := outboxStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	_, _ = s.EnqueueAlertOutbox(ctx, []AlertOutboxInput{{Fingerprint: "fp-race", Rule: "race", Severity: "critical", Message: "m"}}, now)
	items, _, err := s.ClaimAlertOutbox(ctx, "owner", 1, time.Minute, now)
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	if _, err := s.ResolveAlertOutboxMissing(ctx, nil, now+1); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.readDB.QueryRow(`SELECT status FROM alert_deliveries WHERE id=?`, items[0].DeliveryID).Scan(&status); err != nil || status != AlertOutboxResolved {
		t.Fatalf("delivery history status=%q err=%v", status, err)
	}
}

func TestResolveMissingAndClaimConcurrent(t *testing.T) {
	s := outboxStore(t)
	ctx := context.Background()
	insertBulkOutbox(t, s, "active", AlertOutboxPending, 600, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = s.ResolveAlertOutboxMissing(ctx, nil, time.Now().UnixMilli())
	}()
	go func() {
		defer wg.Done()
		_, _, _ = s.ClaimAlertOutbox(ctx, "racing", 10, time.Minute, time.Now().UnixMilli())
	}()
	wg.Wait()
	st, _ := s.AlertOutboxStatusCounts(ctx)
	if st.Pending+st.InFlight+st.RetryPending != 0 {
		t.Fatalf("active rows left after race: %+v", st)
	}
}

func TestResolveMissingAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.sqlite")
	ctx := context.Background()
	s, err := Open(ctx, Options{Path: path, HMACKey: []byte("restart-test-hmac")})
	if err != nil {
		t.Fatal(err)
	}
	insertBulkOutbox(t, s, "old", AlertOutboxPending, 700, 1)
	_ = s.Close()
	restored, err := Open(ctx, Options{Path: path, HMACKey: []byte("restart-test-hmac")})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	n, err := restored.ResolveAlertOutboxMissing(ctx, nil, time.Now().UnixMilli())
	if err != nil || n != 700 {
		t.Fatalf("restart reconcile resolved %d err=%v", n, err)
	}
	st, _ := restored.AlertOutboxStatusCounts(ctx)
	if st.Pending != 0 || st.Resolved != 700 {
		t.Fatalf("restart counts: %+v", st)
	}
}
