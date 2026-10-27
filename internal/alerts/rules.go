package alerts

import (
	"fmt"
	"time"

	"github.com/baicie/account-onboarding/internal/storage"
)

// Alert is a local, actionable operational rule result. The service does not
// send email/IM/webhook notifications; callers may feed these to their own
// approved notification channel.
type Alert struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"` // warning | critical
	Message  string `json:"message"`
	Observed any    `json:"observed,omitempty"`
}

// AlertInput is the bounded set of facts available locally.
type AlertInput struct {
	Metrics                 storage.OperationalMetrics
	Ready                   bool
	ExpectedSchema          int
	NowMS                   int64
	BackupMaxAgeMS          int64
	ReconciliationThreshold int64
	ProviderErrorThreshold  int64
}

// EvaluateAlerts returns deterministic local rules. A metric that is not
// instrumented (nil) never produces a fake alert; it is explicitly skipped.
func EvaluateAlerts(in AlertInput) []Alert {
	var out []Alert
	add := func(rule, sev, msg string, observed any) {
		out = append(out, Alert{Rule: rule, Severity: sev, Message: msg, Observed: observed})
	}
	if !in.Ready {
		add("readyz_failing", "critical", "/readyz is not ready; stop remote writes until resolved", nil)
	}
	if in.ExpectedSchema > 0 && in.Metrics.SchemaVersion != in.ExpectedSchema {
		add("schema_mismatch", "critical",
			fmt.Sprintf("schema %d != expected %d", in.Metrics.SchemaVersion, in.ExpectedSchema),
			in.Metrics.SchemaVersion)
	}
	if in.Metrics.ExternalInFlightOverdue > 0 {
		add("external_in_flight_overdue", "warning",
			"external operation has been IN_FLIGHT/PREPARED for over 5 minutes; do not retry, inspect the provider",
			in.Metrics.ExternalInFlightOverdue)
	}
	if in.ReconciliationThreshold > 0 && in.Metrics.TotalNeedsReconciliation() > in.ReconciliationThreshold {
		add("reconciliation_queue_high", "warning",
			"reconciliation queue exceeds threshold; stop automatic flows and review",
			in.Metrics.TotalNeedsReconciliation())
	}
	if in.BackupMaxAgeMS > 0 {
		if in.Metrics.LastVerifiedBackupMS == nil {
			add("backup_verification_missing", "warning", "no verified backup timestamp is recorded", nil)
		} else if in.NowMS-*in.Metrics.LastVerifiedBackupMS > in.BackupMaxAgeMS {
			age := time.Duration(in.NowMS-*in.Metrics.LastVerifiedBackupMS) * time.Millisecond
			add("backup_verification_stale", "warning",
				"last verified backup is older than the configured window", age.String())
		}
	}
	if in.ProviderErrorThreshold > 0 {
		var total int64
		for _, n := range in.Metrics.ProviderErrorClasses {
			total += n
		}
		if total > in.ProviderErrorThreshold {
			add("provider_errors_high", "warning",
				"provider error count exceeds threshold; inspect provider contracts/status",
				total)
		}
	}
	if in.Metrics.SessionRevocations < 0 {
		// defensive: negative is never possible, kept out of emitted output
	}
	return out
}
