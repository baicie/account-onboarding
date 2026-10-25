package storage

import (
	"context"
	"database/sql"
)

// OperationalMetrics is a bounded, secret-free snapshot for operators. Labels
// are fixed status/error-class enums only; email addresses, tokens, JWTs,
// OAuth state and passwords must never appear here.
type OperationalMetrics struct {
	RunsByStatus             map[string]int64 `json:"runs_by_status"`
	ActiveRuns               int64            `json:"active_runs"`
	RetryWaitRuns            int64            `json:"retry_wait_runs"`
	FailedTerminalRuns       int64            `json:"failed_terminal_runs"`
	ExternalStatusCounts     map[string]int64 `json:"external_status_counts"`
	OldestExternalNeedsAgeMS int64            `json:"oldest_needs_reconciliation_age_ms"`
	ProviderErrorClasses     map[string]int64 `json:"provider_error_classes"`
	EngineErrorClasses       map[string]int64 `json:"engine_error_classes"`
	ExternalUnknownRatio     *float64         `json:"external_unknown_ratio"`
	ExternalInFlight         int64            `json:"external_in_flight_count"`
	ExternalInFlightOverdue  int64            `json:"external_in_flight_overdue_count"`
	SQLiteBusyTimeoutMS      int64            `json:"sqlite_busy_timeout_ms"`
	SQLiteBusyInstrumented   bool             `json:"sqlite_busy_retry_instrumented"`
	SessionRevocations       int64            `json:"session_revocations"`
	SchemaVersion            int              `json:"schema_version"`
	LastVerifiedBackupMS     *int64           `json:"last_verified_backup_at_ms"`
	SQLiteBusyRetryCount     *int64           `json:"sqlite_busy_retry_count"` // nil = not instrumented
}

// OperationalMetrics queries bounded aggregate counts only (no per-run rows and
// no unbounded memory aggregation).
func (s *Store) OperationalMetrics(ctx context.Context) (OperationalMetrics, error) {
	m := OperationalMetrics{
		RunsByStatus:           map[string]int64{},
		ExternalStatusCounts:   map[string]int64{},
		ProviderErrorClasses:   map[string]int64{},
		EngineErrorClasses:     map[string]int64{},
		SQLiteBusyTimeoutMS:    5000,
		SQLiteBusyInstrumented: false, // no retry counter is exposed; never report a fake zero
	}
	if err := s.readDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&m.SchemaVersion); err != nil {
		return m, err
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT status, COUNT(*) FROM runs GROUP BY status`)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return m, err
		}
		m.RunsByStatus[status] = n
	}
	rows.Close()
	m.ActiveRuns = m.RunsByStatus["CREATED"] + m.RunsByStatus["VALIDATING"] +
		m.RunsByStatus["PREPARING_MAILBOX"] + m.RunsByStatus["WAITING_EMAIL"] +
		m.RunsByStatus["VERIFYING"] + m.RunsByStatus["FINALIZING"] +
		m.RunsByStatus["RETRY_WAIT"] + m.RunsByStatus["WAITING_USER_ACTION"] +
		m.RunsByStatus["REAUTHORIZE_REQUIRED"] + m.RunsByStatus["CANCEL_REQUESTED"]
	m.RetryWaitRuns = m.RunsByStatus["RETRY_WAIT"]
	m.FailedTerminalRuns = m.RunsByStatus["FAILED_TERMINAL"]

	rows, err = s.readDB.QueryContext(ctx, `SELECT status, COUNT(*) FROM external_operations GROUP BY status`)
	if err != nil {
		return m, err
	}
	var total, unknown int64
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return m, err
		}
		m.ExternalStatusCounts[status] = n
		total += n
		if status == ExternalOutcomeUnknown || status == ExternalReconciliationReq || status == ExternalInFlight || status == ExternalPrepared {
			unknown += n
		}
	}
	rows.Close()
	if total > 0 {
		ratio := float64(unknown) / float64(total)
		m.ExternalUnknownRatio = &ratio
	}
	m.OldestExternalNeedsAgeMS, err = s.AgeOfOldestNeedsReconciliation(ctx, s.nowMS())
	if err != nil {
		return m, err
	}
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_operations WHERE status=?`, ExternalInFlight).Scan(&m.ExternalInFlight); err != nil {
		return m, err
	}
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_operations
		WHERE status IN (?,?) AND created_at_ms < ?`, ExternalPrepared, ExternalInFlight,
		s.nowMS()-5*60*1000).Scan(&m.ExternalInFlightOverdue); err != nil {
		return m, err
	}

	rows, err = s.readDB.QueryContext(ctx, `SELECT error_class, COUNT(*) FROM steps
		WHERE error_class IS NOT NULL AND error_class <> '' GROUP BY error_class`)
	if err != nil {
		return m, err
	}
	providerClasses := map[string]bool{
		"UPSTREAM_TRANSIENT": true, "RATE_LIMITED": true, "TIMEOUT_UNCERTAIN": true,
		"CONFLICT": true, "AUTH_REQUIRED": true, "AUTH_EXPIRED": true,
		"NOT_FOUND": true, "PROVIDER_BUG": true, "NOT_AUTHORIZED": true,
	}
	for rows.Next() {
		var class string
		var n int64
		if err := rows.Scan(&class, &n); err != nil {
			rows.Close()
			return m, err
		}
		if isProvider, ok := providerClasses[class]; ok && isProvider {
			m.ProviderErrorClasses[class] = n
		} else {
			m.EngineErrorClasses[class] = n
		}
	}
	rows.Close()

	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_revocations`).Scan(&m.SessionRevocations); err != nil {
		return m, err
	}
	var last sql.NullInt64
	err = s.readDB.QueryRowContext(ctx, `SELECT MAX(created_at_ms) FROM audit_log
		WHERE action LIKE 'backup%' OR action LIKE 'restore%'`).Scan(&last)
	if err == nil && last.Valid {
		m.LastVerifiedBackupMS = &last.Int64
	}
	return m, nil
}

// TotalNeedsReconciliation is the sum of pending external-operation states.
func (m OperationalMetrics) TotalNeedsReconciliation() int64 {
	return m.ExternalStatusCounts[ExternalPrepared] + m.ExternalStatusCounts[ExternalInFlight] +
		m.ExternalStatusCounts[ExternalOutcomeUnknown] + m.ExternalStatusCounts[ExternalReconciliationReq]
}
