// Package fake provides deterministic in-memory providers. They are the only
// providers enabled in R0 and they must never perform network I/O.
//
// The fake world is shared by the mailbox, identity, OAuth, credential
// verifier and sandbox driver so an entire sandbox workflow can run offline.
package fake

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/security"
)

// ContractVersion is returned by every fake descriptor.
const ContractVersion = "fake-1"

// World is the shared in-memory state.
type World struct {
	mu             sync.Mutex
	mailboxes      map[string]*fakeMailbox
	identities     map[string]*fakeIdentity
	sessions       map[string]*fakeSession
	credentials    map[string]provider.CredentialResult
	verifiedEmails map[string]bool
	writer         provider.SecretWriter
	now            func() time.Time

	// AutoAuthorize makes the fake OAuth provider complete immediately.
	AutoAuthorize bool
	// OAuthExpire makes Poll return AUTH_EXPIRED (gateway restart / 410).
	OAuthExpire bool
	// OAuthDeny makes Poll return DENIED.
	OAuthDeny bool
}

type fakeMailbox struct {
	mailbox  provider.Mailbox
	messages []provider.Message
	seq      int
}

type fakeIdentity struct {
	handle   provider.IdentityHandle
	verified bool
}

type fakeSession struct {
	ref       string
	subjectID string
	state     provider.AuthState
}

// NewWorld builds an empty fake world. writer may be nil.
func NewWorld(writer provider.SecretWriter) *World {
	return &World{
		mailboxes:      map[string]*fakeMailbox{},
		identities:     map[string]*fakeIdentity{},
		sessions:       map[string]*fakeSession{},
		credentials:    map[string]provider.CredentialResult{},
		verifiedEmails: map[string]bool{},
		writer:         writer,
		now:            time.Now,
		AutoAuthorize:  true,
	}
}

// SetClock overrides the clock for deterministic tests.
func (w *World) SetClock(now func() time.Time) { w.now = now }

func (w *World) clock() time.Time {
	if w.now == nil {
		return time.Now()
	}
	return w.now()
}

// ---------------------------------------------------------------------------
// Mailbox provider
// ---------------------------------------------------------------------------

// Mailbox implements provider.MailboxProvider and provider.MailboxCleaner.
type Mailbox struct{ w *World }

// NewMailbox returns the fake mailbox provider.
func NewMailbox(w *World) *Mailbox { return &Mailbox{w: w} }

// Descriptor implements provider.MailboxProvider.
func (m *Mailbox) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Name:            "fake-mailbox",
		Kind:            provider.KindMailbox,
		ContractVersion: ContractVersion,
		Enabled:         true,
		Capabilities:    []string{"create", "list", "get", "delete"},
	}
}

// CreateAddress implements provider.MailboxProvider.
func (m *Mailbox) CreateAddress(_ context.Context, req provider.CreateAddressRequest) (provider.Mailbox, error) {
	if req.Name == "" {
		return provider.Mailbox{}, errs.New(errs.ClassValidation, "fake-mailbox.create", "empty name")
	}
	domain := req.Domain
	if domain == "" {
		domain = "fake.test"
	}
	w := m.w
	w.mu.Lock()
	defer w.mu.Unlock()
	id := security.NewID()
	mb := provider.Mailbox{
		ID:        id,
		Address:   req.Name + "@" + domain,
		Provider:  "fake-mailbox",
		CreatedAt: w.clock().UTC(),
	}
	w.mailboxes[id] = &fakeMailbox{mailbox: mb}
	return mb, nil
}

// ListMessages implements provider.MailboxProvider.
func (m *Mailbox) ListMessages(_ context.Context, mb provider.Mailbox, since time.Time) ([]provider.Message, error) {
	w := m.w
	w.mu.Lock()
	defer w.mu.Unlock()
	entry, ok := w.mailboxes[mb.ID]
	if !ok {
		return nil, errs.New(errs.ClassValidation, "fake-mailbox.list", "unknown mailbox")
	}
	out := make([]provider.Message, 0, len(entry.messages))
	for _, msg := range entry.messages {
		if msg.ReceivedAt.Before(since) {
			continue
		}
		out = append(out, msg)
	}
	return out, nil
}

