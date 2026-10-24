// Package state defines the durable workflow state machine from
// docs/03-workflows.md. It is deliberately pure (no I/O) so the transition
// matrix can be exhaustively unit tested.
package state

import "fmt"

// RunState is the operator-facing run status.
type RunState string

const (
	Created             RunState = "CREATED"
	Validating          RunState = "VALIDATING"
	PreparingMailbox    RunState = "PREPARING_MAILBOX"
	MailboxReady        RunState = "MAILBOX_READY"
	VerificationRequest RunState = "VERIFICATION_REQUESTED"
	WaitingEmail        RunState = "WAITING_EMAIL"
	WaitingUserAction   RunState = "WAITING_USER_ACTION"
	IdentityVerified    RunState = "IDENTITY_VERIFIED"
	OAuthPending        RunState = "OAUTH_PENDING"
	CredentialVerified  RunState = "CREDENTIAL_VERIFIED"
	Completed           RunState = "COMPLETED"
	RetryWait           RunState = "RETRY_WAIT"
	ReauthorizeRequired RunState = "REAUTHORIZE_REQUIRED"
	FailedTerminal      RunState = "FAILED_TERMINAL"
	CancelRequested     RunState = "CANCEL_REQUESTED"
	Cancelled           RunState = "CANCELLED"
)

// AllRunStates is the closed set of persisted run states.
var AllRunStates = []RunState{
	Created, Validating, PreparingMailbox, MailboxReady, VerificationRequest,
	WaitingEmail, WaitingUserAction, IdentityVerified, OAuthPending,
	CredentialVerified, Completed, RetryWait, ReauthorizeRequired,
	FailedTerminal, CancelRequested, Cancelled,
}

// StepState is the per-step execution status. It is separate from RunState.
type StepState string

const (
	StepPending             StepState = "PENDING"
	StepClaimed             StepState = "CLAIMED"
	StepSucceeded           StepState = "SUCCEEDED"
	StepRetryWait           StepState = "RETRY_WAIT"
	StepWaitingEmail        StepState = "WAITING_EMAIL"
	StepWaitingUserAction   StepState = "WAITING_USER_ACTION"
	StepReauthorizeRequired StepState = "REAUTHORIZE_REQUIRED"
	StepFailedTerminal      StepState = "FAILED_TERMINAL"
	StepCancelled           StepState = "CANCELLED"
	StepSkipped             StepState = "SKIPPED"
)

// IsTerminal reports whether no worker may ever claim the run again.
func (s RunState) IsTerminal() bool {
	return s == Completed || s == FailedTerminal || s == Cancelled
}

// IsWait reports whether the run waits for an external/human event and must not
// hold a worker lease.
func (s RunState) IsWait() bool {
	switch s {
	case WaitingEmail, WaitingUserAction, RetryWait, ReauthorizeRequired:
		return true
	default:
		return false
	}
}

// NeedsManualResume reports whether POST /v1/runs/{id}/resume is legal from s.
func (s RunState) NeedsManualResume() bool {
	switch s {
	case WaitingUserAction, RetryWait, ReauthorizeRequired:
		return true
	default:
		return false
	}
}

var transitions = map[RunState][]RunState{
	Created:             {Validating, CancelRequested, Cancelled, FailedTerminal},
	Validating:          {PreparingMailbox, OAuthPending, CancelRequested, Cancelled, FailedTerminal, RetryWait},
	PreparingMailbox:    {MailboxReady, RetryWait, CancelRequested, Cancelled, FailedTerminal},
	MailboxReady:        {VerificationRequest, RetryWait, CancelRequested, Cancelled, FailedTerminal},
	VerificationRequest: {WaitingEmail, WaitingUserAction, IdentityVerified, RetryWait, CancelRequested, Cancelled, FailedTerminal},
	WaitingEmail:        {WaitingEmail, IdentityVerified, WaitingUserAction, Completed, RetryWait, CancelRequested, Cancelled, FailedTerminal, ReauthorizeRequired},
	WaitingUserAction:   {WaitingUserAction, IdentityVerified, CredentialVerified, WaitingEmail, VerificationRequest, RetryWait, ReauthorizeRequired, CancelRequested, Cancelled, FailedTerminal},
	IdentityVerified:    {OAuthPending, CredentialVerified, Completed, RetryWait, CancelRequested, Cancelled, FailedTerminal},
	OAuthPending:        {OAuthPending, WaitingUserAction, CredentialVerified, ReauthorizeRequired, RetryWait, CancelRequested, Cancelled, FailedTerminal},
	CredentialVerified:  {Completed, RetryWait, ReauthorizeRequired, CancelRequested, Cancelled, FailedTerminal},
	RetryWait: {
		Validating, PreparingMailbox, MailboxReady, VerificationRequest, WaitingEmail,
		WaitingUserAction, IdentityVerified, OAuthPending, CredentialVerified,
		ReauthorizeRequired, CancelRequested, FailedTerminal, Cancelled,
	},
	ReauthorizeRequired: {OAuthPending, WaitingUserAction, RetryWait, CancelRequested, FailedTerminal, Cancelled},
	CancelRequested:     {Cancelled},
	Completed:           {},
	FailedTerminal:      {},
	Cancelled:           {},
}

// CanTransition validates an edge. Unknown states fail closed.
func CanTransition(from, to RunState) error {
	allowed, ok := transitions[from]
	if !ok {
		return fmt.Errorf("unknown run state %q", from)
	}
	for _, candidate := range allowed {
		if candidate == to {
			return nil
		}
	}
	return fmt.Errorf("illegal transition %s -> %s", from, to)
}

// AllowedTransitions returns a copy of the allowlist for tests and docs.
func AllowedTransitions(from RunState) []RunState {
	return append([]RunState(nil), transitions[from]...)
}

// ActiveStateForStepName maps the template step to the run state it runs under.
// Retry and resume transitions restore this state before re-executing a step.
func ActiveStateForStepName(step string) RunState {
	switch step {
	case "validate":
		return Validating
	case "prepare_mailbox":
		return PreparingMailbox
	case "request_verification":
		return VerificationRequest
	case "wait_email":
		return WaitingEmail
	case "confirm_email":
		return WaitingEmail
	case "verify_identity":
		return WaitingEmail
	case "oauth_start":
		return OAuthPending
	case "oauth_poll":
		return OAuthPending
	case "verify_credential":
		return CredentialVerified
	case "complete":
		return CredentialVerified
	default:
		return Validating
	}
}
