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

// External operation statuses (migration 0004/0005). See ADR 0001.
const (
	ExternalPrepared          = "PREPARED"
	ExternalInFlight          = "IN_FLIGHT"
	ExternalConfirmed         = "CONFIRMED"
	ExternalOutcomeUnknown    = "OUTCOME_UNKNOWN"
	ExternalReconciliationReq = "RECONCILIATION_REQUIRED"
	ExternalFailedDefinite    = "FAILED_DEFINITE"
	ExternalResolved          = "RESOLVED"
)

var ErrExternalOperationBlocked = errors.New("external operation blocked: reconciliation required")

// ExternalOperation is the authoritative permit/state for one remote write.
type ExternalOperation struct {
	ID                 string `json:"id"`
	RunID              string `json:"run_id"`
	StepName           string `json:"step_name"`
	OperationKey       string `json:"operation_key"`
	Provider           string `json:"provider"`
	OperationType      string `json:"operation_type"`
	Fingerprint        string `json:"request_fingerprint"`
	Status             string `json:"status"`
	AttemptCount       int64  `json:"attempt_count"`
	Version            int64  `json:"version"`
	WorkerID           string `json:"worker_id,omitempty"`
	LeaseGeneration    int64  `json:"lease_generation"`
	LastErrorClass     string `json:"last_error_class,omitempty"`
	Resolution         string `json:"resolution,omitempty"`
	ResolutionReason   string `json:"resolution_reason,omitempty"`
	EvidenceReference  string `json:"evidence_reference,omitempty"`
	ResolvedBy         string `json:"resolved_by,omitempty"`
	ResolvedFromStatus string `json:"resolved_from_status,omitempty"`
	ResolutionVersion  int64  `json:"resolution_version,omitempty"`
	PreparedAtMS       int64  `json:"prepared_at_ms"`
	StartedAtMS        *int64 `json:"started_at_ms,omitempty"`
	ResolvedAtMS       *int64 `json:"resolved_at_ms,omitempty"`
	CreatedAtMS        int64  `json:"created_at_ms"`
	UpdatedAtMS        int64  `json:"updated_at_ms"`
}

type BeginExternalOperationParams struct {
	RunID             string
	StepName          string
	Provider          string
	OperationType     string
	Fingerprint       string
	LeaseOwner        string
	FencingGeneration int64
}

// CompleteExternalOperationParams fences a completion: a stale worker (lost
// lease/generation), a stale operation version, or an admin resolution cannot
// silently overwrite the current state.
type CompleteExternalOperationParams struct {
	OperationKey      string
	Status            string
	ErrorClass        string
	LeaseOwner        string
	FencingGeneration int64
	OperationVersion  int64
}

type ExternalOperationFilter struct {
	Status   string
	Provider string
	RunID    string
	Limit    int
	Cursor   string
}

type ExternalOperationsPage struct {
	Items                    []ExternalOperation `json:"items"`
	NextCursor               string              `json:"next_cursor,omitempty"`
	StatusCounts             map[string]int64    `json:"status_counts"`
	TotalNeedsReconciliation int64               `json:"total_needs_reconciliation"`
	QueryScope               map[string]any      `json:"query_scope"`
	GeneratedAtMS            int64               `json:"generated_at_ms"`
}

const externalOperationColumns = `id, run_id, step_name, operation_key, provider, operation_type,
	request_fingerprint, status, attempt_count, version, worker_id, lease_generation,
	COALESCE(last_error_class,''), COALESCE(resolution,''), COALESCE(resolution_reason,''),
	COALESCE(evidence_reference,''), COALESCE(resolved_by,''), COALESCE(resolved_from_status,''),
	resolution_version, prepared_at_ms, started_at_ms, resolved_at_ms, created_at_ms, updated_at_ms`

type externalRowScanner interface {
	Scan(dest ...any) error
}

