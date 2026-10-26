package alerts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var ErrRateLimited = errors.New("alerts: notification retry rate-limited")
var ErrDrainInProgress = errors.New("alerts: drain already in progress")

type QueueStatus struct {
	Active         int       `json:"active"`
	Pending        int       `json:"pending"`
	Deduped        uint64    `json:"deduped"`
	Dropped        uint64    `json:"dropped"`
	Sent           uint64    `json:"sent"`
	LastError      string    `json:"last_error,omitempty"`
	LastAttemptUTC string    `json:"last_attempt_utc,omitempty"`
	UpdatedUTC     time.Time `json:"updated_utc"`
}

// Queue is a bounded, deduplicating local notification queue. It never performs
// network I/O itself; a Notifier adapter is supplied by the caller. Alerts are
// considered active until Resolve, so the same firing condition is not sent
// repeatedly. On notification failure entries remain queued and retry is
// rate-limited.
type Queue struct {
	mu          sync.Mutex
	max         int
	minInterval time.Duration
	now         func() time.Time
	pending     []Alert
	active      map[string]bool
	deduped     uint64
	dropped     uint64
	sent        uint64
	lastError   string
	lastAttempt time.Time
	draining    bool
}

func NewQueue(max int, minInterval time.Duration) *Queue {
	if max < 1 {
		max = 1
	}
	if minInterval < 0 {
		minInterval = 0
	}
	return &Queue{max: max, minInterval: minInterval, now: time.Now, active: map[string]bool{}}
}

func alertKey(a Alert) string { return a.Rule + "|" + a.Severity + "|" + a.Message }

// Reconcile makes the queue exactly reflect the current evaluated rules:
// recovered conditions are cleared and re-armed conditions enqueue again. It is
// the production control path for alert recovery.
func (q *Queue) Reconcile(items []Alert) {
	q.mu.Lock()
	defer q.mu.Unlock()
	current := make(map[string]bool, len(items))
	for _, a := range items {
		current[alertKey(a)] = true
	}
	kept := q.pending[:0]
	for _, a := range q.pending {
		if current[alertKey(a)] {
			kept = append(kept, a)
		}
	}
	q.pending = kept
	for key := range q.active {
		if !current[key] {
			delete(q.active, key)
		}
	}
	q.enqueueLocked(items)
}

// Enqueue adds only currently-inactive alerts; duplicates are suppressed until
// Resolve clears the rule. When full, the oldest pending entry is dropped and
// counted; this keeps memory bounded under alert storms.
func (q *Queue) Enqueue(items []Alert) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueueLocked(items)
}

func (q *Queue) enqueueLocked(items []Alert) {
	for _, a := range items {
		key := alertKey(a)
		if q.active[key] {
			q.deduped++
			continue
		}
		if len(q.pending) >= q.max {
			dropped := q.pending[0]
			q.pending = q.pending[1:]
			delete(q.active, alertKey(dropped))
			q.dropped++
		}
		q.pending = append(q.pending, a)
		q.active[key] = true
	}
}

// Resolve clears a currently firing rule and returns a recovery alert for the
// caller to notify (if desired). Repeated Resolve calls are idempotent.
func (q *Queue) Resolve(rule string) (Alert, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	found := false
	kept := q.pending[:0]
	for _, a := range q.pending {
		if strings.HasPrefix(alertKey(a), rule+"|") {
			found = true
			continue
		}
		kept = append(kept, a)
	}
	q.pending = kept
	for key := range q.active {
		if strings.HasPrefix(key, rule+"|") {
			delete(q.active, key)
			found = true
		}
	}
	if !found {
		return Alert{}, false
	}
	return Alert{Rule: rule + "_recovered", Severity: "info", Message: "alert condition cleared"}, true
}

// Drain sends all pending entries to the notifier. On failure entries remain
// queued; callers may retry after the rate-limit interval. Active conditions
// are not re-sent until Resolve.
func (q *Queue) Drain(ctx context.Context, n Notifier) (int, error) {
	q.mu.Lock()
	if q.draining {
		q.mu.Unlock()
		return 0, ErrDrainInProgress
	}
	if q.now().Sub(q.lastAttempt) < q.minInterval {
		q.mu.Unlock()
		return 0, ErrRateLimited
	}
	if len(q.pending) == 0 {
		q.mu.Unlock()
		return 0, nil
	}
	q.draining = true
	batch := append([]Alert(nil), q.pending...)
	q.lastAttempt = q.now()
	q.mu.Unlock()
	if err := n.Notify(ctx, batch); err != nil {
		q.mu.Lock()
		q.lastError = err.Error()
		q.lastAttempt = q.now()
		q.draining = false
		q.mu.Unlock()
		return 0, fmt.Errorf("alerts: notify: %w", err)
	}
	q.mu.Lock()
	q.draining = false
	q.pending = q.pending[len(batch):]
	q.sent += uint64(len(batch))
	q.lastError = ""
	q.mu.Unlock()
	return len(batch), nil
}

func (q *Queue) Status() QueueStatus {
	q.mu.Lock()
	defer q.mu.Unlock()
	st := QueueStatus{Active: len(q.active), Pending: len(q.pending), Deduped: q.deduped,
		Dropped: q.dropped, Sent: q.sent, LastError: q.lastError, UpdatedUTC: q.now().UTC()}
	if !q.lastAttempt.IsZero() {
		st.LastAttemptUTC = q.lastAttempt.UTC().Format(time.RFC3339Nano)
	}
	return st
}
