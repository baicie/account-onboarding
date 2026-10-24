package keycloak

import (
	"context"
	"errors"
	"strings"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

// ErrInvalidAddress reports a syntactically invalid sandbox email address.
var ErrInvalidAddress = errors.New("keycloak: invalid email address")

// identityCapabilities are reported by GET /v1/providers.
var identityCapabilities = []string{
	"identity.request_verification",
	"identity.check_verification",
	"identity.ensure",
}

// identityView is the provider.IdentityProvider view of the shared adapter. It
// also implements provider.IdentityProvisioner so the registry can expose the
// sandbox-only EnsureIdentity capability.
type identityView struct{ a *Adapter }

var (
	_ provider.IdentityProvider    = identityView{}
	_ provider.IdentityProvisioner = identityView{}
)

// Descriptor implements provider.IdentityProvider.
func (v identityView) Descriptor() provider.Descriptor {
	return v.a.descriptor(provider.KindIdentity, identityCapabilities)
}

// RequestVerification triggers the realm's VERIFY_EMAIL action email for a
// sandbox identity. The operation is safe to repeat: the engine retries the
// step when the previous attempt failed before the email was accepted.
func (v identityView) RequestVerification(ctx context.Context, req provider.RequestVerificationRequest) (provider.VerificationState, error) {
	const op = "keycloak.request_verification"
	if err := v.a.ensureEnabled(provider.KindIdentity); err != nil {
		return provider.VerificationUnknown, err
	}
	address, err := normalizeEmail(req.Address)
	if err != nil {
		return provider.VerificationUnknown, errs.New(errs.ClassValidation, op, "address is missing or invalid")
	}
	user, err := v.a.findUser(ctx, req.SubjectID, address)
	if err != nil {
		return provider.VerificationUnknown, err
	}
	if !sameEmail(user.Email, address) {
		return provider.VerificationUnknown, errs.New(errs.ClassValidation, op,
			"sandbox identity email does not match the requested address")
	}
	if !user.Enabled {
		return provider.VerificationRejected, nil
	}
	if user.EmailVerified {
		// Already verified: the step is a no-op, no extra email is sent.
		return provider.VerificationVerified, nil
	}
	if err := v.a.executeActionsEmail(ctx, user.ID, []string{"VERIFY_EMAIL"}, v.a.client); err != nil {
		return provider.VerificationUnknown, err
	}
	return provider.VerificationPending, nil
}

// CheckVerification reads the authoritative emailVerified flag through the
// Admin API.
func (v identityView) CheckVerification(ctx context.Context, req provider.CheckVerificationRequest) (provider.VerificationState, error) {
	const op = "keycloak.check_verification"
	if err := v.a.ensureEnabled(provider.KindIdentity); err != nil {
		return provider.VerificationUnknown, err
	}
	user, err := v.a.findUser(ctx, req.SubjectID, strings.TrimSpace(req.Address))
	if err != nil {
		return provider.VerificationUnknown, err
	}
	if !user.Enabled {
		return provider.VerificationRejected, nil
	}
	if user.EmailVerified {
		return provider.VerificationVerified, nil
	}
	return provider.VerificationPending, nil
}

// EnsureIdentity provisions (or reuses) a controlled sandbox identity. It is the
// optional IdentityProvisioner capability; the sandbox driver exposes the same
// operation through Register.
func (v identityView) EnsureIdentity(ctx context.Context, req provider.EnsureIdentityRequest) (provider.IdentityHandle, error) {
	if err := v.a.ensureEnabled(provider.KindIdentity); err != nil {
		return provider.IdentityHandle{}, err
	}
	subject := strings.TrimSpace(req.SubjectID)
	if subject == "" {
		return provider.IdentityHandle{}, errs.New(errs.ClassValidation, "keycloak.ensure_identity", "subject id is required")
	}
	email, err := normalizeEmail(req.Email)
	if err != nil {
		return provider.IdentityHandle{}, errs.New(errs.ClassValidation, "keycloak.ensure_identity", "email is missing or invalid")
	}
	user, err := v.a.ensureSandboxIdentity(ctx, subject, email, req.DisplayName, req.Password)
	if err != nil {
		return provider.IdentityHandle{}, err
	}
	return IdentityHandleFor(user), nil
}

// ensureSandboxIdentity creates the user, or reuses an existing one with the
// same stable username. Reuse never resets emailVerified, so a retried step can
// never undo a completed verification.
func (a *Adapter) ensureSandboxIdentity(ctx context.Context, subject, email, displayName, password string) (userRepresentation, error) {
	const op = "keycloak.ensure_identity"
	username, err := sandboxUsername(subject)
	if err != nil {
		return userRepresentation{}, err
	}
	payload := createUserRepresentation{
		Username:      username,
		Email:         email,
		FirstName:     displayName,
		LastName:      "Sandbox",
		Enabled:       true,
		EmailVerified: false,
		Attributes: map[string][]string{
			"sandbox": {"account-onboarding-local"},
		},
	}
	if strings.TrimSpace(password) != "" {
		payload.Credentials = []credentialRepresentation{{Type: "password", Value: password, Temporary: false}}
	}
	user, created, err := a.createUser(ctx, payload)
	if err != nil {
		return userRepresentation{}, err
	}
	if !created {
		// Idempotent re-entry. Keep the email authoritative and re-apply the
		// supplied password so a retried step leaves a usable identity.
		if strings.TrimSpace(user.Email) != "" && !sameEmail(user.Email, email) {
			if err := a.updateUserEmail(ctx, user.ID, email); err != nil {
				return userRepresentation{}, err
			}
			user.Email = email
			user.EmailVerified = false
		}
		if strings.TrimSpace(password) != "" {
			if err := a.resetPassword(ctx, user.ID, password); err != nil {
				return userRepresentation{}, err
			}
		}
	}
	if strings.TrimSpace(user.ID) == "" {
		return userRepresentation{}, errs.New(errs.ClassProviderBug, op, "sandbox identity has no subject id")
	}
	user.Username = username
	if strings.TrimSpace(user.Email) == "" {
		user.Email = email
	}
	return user, nil
}

// sandboxUsername maps a workflow subject onto a safe Keycloak username. The
// workflow subject is already "onboard-<12 hex>"; other callers get the same
// prefix so teardown can identify controlled identities by prefix.
func sandboxUsername(subject string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(subject))
	if s == "" {
		return "", errs.New(errs.ClassValidation, "keycloak.username", "subject id is required")
	}
	if len(s) > 128 {
		return "", errs.New(errs.ClassValidation, "keycloak.username", "subject id is too long")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return "", errs.New(errs.ClassValidation, "keycloak.username", "subject id contains characters that are not allowed in a sandbox username")
		}
	}
	return s, nil
}

// IdentityHandleFor projects a user representation onto the provider handle.
func IdentityHandleFor(user userRepresentation) provider.IdentityHandle {
	return provider.IdentityHandle{
		SubjectID: user.ID,
		Address:   user.Email,
		Provider:  ProviderName,
	}
}

// normalizeEmail lowercases and validates a sandbox address.
func normalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" || len(email) > 254 {
		return "", ErrInvalidAddress
	}
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return "", ErrInvalidAddress
	}
	if strings.ContainsAny(email, " \t\r\n,;<>\"") {
		return "", ErrInvalidAddress
	}
	if !strings.Contains(email[at+1:], ".") {
		return "", ErrInvalidAddress
	}
	return email, nil
}

func sameEmail(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