// GetMessage implements provider.MailboxProvider.
func (m *Mailbox) GetMessage(_ context.Context, mb provider.Mailbox, id string) (provider.Message, error) {
	w := m.w
	w.mu.Lock()
	defer w.mu.Unlock()
	entry, ok := w.mailboxes[mb.ID]
	if !ok {
		return provider.Message{}, errs.New(errs.ClassValidation, "fake-mailbox.get", "unknown mailbox")
	}
	for _, msg := range entry.messages {
		if msg.ID == id {
			return msg, nil
		}
	}
	return provider.Message{}, errs.New(errs.ClassNotFound, "fake-mailbox.get", "message not found")
}

// DeleteAddress implements provider.MailboxCleaner.
func (m *Mailbox) DeleteAddress(_ context.Context, mb provider.Mailbox) error {
	w := m.w
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.mailboxes, mb.ID)
	return nil
}

// QueueMessage appends a message to the mailbox with the given address. It is a
// test helper, not part of the provider contract.
func (w *World) QueueMessage(address, sender, subject, text string) provider.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.clock().UTC()
	for _, entry := range w.mailboxes {
		if !strings.EqualFold(entry.mailbox.Address, address) {
			continue
		}
		entry.seq++
		msg := provider.Message{
			ID:         fmt.Sprintf("msg-%d", entry.seq),
			MailboxID:  entry.mailbox.ID,
			Recipient:  address,
			Sender:     sender,
			Subject:    subject,
			ReceivedAt: now,
			Text:       text,
			HTML:       text,
		}
		entry.messages = append(entry.messages, msg)
		return msg
	}
	return provider.Message{}
}

// QueueVerificationLink enqueues a realistic verification email.
func (w *World) QueueVerificationLink(address, sender, link string) provider.Message {
	return w.QueueMessage(address, sender, "Verify your email address",
		"Please verify your email address.\n\n"+link+"\n")
}

// ---------------------------------------------------------------------------
// Identity provider
// ---------------------------------------------------------------------------

// Identity implements provider.IdentityProvider.
type Identity struct{ w *World }

// NewIdentity returns the fake identity provider.
func NewIdentity(w *World) *Identity { return &Identity{w: w} }

// Descriptor implements provider.IdentityProvider.
func (i *Identity) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Name:            "fake-identity",
		Kind:            provider.KindIdentity,
		ContractVersion: ContractVersion,
		Enabled:         true,
		Capabilities:    []string{"request_verification", "check_verification"},
	}
}

// RequestVerification implements provider.IdentityProvider. It enqueues a
// verification email when the address belongs to a known fake mailbox.
func (i *Identity) RequestVerification(_ context.Context, req provider.RequestVerificationRequest) (provider.VerificationState, error) {
	w := i.w
	w.mu.Lock()
	ident, ok := w.identities[req.SubjectID]
	if !ok {
		w.mu.Unlock()
		return provider.VerificationUnknown, errs.New(errs.ClassValidation, "fake-identity.request", "unknown subject")
	}
	w.mu.Unlock()
	if ident.verified {
		return provider.VerificationVerified, nil
	}
	link := fmt.Sprintf("https://idp.fake.test/verify?token=%s", security.RandomToken(12))
	w.QueueVerificationLink(req.Address, "no-reply@idp.fake.test", link)
	return provider.VerificationPending, nil
}

// CheckVerification implements provider.IdentityProvider.
func (i *Identity) CheckVerification(_ context.Context, req provider.CheckVerificationRequest) (provider.VerificationState, error) {
	w := i.w
	w.mu.Lock()
	defer w.mu.Unlock()
	ident, ok := w.identities[req.SubjectID]
	if !ok {
		return provider.VerificationUnknown, errs.New(errs.ClassValidation, "fake-identity.check", "unknown subject")
	}
	if ident.verified {
		return provider.VerificationVerified, nil
	}
	return provider.VerificationPending, nil
}

// ---------------------------------------------------------------------------
// OAuth provider
// ---------------------------------------------------------------------------

// OAuth implements provider.OAuthProvider.
type OAuth struct{ w *World }

// NewOAuth returns the fake OAuth provider.
func NewOAuth(w *World) *OAuth { return &OAuth{w: w} }

// Descriptor implements provider.OAuthProvider.
func (o *OAuth) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Name:            "fake-oauth",
		Kind:            provider.KindOAuth,
		ContractVersion: ContractVersion,
		Enabled:         true,
		Capabilities:    []string{"start", "poll"},
	}
}

