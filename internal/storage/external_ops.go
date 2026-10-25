package storage

import (
	"context"
	"encoding/json"
	"fmt"
)

// ExternalOperationEventPrefix marks audit events for remote side effects. We
// deliberately reuse the durable event log instead of adding a second mutable
// state table: events are append-only, transactional with the run, already
// redacted, and retained with the run. The latest event per operation_key is
// the current reconciliation status (see docs/adr/0001).
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

// ListExternalOperations returns the latest event per operation key, newest
// first. It is bounded (default 200) and never returns secret material.
func (s *Store) ListExternalOperations(ctx context.Context, limit int) ([]ExternalOperation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT run_id, COALESCE(step_name,''), event_type,
		public_payload_json, created_at_ms FROM events
		WHERE event_type LIKE 'EXTERNAL_OPERATION_%' ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
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
		if op.OperationKey == "" {
			op.OperationKey = fmt.Sprintf("%s:%s", op.RunID, op.StepName)
		}
		if seen[op.OperationKey] {
			continue
		}
		seen[op.OperationKey] = true
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
