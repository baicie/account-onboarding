// Package provider: registry of activated adapters.
//
// Lookups are gated: a disabled adapter is rejected with NOT_AUTHORIZED before
// any network call, which is exactly what the acceptance scenario "Forbidden
// provider" asserts.
package provider

import (
	"sort"
	"sync"
)

// Registry is an immutable-after-boot provider catalog. Register at startup,
// then read concurrently from engine workers and API handlers.
type Registry struct {
	mu        sync.RWMutex
	mailbox   map[string]MailboxProvider
	identity  map[string]IdentityProvider
	oauth     map[string]OAuthProvider
	cred      map[string]CredentialVerifier
	email     map[string]EmailManager
	sandbox   map[string]SandboxDriver
	provision map[string]IdentityProvisioner
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		mailbox:   map[string]MailboxProvider{},
		identity:  map[string]IdentityProvider{},
		oauth:     map[string]OAuthProvider{},
		cred:      map[string]CredentialVerifier{},
		email:     map[string]EmailManager{},
		sandbox:   map[string]SandboxDriver{},
		provision: map[string]IdentityProvisioner{},
	}
}

func (r *Registry) AddMailbox(p MailboxProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mailbox[p.Descriptor().Name] = p
	if ip, ok := p.(IdentityProvisioner); ok {
		_ = ip
	}
}

func (r *Registry) AddIdentity(p IdentityProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.identity[p.Descriptor().Name] = p
	if ip, ok := p.(IdentityProvisioner); ok {
		r.provision[p.Descriptor().Name] = ip
	}
}

func (r *Registry) AddOAuth(p OAuthProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.oauth[p.Descriptor().Name] = p
}

func (r *Registry) AddCredential(p CredentialVerifier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cred[p.Descriptor().Name] = p
}

func (r *Registry) AddEmailManager(p EmailManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.email[p.Descriptor().Name] = p
}

func (r *Registry) AddSandbox(p SandboxDriver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sandbox[p.Descriptor().Name] = p
}

// Mailbox resolves an enabled mailbox provider.
func (r *Registry) Mailbox(name string) (MailboxProvider, error) {
	r.mu.RLock()
	p, ok := r.mailbox[name]
	r.mu.RUnlock()
	if !ok {
		return nil, NewUnknownProviderError(KindMailbox, name)
	}
	if d := p.Descriptor(); !d.Enabled {
		return nil, NewDisallowedError(KindMailbox, name, d.RestrictionReason)
	}
	return p, nil
}

// Identity resolves an enabled identity provider.
func (r *Registry) Identity(name string) (IdentityProvider, error) {
	r.mu.RLock()
	p, ok := r.identity[name]
	r.mu.RUnlock()
	if !ok {
		return nil, NewUnknownProviderError(KindIdentity, name)
	}
	if d := p.Descriptor(); !d.Enabled {
		return nil, NewDisallowedError(KindIdentity, name, d.RestrictionReason)
	}
	return p, nil
}

// OAuth resolves an enabled OAuth provider.
func (r *Registry) OAuth(name string) (OAuthProvider, error) {
	r.mu.RLock()
	p, ok := r.oauth[name]
	r.mu.RUnlock()
	if !ok {
		return nil, NewUnknownProviderError(KindOAuth, name)
	}
	if d := p.Descriptor(); !d.Enabled {
		return nil, NewDisallowedError(KindOAuth, name, d.RestrictionReason)
	}
	return p, nil
}

// Credential resolves an enabled credential verifier.
func (r *Registry) Credential(name string) (CredentialVerifier, error) {
	r.mu.RLock()
	p, ok := r.cred[name]
	r.mu.RUnlock()
	if !ok {
		return nil, NewUnknownProviderError(KindCredential, name)
	}
	if d := p.Descriptor(); !d.Enabled {
		return nil, NewDisallowedError(KindCredential, name, d.RestrictionReason)
	}
	return p, nil
}

// EmailManager resolves an enabled existing-account email manager.
func (r *Registry) EmailManager(name string) (EmailManager, error) {
	r.mu.RLock()
	p, ok := r.email[name]
	r.mu.RUnlock()
	if !ok {
		return nil, NewUnknownProviderError(KindEmail, name)
	}
	if d := p.Descriptor(); !d.Enabled {
		return nil, NewDisallowedError(KindEmail, name, d.RestrictionReason)
	}
	return p, nil
}

// Sandbox resolves an enabled sandbox driver.
func (r *Registry) Sandbox(name string) (SandboxDriver, error) {
	r.mu.RLock()
	p, ok := r.sandbox[name]
	r.mu.RUnlock()
	if !ok {
		return nil, NewUnknownProviderError(KindSandbox, name)
	}
	if d := p.Descriptor(); !d.Enabled {
		return nil, NewDisallowedError(KindSandbox, name, d.RestrictionReason)
	}
	return p, nil
}

// Provisioner returns the optional provisioning capability of an identity
// provider (sandbox only).
func (r *Registry) Provisioner(name string) (IdentityProvisioner, error) {
	r.mu.RLock()
	p, ok := r.provision[name]
	r.mu.RUnlock()
	if !ok {
		return nil, NewUnknownProviderError(KindIdentity, name)
	}
	return p, nil
}

// Catalog returns every registered descriptor sorted by kind then name. Used by
// GET /v1/providers and the UI so operators can see activation gates.
func (r *Registry) Catalog() []Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Descriptor, 0, len(r.mailbox)+len(r.identity)+len(r.oauth)+len(r.cred)+len(r.email)+len(r.sandbox))
	for _, p := range r.mailbox {
		out = append(out, p.Descriptor())
	}
	for _, p := range r.identity {
		out = append(out, p.Descriptor())
	}
	for _, p := range r.oauth {
		out = append(out, p.Descriptor())
	}
	for _, p := range r.cred {
		out = append(out, p.Descriptor())
	}
	for _, p := range r.email {
		out = append(out, p.Descriptor())
	}
	for _, p := range r.sandbox {
		out = append(out, p.Descriptor())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// LookupDescriptor returns the descriptor for a provider name.
func (r *Registry) LookupDescriptor(kind Kind, name string) (Descriptor, bool) {
	for _, d := range r.Catalog() {
		if d.Kind == kind && d.Name == name {
			return d, true
		}
	}
	return Descriptor{}, false
}
