package storage

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func outboxStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{
		Path: filepath.Join(t.TempDir(), "outbox.sqlite"), HMACKey: []byte("outbox-test-hmac"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAlertOutboxAtLeastOnceLifecycle(t *testing.T) {
	s := outboxStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	in := []AlertOutboxInput{{Fingerprint: "fp-r", Rule: "r", Severity: "warning", Message: "m"}}
	if n, err := s.EnqueueAlertOutbox(ctx, in, now); err != nil || n != 1 {
		t.Fatalf("enqueue = %d %v", n, err)
	}
	if n, err := s.EnqueueAlertOutbox(ctx, in, now); err != nil || n != 0 {
		t.Fatalf("duplicate enqueue = %d %v", n, err)
	}
	items, deliveries, err := s.ClaimAlertOutbox(ctx, "owner-a", 10, time.Minute, now)
	if err != nil || len(items) != 1 || len(deliveries) != 1 {
		t.Fatalf("claim = %d %d %v", len(items), len(deliveries), err)
	}
	if items[0].Status != AlertOutboxInFlight || items[0].DeliveryID == "" {
		t.Fatalf("claimed item = %+v", items[0])
	}
	// Another owner cannot claim the leased item.
	if more, _, err := s.ClaimAlertOutbox(ctx, "owner-b", 10, time.Minute, now); err != nil || len(more) != 0 {
		t.Fatalf("second claim = %d %v", len(more), err)
	}
	if err := s.CompleteAlertOutbox(ctx, items[0].DeliveryID, "owner-b", true, now+1, 0, 200, "", 3); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale owner completion err=%v", err)
	}
	if err := s.CompleteAlertOutbox(ctx, items[0].DeliveryID, "owner-a", true, now+1, 0, 200, "", 3); err != nil {
		t.Fatal(err)
	}
	st, err := s.AlertOutboxStatusCounts(ctx)
	if err != nil || st.Delivered != 1 || st.Pending != 0 {
		t.Fatalf("status = %+v err=%v", st, err)
	}
}

func TestAlertOutboxRetryBackoffAndDeadLetter(t *testing.T) {
	s := outboxStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	_, _ = s.EnqueueAlertOutbox(ctx, []AlertOutboxInput{{Fingerprint: "fp-x", Rule: "x", Severity: "warning", Message: "x"}}, now)
	for attempt := 1; attempt <= 3; attempt++ {
		items, _, err := s.ClaimAlertOutbox(ctx, "owner", 1, time.Minute, now)
		if err != nil || len(items) != 1 {
			t.Fatalf("attempt %d claim = %d %v", attempt, len(items), err)
		}
		if err := s.CompleteAlertOutbox(ctx, items[0].DeliveryID, "owner", false, now+1, 1000, 500, "boom", 3); err != nil {
			t.Fatal(err)
		}
		now += 1001
	}
	st, _ := s.AlertOutboxStatusCounts(ctx)
	if st.DeadLetter != 1 || st.RetryPending != 0 {
		t.Fatalf("dead letter status = %+v", st)
	}
	if items, _, _ := s.ClaimAlertOutbox(ctx, "owner", 1, time.Minute, now+10000); len(items) != 0 {
		t.Fatalf("dead letter reclaimed: %d", len(items))
	}
}

func TestAlertOutboxResolveAndExpiredLeaseRecovery(t *testing.T) {
	s := outboxStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	_, _ = s.EnqueueAlertOutbox(ctx, []AlertOutboxInput{{Fingerprint: "fp-y", Rule: "y", Severity: "warning", Message: "y"}}, now)
	items, _, err := s.ClaimAlertOutbox(ctx, "owner-a", 1, 100*time.Millisecond, now)
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	n, err := s.ResolveAlertOutbox(ctx, "y", now+1)
	if err != nil || n != 1 {
		t.Fatalf("resolve = %d %v", n, err)
	}
	// Resolve wins even though the delivery was in flight.
	if err := s.CompleteAlertOutbox(ctx, items[0].DeliveryID, "owner-a", true, now+2, 0, 200, "", 3); err != nil {
		// completion may succeed but must not resurrect the resolved alert
		t.Logf("completion after resolve: %v", err)
	}
	st, _ := s.AlertOutboxStatusCounts(ctx)
	if st.Resolved != 1 || st.InFlight != 0 || st.Delivered != 0 {
		t.Fatalf("resolved status = %+v", st)
	}
	var deliveryStatus string
	if err := s.readDB.QueryRowContext(ctx, `SELECT status FROM alert_deliveries WHERE id=?`, items[0].DeliveryID).Scan(&deliveryStatus); err != nil || deliveryStatus != AlertOutboxResolved {
		t.Fatalf("in-flight delivery history = %q err=%v, want RESOLVED", deliveryStatus, err)
	}
	if items, _, _ := s.ClaimAlertOutbox(ctx, "owner", 1, time.Minute, now+10000); len(items) != 0 {
		t.Fatalf("resolved alert reclaimed: %d", len(items))
	}
}

func TestAlertOutboxConcurrentClaimSingleWinner(t *testing.T) {
	s := outboxStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	_, _ = s.EnqueueAlertOutbox(ctx, []AlertOutboxInput{{Fingerprint: "fp-z", Rule: "z", Severity: "warning", Message: "z"}}, now)
	var wg sync.WaitGroup
	results := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			items, _, _ := s.ClaimAlertOutbox(ctx, "owner-"+string(rune('a'+i)), 1, time.Minute, now)
			results <- len(items)
		}(i)
	}
	wg.Wait()
	close(results)
	total := 0
	for n := range results {
		total += n
	}
	if total != 1 {
		t.Fatalf("concurrent claims = %d, want 1", total)
	}
}
