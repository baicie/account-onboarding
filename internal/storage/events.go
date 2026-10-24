package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
)

// Event is an append-only, redacted workflow event.
type Event struct {
	Seq         int64          `json:"seq"`
	RunID       string         `json:"run_id"`
	Type        string         `json:"type"`
	Step        string         `json:"step,omitempty"`
	Payload     map[string]any `json:"payload"`
	CreatedAtMS int64          `json:"-"`
}

// CreatedAt returns the event time in UTC.
func (e Event) CreatedAt() time.Time { return time.UnixMilli(e.CreatedAtMS).UTC() }

func (s *Store) newID() string { return security.NewID() }

// appendEventTx appends one event and returns its per-run sequence number.
// Payload keys are filtered through the redactor allowlist.
func (s *Store) appendEventTx(ctx context.Context, tx *sql.Tx, runID, eventType, step string, payload map[string]any, now int64) (int64, error) {
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(event_seq),0)+1 FROM events WHERE run_id=?`, runID).Scan(&seq); err != nil {
		return 0, err
	}
	safe := s.redactor.SanitizeEventPayload(payload)
	raw, err := json.Marshal(safe)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (run_id, event_seq, event_type, step_name, public_payload_json, created_at_ms)
		VALUES (?,?,?,?,?,?)`, runID, seq, eventType, nullableString(step), string(raw), now); err != nil {
		return 0, err
	}
	return seq, nil
}

// AppendEvent appends an event outside a step commit (cancel/resume).
func (s *Store) AppendEvent(ctx context.Context, runID, eventType, step string, payload map[string]any) error {
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := s.appendEventTx(ctx, tx, runID, eventType, step, payload, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ListEvents returns redacted events after a sequence number.
func (s *Store) ListEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT event_seq, run_id, event_type,
		COALESCE(step_name,''), public_payload_json, created_at_ms
		FROM events WHERE run_id=? AND event_seq > ? ORDER BY event_seq LIMIT ?`, runID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var (
			e    Event
			raw  string
			step string
		)
		if err := rows.Scan(&e.Seq, &e.RunID, &e.Type, &step, &raw, &e.CreatedAtMS); err != nil {
			return nil, err
		}
		e.Step = step
		e.Payload = map[string]any{}
		if err := json.Unmarshal([]byte(raw), &e.Payload); err != nil {
			return nil, fmt.Errorf("decode event payload: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Resource is a persisted external resource link. ExternalID is never stored in
// clear text; only its HMAC digest is persisted.
type Resource struct {
	ID           string `json:"id"`
	RunID        string `json:"run_id"`
	Kind         string `json:"kind"`
	ExternalHMAC string `json:"-"`
	DisplayAlias string `json:"display_alias,omitempty"`
	SecretRef    string `json:"-"`
	ExtraJSON    string `json:"-"`
	CreatedAtMS  int64  `json:"-"`
}

// ListResources returns resources for a run, optionally filtered by kind.
func (s *Store) ListResources(ctx context.Context, runID, kind string) ([]Resource, error) {
	query := `SELECT id, run_id, resource_kind, external_id_hmac, COALESCE(display_alias,''),
		COALESCE(secret_ref,''), COALESCE(extra_json,'{}'), created_at_ms
		FROM resources WHERE run_id=?`
	args := []any{runID}
	if kind != "" {
		query += " AND resource_kind=?"
		args = append(args, kind)
	}
	query += " ORDER BY created_at_ms, id"
	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Resource
	for rows.Next() {
		var r Resource
		if err := rows.Scan(&r.ID, &r.RunID, &r.Kind, &r.ExternalHMAC, &r.DisplayAlias,
			&r.SecretRef, &r.ExtraJSON, &r.CreatedAtMS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestResource returns the most recent resource of a kind.
func (s *Store) LatestResource(ctx context.Context, runID, kind string) (Resource, error) {
	list, err := s.ListResources(ctx, runID, kind)
	if err != nil {
		return Resource{}, err
	}
	if len(list) == 0 {
		return Resource{}, ErrNotFound
	}
	return list[len(list)-1], nil
}

// ConsumeMessage records that a provider message has been consumed exactly once
// for a run. It returns true when this call is the first consumer.
func (s *Store) ConsumeMessage(ctx context.Context, runID, providerName, messageKey string) (bool, error) {
	now := s.nowMS()
	res, err := s.writeDB.ExecContext(ctx, `INSERT OR IGNORE INTO mail_receipts
		(run_id, provider, message_key_hmac, consumed_at_ms) VALUES (?,?,?,?)`,
		runID, providerName, s.messageHMAC(providerName, messageKey), now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// AuditEntry is one operator audit record.
type AuditEntry struct {
	ID          int64          `json:"id"`
	Actor       string         `json:"actor"`
	Action      string         `json:"action"`
	RunID       string         `json:"run_id,omitempty"`
	Detail      map[string]any `json:"detail"`
	CreatedAtMS int64          `json:"-"`
}

// Audit appends an operator audit record.
func (s *Store) Audit(ctx context.Context, actor, action, runID string, detail map[string]any) error {
	safe := s.redactor.SanitizeEventPayload(detail)
	raw, err := json.Marshal(safe)
	if err != nil {
		return err
	}
	_, err = s.writeDB.ExecContext(ctx, `INSERT INTO audit_log (actor, action, run_id, detail_json, created_at_ms)
		VALUES (?,?,?,?,?)`, actor, action, nullableString(runID), string(raw), s.nowMS())
	return err
}

// ListAudit returns the most recent audit entries.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT id, actor, action, COALESCE(run_id,''),
		detail_json, created_at_ms FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var (
			e   AuditEntry
			raw string
		)
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.RunID, &raw, &e.CreatedAtMS); err != nil {
			return nil, err
		}
		e.Detail = map[string]any{}
		_ = json.Unmarshal([]byte(raw), &e.Detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SecretRecord is an encrypted secret row.
type SecretRecord struct {
	ID          string
	Kind        string
	Ciphertext  []byte
	Nonce       []byte
	KeyVersion  int
	CreatedAtMS int64
	UpdatedAtMS int64
}

// PutSecret upserts an encrypted secret.
func (s *Store) PutSecret(ctx context.Context, id, kind string, ciphertext, nonce []byte, keyVersion int) error {
	now := s.nowMS()
	_, err := s.writeDB.ExecContext(ctx, `INSERT INTO secrets (id, secret_kind, ciphertext, nonce, key_version, created_at_ms, updated_at_ms)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET secret_kind=excluded.secret_kind, ciphertext=excluded.ciphertext,
		nonce=excluded.nonce, key_version=excluded.key_version, updated_at_ms=excluded.updated_at_ms`,
		id, kind, ciphertext, nonce, keyVersion, now, now)
	return err
}

// GetSecret loads an encrypted secret. It returns ErrNotFound when absent.
func (s *Store) GetSecret(ctx context.Context, id string) (SecretRecord, error) {
	var rec SecretRecord
	err := s.readDB.QueryRowContext(ctx, `SELECT id, secret_kind, ciphertext, nonce, key_version,
		created_at_ms, updated_at_ms FROM secrets WHERE id=?`, id).
		Scan(&rec.ID, &rec.Kind, &rec.Ciphertext, &rec.Nonce, &rec.KeyVersion, &rec.CreatedAtMS, &rec.UpdatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return SecretRecord{}, ErrNotFound
	}
	return rec, err
}

// DeleteSecret removes a secret row.
func (s *Store) DeleteSecret(ctx context.Context, id string) error {
	_, err := s.writeDB.ExecContext(ctx, `DELETE FROM secrets WHERE id=?`, id)
	return err
}

// CountSecrets reports the number of stored secrets (readiness/debug only).
func (s *Store) CountSecrets(ctx context.Context) (int64, error) {
	var n int64
	err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets`).Scan(&n)
	return n, err
}