// Start implements provider.OAuthProvider.
func (o *OAuth) Start(_ context.Context, req provider.StartAuthRequest) (provider.AuthorizationSession, error) {
	w := o.w
	w.mu.Lock()
	defer w.mu.Unlock()
	ref := secretsRef("fake_oauth")
	w.sessions[ref] = &fakeSession{ref: ref, subjectID: req.SubjectID, state: provider.AuthPending}
	return provider.AuthorizationSession{
		Ref:              ref,
		AuthorizationURL: "https://idp.fake.test/authorize?state=" + security.RandomToken(8),
		ExpiresAt:        w.clock().Add(15 * time.Minute),
	}, nil
}

// Poll implements provider.OAuthProvider.
func (o *OAuth) Poll(_ context.Context, sessionRef string) (provider.AuthorizationStatus, error) {
	w := o.w
	w.mu.Lock()
	defer w.mu.Unlock()
	sess, ok := w.sessions[sessionRef]
	if !ok {
		return provider.AuthorizationStatus{}, errs.New(errs.ClassAuthExpired, "fake-oauth.poll", "authorization session expired")
	}
	if w.OAuthExpire {
		return provider.AuthorizationStatus{}, errs.New(errs.ClassAuthExpired, "fake-oauth.poll", "authorization session expired")
	}
	if w.OAuthDeny {
		return provider.AuthorizationStatus{State: provider.AuthDenied, SubjectID: sess.subjectID}, nil
	}
	if !w.AutoAuthorize {
		return provider.AuthorizationStatus{State: provider.AuthPending, SubjectID: sess.subjectID}, nil
	}
	ref := secretsRef("fake_credential")
	cred := provider.CredentialResult{State: provider.CredentialValid, SubjectID: sess.subjectID, Issuer: "fake-oauth"}
	w.credentials[ref] = cred
	sess.state = provider.AuthAuthorized
	return provider.AuthorizationStatus{State: provider.AuthAuthorized, SubjectID: sess.subjectID, CredentialRef: ref}, nil
}

// AuthorizeWithCredential is a test helper that marks a session authorized.
func (w *World) AuthorizeWithCredential(sessionRef, subjectID string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.sessions[sessionRef]; !ok {
		return "", fmt.Errorf("unknown session")
	}
	ref := secretsRef("fake_credential")
	w.credentials[ref] = provider.CredentialResult{State: provider.CredentialValid, SubjectID: subjectID, Issuer: "fake-oauth"}
	w.sessions[sessionRef].state = provider.AuthAuthorized
	return ref, nil
}

// ---------------------------------------------------------------------------
// Credential verifier
// ---------------------------------------------------------------------------

// Credential implements provider.CredentialVerifier.
type Credential struct{ w *World }

// NewCredential returns the fake credential verifier.
func NewCredential(w *World) *Credential { return &Credential{w: w} }

// Descriptor implements provider.CredentialVerifier.
func (c *Credential) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Name:            "fake-credential",
		Kind:            provider.KindCredential,
		ContractVersion: ContractVersion,
		Enabled:         true,
		Capabilities:    []string{"verify"},
	}
}

// Verify implements provider.CredentialVerifier.
func (c *Credential) Verify(_ context.Context, credentialRef string) (provider.CredentialResult, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if res, ok := c.w.credentials[credentialRef]; ok {
		return res, nil
	}
	return provider.CredentialResult{State: provider.CredentialInvalid}, nil
}

// ---------------------------------------------------------------------------
// Sandbox driver
// ---------------------------------------------------------------------------

// Sandbox implements provider.SandboxDriver.
type Sandbox struct{ w *World }

// NewSandbox returns the fake sandbox driver.
func NewSandbox(w *World) *Sandbox { return &Sandbox{w: w} }

// Descriptor implements provider.SandboxDriver.
func (s *Sandbox) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Name:            "fake-sandbox",
		Kind:            provider.KindSandbox,
		ContractVersion: ContractVersion,
		Enabled:         true,
		Capabilities:    []string{"register", "confirm_email", "authorize_oidc", "validate"},
	}
}

// Register implements provider.SandboxDriver.
func (s *Sandbox) Register(_ context.Context, req provider.SandboxRegisterRequest) (provider.IdentityHandle, error) {
	w := s.w
	w.mu.Lock()
	defer w.mu.Unlock()
	if req.SubjectID == "" || req.Email == "" {
		return provider.IdentityHandle{}, errs.New(errs.ClassValidation, "fake-sandbox.register", "subject and email are required")
	}
	if _, exists := w.identities[req.SubjectID]; exists {
		return provider.IdentityHandle{}, errs.New(errs.ClassConflict, "fake-sandbox.register", "identity already exists")
	}
	handle := provider.IdentityHandle{SubjectID: req.SubjectID, Address: req.Email, Provider: "fake-sandbox"}
	w.identities[req.SubjectID] = &fakeIdentity{handle: handle}
	return handle, nil
}

