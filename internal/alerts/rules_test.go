package alerts

import (
	"testing"

	"github.com/baicie/account-onboarding/internal/storage"
)

func TestEvaluateAlertsActionableRules(t *testing.T) {
	backup := int64(1000)
	m := storage.OperationalMetrics{
		SchemaVersion: 4, ExternalInFlightOverdue: 1,
		ExternalStatusCounts: map[string]int64{storage.ExternalReconciliationReq: 12},
		ProviderErrorClasses: map[string]int64{"UPSTREAM_TRANSIENT": 25},
		LastVerifiedBackupMS: &backup,
	}
	got := EvaluateAlerts(AlertInput{
		Metrics: m, Ready: false, ExpectedSchema: 6, NowMS: 10 * 24 * 3600 * 1000,
		BackupMaxAgeMS: (7 * 24 * 3600 * 1000), ReconciliationThreshold: 10,
		ProviderErrorThreshold: 20,
	})
	rules := map[string]bool{}
	for _, a := range got {
		rules[a.Rule] = true
	}
	for _, want := range []string{"readyz_failing", "schema_mismatch", "external_in_flight_overdue",
		"reconciliation_queue_high", "backup_verification_stale", "provider_errors_high"} {
		if !rules[want] {
			t.Fatalf("missing alert %s in %+v", want, got)
		}
	}
}

func TestEvaluateAlertsReportsMissingBackupOnlyWhenWindowConfigured(t *testing.T) {
	got := EvaluateAlerts(AlertInput{Metrics: storage.OperationalMetrics{SchemaVersion: 5}, Ready: true,
		ExpectedSchema: 6, BackupMaxAgeMS: 1000, NowMS: 2000})
	found := false
	for _, a := range got {
		if a.Rule == "backup_verification_missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing backup alert with configured window: %+v", got)
	}
	// No backup window => no backup alert.
	none := EvaluateAlerts(AlertInput{Metrics: storage.OperationalMetrics{SchemaVersion: 5}, Ready: true, ExpectedSchema: 6})
	for _, a := range none {
		if a.Rule == "backup_verification_missing" || a.Rule == "backup_verification_stale" {
			t.Fatalf("unexpected backup alert without window: %+v", none)
		}
	}
}