func scanExternalOperation(sc externalRowScanner) (ExternalOperation, error) {
	var op ExternalOperation
	err := sc.Scan(&op.ID, &op.RunID, &op.StepName, &op.OperationKey, &op.Provider, &op.OperationType,
		&op.Fingerprint, &op.Status, &op.AttemptCount, &op.Version, &op.WorkerID, &op.LeaseGeneration,
		&op.LastErrorClass, &op.Resolution, &op.ResolutionReason, &op.EvidenceReference,
		&op.ResolvedBy, &op.ResolvedFromStatus, &op.ResolutionVersion, &op.PreparedAtMS,
		&op.StartedAtMS, &op.ResolvedAtMS, &op.CreatedAtMS, &op.UpdatedAtMS)
	return op, err
}

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

	existing, err := scanExternalOperation(tx.QueryRowContext(ctx,
		`SELECT `+externalOperationColumns+` FROM external_operations WHERE operation_key=?`, opKey))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		existing = ExternalOperation{
			ID: security.NewID(), RunID: p.RunID, StepName: p.StepName, OperationKey: opKey,
			Provider: p.Provider, OperationType: p.OperationType, Fingerprint: p.Fingerprint,
			Status: ExternalPrepared, AttemptCount: 1, Version: 1, WorkerID: p.LeaseOwner,
			LeaseGeneration: p.FencingGeneration, PreparedAtMS: now, CreatedAtMS: now, UpdatedAtMS: now,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO external_operations
			(id, run_id, step_name, operation_key, provider, operation_type, request_fingerprint,
			 status, attempt_count, version, worker_id, lease_generation, prepared_at_ms,
			 started_at_ms, created_at_ms, updated_at_ms)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,?,?)`,
			existing.ID, existing.RunID, existing.StepName, existing.OperationKey, existing.Provider,
			existing.OperationType, existing.Fingerprint, existing.Status, existing.AttemptCount,
			existing.Version, existing.WorkerID, existing.LeaseGeneration, existing.PreparedAtMS,
			existing.CreatedAtMS, existing.UpdatedAtMS); err != nil {
			return ExternalOperation{}, err
		}
	case err != nil:
		return ExternalOperation{}, err
	case existing.Provider != p.Provider || existing.OperationType != p.OperationType ||
		existing.Fingerprint != p.Fingerprint:
		return ExternalOperation{}, fmt.Errorf("%w: operation key reused with a different request", ErrConflict)
	case existing.Status == ExternalFailedDefinite:
		res, err := tx.ExecContext(ctx, `UPDATE external_operations
			SET status=?, attempt_count=attempt_count+1, version=version+1, worker_id=?,
			    lease_generation=?, started_at_ms=NULL, last_error_class=NULL, updated_at_ms=?
			WHERE id=? AND status=? AND version=?`,
			ExternalPrepared, p.LeaseOwner, p.FencingGeneration, now, existing.ID,
			ExternalFailedDefinite, existing.Version)
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
		existing.WorkerID = p.LeaseOwner
		existing.LeaseGeneration = p.FencingGeneration
		existing.StartedAtMS = nil
		existing.UpdatedAtMS = now
	case existing.Status == ExternalPrepared:
		// Candidate permit only; MarkExternalOperationInFlight picks one winner.
	default:
		return ExternalOperation{}, fmt.Errorf("%w: status=%s", ErrExternalOperationBlocked, existing.Status)
	}
	if err := tx.Commit(); err != nil {
		return ExternalOperation{}, err
	}
	return existing, nil
}

// MarkExternalOperationInFlight is the single PREPARED->IN_FLIGHT CAS. It
// revalidates the run lease and fencing generation.
func (s *Store) MarkExternalOperationInFlight(ctx context.Context, operationKey, leaseOwner string, fencingGeneration int64) (ExternalOperation, error) {
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return ExternalOperation{}, err
	}
	defer tx.Rollback()
	var runID, owner string
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
		SET status=?, worker_id=?, lease_generation=?, started_at_ms=?, updated_at_ms=?, version=version+1
		WHERE operation_key=? AND status=?`,
		ExternalInFlight, leaseOwner, fencingGeneration, now, now, operationKey, ExternalPrepared)
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

