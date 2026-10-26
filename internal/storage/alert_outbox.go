package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

const (
	AlertOutboxPending      = "PENDING"
	AlertOutboxInFlight     = "IN_FLIGHT"
	AlertOutboxRetryPending = "RETRY_PENDING"
	AlertOutboxDelivered    = "DELIVERED"
	AlertOutboxResolved     = "RESOLVED"
	AlertOutboxDeadLetter   = "DEAD_LETTER"
)

type AlertOutboxInput struct {
	Fingerprint string
	Rule        string
	Severity    string
	Message     string
}

type AlertOutboxItem struct {
	ID             string
	Fingerprint    string
	Rule           string
	Severity       string
	Message        string
	Status         string
	Attempts       int
	NextAttemptMS  int64
	LeaseOwner     string
	LeaseExpiresMS int64
	LastError      string
	DeliveryID     string
	CreatedAtMS    int64
	UpdatedAtMS    int64
	DeliveredAtMS  *int64
}

type AlertDelivery struct {
	ID           string
	AlertID      string
	Attempt      int
	Status       string
	LeaseOwner   string
	StartedAtMS  int64
	FinishedAtMS *int64
	HTTPStatus   int
	Error        string
}

type AlertOutboxStatus struct {
	Pending      int64 `json:"pending"`
	InFlight     int64 `json:"in_flight"`
	RetryPending int64 `json:"retry_pending"`
	Delivered    int64 `json:"delivered"`
	Resolved     int64 `json:"resolved"`
	DeadLetter   int64 `json:"dead_letter"`
}

var alertOutboxSeq atomic.Uint64

func newAlertOutboxID(now time.Time) string {
	return fmt.Sprintf("al-%d-%d", now.UnixNano(), alertOutboxSeq.Add(1))
}
func newAlertDeliveryID(now time.Time) string {
	return fmt.Sprintf("ad-%d-%d", now.UnixNano(), alertOutboxSeq.Add(1))
}

// EnqueueAlertOutbox inserts only fingerprints that are not already active.
// Recovered rules can be enqueued again after Resolve; history remains.
func (s *Store) EnqueueAlertOutbox(ctx context.Context, items []AlertOutboxInput, nowMS int64) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	inserted := 0
	for _, in := range items {
		if in.Fingerprint == "" || in.Rule == "" {
			return 0, errors.New("storage: alert outbox requires fingerprint and rule")
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO alert_outbox
			(id,fingerprint,rule,severity,message,status,attempts,next_attempt_ms,created_at_ms,updated_at_ms)
			SELECT ?,?,?,?,?,'PENDING',0,?,?,?
			WHERE NOT EXISTS (SELECT 1 FROM alert_outbox WHERE fingerprint=? AND status != ?)`,
			newAlertOutboxID(time.UnixMilli(nowMS)), in.Fingerprint, in.Rule, in.Severity, in.Message,
			nowMS, nowMS, nowMS, in.Fingerprint, AlertOutboxResolved)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		inserted += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

// ClaimAlertOutbox selects due pending/retry entries, fences them with a lease,
// and records a delivery attempt in one transaction. Expired IN_FLIGHT leases
// are recovered to RETRY_PENDING first (at-least-once semantics).
func (s *Store) ClaimAlertOutbox(ctx context.Context, owner string, limit int, lease time.Duration, nowMS int64) ([]AlertOutboxItem, []AlertDelivery, error) {
	if owner == "" {
		return nil, nil, errors.New("storage: alert outbox owner required")
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	if lease <= 0 {
		lease = time.Minute
	}
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE alert_deliveries SET status=?, finished_at_ms=?, error=?
		WHERE status=? AND alert_id IN (SELECT id FROM alert_outbox WHERE status=? AND lease_expires_ms>0 AND lease_expires_ms<=?)`,
		AlertOutboxRetryPending, nowMS, "lease expired", AlertOutboxInFlight, AlertOutboxInFlight, nowMS); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE alert_outbox SET status=?, lease_owner='', lease_expires_ms=0, updated_at_ms=?
		WHERE status=? AND lease_expires_ms>0 AND lease_expires_ms<=?`,
		AlertOutboxRetryPending, nowMS, AlertOutboxInFlight, nowMS); err != nil {
		return nil, nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+alertOutboxColumns+` FROM alert_outbox
		WHERE status IN (?,?) AND next_attempt_ms<=?
		ORDER BY next_attempt_ms ASC, created_at_ms ASC LIMIT ?`,
		AlertOutboxPending, AlertOutboxRetryPending, nowMS, limit)
	if err != nil {
		return nil, nil, err
	}
	var items []AlertOutboxItem
	for rows.Next() {
		it, err := scanAlertOutbox(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		items = append(items, it)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	var deliveries []AlertDelivery
	for i := range items {
		delivID := newAlertDeliveryID(time.UnixMilli(nowMS))
		attempt := items[i].Attempts + 1
		if _, err := tx.ExecContext(ctx, `UPDATE alert_outbox SET status=?, attempts=?, lease_owner=?,
			lease_expires_ms=?, delivery_id=?, updated_at_ms=? WHERE id=? AND status IN (?,?)`,
			AlertOutboxInFlight, attempt, owner, nowMS+lease.Milliseconds(), delivID, nowMS,
			items[i].ID, AlertOutboxPending, AlertOutboxRetryPending); err != nil {
			return nil, nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO alert_deliveries
			(id,alert_id,attempt,status,lease_owner,started_at_ms) VALUES(?,?,?,?,?,?)`,
			delivID, items[i].ID, attempt, AlertOutboxInFlight, owner, nowMS); err != nil {
			return nil, nil, err
		}
		items[i].Status = AlertOutboxInFlight
		items[i].Attempts = attempt
		items[i].LeaseOwner = owner
		items[i].LeaseExpiresMS = nowMS + lease.Milliseconds()
		items[i].DeliveryID = delivID
		deliveries = append(deliveries, AlertDelivery{ID: delivID, AlertID: items[i].ID, Attempt: attempt,
			Status: AlertOutboxInFlight, LeaseOwner: owner, StartedAtMS: nowMS})
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return items, deliveries, nil
}

