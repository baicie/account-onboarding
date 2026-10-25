package alerts

import (
	"context"
	"sync"
)

// Notifier is the local notification adapter boundary. Production wiring may
// call an approved local webhook/agent, but this repository ships no real
// external notification integration and tests use RecordingNotifier only.
type Notifier interface {
	Notify(ctx context.Context, alerts []Alert) error
}

// RecordingNotifier is a test/fake sender. It records only the alerts passed to
// it; it never performs network I/O.
type RecordingNotifier struct {
	mu        sync.Mutex
	delivered []Alert
	err       error
}

func (n *RecordingNotifier) Notify(_ context.Context, alerts []Alert) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.delivered = append(n.delivered, alerts...)
	return n.err
}

func (n *RecordingNotifier) Delivered() []Alert {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Alert, len(n.delivered))
	copy(out, n.delivered)
	return out
}
