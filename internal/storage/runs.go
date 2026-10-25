package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Run is the persisted workflow run. Status values are owned by the workflow
// state machine; storage treats them as opaque strings validated by the
// TransitionValidator.
type Run struct {
	ID                  string `json:"id"`
	OwnerID             string `json:"owner_id"`
	Template            string `json:"template"`
	Mode                string `json:"mode"`
	Status              string `json:"status"`
	IdempotencyKey      string `json:"idempotency_key"`
	ProviderSnapshotJSN string `json:"-"`
	RequestFingerprint  string `json:"-"`
	ExistingIdentityRef string `json:"existing_identity_ref,omitempty"`
	DeadlineMS          int64  `json:"-"`
	NextRunAtMS         *int64 `json:"-"`
	LeaseOwner          string `json:"-"`
	LeaseUntilMS        *int64 `json:"-"`
	FencingGeneration   int64  `json:"-"`
	Version             int64  `json:"version"`
	CreatedAtMS         int64  `json:"-"`
	UpdatedAtMS         int64  `json:"-"`
	FinishedAtMS        *int64 `json:"-"`
	CurrentStep         string `json:"current_step,omitempty"`
	ResumeState         string `json:"resume_state,omitempty"`
	LastErrorClass      string `json:"last_error_class,omitempty"`
	ActionRequired      bool   `json:"action_required"`
}

// CreatedAt returns the creation time in UTC.
func (r Run) CreatedAt() time.Time { return time.UnixMilli(r.CreatedAtMS).UTC() }

// UpdatedAt returns the update time in UTC.
func (r Run) UpdatedAt() time.Time { return time.UnixMilli(r.UpdatedAtMS).UTC() }

// Deadline returns the run deadline in UTC.
func (r Run) Deadline() time.Time { return time.UnixMilli(r.DeadlineMS).UTC() }