// CompleteAlertOutbox fences completion on delivery ID and owner; a stale worker
// cannot change a newer lease. Success is DELIVERED; failure is RETRY_PENDING or
// DEAD_LETTER at maxAttempts. deliveryID is the idempotency key for receivers.
func (s *Store) CompleteAlertOutbox(ctx context.Context, deliveryID, owner string, success bool, nowMS int64, retryAfterMS int64, httpStatus int, errText string, maxAttempts int) error {
	if deliveryID == "" || owner == "" {
		return errors.New("storage: alert completion requires delivery id and owner")
	}
	if maxAttempts < 1 {
		maxAttempts = 10
	}
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var alertID, leaseOwner, status string
	var attempt int
	if err := tx.QueryRowContext(ctx, `SELECT alert_id,lease_owner,status,attempt FROM alert_deliveries WHERE id=?`, deliveryID).
		Scan(&alertID, &leaseOwner, &status, &attempt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if leaseOwner != owner || status != AlertOutboxInFlight {
		return fmt.Errorf("%w: stale alert delivery", ErrLeaseLost)
	}
	var currentOwner, currentStatus string
	var leaseExpires int64
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner,status,lease_expires_ms FROM alert_outbox WHERE id=?`, alertID).
		Scan(&currentOwner, &currentStatus, &leaseExpires); err != nil {
		return err
	}
	if currentOwner != owner || currentStatus != AlertOutboxInFlight {
		return fmt.Errorf("%w: alert lease changed", ErrLeaseLost)
	}
	if leaseExpires > 0 && leaseExpires <= nowMS {
		return fmt.Errorf("%w: alert lease expired", ErrLeaseLost)
	}
	deliveryStatus := AlertOutboxRetryPending
	alertStatus := AlertOutboxRetryPending
	next := nowMS + retryAfterMS
	if success {
		deliveryStatus, alertStatus = AlertOutboxDelivered, AlertOutboxDelivered
		next = nowMS
	} else if attempt >= maxAttempts {
		deliveryStatus, alertStatus = AlertOutboxDeadLetter, AlertOutboxDeadLetter
	}
	if _, err := tx.ExecContext(ctx, `UPDATE alert_deliveries SET status=?,finished_at_ms=?,http_status=?,error=? WHERE id=?`,
		deliveryStatus, nowMS, httpStatus, errText, deliveryID); err != nil {
		return err
	}
	deliveredAt := sql.NullInt64{}
	if success {
		deliveredAt = sql.NullInt64{Int64: nowMS, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE alert_outbox SET status=?, next_attempt_ms=?, last_error=?,
		lease_owner='', lease_expires_ms=0, updated_at_ms=?, delivered_at_ms=? WHERE id=?`,
		alertStatus, next, errText, nowMS, deliveredAt, alertID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveAlertOutbox closes one or all active rows for a rule, preserving history.
func (s *Store) ResolveAlertOutbox(ctx context.Context, rule string, nowMS int64) (int64, error) {
	if rule == "" {
		return 0, errors.New("storage: alert rule required")
	}
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE alert_deliveries SET status=?, finished_at_ms=?, error=?
		WHERE status=? AND alert_id IN (SELECT id FROM alert_outbox WHERE rule=? AND status IN (?,?,?))`,
		AlertOutboxResolved, nowMS, "resolved while in flight", AlertOutboxInFlight, rule,
		AlertOutboxPending, AlertOutboxInFlight, AlertOutboxRetryPending); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE alert_outbox SET status=?, lease_owner='', lease_expires_ms=0, updated_at_ms=?
		WHERE rule=? AND status != ?`, AlertOutboxResolved, nowMS, rule, AlertOutboxResolved)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// ResolveAlertOutboxMissing resolves every active row whose rule is absent from
// the current evaluated set in one bounded SQLite transaction. It does not scan
// a history page and cannot miss old active rows behind >500 resolved/delivered
// history. DELIVERED/DEAD_LETTER semantics are unchanged; they remain until an
// explicit Resolve. Delivery rows for the affected in-flight attempts are
// closed atomically with the outbox transition.
func (s *Store) ResolveAlertOutboxMissing(ctx context.Context, currentRules []string, nowMS int64) (int64, error) {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// A temp set avoids SQLite's bound-parameter limit for large rule sets while
	// keeping every rule parameterized. writeDB has a single connection, and the
	// set is rebuilt in this transaction.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS r151_current_rules(rule TEXT PRIMARY KEY)`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM r151_current_rules`); err != nil {
		return 0, err
	}
	for _, rule := range currentRules {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO r151_current_rules(rule) VALUES(?)`, rule); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE alert_deliveries SET status=?, finished_at_ms=?, error=?
		WHERE status=? AND alert_id IN (
			SELECT id FROM alert_outbox
			WHERE status IN (?,?,?) AND rule NOT IN (SELECT rule FROM r151_current_rules)
		)`, AlertOutboxResolved, nowMS, "resolved by reconcile", AlertOutboxInFlight,
		AlertOutboxPending, AlertOutboxInFlight, AlertOutboxRetryPending); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE alert_outbox SET status=?, lease_owner='', lease_expires_ms=0, updated_at_ms=?
		WHERE status IN (?,?,?) AND rule NOT IN (SELECT rule FROM r151_current_rules)`,
		AlertOutboxResolved, nowMS, AlertOutboxPending, AlertOutboxInFlight, AlertOutboxRetryPending)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) AlertOutboxStatusCounts(ctx context.Context) (AlertOutboxStatus, error) {
	var st AlertOutboxStatus
	rows, err := s.readDB.QueryContext(ctx, `SELECT status,count(*) FROM alert_outbox GROUP BY status`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return st, err
		}
		switch status {
		case AlertOutboxPending:
			st.Pending = n
		case AlertOutboxInFlight:
			st.InFlight = n
		case AlertOutboxRetryPending:
			st.RetryPending = n
		case AlertOutboxDelivered:
			st.Delivered = n
		case AlertOutboxResolved:
			st.Resolved = n
		case AlertOutboxDeadLetter:
			st.DeadLetter = n
		}
	}
	return st, rows.Err()
}

func (s *Store) ListAlertOutbox(ctx context.Context, limit int) ([]AlertOutboxItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT `+alertOutboxColumns+` FROM alert_outbox ORDER BY updated_at_ms DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertOutboxItem
	for rows.Next() {
		it, err := scanAlertOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

const alertOutboxColumns = `id,fingerprint,rule,severity,message,status,attempts,next_attempt_ms,
	lease_owner,lease_expires_ms,last_error,delivery_id,created_at_ms,updated_at_ms,delivered_at_ms`

func scanAlertOutbox(row rowScanner) (AlertOutboxItem, error) {
	var it AlertOutboxItem
	var delivered sql.NullInt64
	err := row.Scan(&it.ID, &it.Fingerprint, &it.Rule, &it.Severity, &it.Message, &it.Status,
		&it.Attempts, &it.NextAttemptMS, &it.LeaseOwner, &it.LeaseExpiresMS, &it.LastError,
		&it.DeliveryID, &it.CreatedAtMS, &it.UpdatedAtMS, &delivered)
	if delivered.Valid {
		v := delivered.Int64
		it.DeliveredAtMS = &v
	}
	return it, err
}
