package storage

import (
	"context"
	"encoding/json"
)

// ExternalOperationEventPrefix marks audit events for remote side effects. We
// deliberately reuse the durable event log instead of adding a second mutable
// state table: events are append-only, transactional with the run, already
// redacted, and retained with the run. The latest event per operation_key is
// the current status; any OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED in the history
// keeps needs_reconciliation sticky until an operator resolves it (a later
// deterministic FAILED must not erase the possibility that the remote write
// landed). See docs/adr/0001.
const ExternalOperationEventPrefix = "EXTERNAL_OPERATION_"

// ExternalOperation is a read-only projection used by diagnostics/reconciliation.
type ExternalOperation struct {
	RunID         string `json:"run_id"`
	StepName      string `json:"step_name"`
	OperationKey  string `json:"operation_key"`
	Provider      string `json:"provider"`
	OperationType string `json:"operation_type"`
	Fingerprint   string `json:"request_fingerprint"`
	Status        string `json:"status"`
	ErrorClass    string `json:"error_class,omitempty"`
	EventType     string `json:"event_type"`
	CreatedAtMS   int64  `json:"created_at_ms"`
	NeedsReview   bool   `json:"needs_reconciliation"`
	ReviewReason  string `json:"review_reason,omitempty"`
}

// ListExternalOperations returns the latest event per run/step (operation key),
// newest first, bounded by limit. Sticky review flags are computed from the full
// per-operation history; the query no longer applies a global LIMIT before
// deduplicating, so a pending operation cannot be silently dropped in favor of a
// newer unrelated run.
func (s *Store) ListExternalOperations(ctx context.Context, limit int) ([]ExternalOperation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT e.run_id, COALESCE(e.step_name,''),
		e.event_type, e.public_payload_json, e.created_at_ms
		FROM events e
		WHERE e.event_type LIKE 'EXTERNAL_OPERATION_%'
		  AND e.id = (
			SELECT MAX(x.id) FROM events x
			WHERE x.run_id = e.run_id
			  AND COALESCE(x.step_name,'') = COALESCE(e.step_name,'')
			  AND x.event_type LIKE 'EXTERNAL_OPERATION_%'
		  )
		ORDER BY e.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// sticky[run|step] is true if any event in the operation history was
	// ambiguous. This is history, not the latest event, so a later deterministic
	// failure cannot clear the reconciliation requirement.
	stickyRows, err := s.readDB.QueryContext(ctx, `SELECT run_id, COALESCE(step_name,''),
		MAX(CASE WHEN event_type IN ('EXTERNAL_OPERATION_OUTCOME_UNKNOWN',
		                             'EXTERNAL_OPERATION_RECONCILIATION_REQUIRED')
		         THEN 1 ELSE 0 END)
		FROM events WHERE event_type LIKE 'EXTERNAL_OPERATION_%'
		GROUP BY run_id, COALESCE(step_name,'')`)
	if err != nil {
		return nil, err
	}
	sticky := map[string]bool{}
	for stickyRows.Next() {
		var runID, step string
		var flag int
		if err := stickyRows.Scan(&runID, &step, &flag); err != nil {
			stickyRows.Close()
			return nil, err
		}
		if flag == 1 {
			sticky[runID+"\x00"+step] = true
		}
	}
	if err := stickyRows.Err(); err != nil {
		stickyRows.Close()
		return nil, err
	}
	stickyRows.Close()

	var out []ExternalOperation
	for rows.Next() {
		var op ExternalOperation
		var raw string
		if err := rows.Scan(&op.RunID, &op.StepName, &op.EventType, &raw, &op.CreatedAtMS); err != nil {
			return nil, err
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			continue
		}
		op.OperationKey = stringField(payload, "operation_key")
		op.Provider = stringField(payload, "provider")
		op.OperationType = stringField(payload, "operation_type")
		op.Fingerprint = stringField(payload, "request_fingerprint")
		op.Status = stringField(payload, "status")
		op.ErrorClass = stringField(payload, "error_class")
		switch op.Status {
		case "PREPARED", "OUTCOME_UNKNOWN", "RECONCILIATION_REQUIRED":
			op.NeedsReview = true
			op.ReviewReason = op.Status
		}
		// A CONFIRMED step commit resolves the operation; an ambiguous attempt
		// before it is no longer actionable. Otherwise the review requirement is
		// sticky across retries/failures.
		if op.Status != "CONFIRMED" && sticky[op.RunID+"\x00"+op.StepName] {
			op.NeedsReview = true
			if op.ReviewReason == "" {
				op.ReviewReason = "history_outcome_unknown"
			}
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
