package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

// CreateRunRequest is the validated control-plane creation request.
type CreateRunRequest struct {
	Template            string `json:"template"`
	Mode                string `json:"mode"`
	MailboxProvider     string `json:"mailbox_provider"`
	IdentityProvider    string `json:"identity_provider"`
	OAuthProvider       string `json:"oauth_provider"`
	EmailProvider       string `json:"email_provider,omitempty"`
	CredentialProvider  string `json:"credential_provider,omitempty"`
	SandboxProvider     string `json:"sandbox_provider,omitempty"`
	ExistingIdentityRef string `json:"existing_identity_ref,omitempty"`
	IdempotencyKey      string `json:"idempotency_key,omitempty"`
}

// PrepareRun validates provider gates and builds the idempotent run parameters.
// It never performs a network call.
func (e *Engine) PrepareRun(req CreateRunRequest, ownerID string, now time.Time) (storage.CreateRunParams, error) {
	tmpl, ok := e.templates[req.Template]
	if !ok {
		return storage.CreateRunParams{}, errs.New(errs.ClassValidation, "run.create", "unknown template "+req.Template)
	}
	if req.Mode != tmpl.Mode {
		return storage.CreateRunParams{}, errs.New(errs.ClassValidation, "run.create",
			fmt.Sprintf("template %s requires mode %s", tmpl.Name, tmpl.Mode))
	}
	if strings.TrimSpace(req.MailboxProvider) == "" || strings.TrimSpace(req.IdentityProvider) == "" || strings.TrimSpace(req.OAuthProvider) == "" {
		return storage.CreateRunParams{}, errs.New(errs.ClassValidation, "run.create",
			"mailbox_provider, identity_provider and oauth_provider are required")
	}

	emailName := req.EmailProvider
	if emailName == "" {
		emailName = "github"
	}
	creteName := req.CredentialProvider
	if creteName == "" {
		creteName = req.OAuthProvider
	}
	sandboxName := req.SandboxProvider
	if sandboxName == "" {
		sandboxName = req.IdentityProvider
	}

	snapshot, err := e.resolveSnapshot(req, emailName, creteName, sandboxName)
	if err != nil {
		return storage.CreateRunParams{}, err
	}
	if tmpl.Mode == "real-account" {
		if err := validateExistingIdentityRef(req.ExistingIdentityRef); err != nil {
			return storage.CreateRunParams{}, err
		}
	}
	if err := e.requireKinds(tmpl, snapshot); err != nil {
		return storage.CreateRunParams{}, err
	}
	rawSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return storage.CreateRunParams{}, err
	}
	canonical := strings.Join([]string{
		req.Template, req.Mode, req.MailboxProvider, req.IdentityProvider, req.OAuthProvider,
		emailName, creteName, sandboxName, req.ExistingIdentityRef,
	}, "|")
	fingerprint := security.HMACHex(e.cfg.FingerprintKey, []byte(canonical))

	steps := make([]storage.StepSeed, 0, len(tmpl.Steps))
	for _, s := range tmpl.Steps {
		steps = append(steps, storage.StepSeed{Name: s.Name})
	}
	return storage.CreateRunParams{
		ID:                  security.NewID(),
		OwnerID:             ownerID,
		Template:            req.Template,
		Mode:                req.Mode,
		IdempotencyKey:      req.IdempotencyKey,
		ProviderSnapshotJSN: string(rawSnapshot),
		RequestFingerprint:  fingerprint,
		ExistingIdentityRef: req.ExistingIdentityRef,
		DeadlineMS:          now.UTC().Add(e.cfg.RunDeadline).UnixMilli(),
		Steps:               steps,
	}, nil
}

func (e *Engine) resolveSnapshot(req CreateRunRequest, emailName, credentialName, sandboxName string) (provider.ProviderSnapshot, error) {
	var snap provider.ProviderSnapshot
	required := map[provider.Kind]bool{}
	for _, kind := range e.templates[req.Template].RequiredKinds {
		required[kind] = true
	}
	// core providers are mandatory only when the template actually needs them;
	// otherwise an unknown name is tolerated so one adapter can serve several
	// roles without the operator having to register placeholder providers.
	resolveCore := func(kind provider.Kind, name string) (provider.Descriptor, error) {
		if name == "" {
			if required[kind] {
				return provider.Descriptor{}, errs.New(errs.ClassValidation, "run.create",
					fmt.Sprintf("template %s requires a %s provider", req.Template, kind))
			}
			return provider.Descriptor{}, nil
		}
		d, ok := e.registry.LookupDescriptor(kind, name)
		if !ok {
			if required[kind] {
				return provider.Descriptor{}, errs.New(errs.ClassValidation, "run.create",
					fmt.Sprintf("unknown %s provider %q", kind, name))
			}
			return provider.Descriptor{}, nil
		}
		if !d.Enabled {
			if required[kind] {
				return provider.Descriptor{}, provider.NewDisallowedError(kind, name, d.RestrictionReason)
			}
			return provider.Descriptor{}, nil
		}
		return d, nil
	}
	var err error
	if snap.Mailbox, err = resolveCore(provider.KindMailbox, req.MailboxProvider); err != nil {
		return snap, err
	}
	if snap.Identity, err = resolveCore(provider.KindIdentity, req.IdentityProvider); err != nil {
		return snap, err
	}
	if snap.OAuth, err = resolveCore(provider.KindOAuth, req.OAuthProvider); err != nil {
		return snap, err
	}
	snap.Email = e.optionalDescriptor(provider.KindEmail, emailName)
	snap.Credential = e.optionalDescriptor(provider.KindCredential, credentialName)
	snap.Sandbox = e.optionalDescriptor(provider.KindSandbox, sandboxName)
	return snap, nil
}