// CompleteExternalOperation fences completion on run lease/gen, operation
// version and current state. If the lease is no longer valid (or was superseded
// by a newer generation) while the operation is IN_FLIGHT, the remote outcome
// may be successful but cannot be confirmed by this worker: the operation is
// conservatively moved to RECONCILIATION_REQUIRED and ErrLeaseLost is returned.
func (s *Store) CompleteExternalOperation(ctx context.Context, p CompleteExternalOperationParams) (ExternalOperation, error) {
	switch p.Status {
	case ExternalConfirmed, ExternalOutcomeUnknown, ExternalReconciliationReq, ExternalFailedDefinite:
	default:
		return ExternalOperation{}, fmt.Errorf("storage: invalid external completion status %q", p.Status)
	}
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return ExternalOperation{}, err
	}
	defer tx.Rollback()
	var runID, status, owner, opID string
	var opVersion, gen int64
	var leaseUntil sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT e.id, e.run_id, e.status, e.version,
		COALESCE(r.lease_owner,''), r.fencing_generation, r.lease_until_ms
		FROM external_operations e JOIN runs r ON r.id=e.run_id WHERE e.operation_key=?`, p.OperationKey).
		Scan(&opID, &runID, &status, &opVersion, &owner, &gen, &leaseUntil); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExternalOperation{}, ErrNotFound
		}
		return ExternalOperation{}, err
	}
	if status == ExternalResolved {
		return ExternalOperation{}, fmt.Errorf("%w: operator already resolved the operation", ErrConflict)
	}
	if status != ExternalInFlight {
		return ExternalOperation{}, fmt.Errorf("%w: operation is %s, not IN_FLIGHT", ErrConflict, status)
	}
	if p.OperationVersion > 0 && opVersion != p.OperationVersion {
		return ExternalOperation{}, fmt.Errorf("%w: operation version changed", ErrConflict)
	}
	leaseValid := owner == p.LeaseOwner && gen == p.FencingGeneration &&
		leaseUntil.Valid && leaseUntil.Int64 > now
	if !leaseValid {
		// Preserve the possibility of a remote side effect for an operator.
		res, err := tx.ExecContext(ctx, `UPDATE external_operations
			SET status=?, last_error_class=?, version=version+1, updated_at_ms=?
			WHERE operation_key=? AND status=? AND version=?`,
			ExternalReconciliationReq, "LEASE_LOST_AFTER_REMOTE_CALL_MAYBE_SENT", now,
			p.OperationKey, ExternalInFlight, opVersion)
		if err != nil {
			return ExternalOperation{}, err
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			detail := s.redactor.SanitizeEventPayload(map[string]any{
				"operation_id": opID, "run_id": runID, "from_status": ExternalInFlight,
				"to_status": ExternalReconciliationReq, "version_before": opVersion,
				"version_after": opVersion + 1, "reason": "LEASE_LOST_AFTER_REMOTE_CALL_MAYBE_SENT",
			})
			raw, _ := json.Marshal(detail)
			if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (actor, action, run_id, detail_json, created_at_ms)
				VALUES (?,?,?,?,?)`, "system:external-operation", "external_operation.completion_fenced",
				runID, string(raw), now); err != nil {
				return ExternalOperation{}, err
			}
			if err := tx.Commit(); err != nil {
				return ExternalOperation{}, err
			}
			return ExternalOperation{}, fmt.Errorf("%w: lease lost; operation requires reconciliation", ErrLeaseLost)
		}
		return ExternalOperation{}, fmt.Errorf("%w: operation changed during completion", ErrConflict)
	}
	res, err := tx.ExecContext(ctx, `UPDATE external_operations
		SET status=?, last_error_class=?, version=version+1, updated_at_ms=?
		WHERE operation_key=? AND status=? AND version=?`,
		p.Status, p.ErrorClass, now, p.OperationKey, ExternalInFlight, opVersion)
	if err != nil {
		return ExternalOperation{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ExternalOperation{}, fmt.Errorf("%w: completion raced or operation changed", ErrConflict)
	}
	if err := tx.Commit(); err != nil {
		return ExternalOperation{}, err
	}
	return s.GetExternalOperation(ctx, p.OperationKey)
}

