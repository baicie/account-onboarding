package keycloak

import (
	"context"
	"strings"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

// sandboxCapabilities are reported by GET /v1/providers.
var sandboxCapabilities = []string{
	"sandbox.register",
	"sandbox.confirm_email",
	"oidc.authorize_pkce",
	"oidc.validate",
	"sandbox.local_only",
}

// sandboxView is the provider.SandboxDriver view of the shared adapter. It must
// never be implemented for a real third-party platform: the adapter refuses to
// start unless the base URL is loopback.
type sandboxView struct{ a *Adapter }

var _ provider.SandboxDriver = sandboxView{}

// Descriptor implements provider.SandboxDriver.
func (v sandboxView) Descriptor() provider.Descriptor {
	return v.a.descriptor(provider.KindSandbox, sandboxCapabilities)
}

// Register provisions a controlled test identity. It is idempotent on the
// stable workflow subject: a retried step or a restarted process finds the
// existing Keycloak user instead of creating a duplicate identity.
func (v sandboxView) Register(ctx context.Context, req provider.SandboxRegisterRequest) (provider.IdentityHandle, error) {
	if err := v.a.ensureEnabled(provider.KindSandbox); err != nil {
		return provider.IdentityHandle{}, err
	}
	address, err := normalizeEmail(req.Email)
	if err != nil {
		return provider.IdentityHandle{}, errs.New(errs.ClassValidation, "keycloak.register", "email is missing or invalid")
	}
	display := strings.TrimSpace(req.DisplayName)
	if display == "" {
		display = strings.TrimSpace(req.SubjectID)
	}
	user, err := v.a.ensureSandboxIdentity(ctx, req.SubjectID, address, display, req.Password)
	if err != nil {
		return provider.IdentityHandle{}, err
	}
	return IdentityHandleFor(user), nil
}

// ConfirmEmail follows an extracted verification link. Keycloak renders an
// interstitial page ("Click here to proceed") for VERIFY_EMAIL action tokens,
// so the driver performs up to two same-origin GETs and then re-reads the
// authoritative emailVerified flag through the Admin API.
func (v sandboxView) ConfirmEmail(ctx context.Context, req provider.SandboxConfirmRequest) (provider.VerificationState, error) {
	const op = "keycloak.confirm_email"
	if err := v.a.ensureEnabled(provider.KindSandbox); err != nil {
		return provider.VerificationUnknown, err
	}
	link := strings.TrimSpace(req.VerificationURL)
	first, err := v.a.parseActionTokenURL(link)
	if err != nil {
		return provider.VerificationUnknown, err
	}
	client, err := v.a.newBrowserClient(nil)
	if err != nil {
		return provider.VerificationUnknown, err
	}
	page, resp, err := v.a.getHTML(ctx, client, op, first.String(), maxLoginPageBytes)
	if err != nil {
		return provider.VerificationUnknown, err
	}
	// The first GET renders the confirmation page; the action itself executes
	// when its "Click here to proceed" link is followed. Follow at most one
	// additional same-origin action-token link so a hostile page cannot make
	// this driver loop.
	next, ok := findActionTokenHref(page, resp)
	if ok {
		target, parseErr := v.a.parseActionTokenURL(next)
		if parseErr != nil {
			return provider.VerificationUnknown, parseErr
		}
		if strings.EqualFold(target.String(), first.String()) {
			// Defensive: never re-fetch the same URL in a loop.
			target = nil
		}
		if target != nil {
			if _, _, err := v.a.getHTML(ctx, client, op, target.String(), maxLoginPageBytes); err != nil {
				return provider.VerificationUnknown, err
			}
		}
	}
	// The remote state is the source of truth: a 200 page does not prove the
	// email was verified.
	user, err := v.a.findUser(ctx, req.Identity.SubjectID, strings.TrimSpace(req.Identity.Address))
	if err != nil {
		return provider.VerificationUnknown, err
	}
	switch {
	case user.EmailVerified:
		return provider.VerificationVerified, nil
	case !user.Enabled:
		return provider.VerificationRejected, nil
	case pageMentionsExpiry(page):
		return provider.VerificationExpired, nil
	default:
		return provider.VerificationPending, nil
	}
}

// AuthorizeOIDC drives the complete Authorization Code + PKCE S256 flow with a
// private cookie jar: it authorizes, posts the sandbox login form, reads the
// code from the callback redirect, exchanges it with the code verifier, and
// confirms the subject through userinfo. Tokens are written to the SecretStore
// only; the caller receives an opaque reference plus non-secret claims.
func (v sandboxView) AuthorizeOIDC(ctx context.Context, req provider.SandboxAuthorizeRequest) (provider.SandboxCredentialRef, error) {
	if err := v.a.ensureEnabled(provider.KindSandbox); err != nil {
		return provider.SandboxCredentialRef{}, err
	}
	return v.a.authorizeOIDC(ctx, req)
}

// Validate resolves an opaque credential reference and asks userinfo whether the
// stored access token is still accepted.
func (v sandboxView) Validate(ctx context.Context, cred provider.SandboxCredentialRef) (provider.CredentialResult, error) {
	if err := v.a.ensureEnabled(provider.KindSandbox); err != nil {
		return provider.CredentialResult{}, err
	}
	return v.a.validateRef(ctx, strings.TrimSpace(cred.Ref), "keycloak.validate")
}
