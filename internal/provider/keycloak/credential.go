package keycloak

import (
	"context"
	"strings"

	"github.com/baicie/account-onboarding/internal/provider"
)

// credentialCapabilities are reported by GET /v1/providers.
var credentialCapabilities = []string{"credential.verify"}

// credentialView is the provider.CredentialVerifier view of the shared adapter.
// The verify_credential workflow step uses it to re-read the authoritative
// remote state before a run is declared COMPLETED.
type credentialView struct{ a *Adapter }

var _ provider.CredentialVerifier = credentialView{}

// Descriptor implements provider.CredentialVerifier.
func (v credentialView) Descriptor() provider.Descriptor {
	return v.a.descriptor(provider.KindCredential, credentialCapabilities)
}

// Verify resolves a stored OIDC credential and validates it through userinfo.
func (v credentialView) Verify(ctx context.Context, credentialRef string) (provider.CredentialResult, error) {
	if err := v.a.ensureEnabled(provider.KindCredential); err != nil {
		return provider.CredentialResult{}, err
	}
	return v.a.validateRef(ctx, strings.TrimSpace(credentialRef), "keycloak.verify_credential")
}
