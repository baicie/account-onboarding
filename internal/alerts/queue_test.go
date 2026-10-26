package alerts

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueDeduplicatesAndResolves(t *testing.T) {
	q := NewQueue(4, 0)
	alert := Alert{Rule: "readyz_failing", Severity: "critical", Message: "not ready"}
	q.Enqueue([]Alert{alert, alert})
	if q.Status().Pending != 1 || q.Status().Deduped != 1 {
		t.Fatalf("status after duplicate: %+v", q.Status())
	}
	n := &RecordingNotifier{}
	if sent, err := q.Drain(context.Background(), n); err != nil || sent != 1 {
		t.Fatalf("drain sent=%d err=%v", sent, err)
	}
	// Still active until resolved: a later identical condition is deduped.
	q.Enqueue([]Alert{alert})
	if q.Status().Deduped != 2 || q.Status().Pending != 0 {
		t.Fatalf("active condition re-enqueued: %+v", q.Status())
	}
	if _, ok := q.Resolve("readyz_failing"); !ok {
		t.Fatal("resolve did not find active rule")
	}
	q.Enqueue([]Alert{alert})
	if q.Status().Pending != 1 {
		t.Fatalf("rule did not re-arm after resolve: %+v", q.Status())
	}
}

func TestQueueBoundedAndRetryRateLimited(t *testing.T) {
	q := NewQueue(2, time.Minute)
	q.Enqueue([]Alert{
		{Rule: "a", Severity: "warning", Message: "a"},
		{Rule: "b", Severity: "warning", Message: "b"},
		{Rule: "c", Severity: "warning", Message: "c"},
	})
	st := q.Status()
	if st.Pending != 2 || st.Dropped != 1 {
		t.Fatalf("bounded status: %+v", st)
	}
	now := time.Unix(1000, 0)
	q.now = func() time.Time { return now }
	failing := &RecordingNotifier{err: errors.New("sink down")}
	if _, err := q.Drain(context.Background(), failing); err == nil {
		t.Fatal("notifier failure swallowed")
	}
	if q.Status().Pending != 2 || q.Status().LastError == "" {
		t.Fatalf("failed drain lost queue/error: %+v", q.Status())
	}
	if _, err := q.Drain(context.Background(), failing); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("retry not rate-limited: %v", err)
	}
	now = now.Add(2 * time.Minute)
	n := &RecordingNotifier{}
	if sent, err := q.Drain(context.Background(), n); err != nil || sent != 2 {
		t.Fatalf("retry sent=%d err=%v", sent, err)
	}
	if len(n.Delivered()) != 2 {
		t.Fatalf("delivered=%d", len(n.Delivered()))
	}
}

func TestQueueSmallAndEmptyDrain(t *testing.T) {
	q := NewQueue(1, 0)
	n := &RecordingNotifier{}
	if sent, err := q.Drain(context.Background(), n); err != nil || sent != 0 {
		t.Fatalf("empty drain sent=%d err=%v", sent, err)
	}
	q.Enqueue([]Alert{{Rule: "r", Severity: "warning", Message: "m"}})
	if _, err := q.Drain(context.Background(), n); err != nil {
		t.Fatal(err)
	}
}

func TestQueueDropReenqueueAndResolveDropsPending(t *testing.T) {
	q := NewQueue(1, 0)
	a := Alert{Rule: "a", Severity: "warning", Message: "a"}
	b := Alert{Rule: "b", Severity: "warning", Message: "b"}
	q.Enqueue([]Alert{a})
	q.Enqueue([]Alert{b}) // drops a and clears a.active
	if st := q.Status(); st.Dropped != 1 || st.Pending != 1 {
		t.Fatalf("drop status: %+v", st)
	}
	q.Enqueue([]Alert{a}) // must be accepted again, not deduped forever
	if st := q.Status(); st.Pending != 1 || st.Deduped != 0 {
		t.Fatalf("dropped alert stuck active: %+v", st)
	}
	// b was dropped by a; resolve the active a and ensure no stale pending.
	if _, ok := q.Resolve("a"); !ok {
		t.Fatal("resolve a")
	}
	q.Enqueue([]Alert{b})
	if _, ok := q.Resolve("b"); !ok {
		t.Fatal("resolve b")
	}
	n := &RecordingNotifier{}
	if sent, err := q.Drain(context.Background(), n); err != nil || sent != 0 {
		t.Fatalf("resolved stale alerts were delivered: sent=%d err=%v", sent, err)
	}
}