// Step is a persisted step row.
type Step struct {
	ID            string `json:"id"`
	RunID         string `json:"run_id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	OperationKey  string `json:"-"`
	AttemptCount  int64  `json:"attempt_count"`
	ErrorClass    string `json:"error_class,omitempty"`
	ErrorSummary  string `json:"error_summary,omitempty"`
	NextRetryAtMS *int64 `json:"-"`
	StartedAtMS   *int64 `json:"-"`
	FinishedAtMS  *int64 `json:"-"`
	UpdatedAtMS   int64  `json:"-"`
	ResultJSON    string `json:"-"`
	WaitingReason string `json:"waiting_reason,omitempty"`
	FailureCount  int64  `json:"failure_count"`
}

// StepSeed creates a step row at run creation.
type StepSeed struct {
	Name string
}

// CreateRunParams is the idempotent run creation input.
type CreateRunParams struct {
	ID                  string
	OwnerID             string
	Template            string
	Mode                string
	IdempotencyKey      string
	ProviderSnapshotJSN string
	RequestFingerprint  string
	ExistingIdentityRef string
	DeadlineMS          int64
	Steps               []StepSeed
}

// CreateRun inserts a run and its step rows atomically. When the idempotency
// key already exists:
//   - identical request fingerprint -> return the existing run, created=false;
//   - different fingerprint -> ErrIdempotencyConflict.
func (s *Store) CreateRun(ctx context.Context, p CreateRunParams) (Run, bool, error) {
	if p.ID == "" || p.OwnerID == "" || p.Template == "" || p.IdempotencyKey == "" {
		return Run{}, false, errors.New("storage: incomplete run params")
	}
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, false, err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `INSERT INTO runs
		(id, owner_id, template, mode, status, idempotency_key, provider_snapshot_json,
		 existing_identity_ref, deadline_ms, next_run_at_ms, fencing_generation, version,
		 created_at_ms, updated_at_ms, current_step, request_fingerprint, action_required)
		VALUES (?,?,?,?, 'CREATED', ?, ?, ?, ?, NULL, 0, 1, ?, ?, NULL, ?, 0)`,
		p.ID, p.OwnerID, p.Template, p.Mode, p.IdempotencyKey, p.ProviderSnapshotJSN,
		nullableString(p.ExistingIdentityRef), p.DeadlineMS, now, now, p.RequestFingerprint)
	if err != nil {
		if !isUniqueConstraint(err) {
			return Run{}, false, fmt.Errorf("storage: insert run: %w", err)
		}
		existing, getErr := s.getRunTx(ctx, tx, "owner_id = ? AND template = ? AND idempotency_key = ?", p.OwnerID, p.Template, p.IdempotencyKey)
		if getErr != nil {
			return Run{}, false, getErr
		}
		if existing.RequestFingerprint != p.RequestFingerprint {
			return Run{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(); err != nil {
			return Run{}, false, err
		}
		return existing, false, nil
	}
	for _, step := range p.Steps {
		operationKey := p.ID + ":" + step.Name + ":v1"
		_, err := tx.ExecContext(ctx, `INSERT INTO steps
			(id, run_id, step_name, status, operation_key, attempt_count, updated_at_ms)
			VALUES (?,?,?, 'PENDING', ?, 0, ?)`,
			s.newID(), p.ID, step.Name, operationKey, now)
		if err != nil {
			return Run{}, false, fmt.Errorf("storage: insert step %q: %w", step.Name, err)
		}
	}
	if _, err := s.appendEventTx(ctx, tx, p.ID, "RUN_CREATED", "", map[string]any{
		"reason":      "run created",
		"deadline_at": p.DeadlineMS,
		"provider":    p.Mode,
	}, now); err != nil {
		return Run{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, false, err
	}
	run, err := s.GetRun(ctx, p.ID)
	return run, true, err
}

// GetRun loads one run.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	row := s.readDB.QueryRowContext(ctx, runSelectColumns+` WHERE id = ?`, id)
	return scanRun(row)
}

func (s *Store) getRunTx(ctx context.Context, tx *sql.Tx, where string, args ...any) (Run, error) {
	row := tx.QueryRowContext(ctx, runSelectColumns+` WHERE `+where, args...)
	return scanRun(row)
}

const runSelectColumns = `SELECT id, owner_id, template, mode, status, idempotency_key,
	provider_snapshot_json, COALESCE(request_fingerprint,''), COALESCE(existing_identity_ref,''),
	deadline_ms, next_run_at_ms, COALESCE(lease_owner,''), lease_until_ms, fencing_generation,
	version, created_at_ms, updated_at_ms, finished_at_ms, COALESCE(current_step,''),
	COALESCE(resume_state,''), COALESCE(last_error_class,''), action_required FROM runs`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(row rowScanner) (Run, error) {
	var (
		r                             Run
		nextRun, leaseUntil, finished sql.NullInt64
	)
	err := row.Scan(&r.ID, &r.OwnerID, &r.Template, &r.Mode, &r.Status, &r.IdempotencyKey,
		&r.ProviderSnapshotJSN, &r.RequestFingerprint, &r.ExistingIdentityRef,
		&r.DeadlineMS, &nextRun, &r.LeaseOwner, &leaseUntil, &r.FencingGeneration,
		&r.Version, &r.CreatedAtMS, &r.UpdatedAtMS, &finished, &r.CurrentStep,
		&r.ResumeState, &r.LastErrorClass, &r.ActionRequired)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, ErrNotFound
		}
		return Run{}, err
	}
	r.NextRunAtMS = ptrMS(nextRun)
	r.LeaseUntilMS = ptrMS(leaseUntil)
	r.FinishedAtMS = ptrMS(finished)
	return r, nil
}

// RunListFilter filters ListRuns.
type RunListFilter struct {
	Status   string
	Template string
	Limit    int
	Cursor   string
}

// RunPage is a cursor-paginated run list.
type RunPage struct {
	Items      []Run  `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// ListRuns returns runs newest first.
func (s *Store) ListRuns(ctx context.Context, f RunListFilter) (RunPage, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 50
	}
	query := runSelectColumns
	var args []any
	where := []string{}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.Template != "" {
		where = append(where, "template = ?")
		args = append(args, f.Template)
	}
	if f.Cursor != "" {
		created, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return RunPage{}, fmt.Errorf("%w: invalid cursor", ErrConflict)
		}
		where = append(where, "(created_at_ms < ? OR (created_at_ms = ? AND id < ?))")
		args = append(args, created, created, id)
	}
	if len(where) > 0 {
		query += " WHERE " + where[0]
		for _, w := range where[1:] {
			query += " AND " + w
		}
	}
	query += " ORDER BY created_at_ms DESC, id DESC LIMIT ?"
	args = append(args, f.Limit+1)

	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return RunPage{}, err
	}
	defer rows.Close()
	var page RunPage
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return RunPage{}, err
		}
		page.Items = append(page.Items, r)
	}
	if err := rows.Err(); err != nil {
		return RunPage{}, err
	}
	if len(page.Items) > f.Limit {
		last := page.Items[f.Limit-1]
		page.Items = page.Items[:f.Limit]
		page.NextCursor = encodeCursor(last.CreatedAtMS, last.ID)
	}
	return page, nil
}

