package workflow

import (
	"context"
	"time"

	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

// StepHandler executes one workflow step. It must be idempotent with respect to
// its operation_key and must not persist secrets.
type StepHandler func(ctx context.Context, rc *RunContext) (Outcome, error)

// StepDef is one step in a template.
type StepDef struct {
	Name        string
	ActiveState state.RunState
	Next        state.RunState
	Handler     StepHandler
}

// Template is a durable workflow definition.
type Template struct {
	Name          string
	Mode          string
	RequiredKinds []provider.Kind
	Steps         []StepDef
}

// StepDef returns the definition for a step name.
func (t *Template) StepDef(name string) *StepDef {
	for i := range t.Steps {
		if t.Steps[i].Name == name {
			return &t.Steps[i]
		}
	}
	return nil
}

// StepNames returns the ordered step names.
func (t *Template) StepNames() []string {
	out := make([]string, 0, len(t.Steps))
	for _, s := range t.Steps {
		out = append(out, s.Name)
	}
	return out
}

func (e *Engine) buildTemplates() []*Template {
	return []*Template{
		{
			Name:          "sandbox-email-verify-oidc",
			Mode:          "sandbox-e2e",
			RequiredKinds: []provider.Kind{provider.KindMailbox, provider.KindIdentity, provider.KindSandbox},
			Steps: []StepDef{
				{Name: "validate", ActiveState: state.Validating, Next: state.PreparingMailbox, Handler: e.stepValidate},
				{Name: "prepare_mailbox", ActiveState: state.PreparingMailbox, Next: state.MailboxReady, Handler: e.stepPrepareMailbox},
				{Name: "request_verification", ActiveState: state.VerificationRequest, Next: state.WaitingEmail, Handler: e.stepRequestVerification},
				{Name: "wait_email", ActiveState: state.WaitingEmail, Next: state.IdentityVerified, Handler: e.stepWaitEmail},
				{Name: "oauth", ActiveState: state.OAuthPending, Next: state.CredentialVerified, Handler: e.stepSandboxOAuth},
				{Name: "verify_credential", ActiveState: state.CredentialVerified, Next: state.Completed, Handler: e.stepVerifyCredential},
			},
		},
		{
			Name:          "cloudflare-mail-smoke",
			Mode:          "cloudflare-integration",
			RequiredKinds: []provider.Kind{provider.KindMailbox},
			Steps: []StepDef{
				{Name: "validate", ActiveState: state.Validating, Next: state.PreparingMailbox, Handler: e.stepValidate},
				{Name: "prepare_mailbox", ActiveState: state.PreparingMailbox, Next: state.MailboxReady, Handler: e.stepPrepareMailbox},
				{Name: "request_verification", ActiveState: state.VerificationRequest, Next: state.WaitingEmail, Handler: e.stepRequestVerification},
				{Name: "wait_email", ActiveState: state.WaitingEmail, Next: state.Completed, Handler: e.stepWaitEmail},
			},
		},
		{
			Name:          "github-existing-email",
			Mode:          "real-account",
			RequiredKinds: []provider.Kind{provider.KindMailbox, provider.KindEmail},
			Steps: []StepDef{
				{Name: "validate", ActiveState: state.Validating, Next: state.PreparingMailbox, Handler: e.stepValidate},
				{Name: "prepare_mailbox", ActiveState: state.PreparingMailbox, Next: state.MailboxReady, Handler: e.stepPrepareMailbox},
				{Name: "request_verification", ActiveState: state.VerificationRequest, Next: state.WaitingUserAction, Handler: e.githubAddEmail},
				{Name: "wait_human_verification", ActiveState: state.WaitingUserAction, Next: state.IdentityVerified, Handler: e.stepGithubWaitVerification},
				{Name: "complete", ActiveState: state.CredentialVerified, Next: state.Completed, Handler: e.stepComplete},
			},
		},
		{
			Name:          "oauth-existing-account",
			Mode:          "real-account",
			RequiredKinds: []provider.Kind{provider.KindOAuth, provider.KindCredential},
			Steps: []StepDef{
				{Name: "validate", ActiveState: state.Validating, Next: state.OAuthPending, Handler: e.stepValidate},
				{Name: "oauth_start", ActiveState: state.OAuthPending, Next: state.WaitingUserAction, Handler: e.stepOAuthStart},
				{Name: "wait_oauth", ActiveState: state.WaitingUserAction, Next: state.CredentialVerified, Handler: e.stepWaitOAuth},
				{Name: "verify_credential", ActiveState: state.CredentialVerified, Next: state.Completed, Handler: e.stepVerifyCredential},
			},
		},
	}
}

// OIDCScopes returns the configured authorization scopes.
func (c Config) OIDCScopesOrDefault() []string {
	if len(c.OIDCScopes) > 0 {
		return c.OIDCScopes
	}
	return []string{"openid", "email", "profile"}
}

// PollDelay returns the configured poll interval bounded to a sane minimum.
func (c Config) PollDelay() time.Duration {
	if c.PollInterval < time.Second {
		return time.Second
	}
	return c.PollInterval
}
