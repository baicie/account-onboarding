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
	Ephemeral      bool      `json:"ephemeral"`
	Active         int       `json:"active"`
	Pending        int       `json:"pending"`
	InFlight       int       `json:"in_flight"`
	Delivered      uint64    `json:"delivered"`
	Resolved       uint64    `json:"resolved"`
	Deduped        uint64    `json:"deduped"`
	Dropped        uint64    `json:"dropped"`
	Sent           uint64    `json:"sent"`
	LastError      string    `json:"last_error,omitempty"`
	LastAttemptUTC string    `json:"last_attempt_utc,omitempty"`
	UpdatedUTC     time.Time `json:"updated_utc"`
}

type entryState uint8

const (
	statePending entryState = iota
	stateInFlight
	stateRetryPending
)

type queueEntry struct {
	id       uint64
	alert    Alert
	state    entryState
	attempts int
}

type deliveryBatch struct {
	id  uint64
	ids []uint64
}

// Queue is a bounded, deduplicating local notification queue. It never performs
// network I/O itself; a Notifier adapter is supplied by the caller. Every entry
// has an ID and explicit PENDING/IN_FLIGHT/RETRY_PENDING lifecycle so a
// Resolve/Reconcile during a Notifier call cannot corrupt the queue or cause a
// stale delivery to be acknowledged. Delivered/resolved entries are removed;
// counters remain observable.
type Queue struct {
	mu          sync.Mutex
	max         int
	minInterval time.Duration
	now         func() time.Time
	nextID      uint64
	entries     map[uint64]*queueEntry
	order       []uint64 // pending/retry order; may contain stale ids
	active      map[string]uint64
	batch       *deliveryBatch
	batchSeq    uint64
	deduped     uint64
	dropped     uint64
	delivered   uint64
	resolved    uint64
	sent        uint64
	lastError   string
	lastAttempt time.Time
}

func NewQueue(max int, minInterval time.Duration) *Queue {
	if max < 1 {
		max = 1
	}
	if minInterval < 0 {
		minInterval = 0
	}
	return &Queue{max: max, minInterval: minInterval, now: time.Now,
		entries: map[uint64]*queueEntry{}, active: map[string]uint64{}}
}

func alertKey(a Alert) string { return a.Rule + "|" + a.Severity + "|" + a.Message }

// Reconcile makes the queue exactly reflect the latest evaluated rules. It is
// safe while Drain is in flight: removed entries are marked resolved and the
// in-flight completion will skip them; newly firing rules are enqueued for a
// later batch.
func (q *Queue) Reconcile(items []Alert) {
	q.mu.Lock()
	defer q.mu.Unlock()
	current := make(map[string]bool, len(items))
	for _, a := range items {
		current[alertKey(a)] = true
	}
	for key := range q.active {
		if !current[key] {
			delete(q.active, key)
			q.resolved++
		}
	}
	for id, e := range q.entries {
		if !current[alertKey(e.alert)] {
			delete(q.entries, id)
		}
	}
	q.compactOrderLocked()
	for _, a := range items {
		q.enqueueLocked(a)
	}
}

// Enqueue adds only currently-inactive alerts; duplicates are suppressed until
// Resolve/Reconcile clears the condition. When full, the oldest retry/pending
// entry is dropped and counted; an in-flight batch is never removed by a drop.
func (q *Queue) Enqueue(items []Alert) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, a := range items {
		q.enqueueLocked(a)
	}
}

func (q *Queue) enqueueLocked(a Alert) {
	key := alertKey(a)
	if _, ok := q.active[key]; ok {
		q.deduped++
		return
	}
	if len(q.entries) >= q.max {
		var victim uint64
		found := false
		for _, id := range q.order {
			e, ok := q.entries[id]
			if ok && e.state != stateInFlight {
				victim, found = id, true
				break
			}
		}
		if !found {
			q.dropped++
			return
		}
		victimEntry := q.entries[victim]
		delete(q.entries, victim)
		delete(q.active, alertKey(victimEntry.alert))
		q.dropped++
		q.compactOrderLocked()
	}
	q.nextID++
	id := q.nextID
	q.entries[id] = &queueEntry{id: id, alert: a, state: statePending}
	q.order = append(q.order, id)
	q.active[key] = id
}