// optionalDescriptor resolves an optional provider. When the preferred name is
// absent it falls back to the first enabled adapter of that kind, so that one
// adapter implementing several roles (for example Mailpit + OIDC) works without
// forcing every caller to know the registry layout.
func (e *Engine) optionalDescriptor(kind provider.Kind, preferred string) provider.Descriptor {
	if preferred != "" {
		if d, ok := e.registry.LookupDescriptor(kind, preferred); ok && d.Enabled {
			return d
		}
	}
	for _, d := range e.registry.Catalog() {
		if d.Kind == kind && d.Enabled {
			return d
		}
	}
	return provider.Descriptor{}
}

func (e *Engine) requireKinds(tmpl *Template, snap provider.ProviderSnapshot) error {
	for _, kind := range tmpl.RequiredKinds {
		var d provider.Descriptor
		switch kind {
		case provider.KindMailbox:
			d = snap.Mailbox
		case provider.KindIdentity:
			d = snap.Identity
		case provider.KindOAuth:
			d = snap.OAuth
		case provider.KindEmail:
			d = snap.Email
		case provider.KindCredential:
			d = snap.Credential
		case provider.KindSandbox:
			d = snap.Sandbox
		default:
			return errs.New(errs.ClassValidation, "run.create", "unsupported provider kind "+string(kind))
		}
		if d.Name == "" {
			return errs.New(errs.ClassValidation, "run.create",
				fmt.Sprintf("template %s requires a %s provider", tmpl.Name, kind))
		}
	}
	return nil
}

func validateExistingIdentityRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return errs.New(errs.ClassValidation, "run.create",
			"existing_identity_ref is required for real-account runs; this service never creates third-party accounts")
	}
	if len(ref) > 256 {
		return errs.New(errs.ClassValidation, "run.create", "existing_identity_ref is too long")
	}
	if strings.ContainsAny(ref, " \t\r\n") {
		return errs.New(errs.ClassValidation, "run.create", "existing_identity_ref must not contain whitespace")
	}
	return nil
}

// selectionFromSnapshot decodes the provider snapshot locked at creation time.
func (e *Engine) selectionFromSnapshot(run storage.Run) provider.ProviderSnapshot {
	var snap provider.ProviderSnapshot
	if run.ProviderSnapshotJSN != "" {
		_ = json.Unmarshal([]byte(run.ProviderSnapshotJSN), &snap)
	}
	return snap
}

// RunSummary is the operator-facing read model.
type RunSummary struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	Template         string `json:"template"`
	Mode             string `json:"mode"`
	CurrentStep      string `json:"current_step,omitempty"`
	ActionRequired   bool   `json:"action_required"`
	LastErrorClass   string `json:"last_error_class,omitempty"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	DeadlineAt       string `json:"deadline_at"`
	AuthorizationURL string `json:"authorization_url,omitempty"`
}

// Summarize converts a stored run into the public summary. The authorization
// URL is only decrypted for waiting real-account runs and is never logged.
func (e *Engine) Summarize(ctx context.Context, run storage.Run) RunSummary {
	sum := RunSummary{
		ID:             run.ID,
		Status:         run.Status,
		Template:       run.Template,
		Mode:           run.Mode,
		CurrentStep:    run.CurrentStep,
		ActionRequired: run.ActionRequired,
		LastErrorClass: run.LastErrorClass,
		CreatedAt:      run.CreatedAt().Format(time.RFC3339),
		UpdatedAt:      run.UpdatedAt().Format(time.RFC3339),
		DeadlineAt:     run.Deadline().Format(time.RFC3339),
	}
	if run.Status == "WAITING_USER_ACTION" {
		if res, err := e.store.LatestResource(ctx, run.ID, "oauth_session"); err == nil {
			var extra struct {
				AuthorizationURLSecretRef string `json:"authorization_url_secret_ref"`
			}
			if res.ExtraJSON != "" {
				_ = json.Unmarshal([]byte(res.ExtraJSON), &extra)
			}
			if extra.AuthorizationURLSecretRef != "" {
				if raw, err := e.secrets.Resolve(ctx, extra.AuthorizationURLSecretRef); err == nil {
					sum.AuthorizationURL = raw
				}
			}
		}
	}
	return sum
}
