package alerts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// Fingerprint returns a stable, non-secret identity for deduplicating an active
// alert across restarts. It is not a credential or a delivery ID.
func Fingerprint(a Alert) string {
	h := sha256.Sum256([]byte(a.Rule + "\x00" + a.Severity + "\x00" + a.Message))
	return hex.EncodeToString(h[:])
}

// DeliverBatch invokes a notifier with panic containment. It is exported for
// the durable outbox dispatcher; a panic is a delivery failure, not a process
// crash or a stuck IN_FLIGHT batch.
func DeliverBatch(ctx context.Context, n Notifier, items []Alert) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("alerts: notifier panic: %v", r)
		}
	}()
	return n.Notify(ctx, items)
}
