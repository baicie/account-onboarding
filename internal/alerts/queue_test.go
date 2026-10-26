package alerts

import (
	"context"
	"errors"
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