func TestQueueConcurrentDrainDoesNotDoubleSend(t *testing.T) {
	q := NewQueue(10, 0)
	q.Enqueue([]Alert{{Rule: "r", Severity: "warning", Message: "m"}})
	block := make(chan struct{})
	n := &blockingNotifier{block: block, entered: make(chan struct{})}
	errs := make(chan error, 2)
	go func() { _, err := q.Drain(context.Background(), n); errs <- err }()
	<-n.entered
	go func() { _, err := q.Drain(context.Background(), n); errs <- err }()
	close(block)
	for i := 0; i < 2; i++ {
		err := <-errs
		if err != nil && !errors.Is(err, ErrDrainInProgress) {
			t.Fatalf("unexpected concurrent drain error: %v", err)
		}
	}
	if n.calls.Load() != 1 {
		t.Fatalf("notifier called %d times", n.calls.Load())
	}
}

type blockingNotifier struct {
	block   chan struct{}
	entered chan struct{}
	calls   atomic.Int32
	once    sync.Once
	err     error
}

func (n *blockingNotifier) Notify(ctx context.Context, alerts []Alert) error {
	n.calls.Add(1)
	if n.entered == nil {
		n.entered = make(chan struct{})
	}
	n.once.Do(func() { close(n.entered) })
	select {
	case <-n.block:
		return n.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestQueueReconcileRearmsRecoveredRules(t *testing.T) {
	q := NewQueue(10, 0)
	a := Alert{Rule: "a", Severity: "warning", Message: "a"}
	b := Alert{Rule: "b", Severity: "warning", Message: "b"}
	q.Enqueue([]Alert{a})
	// a recovered, b is now firing: reconcile clears a and enqueues b.
	q.Reconcile([]Alert{b})
	if st := q.Status(); st.Pending != 1 || st.Active != 1 {
		t.Fatalf("reconcile status: %+v", st)
	}
	// a fires again: after recovery it must be re-armed, not deduped forever.
	q.Reconcile([]Alert{a, b})
	if st := q.Status(); st.Pending != 2 || st.Active != 2 {
		t.Fatalf("re-arm status: %+v", st)
	}
	// Empty evaluation resolves all active rules and drops pending stale entries.
	q.Reconcile(nil)
	if st := q.Status(); st.Pending != 0 || st.Active != 0 {
		t.Fatalf("empty reconcile status: %+v", st)
	}
}

func startBlockedDrain(t *testing.T, q *Queue, n *blockingNotifier) <-chan error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { _, err := q.Drain(context.Background(), n); ch <- err }()
	select {
	case <-n.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("notifier did not enter")
	}
	return ch
}

func TestQueueResolveWhileDrainBlocked(t *testing.T) {
	q := NewQueue(10, 0)
	q.Enqueue([]Alert{{Rule: "r", Severity: "warning", Message: "m"}})
	block := make(chan struct{})
	n := &blockingNotifier{block: block, entered: make(chan struct{})}
	done := startBlockedDrain(t, q, n)
	if _, ok := q.Resolve("r"); !ok {
		t.Fatal("resolve during in-flight failed")
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st := q.Status(); st.Pending != 0 || st.InFlight != 0 || st.Delivered != 0 {
		t.Fatalf("resolved in-flight entry was resurrected: %+v", st)
	}
}

func TestQueueReconcileWhileDrainBlocked(t *testing.T) {
	q := NewQueue(10, 0)
	q.Enqueue([]Alert{{Rule: "r", Severity: "warning", Message: "m"}})
	block := make(chan struct{})
	n := &blockingNotifier{block: block, entered: make(chan struct{})}
	done := startBlockedDrain(t, q, n)
	q.Reconcile(nil)
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st := q.Status(); st.Pending != 0 || st.InFlight != 0 || st.Active != 0 {
		t.Fatalf("reconcile during in-flight left stale state: %+v", st)
	}
}

func TestQueueEnqueueAndDropWhileDrainBlocked(t *testing.T) {
	q := NewQueue(1, 0)
	q.Enqueue([]Alert{{Rule: "a", Severity: "warning", Message: "a"}})
	block := make(chan struct{})
	n := &blockingNotifier{block: block, entered: make(chan struct{})}
	done := startBlockedDrain(t, q, n)
	q.Enqueue([]Alert{{Rule: "b", Severity: "warning", Message: "b"}})
	if st := q.Status(); st.InFlight != 1 || st.Pending != 0 || st.Dropped != 1 {
		t.Fatalf("drop while in-flight: %+v", st)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st := q.Status(); st.Pending != 0 || st.Delivered != 1 {
		t.Fatalf("after in-flight delivery: %+v", st)
	}
}

func TestQueueFailureResolveWhileDrainBlocked(t *testing.T) {
	q := NewQueue(10, 0)
	q.Enqueue([]Alert{{Rule: "r", Severity: "warning", Message: "m"}})
	block := make(chan struct{})
	n := &blockingNotifier{block: block, entered: make(chan struct{}), err: errors.New("sink down")}
	done := startBlockedDrain(t, q, n)
	if _, ok := q.Resolve("r"); !ok {
		t.Fatal("resolve")
	}
	close(block)
	if err := <-done; err == nil {
		t.Fatal("expected notifier failure")
	}
	if st := q.Status(); st.Pending != 0 || st.InFlight != 0 || st.LastError == "" {
		t.Fatalf("resolved failed batch was retried: %+v", st)
	}
}

func TestQueueContextCancelRetries(t *testing.T) {
	q := NewQueue(10, 0)
	q.Enqueue([]Alert{{Rule: "r", Severity: "warning", Message: "m"}})
	ctx, cancel := context.WithCancel(context.Background())
	n := &blockingNotifier{block: make(chan struct{}), entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := q.Drain(ctx, n); done <- err }()
	<-n.entered
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled drain returned success")
	}
	if st := q.Status(); st.Pending != 1 || st.InFlight != 0 {
		t.Fatalf("canceled batch not retryable: %+v", st)
	}
}

func TestQueueReconcileRearmsAfterDelivered(t *testing.T) {
	q := NewQueue(10, 0)
	alert := Alert{Rule: "r", Severity: "warning", Message: "m"}
	q.Enqueue([]Alert{alert})
	if _, err := q.Drain(context.Background(), &RecordingNotifier{}); err != nil {
		t.Fatal(err)
	}
	// Still firing: no duplicate.
	q.Reconcile([]Alert{alert})
	if st := q.Status(); st.Pending != 0 || st.Deduped == 0 {
		t.Fatalf("still-firing delivered alert duplicated: %+v", st)
	}
	// Recovered then fires again: must re-arm.
	q.Reconcile(nil)
	q.Reconcile([]Alert{alert})
	if st := q.Status(); st.Pending != 1 {
		t.Fatalf("recovered alert not re-armed: %+v", st)
	}
}

func TestQueueRepeatedResolveIsIdempotent(t *testing.T) {
	q := NewQueue(10, 0)
	q.Enqueue([]Alert{{Rule: "r", Severity: "warning", Message: "m"}})
	if _, ok := q.Resolve("r"); !ok {
		t.Fatal("first resolve")
	}
	if _, ok := q.Resolve("r"); ok {
		t.Fatal("second resolve reported active")
	}
}

func TestQueueControlledConcurrentChurn(t *testing.T) {
	q := NewQueue(32, 0)
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			alert := Alert{Rule: "r", Severity: "warning", Message: "m"}
			q.Enqueue([]Alert{alert})
			if i%3 == 0 {
				q.Resolve("r")
			}
			if i%7 == 0 {
				_, _ = q.Drain(context.Background(), &RecordingNotifier{})
			}
			_ = q.Status()
		}(i)
	}
	wg.Wait()
	if st := q.Status(); st.Pending > 32 || st.InFlight > 32 {
		t.Fatalf("queue exceeded bound: %+v", st)
	}
}
