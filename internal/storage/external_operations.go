package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/baicie/account-onboarding/internal/security"
)

// External operation statuses. See migrations/0004 and ADR 0001.
const (
	ExternalPrepared          = "PREPARED"
	ExternalInFlight          = "IN_FLIGHT"
	ExternalConfirmed         = "CONFIRMED"
	ExternalOutcomeUnknown    = "OUTCOME_UNKNOWN"
	ExternalReconciliationReq = "RECONCILIATION_REQUIRED"
	ExternalFailedDefinite    = "FAILED_DEFINITE"
	ExternalResolved          = "RESOLVED"
)

// ErrExternalOperationBlocked means an operation may not be called again
// automatically: it is in flight, outcome is unknown, needs reconciliation, is
// already confirmed, or was resolved by an operator.
var ErrExternalOperationBlocked = errors.New("external operation blocked: reconciliation required")

// ExternalOperation is the authoritative permit/state for one remote write.
type ExternalOperation struct {
	ID                string `json:"id"`
	RunID             string `json:"run_id"`
	StepName          string `json:"step_name"`
	OperationKey      string `json:"operation_key"`
	Provider          string `json:"provider"`
	OperationType     string `json:"operation_type"`
	Fingerprint       string `json:"request_fingerprint"`
	Status            string `json:"status"`
	AttemptCount      int64  `json:"attempt_count"`
	Version           int64  `json:"version"`
	LastErrorClass    string `json:"last_error_class,omitempty"`
	Resolution        string `json:"resolution,omitempty"`
	ResolutionReason  string `json:"resolution_reason,omitempty"`
	EvidenceReference string `json:"evidence_reference,omitempty"`
	PreparedAtMS      int64  `json:"prepared_at_ms"`
	StartedAtMS       *int64 `json:"started_at_ms,omitempty"`
	ResolvedAtMS      *int64 `json:"resolved_at_ms,omitempty"`
	CreatedAtMS       int64  `json:"created_at_ms"`
	UpdatedAtMS       int64  `json:"updated_at_ms"`
}

// BeginExternalOperationParams gates one remote write attempt. LeaseOwner and
// FencingGeneration are mandatory when called from a worker: a stale/expired
// worker must not be able to acquire a new permit.
type BeginExternalOperationParams struct {
	RunID             string
	StepName          string
	Provider          string
	OperationType     string
	Fingerprint       string
	LeaseOwner        string
	FencingGeneration int64
}

// ExternalOperationFilter controls pagination/filtering for diagnostics.
type ExternalOperationFilter struct {
	Status   string
	Provider string
	RunID    string
	Limit    int
	Cursor   string
}

// ExternalOperationsPage is the paginated diagnostics contract. Counts are
// global (not derived from the current page).
type ExternalOperationsPage struct {
	Items                    []ExternalOperation `json:"items"`
	NextCursor               string              `json:"next_cursor,omitempty"`
	StatusCounts             map[string]int64    `json:"status_counts"`
	TotalNeedsReconciliation int64               `json:"total_needs_reconciliation"`
	QueryScope               map[string]any      `json:"query_scope"`
	GeneratedAtMS            int64               `json:"generated_at_ms"`
}

