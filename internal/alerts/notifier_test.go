package alerts

import (
	"context"
	"errors"
	"testing"

	"github.com/baicie/account-onboarding/internal/storage"
)

func TestRecordingNotifierOnlyDeliversEvaluatedAlerts(t *testing.T) {
	in := AlertInput{Ready: false, Metrics: storage.OperationalMetrics{SchemaVersion: 4}, ExpectedSchema: 5, NowMS: 1000}
	evaluated := EvaluateAlerts(in)
	n := &RecordingNotifier{}
	if err := n.Notify(context.Background(), evaluated); err != nil {
		t.Fatal(err)
	}
	if len(n.Delivered()) != len(evaluated) || len(evaluated) == 0 {
		t.Fatalf("delivered=%d evaluated=%d", len(n.Delivered()), len(evaluated))
	}
	// Ensure the fake cannot mutate internal state through the returned slice.
	got := n.Delivered()
	got[0].Rule = "mutated"
	if n.Delivered()[0].Rule == "mutated" {
		t.Fatal("Delivered returned aliased storage")
	}
}

func TestRecordingNotifierPropagatesFailureForCaller(t *testing.T) {
	n := &RecordingNotifier{err: errors.New("local sink unavailable")}
	if err := n.Notify(context.Background(), []Alert{{Rule: "readyz_failing", Severity: "critical"}}); err == nil {
		t.Fatal("notification failure was swallowed")
	}
}