func (s *Store) GetExternalOperation(ctx context.Context, operationKey string) (ExternalOperation, error) {
	op, err := scanExternalOperation(s.readDB.QueryRowContext(ctx,
		`SELECT `+externalOperationColumns+` FROM external_operations WHERE operation_key=?`, operationKey))
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalOperation{}, ErrNotFound
	}
	return op, err
}

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
	q := `SELECT ` + externalOperationColumns + ` FROM external_operations WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY created_at_ms DESC, id DESC LIMIT ?`
	args = append(args, f.Limit+1)
	rows, err := s.readDB.QueryContext(ctx, q, args...)
	if err != nil {
		return ExternalOperationsPage{}, err
	}
	defer rows.Close()
	page := ExternalOperationsPage{Items: []ExternalOperation{}, StatusCounts: map[string]int64{},
		QueryScope:    map[string]any{"status": status, "provider": f.Provider, "run_id": f.RunID},
		GeneratedAtMS: s.nowMS()}
	var count int
	for rows.Next() {
		op, err := scanExternalOperation(rows)
		if err != nil {
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

type ExternalResolution struct {
	Resolution        string
	Reason            string
	EvidenceReference string
	ExpectedVersion   int64
	Actor             string
	RequestedAtMS     int64
}

// ResolveExternalOperation closes an ambiguous or confirmed operation. IN_FLIGHT
// is never resolvable directly: the worker lifecycle barrier converts leftover
// IN_FLIGHT rows to RECONCILIATION_REQUIRED before an operator may decide. The
// state change and audit row commit atomically.
func (s *Store) ResolveExternalOperation(ctx context.Context, id string, r ExternalResolution) (ExternalOperation, error) {
	switch r.Resolution {
	case "RESOURCE_CONFIRMED", "NO_RESOURCE_CREATED", "MANUAL_COMPENSATION_COMPLETED", "UNRESOLVED":
	default:
		return ExternalOperation{}, fmt.Errorf("%w: invalid resolution", ErrConflict)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return ExternalOperation{}, fmt.Errorf("%w: resolution reason is required", ErrConflict)
	}
	if len(r.Reason) > 2000 || len(r.EvidenceReference) > 512 ||
		strings.ContainsAny(r.Reason, "\x00\r\n") || strings.ContainsAny(r.EvidenceReference, "\x00\r\n") {
		return ExternalOperation{}, fmt.Errorf("%w: resolution reason/evidence invalid or too long", ErrConflict)
	}
	if r.ExpectedVersion <= 0 {
		return ExternalOperation{}, fmt.Errorf("%w: expected_version is required", ErrConflict)
	}
	if strings.TrimSpace(r.Actor) == "" {
		return ExternalOperation{}, fmt.Errorf("%w: actor is required", ErrConflict)
	}
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return ExternalOperation{}, err
	}
	defer tx.Rollback()
	op, err := scanExternalOperation(tx.QueryRowContext(ctx,
		`SELECT `+externalOperationColumns+` FROM external_operations WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalOperation{}, ErrNotFound
	}
	if err != nil {
		return ExternalOperation{}, err
	}
	switch op.Status {
	case ExternalOutcomeUnknown, ExternalReconciliationReq:
		// normal operator path
	case ExternalConfirmed:
		// post-commit-failure recovery; must not claim no-resource-created
		if r.Resolution == "NO_RESOURCE_CREATED" {
			return ExternalOperation{}, fmt.Errorf("%w: CONFIRMED cannot be resolved as no-resource-created", ErrConflict)
		}
	case ExternalInFlight:
		return ExternalOperation{}, fmt.Errorf("%w: worker lifecycle barrier has not cleared IN_FLIGHT; resolve after restart/stop recovery", ErrConflict)
	default:
		return ExternalOperation{}, fmt.Errorf("%w: operation status %s is not resolvable", ErrConflict, op.Status)
	}
	// An UNRESOLVED review is NOT a closure. Keep the operation in its pending
	// status (OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED, or CONFIRMED for a
	// post-commit recovery review), record an auditable review event and bump
	// the optimistic version so a concurrent reviewer cannot reuse it.
	if r.Resolution == "UNRESOLVED" {
		res, err := tx.ExecContext(ctx, `UPDATE external_operations
			SET version=version+1, updated_at_ms=?
			WHERE id=? AND version=? AND status IN (?,?,?)`,
			now, id, r.ExpectedVersion, ExternalOutcomeUnknown, ExternalReconciliationReq, ExternalConfirmed)
		if err != nil {
			return ExternalOperation{}, err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ExternalOperation{}, fmt.Errorf("%w: version conflict or operation is not reviewable", ErrConflict)
		}
		detail := s.redactor.SanitizeEventPayload(map[string]any{
			"operation_id": id, "run_id": op.RunID, "resolution": r.Resolution,
			"reason": r.Reason, "evidence_reference": r.EvidenceReference, "actor": r.Actor,
			"from_status": op.Status, "to_status": op.Status,
			"version_before": op.Version, "version_after": op.Version + 1,
			"requested_at_ms": r.RequestedAtMS, "reviewed_at_ms": now,
			"provider": op.Provider, "operation_type": op.OperationType,
			"review_action": "UNRESOLVED_KEEP_PENDING",
		})
		rawDetail, err := json.Marshal(detail)
		if err != nil {
			return ExternalOperation{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (actor, action, run_id, detail_json, created_at_ms)
			VALUES (?,?,?,?,?)`, r.Actor, "external_operation.review", op.RunID, string(rawDetail), now); err != nil {
			return ExternalOperation{}, err
		}
		if err := tx.Commit(); err != nil {
			return ExternalOperation{}, err
		}
		op.Version++
		op.UpdatedAtMS = now
		return op, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE external_operations
		SET status=?, resolution=?, resolution_reason=?, evidence_reference=?, resolved_by=?,
		    resolved_from_status=?, resolution_version=?, resolved_at_ms=?, version=version+1, updated_at_ms=?
		WHERE id=? AND version=? AND status IN (?,?,?)`,
		ExternalResolved, r.Resolution, r.Reason, r.EvidenceReference, r.Actor,
		op.Status, op.Version, now, now,
		id, r.ExpectedVersion, ExternalOutcomeUnknown, ExternalReconciliationReq, ExternalConfirmed)
	if err != nil {
		return ExternalOperation{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ExternalOperation{}, fmt.Errorf("%w: version conflict or operation is not resolvable", ErrConflict)
	}
	detail := s.redactor.SanitizeEventPayload(map[string]any{
		"operation_id": id, "run_id": op.RunID, "resolution": r.Resolution, "reason": r.Reason,
		"evidence_reference": r.EvidenceReference, "actor": r.Actor,
		"from_status": op.Status, "to_status": ExternalResolved,
		"version_before": op.Version, "version_after": op.Version + 1,
		"requested_at_ms": r.RequestedAtMS, "resolved_at_ms": now,
		"provider": op.Provider, "operation_type": op.OperationType,
	})
	rawDetail, err := json.Marshal(detail)
	if err != nil {
		return ExternalOperation{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (actor, action, run_id, detail_json, created_at_ms)
		VALUES (?,?,?,?,?)`, r.Actor, "external_operation.resolve", op.RunID, string(rawDetail), now); err != nil {
		return ExternalOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExternalOperation{}, err
	}
	return s.GetExternalOperation(ctx, op.OperationKey)
}

// RecoverExternalOperations is the single-process worker lifecycle barrier. It
// must run after the previous process is known dead (single-writer lock) and
// before workers start, and again after graceful Stop. PREPARED means the
// provider call was never started (Mark precedes it), so it can be marked
// FAILED_DEFINITE. IN_FLIGHT may have reached the provider: it becomes
// RECONCILIATION_REQUIRED; no automatic retry.
func (s *Store) RecoverExternalOperations(ctx context.Context, reason string) (int, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "WORKER_STOPPED"
	}
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE external_operations
		SET status=?, last_error_class=?, version=version+1, updated_at_ms=?
		WHERE status=?`, ExternalFailedDefinite, reason, now, ExternalPrepared)
	if err != nil {
		return 0, err
	}
	n1, _ := res.RowsAffected()
	res, err = tx.ExecContext(ctx, `UPDATE external_operations
		SET status=?, last_error_class=?, version=version+1, updated_at_ms=?
		WHERE status=?`, ExternalReconciliationReq, reason, now, ExternalInFlight)
	if err != nil {
		return 0, err
	}
	n2, _ := res.RowsAffected()
	if n1+n2 > 0 {
		detail := s.redactor.SanitizeEventPayload(map[string]any{
			"reason": reason, "prepared_failed_definite": n1, "inflight_reconciliation": n2,
		})
		raw, _ := json.Marshal(detail)
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (actor, action, run_id, detail_json, created_at_ms)
			VALUES (?,?,?,?,?)`, "system:worker-lifecycle", "external_operation.lifecycle_recovery", "", string(raw), now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n1 + n2), nil
}

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

// ListExternalOperationAudit returns the audited review/resolution history for
// one operation (newest first). It intentionally derives operation_id from the
// audit detail instead of trusting a caller-supplied run.
func (s *Store) ListExternalOperationAudit(ctx context.Context, operationID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT id, actor, action, COALESCE(run_id,''), detail_json, created_at_ms
		FROM audit_log WHERE json_extract(detail_json, '$.operation_id')=? ORDER BY id DESC LIMIT ?`,
		operationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var raw string
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.RunID, &raw, &e.CreatedAtMS); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &e.Detail); err != nil {
			e.Detail = map[string]any{}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) GetExternalOperationByID(ctx context.Context, id string) (ExternalOperation, error) {
	op, err := scanExternalOperation(s.readDB.QueryRowContext(ctx,
		`SELECT `+externalOperationColumns+` FROM external_operations WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalOperation{}, ErrNotFound
	}
	return op, err
}
