package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/storage"
)

// External side effects (mailbox provisioning) are audited in the durable event
// log. The remote create and the local commit cannot share a transaction, so we
// record PREPARED before the call, CONFIRMED in the step-commit transaction on
// success, and OUTCOME_UNKNOWN / RECONCILIATION_REQUIRED on ambiguous failures.
// The latest event per operation_key is the reconciliation state; see
// docs/adr/0001-external-operation-reconciliation.md.
func externalOperationKey(runID, step string) string {
	return runID + ":" + step + ":v1"
}

func externalRequestFingerprint(provider, name, domain string) string {
	sum := sha256.Sum256([]byte("external-op|" + provider + "|" + name + "|" + domain))
	// Truncated digest is enough for correlation and avoids storing long tokens.
	return hex.EncodeToString(sum[:8])
}

func externalOpPayload(runID, step, provider, opType, fingerprint, status, errClass string, attempt int64) map[string]any {
	return map[string]any{
		"operation_key":       externalOperationKey(runID, step),
		"provider":            provider,
		"operation_type":      opType,
		"request_fingerprint": fingerprint,
		"status":              status,
		"error_class":         errClass,
		"attempt":             attempt,
	}
}

// ExternalFailureStatus maps a provider error to the reconciliation state. A
// create call is a write: transport/5xx/timeouts may have landed remotely, so
// they must never be treated as "definitely not created".
func ExternalFailureStatus(err error) string {
	switch errs.ClassOf(err) {
	case errs.ClassConflict:
		return "RECONCILIATION_REQUIRED"
	case errs.ClassTimeoutUncertain, errs.ClassUpstreamTransient, errs.ClassInternal, errs.ClassProviderBug:
		return "OUTCOME_UNKNOWN"
	default:
		return "FAILED"
	}
}

// appendExternalEvent is best-effort for failure paths (the original provider
// error is more important) but fail-closed for the pre-call PREPARED write.
func (rc *RunContext) appendExternalEvent(ctx context.Context, eventType, status, errClass, provider, opType, fingerprint string) error {
	return rc.Store().AppendEvent(ctx, rc.Run.ID, eventType, rc.Def.Name,
		externalOpPayload(rc.Run.ID, rc.Def.Name, provider, opType, fingerprint, status, errClass, rc.Step.AttemptCount))
}

func (rc *RunContext) beginExternalOperation(ctx context.Context, provider, opType, fingerprint string) error {
	if err := rc.appendExternalEvent(ctx, storage.ExternalOperationEventPrefix+"PREPARED", "PREPARED", "", provider, opType, fingerprint); err != nil {
		return errs.Wrap(errs.ClassInternal, "external.prepare", "could not record external operation audit", err)
	}
	return nil
}

func (rc *RunContext) failedExternalOperation(ctx context.Context, provider, opType, fingerprint string, cause error) {
	status := ExternalFailureStatus(cause)
	eventType := storage.ExternalOperationEventPrefix + status
	if err := rc.appendExternalEvent(ctx, eventType, status, string(errs.ClassOf(cause)), provider, opType, fingerprint); err != nil {
		rc.Log.Error("external operation audit write failed", "error", err, "status", status)
	}
}

func (rc *RunContext) confirmedExternalOperation(provider, opType, fingerprint string) storage.EventDraft {
	return storage.EventDraft{
		Type: storage.ExternalOperationEventPrefix + "CONFIRMED",
		Payload: externalOpPayload(rc.Run.ID, rc.Def.Name, provider, opType, fingerprint,
			"CONFIRMED", "", rc.Step.AttemptCount),
	}
}