func encodeCursor(created int64, id string) string {
	raw, _ := json.Marshal(map[string]any{"c": created, "i": id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, "", err
	}
	var v struct {
		C int64  `json:"c"`
		I string `json:"i"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, "", err
	}
	if v.I == "" {
		return 0, "", errors.New("empty cursor id")
	}
	return v.C, v.I, nil
}

// ListSteps returns the step rows for a run in creation order.
func (s *Store) ListSteps(ctx context.Context, runID string) ([]Step, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT id, run_id, step_name, status, operation_key,
		attempt_count, COALESCE(error_class,''), COALESCE(error_summary,''), next_retry_at_ms,
		started_at_ms, finished_at_ms, updated_at_ms, COALESCE(result_json,'{}'), COALESCE(waiting_reason,''), failure_count
		FROM steps WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var (
			st                        Step
			nextRetry, started, finis sql.NullInt64
		)
		if err := rows.Scan(&st.ID, &st.RunID, &st.Name, &st.Status, &st.OperationKey,
			&st.AttemptCount, &st.ErrorClass, &st.ErrorSummary, &nextRetry,
			&started, &finis, &st.UpdatedAtMS, &st.ResultJSON, &st.WaitingReason, &st.FailureCount); err != nil {
			return nil, err
		}
		st.NextRetryAtMS = ptrMS(nextRetry)
		st.StartedAtMS = ptrMS(started)
		st.FinishedAtMS = ptrMS(finis)
		out = append(out, st)
	}
	return out, rows.Err()
}

// RequestCancel marks a run CANCEL_REQUESTED. Terminal runs are a conflict.
func (s *Store) RequestCancel(ctx context.Context, id string) (Run, error) {
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	run, err := s.getRunTx(ctx, tx, `id = ?`, id)
	if err != nil {
		return Run{}, err
	}
	switch run.Status {
	case "COMPLETED", "FAILED_TERMINAL", "CANCELLED":
		return Run{}, fmt.Errorf("%w: run already terminal (%s)", ErrConflict, run.Status)
	}
	if run.Status == "CANCEL_REQUESTED" {
		return run, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='CANCEL_REQUESTED', next_run_at_ms=?,
		updated_at_ms=?, version=version+1, action_required=0 WHERE id=?`, now, now, id); err != nil {
		return Run{}, err
	}
	if _, err := s.appendEventTx(ctx, tx, id, "RUN_CANCEL_REQUESTED", "", map[string]any{
		"reason": "operator requested cancellation",
	}, now); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return s.GetRun(ctx, id)
}

// RequestResume makes a waiting run runnable again. Only legal from states that
// explicitly require an operator action.
func (s *Store) RequestResume(ctx context.Context, id string) (Run, error) {
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	run, err := s.getRunTx(ctx, tx, `id = ?`, id)
	if err != nil {
		return Run{}, err
	}
	if !resumableStatus(run.Status) {
		return Run{}, fmt.Errorf("%w: run is not waiting for an operator (status %s)", ErrConflict, run.Status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET next_run_at_ms=?, updated_at_ms=?, version=version+1,
		action_required=0 WHERE id=?`, now, now, id); err != nil {
		return Run{}, err
	}
	if _, err := s.appendEventTx(ctx, tx, id, "RUN_RESUMED", "", map[string]any{
		"reason": "operator resumed the run",
	}, now); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return s.GetRun(ctx, id)
}

func resumableStatus(status string) bool {
	switch status {
	case "WAITING_USER_ACTION", "RETRY_WAIT", "REAUTHORIZE_REQUIRED":
		return true
	default:
		return false
	}
}

// sweepAfterSelectHook is a test-only synchronization point invoked after the
// expired-run SELECT and before the per-run write transaction. It lets tests
// deterministically interleave RequestCancel / worker commits between the read
// and the write, which is the race this code must tolerate.
var sweepAfterSelectHook func()

// SweepDeadlines fails runs whose absolute deadline has passed. It returns the
// IDs it transitioned.
func (s *Store) SweepDeadlines(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	now := s.nowMS()
	rows, err := s.readDB.QueryContext(ctx, `SELECT id FROM runs
		WHERE deadline_ms <= ? AND status NOT IN ('COMPLETED','FAILED_TERMINAL','CANCELLED','CANCEL_REQUESTED')
		ORDER BY deadline_ms LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if sweepAfterSelectHook != nil {
		sweepAfterSelectHook()
	}
	var done []string
	for _, id := range ids {
		// Re-evaluate the deadline and status inside the write transaction: the
		// SELECT above may be stale because an operator cancelled the run, a
		// worker committed a terminal result, or the deadline moved meanwhile.
		// CANCEL_REQUESTED must never be overwritten by the timeout terminal
		// state, otherwise an explicit operator action would be lost.
		txNow := s.nowMS()
		tx, err := s.writeDB.BeginTx(ctx, nil)
		if err != nil {
			return done, err
		}
		run, err := s.getRunTx(ctx, tx, `id = ?`, id)
		if err != nil {
			tx.Rollback()
			continue
		}
		if run.Status == "COMPLETED" || run.Status == "FAILED_TERMINAL" ||
			run.Status == "CANCELLED" || run.Status == "CANCEL_REQUESTED" ||
			run.DeadlineMS > txNow {
			tx.Rollback()
			continue
		}
		res, err := tx.ExecContext(ctx, `UPDATE runs SET status='FAILED_TERMINAL', finished_at_ms=?,
			updated_at_ms=?, next_run_at_ms=NULL, lease_owner=NULL, lease_until_ms=NULL,
			version=version+1, action_required=0, last_error_class='TIMEOUT_UNCERTAIN'
			WHERE id=? AND status NOT IN ('COMPLETED','FAILED_TERMINAL','CANCELLED','CANCEL_REQUESTED')
			AND deadline_ms <= ?`, txNow, txNow, id, txNow)
		if err != nil {
			tx.Rollback()
			return done, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			tx.Rollback()
			return done, err
		}
		if affected != 1 {
			// A concurrent writer won; do not emit a terminal event for a
			// transition this sweep did not perform.
			tx.Rollback()
			continue
		}
		if _, err := s.appendEventTx(ctx, tx, id, "RUN_FAILED", "", map[string]any{
			"failure_class": "TIMEOUT_UNCERTAIN",
			"reason":        "absolute run deadline exceeded",
		}, txNow); err != nil {
			tx.Rollback()
			return done, err
		}
		if err := tx.Commit(); err != nil {
			return done, err
		}
		done = append(done, id)
	}
	return done, nil
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	// Only the UNIQUE violation is treated as an idempotency conflict; CHECK,
	// FOREIGN KEY or NOT NULL violations are real data errors.
	return contains(err.Error(), "UNIQUE constraint failed")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
