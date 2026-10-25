package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/storage"
)

// External side effects use a durable write-ahead gate (migration 0004):
// BeginExternalOperation atomically acquires an IN_FLIGHT permit for the
// run/step operation key; CompleteExternalOperation is the only way to reach
// CONFIRMED/OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED/FAILED_DEFINITE. Ambiguous
// outcomes block all later automatic attempts until an admin resolves them.
// Events are human-readable audit side records; the table is authoritative.
func externalOperationKey(runID, step string) string {
	return runID + ":" + step + ":v1"
}

func externalRequestFingerprint(provider, name, domain string) string {
	sum := sha256.Sum256([]byte("external-op|" + provider + "|" + name + "|" + domain))
	return hex.EncodeToString(sum[:8])
}

// ExternalCompletionStatus maps a provider error to the final gate state. A
// create call is a write: transport/5xx/timeouts/cancel may have reached the
// provider, so they must never become FAILED_DEFINITE.
func ExternalCompletionStatus(err error) string {
	switch errs.ClassOf(err) {
	case errs.ClassConflict:
		return storage.ExternalReconciliationReq
	case errs.ClassTimeoutUncertain, errs.ClassUpstreamTransient, errs.ClassInternal,
		errs.ClassProviderBug, errs.ClassCancelled:
		return storage.ExternalOutcomeUnknown
	default:
		return storage.ExternalFailedDefinite
	}
}

func (rc *RunContext) beginExternalOperation(ctx context.Context, provider, opType, fingerprint string) (storage.ExternalOperation, error) {
	claim := rc.Claim
	op, err := rc.Store().BeginExternalOperation(ctx, storage.BeginExternalOperationParams{
		RunID:             rc.Run.ID,
		StepName:          rc.Def.Name,
		Provider:          provider,
		OperationType:     opType,
		Fingerprint:       fingerprint,
		LeaseOwner:        claim.Owner,
		FencingGeneration: claim.Generation,
	})
	if err != nil {
		if errors.Is(err, storage.ErrExternalOperationBlocked) || errors.Is(err, storage.ErrConflict) {
			return storage.ExternalOperation{}, errs.Wrap(errs.ClassConflict, "external.operation",
				"external operation is not retry-safe and requires reconciliation", err)
		}
		return storage.ExternalOperation{}, errs.Wrap(errs.ClassInternal, "external.operation",
			"could not acquire external operation permit", err)
	}
	// Only the atomic PREPARED -> IN_FLIGHT CAS grants the remote-call permit.
	// Concurrent workers may both hold a PREPARED candidate; exactly one wins
	// and losers must never call the provider.
	op, err = rc.Store().MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation)
	if err != nil {
		return storage.ExternalOperation{}, errs.Wrap(errs.ClassConflict, "external.operation",
			"could not acquire remote-write permit", err)
	}
	rc.auditExternal(ctx, "EXTERNAL_OPERATION_IN_FLIGHT", op, "")
	return op, nil
}

func (rc *RunContext) completeExternalOperation(ctx context.Context, op storage.ExternalOperation, status, errorClass string) error {
	updated, err := rc.Store().CompleteExternalOperation(ctx, op.OperationKey, status, errorClass)
	if err != nil {
		return err
	}
	rc.auditExternal(ctx, "EXTERNAL_OPERATION_"+status, updated, errorClass)
	return nil
}

func (rc *RunContext) auditExternal(ctx context.Context, eventType string, op storage.ExternalOperation, errorClass string) {
	payload := map[string]any{
		"operation_key":       op.OperationKey,
		"provider":            op.Provider,
		"operation_type":      op.OperationType,
		"request_fingerprint": op.Fingerprint,
		"status":              op.Status,
		"error_class":         errorClass,
		"attempt":             op.AttemptCount,
	}
	if err := rc.Store().AppendEvent(ctx, rc.Run.ID, eventType, rc.Def.Name, payload); err != nil {
		rc.Log.Error("external operation audit event failed", "error", err, "event", eventType)
	}
}