// BeginExternalOperation durably records the intent (PREPARED) and returns a
// candidate permit. Only MarkExternalOperationInFlight can turn that into the
// single allowed remote-call permit; callers must check it immediately before
// the provider call. A retry is allowed only when the prior attempt is known not
// to have produced a remote write (FAILED_DEFINITE) or a PREPARED intent never
// reached IN_FLIGHT.
func (s *Store) BeginExternalOperation(ctx context.Context, p BeginExternalOperationParams) (ExternalOperation, error) {
	if strings.TrimSpace(p.RunID) == "" || strings.TrimSpace(p.StepName) == "" ||
		strings.TrimSpace(p.Provider) == "" || strings.TrimSpace(p.OperationType) == "" ||
		strings.TrimSpace(p.Fingerprint) == "" {
		return ExternalOperation{}, errors.New("storage: invalid external operation params")
	}
	if strings.TrimSpace(p.LeaseOwner) == "" || p.FencingGeneration <= 0 {
		return ExternalOperation{}, errors.New("storage: external operation requires a fenced lease")
	}
	opKey := p.RunID + ":" + p.StepName + ":v1"
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return ExternalOperation{}, err
	}
	defer tx.Rollback()

	var owner string
	var gen int64
	var leaseUntil sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(lease_owner,''), fencing_generation, lease_until_ms FROM runs WHERE id=?`,
		p.RunID).Scan(&owner, &gen, &leaseUntil); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExternalOperation{}, ErrNotFound
		}
		return ExternalOperation{}, err
	}
	if owner != p.LeaseOwner || gen != p.FencingGeneration {
		return ExternalOperation{}, fmt.Errorf("%w: lease owner/generation changed", ErrLeaseLost)
	}
	if !leaseUntil.Valid || leaseUntil.Int64 <= now {
		return ExternalOperation{}, fmt.Errorf("%w: lease expired before external operation", ErrLeaseLost)
	}

	var existing ExternalOperation
	err = tx.QueryRowContext(ctx, `SELECT id, run_id, step_name, operation_key, provider, operation_type,
		request_fingerprint, status, attempt_count, version, COALESCE(last_error_class,''),
		COALESCE(resolution,''), COALESCE(resolution_reason,''), COALESCE(evidence_reference,''),
		prepared_at_ms, started_at_ms, resolved_at_ms, created_at_ms, updated_at_ms
		FROM external_operations WHERE operation_key=?`, opKey).Scan(
		&existing.ID, &existing.RunID, &existing.StepName, &existing.OperationKey, &existing.Provider,
		&existing.OperationType, &existing.Fingerprint, &existing.Status, &existing.AttemptCount,
		&existing.Version, &existing.LastErrorClass, &existing.Resolution, &existing.ResolutionReason,
		&existing.EvidenceReference, &existing.PreparedAtMS, &existing.StartedAtMS, &existing.ResolvedAtMS,
		&existing.CreatedAtMS, &existing.UpdatedAtMS)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		existing = ExternalOperation{
			ID:            security.NewID(),
			RunID:         p.RunID,
			StepName:      p.StepName,
			OperationKey:  opKey,
			Provider:      p.Provider,
			OperationType: p.OperationType,
			Fingerprint:   p.Fingerprint,
			Status:        ExternalPrepared,
			AttemptCount:  1,
			Version:       1,
			PreparedAtMS:  now,
			CreatedAtMS:   now,
			UpdatedAtMS:   now,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO external_operations
			(id, run_id, step_name, operation_key, provider, operation_type, request_fingerprint,
			 status, attempt_count, version, prepared_at_ms, started_at_ms, created_at_ms, updated_at_ms)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			existing.ID, existing.RunID, existing.StepName, existing.OperationKey, existing.Provider,
			existing.OperationType, existing.Fingerprint, existing.Status, existing.AttemptCount,
			existing.Version, existing.PreparedAtMS, existing.StartedAtMS, existing.CreatedAtMS, existing.UpdatedAtMS); err != nil {
			return ExternalOperation{}, err
		}
	case err != nil:
		return ExternalOperation{}, err
	case existing.Provider != p.Provider || existing.OperationType != p.OperationType ||
		existing.Fingerprint != p.Fingerprint:
		return ExternalOperation{}, fmt.Errorf("%w: operation key reused with a different request", ErrConflict)
	case existing.Status == ExternalFailedDefinite:
		res, err := tx.ExecContext(ctx, `UPDATE external_operations
			SET status=?, attempt_count=attempt_count+1, version=version+1, started_at_ms=NULL,
			    last_error_class=NULL, updated_at_ms=?
			WHERE id=? AND status=? AND version=?`,
			ExternalPrepared, now, existing.ID, ExternalFailedDefinite, existing.Version)
		if err != nil {
			return ExternalOperation{}, err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ExternalOperation{}, fmt.Errorf("%w: concurrent external operation acquisition", ErrConflict)
		}
		existing.Status = ExternalPrepared
		existing.AttemptCount++
		existing.Version++
		existing.StartedAtMS = nil
		existing.UpdatedAtMS = now
	case existing.Status == ExternalPrepared:
		// Another worker may already hold this PREPARED permit. Marking
		// IN_FLIGHT is the atomic single-winner step, so return the permit
		// without changing the row here.
	default:
		// PREPARED/IN_FLIGHT/CONFIRMED/OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED/
		// RESOLVED: never issue another remote write automatically.
		return ExternalOperation{}, fmt.Errorf("%w: status=%s", ErrExternalOperationBlocked, existing.Status)
	}
	if err := tx.Commit(); err != nil {
		return ExternalOperation{}, err
	}
	return existing, nil
}

// MarkExternalOperationInFlight atomically converts a PREPARED intent into the
// single IN_FLIGHT remote-call permit. Concurrent workers may each hold a
// PREPARED candidate, but exactly one CAS wins; losers must not call the
// provider. A stale/expired lease is rejected again here.
func (s *Store) MarkExternalOperationInFlight(ctx context.Context, operationKey, leaseOwner string, fencingGeneration int64) (ExternalOperation, error) {
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return ExternalOperation{}, err
	}
	defer tx.Rollback()
	var runID string
	var owner string
	var gen int64
	var leaseUntil sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT e.run_id, COALESCE(r.lease_owner,''), r.fencing_generation, r.lease_until_ms
		FROM external_operations e JOIN runs r ON r.id=e.run_id WHERE e.operation_key=?`, operationKey).
		Scan(&runID, &owner, &gen, &leaseUntil); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExternalOperation{}, ErrNotFound
		}
		return ExternalOperation{}, err
	}
	if owner != leaseOwner || gen != fencingGeneration || !leaseUntil.Valid || leaseUntil.Int64 <= now {
		return ExternalOperation{}, fmt.Errorf("%w: lease no longer valid for remote write", ErrLeaseLost)
	}
	res, err := tx.ExecContext(ctx, `UPDATE external_operations
		SET status=?, started_at_ms=?, updated_at_ms=?, version=version+1
		WHERE operation_key=? AND status=?`, ExternalInFlight, now, now, operationKey, ExternalPrepared)
	if err != nil {
		return ExternalOperation{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ExternalOperation{}, fmt.Errorf("%w: another worker already acquired the remote-write permit", ErrExternalOperationBlocked)
	}
	if err := tx.Commit(); err != nil {
		return ExternalOperation{}, err
	}
	return s.GetExternalOperation(ctx, operationKey)
}