// Resolve clears a rule from pending/in-flight/retry and returns a recovery
// notification. An in-flight notifier result for that rule will be ignored.
func (q *Queue) Resolve(rule string) (Alert, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	found := false
	for key := range q.active {
		if strings.HasPrefix(key, rule+"|") {
			delete(q.active, key)
			found = true
		}
	}
	for id, e := range q.entries {
		if e.alert.Rule == rule {
			delete(q.entries, id)
			found = true
			q.resolved++
		}
	}
	if found {
		q.compactOrderLocked()
		return Alert{Rule: rule + "_recovered", Severity: "info", Message: "alert condition cleared"}, true
	}
	return Alert{}, false
}

func (q *Queue) compactOrderLocked() {
	out := q.order[:0]
	for _, id := range q.order {
		if _, ok := q.entries[id]; ok {
			out = append(out, id)
		}
	}
	q.order = out
}

// Drain moves up to all pending entries to IN_FLIGHT under a stable batch ID,
// releases the lock while Notify runs, then acknowledges only entries that are
// still present. Success removes only this batch; failure returns entries that
// still exist to RETRY_PENDING for a rate-limited retry. Context cancellation
// is treated as a send failure and leaves the batch retryable.
func (q *Queue) Drain(ctx context.Context, n Notifier) (int, error) {
	q.mu.Lock()
	if q.batch != nil {
		q.mu.Unlock()
		return 0, ErrDrainInProgress
	}
	if q.now().Sub(q.lastAttempt) < q.minInterval {
		q.mu.Unlock()
		return 0, ErrRateLimited
	}
	q.compactOrderLocked()
	ids := make([]uint64, 0, len(q.order))
	for _, id := range q.order {
		if e, ok := q.entries[id]; ok && e.state != stateInFlight {
			e.state = stateInFlight
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		q.mu.Unlock()
		return 0, nil
	}
	q.batchSeq++
	batch := &deliveryBatch{id: q.batchSeq, ids: ids}
	q.batch = batch
	q.order = nil
	q.lastAttempt = q.now()
	alerts := make([]Alert, 0, len(ids))
	for _, id := range ids {
		alerts = append(alerts, q.entries[id].alert)
	}
	q.mu.Unlock()

	err := safeNotify(ctx, n, alerts)

	q.mu.Lock()
	defer q.mu.Unlock()
	q.batch = nil
	q.lastAttempt = q.now()
	sent := 0
	if err != nil {
		q.lastError = err.Error()
		for _, id := range ids {
			e, ok := q.entries[id]
			if !ok {
				continue // resolved while the notifier was running
			}
			e.state = stateRetryPending
			e.attempts++
			if e.attempts >= 10 {
				delete(q.entries, id)
				delete(q.active, alertKey(e.alert))
				q.dropped++
				continue
			}
			q.order = append(q.order, id)
		}
		return 0, fmt.Errorf("alerts: notify: %w", err)
	}
	q.lastError = ""
	for _, id := range ids {
		if _, ok := q.entries[id]; !ok {
			continue // resolved/reconciled during delivery: do not resurrect
		}
		delete(q.entries, id)
		// Keep the rule active after successful delivery: a still-firing
		// condition must not be re-sent every drain. Reconcile/Resolve clears it
		// when the condition disappears (or is explicitly acknowledged).
		q.delivered++
		q.sent++
		sent++
	}
	return sent, nil
}

func safeNotify(ctx context.Context, n Notifier, alerts []Alert) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("alerts: notifier panic: %v", r)
		}
	}()
	return n.Notify(ctx, alerts)
}

func (q *Queue) Status() QueueStatus {
	q.mu.Lock()
	defer q.mu.Unlock()
	st := QueueStatus{Ephemeral: true, Active: len(q.active), Delivered: q.delivered, Resolved: q.resolved,
		Deduped: q.deduped, Dropped: q.dropped, Sent: q.sent, LastError: q.lastError,
		UpdatedUTC: q.now().UTC()}
	for _, e := range q.entries {
		switch e.state {
		case stateInFlight:
			st.InFlight++
		default:
			st.Pending++
		}
	}
	if !q.lastAttempt.IsZero() {
		st.LastAttemptUTC = q.lastAttempt.UTC().Format(time.RFC3339Nano)
	}
	return st
}
