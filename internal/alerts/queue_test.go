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
}

func (n *blockingNotifier) Notify(ctx context.Context, alerts []Alert) error {
	n.calls.Add(1)
	if n.entered == nil {
		n.entered = make(chan struct{})
	}
	n.once.Do(func() { close(n.entered) })
	select {
	case <-n.block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