// CompleteExternalOperation transitions IN_FLIGHT to the final/audit state with
// a compare-and-set on status/version. A concurrent or stale completion fails.
func (s *Store) CompleteExternalOperation(ctx context.Context, operationKey, status, errorClass string) (ExternalOperation, error) {
	switch status {
	case ExternalConfirmed, ExternalOutcomeUnknown, ExternalReconciliationReq, ExternalFailedDefinite:
	default:
		return ExternalOperation{}, fmt.Errorf("storage: invalid external completion status %q", status)
	}
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return ExternalOperation{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE external_operations
		SET status=?, last_error_class=?, version=version+1, updated_at_ms=?
		WHERE operation_key=? AND status=?`,
		status, errorClass, now, operationKey, ExternalInFlight)
	if err != nil {
		return ExternalOperation{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ExternalOperation{}, fmt.Errorf("%w: completion raced or operation is not IN_FLIGHT", ErrConflict)
	}
	if err := tx.Commit(); err != nil {
		return ExternalOperation{}, err
	}
	return s.GetExternalOperation(ctx, operationKey)
}

// GetExternalOperation returns one operation by its operation key.
func (s *Store) GetExternalOperation(ctx context.Context, operationKey string) (ExternalOperation, error) {
	row := s.readDB.QueryRowContext(ctx, `SELECT id, run_id, step_name, operation_key, provider,
		operation_type, request_fingerprint, status, attempt_count, version, COALESCE(last_error_class,''),
		COALESCE(resolution,''), COALESCE(resolution_reason,''), COALESCE(evidence_reference,''),
		prepared_at_ms, started_at_ms, resolved_at_ms, created_at_ms, updated_at_ms
		FROM external_operations WHERE operation_key=?`, operationKey)
	var op ExternalOperation
	err := row.Scan(&op.ID, &op.RunID, &op.StepName, &op.OperationKey, &op.Provider, &op.OperationType,
		&op.Fingerprint, &op.Status, &op.AttemptCount, &op.Version, &op.LastErrorClass,
		&op.Resolution, &op.ResolutionReason, &op.EvidenceReference, &op.PreparedAtMS,
		&op.StartedAtMS, &op.ResolvedAtMS, &op.CreatedAtMS, &op.UpdatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalOperation{}, ErrNotFound
	}
	return op, err
}

// ListExternalOperations returns a stable, filtered page plus global counts.
func (s *Store) ListExternalOperations(ctx context.Context, f ExternalOperationFilter) (ExternalOperationsPage, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	status := strings.ToUpper(strings.TrimSpace(f.Status))
	if status != "" && !validExternalStatus(status) {
		return ExternalOperationsPage{}, fmt.Errorf("%w: invalid status filter", ErrConflict)
	}
	where := []string{"1=1"}
	args := []any{}
	if status != "" {
		where = append(where, "status=?")
		args = append(args, status)
	}
	if f.Provider != "" {
		where = append(where, "provider=?")
		args = append(args, f.Provider)
	}
	if f.RunID != "" {
		where = append(where, "run_id=?")
		args = append(args, f.RunID)
	}
	if f.Cursor != "" {
		created, id, err := decodeExternalCursor(f.Cursor)
		if err != nil {
			return ExternalOperationsPage{}, err
		}
		where = append(where, "(created_at_ms < ? OR (created_at_ms = ? AND id < ?))")
		args = append(args, created, created, id)
	}
	q := `SELECT id, run_id, step_name, operation_key, provider, operation_type, request_fingerprint,
		status, attempt_count, version, COALESCE(last_error_class,''), COALESCE(resolution,''),
		COALESCE(resolution_reason,''), COALESCE(evidence_reference,''), prepared_at_ms,
		started_at_ms, resolved_at_ms, created_at_ms, updated_at_ms
		FROM external_operations WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY created_at_ms DESC, id DESC LIMIT ?`
	args = append(args, f.Limit+1)
	rows, err := s.readDB.QueryContext(ctx, q, args...)
	if err != nil {
		return ExternalOperationsPage{}, err
	}
	defer rows.Close()
	page := ExternalOperationsPage{
		Items:         []ExternalOperation{},
		StatusCounts:  map[string]int64{},
		QueryScope:    map[string]any{"status": status, "provider": f.Provider, "run_id": f.RunID},
		GeneratedAtMS: s.nowMS(),
	}
	var count int
	for rows.Next() {
		var op ExternalOperation
		if err := rows.Scan(&op.ID, &op.RunID, &op.StepName, &op.OperationKey, &op.Provider,
			&op.OperationType, &op.Fingerprint, &op.Status, &op.AttemptCount, &op.Version,
			&op.LastErrorClass, &op.Resolution, &op.ResolutionReason, &op.EvidenceReference,
			&op.PreparedAtMS, &op.StartedAtMS, &op.ResolvedAtMS, &op.CreatedAtMS,
			&op.UpdatedAtMS); err != nil {
			return ExternalOperationsPage{}, err
		}
		count++
		if count <= f.Limit {
			page.Items = append(page.Items, op)
		} else {
			last := page.Items[len(page.Items)-1]
			page.NextCursor = encodeExternalCursor(last.CreatedAtMS, last.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return ExternalOperationsPage{}, err
	}
	// Global counts, independent of the current page/filter.
	crows, err := s.readDB.QueryContext(ctx, `SELECT status, COUNT(*) FROM external_operations GROUP BY status`)
	if err != nil {
		return ExternalOperationsPage{}, err
	}
	for crows.Next() {
		var st string
		var n int64
		if err := crows.Scan(&st, &n); err != nil {
			crows.Close()
			return ExternalOperationsPage{}, err
		}
		page.StatusCounts[st] = n
	}
	crows.Close()
	for _, st := range []string{ExternalPrepared, ExternalInFlight, ExternalOutcomeUnknown, ExternalReconciliationReq} {
		page.TotalNeedsReconciliation += page.StatusCounts[st]
	}
	return page, nil
}

func validExternalStatus(s string) bool {
	switch s {
	case ExternalPrepared, ExternalInFlight, ExternalConfirmed, ExternalOutcomeUnknown,
		ExternalReconciliationReq, ExternalFailedDefinite, ExternalResolved:
		return true
	}
	return false
}

type externalCursor struct {
	CreatedAtMS int64  `json:"t"`
	ID          string `json:"i"`
}

func encodeExternalCursor(created int64, id string) string {
	raw, _ := json.Marshal(externalCursor{CreatedAtMS: created, ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeExternalCursor(cursor string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", fmt.Errorf("%w: invalid cursor encoding", ErrConflict)
	}
	var c externalCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.ID == "" || c.CreatedAtMS <= 0 {
		return 0, "", fmt.Errorf("%w: invalid cursor", ErrConflict)
	}
	return c.CreatedAtMS, c.ID, nil
}

// ExternalResolution is the operator's verified decision.
type ExternalResolution struct {
	Resolution        string
	Reason            string
	EvidenceReference string
	ExpectedVersion   int64
}

// ResolveExternalOperation closes an ambiguous operation. It does not delete
// remote resources and never triggers automatic retries; only an audited
// operator decision may move an operation to RESOLVED.
func (s *Store) ResolveExternalOperation(ctx context.Context, id string, r ExternalResolution) (ExternalOperation, error) {
	switch r.Resolution {
	case "RESOURCE_CONFIRMED", "NO_RESOURCE_CREATED", "MANUAL_COMPENSATION_COMPLETED", "UNRESOLVED":
	default:
		return ExternalOperation{}, fmt.Errorf("%w: invalid resolution", ErrConflict)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return ExternalOperation{}, fmt.Errorf("%w: resolution reason is required", ErrConflict)
	}
	if r.ExpectedVersion <= 0 {
		return ExternalOperation{}, fmt.Errorf("%w: expected_version is required", ErrConflict)
	}
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return ExternalOperation{}, err
	}
	defer tx.Rollback()
	var currentStatus string
	var leaseUntil sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT e.status, r.lease_until_ms
		FROM external_operations e JOIN runs r ON r.id=e.run_id WHERE e.id=?`, id).
		Scan(&currentStatus, &leaseUntil); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExternalOperation{}, ErrNotFound
		}
		return ExternalOperation{}, err
	}
	switch currentStatus {
	case ExternalOutcomeUnknown, ExternalReconciliationReq:
	case ExternalConfirmed:
		// A step commit can fail after the remote write was confirmed. This is
		// the audited recovery path, but only for outcomes that acknowledge the
		// resource exists; it can never claim "no resource" for a confirmation.
		if r.Resolution == "NO_RESOURCE_CREATED" {
			return ExternalOperation{}, fmt.Errorf("%w: CONFIRMED cannot be resolved as no-resource-created", ErrConflict)
		}
	case ExternalInFlight:
		// A live worker may still be inside the provider call. Only a lease
		// that has expired (worker stopped) may be manually resolved.
		if leaseUntil.Valid && leaseUntil.Int64 > now {
			return ExternalOperation{}, fmt.Errorf("%w: operation is IN_FLIGHT under a live lease", ErrConflict)
		}
	default:
		return ExternalOperation{}, fmt.Errorf("%w: operation status %s is not resolvable", ErrConflict, currentStatus)
	}
	res, err := tx.ExecContext(ctx, `UPDATE external_operations
		SET status=?, resolution=?, resolution_reason=?, evidence_reference=?, resolved_at_ms=?,
		    version=version+1, updated_at_ms=?
		WHERE id=? AND version=? AND status IN (?,?,?,?)`,
		ExternalResolved, r.Resolution, r.Reason, r.EvidenceReference, now, now,
		id, r.ExpectedVersion, ExternalInFlight, ExternalOutcomeUnknown, ExternalReconciliationReq, ExternalConfirmed)
	if err != nil {
		return ExternalOperation{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ExternalOperation{}, fmt.Errorf("%w: version conflict or operation is not resolvable", ErrConflict)
	}
	// The operator decision and its audit row commit atomically: if the audit
	// write fails, the resolution must not appear successful.
	detail := s.redactor.SanitizeEventPayload(map[string]any{
		"resolution": r.Resolution, "reason": r.Reason,
		"evidence_reference": r.EvidenceReference,
	})
	rawDetail, err := json.Marshal(detail)
	if err != nil {
		return ExternalOperation{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (actor, action, run_id, detail_json, created_at_ms)
		VALUES (?,?,?,?,?)`, "operator", "external_operation.resolve", id, string(rawDetail), now); err != nil {
		return ExternalOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExternalOperation{}, err
	}
	row := s.readDB.QueryRowContext(ctx, `SELECT id, run_id, step_name, operation_key, provider,
		operation_type, request_fingerprint, status, attempt_count, version, COALESCE(last_error_class,''),
		COALESCE(resolution,''), COALESCE(resolution_reason,''), COALESCE(evidence_reference,''),
		prepared_at_ms, started_at_ms, resolved_at_ms, created_at_ms, updated_at_ms
		FROM external_operations WHERE id=?`, id)
	var op ExternalOperation
	if err := row.Scan(&op.ID, &op.RunID, &op.StepName, &op.OperationKey, &op.Provider, &op.OperationType,
		&op.Fingerprint, &op.Status, &op.AttemptCount, &op.Version, &op.LastErrorClass,
		&op.Resolution, &op.ResolutionReason, &op.EvidenceReference, &op.PreparedAtMS,
		&op.StartedAtMS, &op.ResolvedAtMS, &op.CreatedAtMS, &op.UpdatedAtMS); err != nil {
		return ExternalOperation{}, err
	}
	return op, nil
}

// AgeOfOldestNeedsReconciliation supports operational diagnostics.
func (s *Store) AgeOfOldestNeedsReconciliation(ctx context.Context, nowMS int64) (int64, error) {
	var oldest sql.NullInt64
	err := s.readDB.QueryRowContext(ctx, `SELECT MIN(created_at_ms) FROM external_operations
		WHERE status IN (?,?,?,?)`, ExternalPrepared, ExternalInFlight,
		ExternalOutcomeUnknown, ExternalReconciliationReq).Scan(&oldest)
	if err != nil || !oldest.Valid {
		return 0, err
	}
	if nowMS <= oldest.Int64 {
		return 0, nil
	}
	return nowMS - oldest.Int64, nil
}