// ConfirmEmail implements provider.SandboxDriver.
func (s *Sandbox) ConfirmEmail(_ context.Context, req provider.SandboxConfirmRequest) (provider.VerificationState, error) {
	w := s.w
	w.mu.Lock()
	defer w.mu.Unlock()
	ident, ok := w.identities[req.Identity.SubjectID]
	if !ok {
		return provider.VerificationUnknown, errs.New(errs.ClassValidation, "fake-sandbox.confirm", "unknown identity")
	}
	if !strings.Contains(req.VerificationURL, "token=") {
		return provider.VerificationPending, errs.New(errs.ClassValidation, "fake-sandbox.confirm", "invalid verification link")
	}
	ident.verified = true
	return provider.VerificationVerified, nil
}

// AuthorizeOIDC implements provider.SandboxDriver.
func (s *Sandbox) AuthorizeOIDC(_ context.Context, req provider.SandboxAuthorizeRequest) (provider.SandboxCredentialRef, error) {
	w := s.w
	w.mu.Lock()
	defer w.mu.Unlock()
	ident, ok := w.identities[req.Identity.SubjectID]
	if !ok {
		return provider.SandboxCredentialRef{}, errs.New(errs.ClassValidation, "fake-sandbox.authorize", "unknown identity")
	}
	ref := secretsRef("fake_oidc")
	w.credentials[ref] = provider.CredentialResult{State: provider.CredentialValid, SubjectID: ident.handle.SubjectID, Issuer: "fake-idp"}
	return provider.SandboxCredentialRef{
		Ref:       ref,
		Issuer:    "fake-idp",
		Subject:   ident.handle.SubjectID,
		Audience:  "fake-client",
		ExpiresAt: w.clock().Add(time.Hour),
	}, nil
}

// Validate implements provider.SandboxDriver.
func (s *Sandbox) Validate(_ context.Context, cred provider.SandboxCredentialRef) (provider.CredentialResult, error) {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	if res, ok := s.w.credentials[cred.Ref]; ok {
		return res, nil
	}
	return provider.CredentialResult{State: provider.CredentialInvalid}, nil
}

func secretsRef(kind string) string {
	return "secret://" + kind + "/" + security.NewID()
}

// ---------------------------------------------------------------------------
// Email manager (existing-account fake)
// ---------------------------------------------------------------------------

// EmailManager implements provider.EmailManager. Emails start unverified; a
// test calls VerifyEmail to simulate the human confirmation step.
type EmailManager struct {
	w     *World
	added map[string]bool
}

// NewEmailManager returns the fake existing-account email manager.
func NewEmailManager(w *World) *EmailManager {
	return &EmailManager{w: w, added: map[string]bool{}}
}

// Descriptor implements provider.EmailManager.
func (m *EmailManager) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Name:            "fake-github",
		Kind:            provider.KindEmail,
		ContractVersion: ContractVersion,
		Enabled:         true,
		Capabilities:    []string{"add_email", "list_emails"},
	}
}

// AddEmail implements provider.EmailManager.
func (m *EmailManager) AddEmail(_ context.Context, email string) (provider.EmailAddress, error) {
	m.w.mu.Lock()
	defer m.w.mu.Unlock()
	if email == "" {
		return provider.EmailAddress{}, errs.New(errs.ClassValidation, "fake-github.add", "empty email")
	}
	verified := m.w.verifiedEmails[email]
	m.added[email] = true
	return provider.EmailAddress{Email: email, Verified: verified, Primary: false, Visibility: "private"}, nil
}

// ListEmails implements provider.EmailManager.
func (m *EmailManager) ListEmails(_ context.Context) ([]provider.EmailAddress, error) {
	m.w.mu.Lock()
	defer m.w.mu.Unlock()
	out := make([]provider.EmailAddress, 0, len(m.added))
	for email := range m.added {
		out = append(out, provider.EmailAddress{
			Email: email, Verified: m.w.verifiedEmails[email], Primary: false, Visibility: "private",
		})
	}
	return out, nil
}

// VerifyEmail simulates the account holder confirming the verification email.
func (w *World) VerifyEmail(email string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.verifiedEmails == nil {
		w.verifiedEmails = map[string]bool{}
	}
	w.verifiedEmails[email] = true
}