// LatestStep loads one step row by name.
func (s *Store) LatestStep(ctx context.Context, runID, name string) (Step, error) {
	var (
		st                        Step
		nextRetry, started, finis sql.NullInt64
	)
	err := s.readDB.QueryRowContext(ctx, `SELECT id, run_id, step_name, status, operation_key,
		attempt_count, COALESCE(error_class,''), COALESCE(error_summary,''), next_retry_at_ms,
		started_at_ms, finished_at_ms, updated_at_ms, COALESCE(result_json,'{}'), COALESCE(waiting_reason,''), failure_count
		FROM steps WHERE run_id=? AND step_name=?`, runID, name).
		Scan(&st.ID, &st.RunID, &st.Name, &st.Status, &st.OperationKey, &st.AttemptCount,
			&st.ErrorClass, &st.ErrorSummary, &nextRetry, &started, &finis, &st.UpdatedAtMS,
			&st.ResultJSON, &st.WaitingReason, &st.FailureCount)
	if errors.Is(err, sql.ErrNoRows) {
		return Step{}, ErrNotFound
	}
	if err != nil {
		return Step{}, err
	}
	st.NextRetryAtMS = ptrMS(nextRetry)
	st.StartedAtMS = ptrMS(started)
	st.FinishedAtMS = ptrMS(finis)
	return st, nil
}

// CountByStatus returns run counts grouped by status.
func (s *Store) CountByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT status, COUNT(*) FROM runs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// MessageConsumed reports whether a provider message was already consumed by
// this run. It is used to pre-filter mailbox listings before content matching.
func (s *Store) MessageConsumed(ctx context.Context, runID, providerName, messageKey string) (bool, error) {
	var n int
	err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_receipts
		WHERE run_id=? AND provider=? AND message_key_hmac=?`,
		runID, providerName, s.messageHMAC(providerName, messageKey)).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
