package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ClaimRequest describes who may claim and how long the lease lasts.
type ClaimRequest struct {
	Owner            string
	LeaseTTL         time.Duration
	RunnableStatuses []string
	// ActiveState resolves the run state a step runs under. It is provided by
	// the engine and keeps storage free of workflow-specific knowledge.
	ActiveState func(template, stepName string) (string, bool)
}

// Claim is a leased step assignment. The fencing generation must be presented
// back on commit; a stale worker is rejected.
type Claim struct {
	Run             Run
	StepName        string
	StepStatus      string
	Attempt         int64
	Failures        int64
	Owner           string
	Generation      int64
	StateBefore     string
	StateAfter      string
	CancelRequested bool
	LeaseUntilMS    int64
}

// ClaimNext leases the next runnable step. The whole selection, transition and
// lease bump happens inside one BEGIN IMMEDIATE transaction.
func (s *Store) ClaimNext(ctx context.Context, req ClaimRequest) (*Claim, error) {
	if req.Owner == "" || req.LeaseTTL <= 0 || len(req.RunnableStatuses) == 0 {
		return nil, errors.New("storage: invalid claim request")
	}
	if req.ActiveState == nil {
		return nil, errors.New("storage: ActiveState resolver is required")
	}
	now := s.nowMS()
	leaseUntil := now + req.LeaseTTL.Milliseconds()

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	placeholders := ""
	args := make([]any, 0, len(req.RunnableStatuses)+4)
	for i, status := range req.RunnableStatuses {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, status)
	}
	args = append(args, now, now, now, now)
	var runID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM runs
		WHERE status IN (`+placeholders+`)
		  AND (
		    (status IN ('RETRY_WAIT','REAUTHORIZE_REQUIRED','WAITING_USER_ACTION')
		      AND next_run_at_ms IS NOT NULL AND next_run_at_ms <= ?)
		    OR
		    (status NOT IN ('RETRY_WAIT','REAUTHORIZE_REQUIRED','WAITING_USER_ACTION')
		      AND (next_run_at_ms IS NULL OR next_run_at_ms <= ?))
		  )
		  AND (lease_until_ms IS NULL OR lease_until_ms <= ?)
		  AND deadline_ms > ?
		ORDER BY COALESCE(next_run_at_ms,0), created_at_ms
		LIMIT 1`, args...).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	run, err := s.getRunTx(ctx, tx, `id = ?`, runID)
	if err != nil {
		return nil, err
	}
	claim := &Claim{
		Run:          run,
		Owner:        req.Owner,
		Generation:   run.FencingGeneration + 1,
		StateBefore:  run.Status,
		StateAfter:   run.Status,
		LeaseUntilMS: leaseUntil,
	}
	effective := run.Status

	if effective != "CANCEL_REQUESTED" {
		// Retry / reauthorize / human-action waits resume the step's home state.
		switch effective {
		case "RETRY_WAIT", "REAUTHORIZE_REQUIRED", "WAITING_USER_ACTION":
			if run.ResumeState == "" {
				return nil, fmt.Errorf("storage: run %s in %s without resume_state", run.ID, effective)
			}
			if s.validate != nil {
				if err := s.validate(effective, run.ResumeState); err != nil {
					return nil, fmt.Errorf("%w: %v", ErrConflict, err)
				}
			}
			effective = run.ResumeState
		}

		if err := tx.QueryRowContext(ctx, `SELECT step_name FROM steps
			WHERE run_id = ? AND status NOT IN ('SUCCEEDED','SKIPPED')
			ORDER BY rowid LIMIT 1`, run.ID).Scan(&claim.StepName); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w: run %s has no pending step", ErrConflict, run.ID)
			}
			return nil, err
		}
		active, ok := req.ActiveState(run.Template, claim.StepName)
		if !ok {
			return nil, fmt.Errorf("storage: unknown step %q for template %q", claim.StepName, run.Template)
		}
		if effective != active {
			if s.validate != nil {
				if err := s.validate(effective, active); err != nil {
					return nil, fmt.Errorf("%w: %v", ErrConflict, err)
				}
			}
			effective = active
		}
		if _, err := tx.ExecContext(ctx, `UPDATE steps SET status='CLAIMED',
			attempt_count=attempt_count+1,
			started_at_ms=COALESCE(started_at_ms, ?), updated_at_ms=?
			WHERE run_id=? AND step_name=?`, now, now, run.ID, claim.StepName); err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT attempt_count, failure_count FROM steps
			WHERE run_id=? AND step_name=?`, run.ID, claim.StepName).Scan(&claim.Attempt, &claim.Failures); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=?, lease_owner=?, lease_until_ms=?,
		fencing_generation=?, updated_at_ms=?, version=version+1, action_required=0
		WHERE id=?`, effective, req.Owner, leaseUntil, claim.Generation, now, run.ID); err != nil {
		return nil, err
	}
	claim.StateAfter = effective
	claim.CancelRequested = run.Status == "CANCEL_REQUESTED"
	claim.Run.Status = effective
	claim.Run.LeaseOwner = req.Owner
	claim.Run.LeaseUntilMS = &leaseUntil
	claim.Run.FencingGeneration = claim.Generation
	claim.Run.UpdatedAtMS = now

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claim, nil
}

// StepResult is the outcome of executing a step.
type StepResult struct {
	NextState      string
	StepStatus     string
	ErrorClass     string
	ErrorSummary   string
	RetryAtMS      *int64
	Result         map[string]any
	WaitingReason  string
	ActionRequired bool
	ResumeState    string
	Events         []EventDraft
	Resources      []ResourceDraft
	// MailReceipts are inserted in the same transaction as the step commit.
	MailReceipts []MailReceiptDraft
	// CountFailure increments the step failure counter; used for retry budgets.
	CountFailure bool
}

// MailReceiptDraft records one consumed provider message. MessageKey is hashed
// before persistence; it is never stored in clear text.
type MailReceiptDraft struct {
	Provider   string
	MessageKey string
}

// EventDraft is an event appended atomically with the step result.
type EventDraft struct {
	Type    string
	Payload map[string]any
}

// ResourceDraft links an external resource to the run. ExternalID is hashed
// before storage; DisplayAlias is safe to show.
type ResourceDraft struct {
	Kind         string
	ExternalID   string
	DisplayAlias string
	SecretRef    string
	Extra        map[string]any
}

// CommitStepResult persists a step outcome after verifying the lease fencing
// token and the workflow transition. A stale worker gets ErrLeaseLost.
func (s *Store) CommitStepResult(ctx context.Context, claim *Claim, res StepResult) error {
	if claim == nil {
		return errors.New("storage: nil claim")
	}
	now := s.nowMS()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var (
		status, leaseOwner, resumeState string
		generation                      int64
	)
	if err := tx.QueryRowContext(ctx, `SELECT status, COALESCE(lease_owner,''), fencing_generation,
		COALESCE(resume_state,'') FROM runs WHERE id=?`, claim.Run.ID).
		Scan(&status, &leaseOwner, &generation, &resumeState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if leaseOwner != claim.Owner || generation != claim.Generation {
		return ErrLeaseLost
	}
	if status == "COMPLETED" || status == "FAILED_TERMINAL" || status == "CANCELLED" {
		return fmt.Errorf("%w: run already terminal", ErrLeaseLost)
	}

	// Cancellation wins over the worker result: stop subsequent steps, release
	// the lease, and never assume the remote side rolled back.
	if status == "CANCEL_REQUESTED" {
		if err := s.finalizeCancelTx(ctx, tx, claim.Run.ID, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	if status != claim.StateAfter {
		return fmt.Errorf("%w: run state changed under lease (%s != %s)", ErrLeaseLost, status, claim.StateAfter)
	}
	if s.validate != nil {
		if err := s.validate(status, res.NextState); err != nil {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
	}
	resultJSON := "{}"
	if len(res.Result) > 0 {
		filtered := s.redactor.SanitizeEventPayload(res.Result)
		if raw, err := json.Marshal(filtered); err == nil {
			resultJSON = string(raw)
		}
	}
	var finished any
	if isTerminalStep(res.StepStatus) {
		finished = now
	}
	failureDelta := 0
	if res.CountFailure {
		failureDelta = 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE steps SET status=?, error_class=?, error_summary=?,
		next_retry_at_ms=?, result_json=?, waiting_reason=?, updated_at_ms=?,
		finished_at_ms=?, failure_count=failure_count+?
		WHERE run_id=? AND step_name=?`,
		res.StepStatus, nullableString(res.ErrorClass), nullableString(truncate(res.ErrorSummary, 512)),
		nullableMS(res.RetryAtMS), resultJSON, nullableString(res.WaitingReason), now,
		finished, failureDelta, claim.Run.ID, claim.StepName); err != nil {
		return err
	}
	for _, ev := range res.Events {
		if _, err := s.appendEventTx(ctx, tx, claim.Run.ID, ev.Type, claim.StepName, ev.Payload, now); err != nil {
			return err
		}
	}
	for _, receipt := range res.MailReceipts {
		if receipt.Provider == "" || receipt.MessageKey == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO mail_receipts
			(run_id, provider, message_key_hmac, consumed_at_ms) VALUES (?,?,?,?)`,
			claim.Run.ID, receipt.Provider, s.messageHMAC(receipt.Provider, receipt.MessageKey), now); err != nil {
			return err
		}
	}
	for _, r := range res.Resources {
		extra := "{}"
		if len(r.Extra) > 0 {
			if raw, err := json.Marshal(r.Extra); err == nil {
				extra = string(raw)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO resources
			(id, run_id, resource_kind, external_id_hmac, display_alias, secret_ref, extra_json, created_at_ms)
			VALUES (?,?,?,?,?,?,?,?)`,
			s.newID(), claim.Run.ID, r.Kind, s.resourceHMAC(r.Kind, r.ExternalID),
			nullableString(r.DisplayAlias), nullableString(r.SecretRef), extra, now); err != nil {
			return err
		}
	}
	var finishedAt any
	if isTerminalRunState(res.NextState) {
		finishedAt = now
	}
	actionRequired := 0
	if res.ActionRequired {
		actionRequired = 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=?, next_run_at_ms=?, lease_owner=NULL,
		lease_until_ms=NULL, updated_at_ms=?, version=version+1, current_step=?,
		resume_state=?, last_error_class=?, action_required=?, finished_at_ms=?
		WHERE id=?`,
		res.NextState, nullableMS(res.RetryAtMS), now, claim.StepName,
		nullableString(res.ResumeState), nullableString(res.ErrorClass), actionRequired,
		finishedAt, claim.Run.ID); err != nil {
		return err
	}
	if ev, ok := terminalEventFor(res.NextState); ok {
		if _, err := s.appendEventTx(ctx, tx, claim.Run.ID, ev, claim.StepName, map[string]any{
			"failure_class": res.ErrorClass,
			"reason":        truncate(res.ErrorSummary, 256),
		}, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) finalizeCancelTx(ctx context.Context, tx *sql.Tx, runID string, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE steps SET status='CANCELLED', finished_at_ms=?,
		updated_at_ms=? WHERE run_id=? AND status NOT IN ('SUCCEEDED','SKIPPED','FAILED_TERMINAL','CANCELLED')`,
		now, now, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='CANCELLED', lease_owner=NULL,
		lease_until_ms=NULL, next_run_at_ms=NULL, finished_at_ms=?, updated_at_ms=?, version=version+1,
		action_required=0 WHERE id=?`, now, now, runID); err != nil {
		return err
	}
	_, err := s.appendEventTx(ctx, tx, runID, "RUN_CANCELLED", "", map[string]any{
		"reason": "cancelled by operator",
	}, now)
	return err
}

func terminalEventFor(status string) (string, bool) {
	switch status {
	case "COMPLETED":
		return "RUN_COMPLETED", true
	case "FAILED_TERMINAL":
		return "RUN_FAILED", true
	case "CANCELLED":
		return "RUN_CANCELLED", true
	default:
		return "", false
	}
}

func isTerminalRunState(status string) bool {
	switch status {
	case "COMPLETED", "FAILED_TERMINAL", "CANCELLED":
		return true
	default:
		return false
	}
}

func isTerminalStep(status string) bool {
	switch status {
	case "SUCCEEDED", "FAILED_TERMINAL", "CANCELLED", "SKIPPED":
		return true
	default:
		return false
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
