package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

// Outcome is what a step handler reports back to the engine.
type Outcome struct {
	NextState      state.RunState
	StepStatus     state.StepState
	ErrorClass     string
	ErrorSummary   string
	RetryAfter     time.Duration
	Wait           time.Duration
	Result         map[string]any
	WaitingReason  string
	ActionRequired bool
	ResumeState    state.RunState
	Events         []storage.EventDraft
	Resources      []storage.ResourceDraft
	// MailReceipts marks provider messages as consumed atomically with the
	// successful step commit, so a crash can never consume a link before the
	// step that used it is durable.
	MailReceipts []storage.MailReceiptDraft
	CountFailure bool
}

// WaitEmail reports that the run is waiting for an incoming verification email.
func WaitEmail(wait time.Duration, reason string) Outcome {
	return Outcome{
		NextState:     state.WaitingEmail,
		StepStatus:    state.StepWaitingEmail,
		Wait:          wait,
		WaitingReason: reason,
		ResumeState:   state.WaitingEmail,
	}
}

// WaitHuman reports that the run is waiting for an operator action.
func WaitHuman(wait time.Duration, reason string) Outcome {
	return Outcome{
		NextState:      state.WaitingUserAction,
		StepStatus:     state.StepWaitingUserAction,
		Wait:           wait,
		WaitingReason:  reason,
		ActionRequired: true,
		ResumeState:    state.WaitingUserAction,
	}
}

// RunContext is the only surface step handlers get. It exposes resolved
// providers, the encrypted secret service and persisted resource lookups, never
// raw credentials.
type RunContext struct {
	Engine    *Engine
	Run       storage.Run
	Step      storage.Step
	Template  *Template
	Def       *StepDef
	Now       time.Time
	Deadline  time.Time
	Selection provider.ProviderSnapshot
	Log       *slog.Logger
}

// Store exposes the persistence layer for resource/mail-receipt lookups.
func (rc *RunContext) Store() *storage.Store { return rc.Engine.store }

// Config exposes the engine limits to handlers.
func (rc *RunContext) Config() Config { return rc.Engine.cfg }

// Success is the common "step completed, move to the next state" outcome.
func (rc *RunContext) Success() Outcome {
	return Outcome{NextState: rc.Def.Next, StepStatus: state.StepSucceeded}
}

// MailboxProvider resolves the run's mailbox adapter (gated by enablement).
func (rc *RunContext) MailboxProvider() (provider.MailboxProvider, error) {
	return rc.Engine.registry.Mailbox(rc.Selection.Mailbox.Name)
}

// IdentityProvider resolves the run's identity adapter.
func (rc *RunContext) IdentityProvider() (provider.IdentityProvider, error) {
	return rc.Engine.registry.Identity(rc.Selection.Identity.Name)
}

// OAuthProvider resolves the run's OAuth adapter.
func (rc *RunContext) OAuthProvider() (provider.OAuthProvider, error) {
	return rc.Engine.registry.OAuth(rc.Selection.OAuth.Name)
}

// EmailManager resolves the run's existing-account email adapter.
func (rc *RunContext) EmailManager() (provider.EmailManager, error) {
	return rc.Engine.registry.EmailManager(rc.Selection.Email.Name)
}

// CredentialVerifier resolves the run's credential verifier.
func (rc *RunContext) CredentialVerifier() (provider.CredentialVerifier, error) {
	return rc.Engine.registry.Credential(rc.Selection.Credential.Name)
}

// SandboxDriver resolves the run's sandbox automation driver.
func (rc *RunContext) SandboxDriver() (provider.SandboxDriver, error) {
	return rc.Engine.registry.Sandbox(rc.Selection.Sandbox.Name)
}

// NewSecret encrypts a value and returns its opaque reference.
func (rc *RunContext) NewSecret(ctx context.Context, kind, plaintext string) (string, error) {
	return rc.Engine.secrets.PutNew(ctx, kind, plaintext)
}

// ResolveSecret decrypts an opaque reference.
func (rc *RunContext) ResolveSecret(ctx context.Context, ref string) (string, error) {
	return rc.Engine.secrets.Resolve(ctx, ref)
}

// MailboxResource reconstructs the provider mailbox handle from persisted data.
func (rc *RunContext) MailboxResource(ctx context.Context) (provider.Mailbox, error) {
	res, err := rc.Store().LatestResource(ctx, rc.Run.ID, "mailbox")
	if err != nil {
		return provider.Mailbox{}, fmt.Errorf("mailbox resource missing: %w", err)
	}
	var extra struct {
		Address           string `json:"address"`
		Provider          string `json:"provider"`
		ProviderMailboxID string `json:"provider_mailbox_id"`
	}
	if res.ExtraJSON != "" {
		_ = json.Unmarshal([]byte(res.ExtraJSON), &extra)
	}
	mb := provider.Mailbox{
		ID:        extra.ProviderMailboxID,
		Address:   extra.Address,
		Provider:  extra.Provider,
		SecretRef: res.SecretRef,
	}
	if mb.Address == "" {
		mb.Address = res.DisplayAlias
	}
	if mb.ID == "" {
		mb.ID = res.DisplayAlias
	}
	return mb, nil
}

// IdentityResource reconstructs the sandbox identity handle and password ref.
func (rc *RunContext) IdentityResource(ctx context.Context) (provider.IdentityHandle, string, error) {
	res, err := rc.Store().LatestResource(ctx, rc.Run.ID, "identity")
	if err != nil {
		return provider.IdentityHandle{}, "", fmt.Errorf("identity resource missing: %w", err)
	}
	var extra struct {
		SubjectID string `json:"subject_id"`
		Address   string `json:"address"`
		Provider  string `json:"provider"`
	}
	if res.ExtraJSON != "" {
		_ = json.Unmarshal([]byte(res.ExtraJSON), &extra)
	}
	return provider.IdentityHandle{
		SubjectID: extra.SubjectID,
		Address:   extra.Address,
		Provider:  extra.Provider,
	}, res.SecretRef, nil
}

// CredentialRef reads the opaque credential reference persisted by an earlier
// step. It never returns a raw token.
func (rc *RunContext) CredentialRef(ctx context.Context) (string, error) {
	res, err := rc.Store().LatestResource(ctx, rc.Run.ID, "credential_ref")
	if err != nil {
		return "", fmt.Errorf("credential resource missing: %w", err)
	}
	if res.SecretRef == "" {
		return "", fmt.Errorf("credential resource has no secret reference")
	}
	return res.SecretRef, nil
}

// OAuthSessionRef reads the opaque OAuth session reference.
func (rc *RunContext) OAuthSessionRef(ctx context.Context) (string, error) {
	res, err := rc.Store().LatestResource(ctx, rc.Run.ID, "oauth_session")
	if err != nil {
		return "", fmt.Errorf("oauth session resource missing: %w", err)
	}
	if res.SecretRef == "" {
		return "", fmt.Errorf("oauth session resource has no secret reference")
	}
	return res.SecretRef, nil
}

// OAuthAuthorizationURLRef reads the opaque reference of the stored
// authorization URL, when the adapter provided one.
func (rc *RunContext) OAuthAuthorizationURLRef(ctx context.Context) (string, error) {
	res, err := rc.Store().LatestResource(ctx, rc.Run.ID, "oauth_session")
	if err != nil {
		return "", err
	}
	var extra struct {
		AuthorizationURLSecretRef string `json:"authorization_url_secret_ref"`
	}
	if res.ExtraJSON != "" {
		_ = json.Unmarshal([]byte(res.ExtraJSON), &extra)
	}
	return extra.AuthorizationURLSecretRef, nil
}
