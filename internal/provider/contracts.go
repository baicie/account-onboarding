// Package provider holds the type-safe domain contracts every adapter must
// satisfy. Adapters translate protocol details (HTTP status codes, cookies,
// JSON shapes) into these domain types and the errs taxonomy.
//
// No contract returns a raw credential. Credentials are written through the
// SecretStore and referenced by opaque secret references only.
package provider

import (
	"context"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
)

// Kind is the provider family.
type Kind string

const (
	KindMailbox    Kind = "mailbox"
	KindIdentity   Kind = "identity"
	KindOAuth      Kind = "oauth"
	KindCredential Kind = "credential"
	KindEmail      Kind = "email"
	KindSandbox    Kind = "sandbox"
)

// Descriptor describes an adapter and its activation gate. Disabled adapters
// must be rejected before any network call is attempted.
type Descriptor struct {
	Name              string   `json:"name"`
	Kind              Kind     `json:"kind"`
	ContractVersion   string   `json:"contract_version"`
	Enabled           bool     `json:"enabled"`
	Live              bool     `json:"live"`
	Capabilities      []string `json:"capabilities,omitempty"`
	RestrictionReason string   `json:"restriction_reason,omitempty"`
}

// SecretResolver resolves an opaque secret reference to its plaintext. It is
// used by adapters that need provider credentials; the plaintext must never be
// logged or returned in DTOs.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// SecretWriter lets adapters persist credentials (for example an address JWT or
// an OIDC access token) and hand back only an opaque reference.
type SecretWriter interface {
	Put(ctx context.Context, ref, kind, plaintext string) error
}

// SecretStore is the full secret boundary used by the engine and adapters.
type SecretStore interface {
	SecretResolver
	SecretWriter
	Delete(ctx context.Context, ref string) error
}

// SecretRefPrefix is the canonical opaque reference namespace.
const SecretRefPrefix = "secret://"

// ---------------------------------------------------------------------------
// Mailbox
// ---------------------------------------------------------------------------

// CreateAddressRequest asks a mailbox provider for a new address. Name and
// Domain must be pre-validated against the provider allowlist by the caller.
type CreateAddressRequest struct {
	Key          string
	Name         string
	Domain       string
	EnablePrefix bool
}

// Mailbox is a provisioned address. SecretRef points at the address JWT (or the
// provider equivalent) inside the SecretStore; ID and Address are safe to show.
type Mailbox struct {
	ID        string    `json:"id"`
	Address   string    `json:"address"`
	Provider  string    `json:"provider"`
	SecretRef string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// Message is a mailbox message. Text/HTML are transient: they exist only inside
// the current step and must never be persisted or logged by default.
type Message struct {
	ID         string    `json:"id"`
	MailboxID  string    `json:"mailbox_id"`
	Recipient  string    `json:"recipient"`
	Sender     string    `json:"sender"`
	Subject    string    `json:"subject"`
	ReceivedAt time.Time `json:"received_at"`
	Text       string    `json:"-"`
	HTML       string    `json:"-"`
}

// MailboxProvider is the address/message contract from docs/04-providers.md.
type MailboxProvider interface {
	Descriptor() Descriptor
	CreateAddress(ctx context.Context, req CreateAddressRequest) (Mailbox, error)
	ListMessages(ctx context.Context, mailbox Mailbox, since time.Time) ([]Message, error)
	GetMessage(ctx context.Context, mailbox Mailbox, id string) (Message, error)
}

// MailboxCleaner is the optional compensating action for sandbox-owned
// addresses. Real long-lived addresses are never deleted automatically.
type MailboxCleaner interface {
	DeleteAddress(ctx context.Context, mailbox Mailbox) error
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

// VerificationState is the remote email verification status.
type VerificationState string

const (
	VerificationUnknown  VerificationState = "UNKNOWN"
	VerificationPending  VerificationState = "PENDING"
	VerificationVerified VerificationState = "VERIFIED"
	VerificationExpired  VerificationState = "EXPIRED"
	VerificationRejected VerificationState = "REJECTED"
)

// IdentityHandle is an opaque, provider-scoped identity reference.
type IdentityHandle struct {
	SubjectID string `json:"subject_id"`
	Address   string `json:"address"`
	Provider  string `json:"provider"`
	SecretRef string `json:"-"`
}

// RequestVerificationRequest triggers the provider side verification email.
type RequestVerificationRequest struct {
	SubjectID      string
	Address        string
	IdempotencyKey string
}

// CheckVerificationRequest polls the provider side status.
type CheckVerificationRequest struct {
	SubjectID string
	Address   string
}

// IdentityProvider verifies mailbox ownership at an identity provider.
type IdentityProvider interface {
	Descriptor() Descriptor
	RequestVerification(ctx context.Context, req RequestVerificationRequest) (VerificationState, error)
	CheckVerification(ctx context.Context, req CheckVerificationRequest) (VerificationState, error)
}

// EnsureIdentityRequest provisions a controlled test identity (sandbox only).
type EnsureIdentityRequest struct {
	SubjectID   string
	Email       string
	DisplayName string
	Password    string
}

// IdentityProvisioner is implemented by sandbox IdP adapters only. Production
// adapters must not create real third-party accounts.
type IdentityProvisioner interface {
	EnsureIdentity(ctx context.Context, req EnsureIdentityRequest) (IdentityHandle, error)
}

// ---------------------------------------------------------------------------
// OAuth
// ---------------------------------------------------------------------------

// AuthState is the state of an authorization session.
type AuthState string

const (
	AuthPending    AuthState = "PENDING"
	AuthAuthorized AuthState = "AUTHORIZED"
	AuthDenied     AuthState = "DENIED"
	AuthExpired    AuthState = "EXPIRED"
)

// StartAuthRequest opens an authorization session for an existing subject.
type StartAuthRequest struct {
	SubjectID   string
	RedirectURI string
	Scopes      []string
}

// AuthorizationSession references an in-flight authorization. The URL is safe
// to show to the operator; the state parameter inside it is single-use.
type AuthorizationSession struct {
	Ref              string    `json:"ref"`
	AuthorizationURL string    `json:"authorization_url"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// AuthorizationStatus is the polled authorization outcome. CredentialRef points
// at the SecretStore entry, never at a raw token.
type AuthorizationStatus struct {
	State         AuthState `json:"state"`
	SubjectID     string    `json:"subject_id"`
	CredentialRef string    `json:"-"`
	Detail        string    `json:"detail,omitempty"`
}

// OAuthProvider starts and polls authorization sessions.
type OAuthProvider interface {
	Descriptor() Descriptor
	Start(ctx context.Context, req StartAuthRequest) (AuthorizationSession, error)
	Poll(ctx context.Context, sessionRef string) (AuthorizationStatus, error)
}

// ---------------------------------------------------------------------------
// Credential verification
// ---------------------------------------------------------------------------

// CredentialState is the outcome of verifying a stored credential.
type CredentialState string

const (
	CredentialValid   CredentialState = "VALID"
	CredentialInvalid CredentialState = "INVALID"
	CredentialExpired CredentialState = "EXPIRED"
	CredentialUnknown CredentialState = "UNKNOWN"
)

// CredentialResult is a credential verification summary. It never contains the
// credential itself.
type CredentialResult struct {
	State     CredentialState `json:"state"`
	SubjectID string          `json:"subject_id,omitempty"`
	Issuer    string          `json:"issuer,omitempty"`
	Detail    string          `json:"detail,omitempty"`
}

// CredentialVerifier validates a credential reference.
type CredentialVerifier interface {
	Descriptor() Descriptor
	Verify(ctx context.Context, credentialRef string) (CredentialResult, error)
}

// ---------------------------------------------------------------------------
// Sandbox automation
// ---------------------------------------------------------------------------

// SandboxRegisterRequest provisions a controlled test identity. Passwords only
// live in memory and, when needed, in the encrypted SecretStore.
type SandboxRegisterRequest struct {
	SubjectID   string
	Email       string
	DisplayName string
	Password    string
}

// SandboxConfirmRequest confirms an email using an extracted verification link
// obtained from a mailbox provider. The link is transient.
type SandboxConfirmRequest struct {
	Identity        IdentityHandle
	VerificationURL string
}

// SandboxAuthorizeRequest performs an OIDC authorization code + PKCE flow
// against a self-controlled IdP. The resulting tokens are written to the
// SecretStore by the driver; only the reference leaves the adapter.
type SandboxAuthorizeRequest struct {
	Identity    IdentityHandle
	Scopes      []string
	RedirectURI string
}

// SandboxCredentialRef is an opaque credential reference plus non-secret claims.
type SandboxCredentialRef struct {
	Ref       string    `json:"-"`
	Issuer    string    `json:"issuer"`
	Subject   string    `json:"subject"`
	Audience  string    `json:"audience,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// SandboxDriver automates only self-controlled sandbox IdPs. It must never be
// implemented for a real third-party platform.
type SandboxDriver interface {
	Descriptor() Descriptor
	Register(ctx context.Context, req SandboxRegisterRequest) (IdentityHandle, error)
	ConfirmEmail(ctx context.Context, req SandboxConfirmRequest) (VerificationState, error)
	AuthorizeOIDC(ctx context.Context, req SandboxAuthorizeRequest) (SandboxCredentialRef, error)
	Validate(ctx context.Context, cred SandboxCredentialRef) (CredentialResult, error)
}

// ProviderSnapshot locks adapter contract versions into a run at creation time.
type ProviderSnapshot struct {
	Mailbox    Descriptor `json:"mailbox"`
	Identity   Descriptor `json:"identity"`
	OAuth      Descriptor `json:"oauth"`
	Email      Descriptor `json:"email,omitempty"`
	Credential Descriptor `json:"credential,omitempty"`
	Sandbox    Descriptor `json:"sandbox,omitempty"`
}

// Mismatch returns the first descriptor whose contract version changed.
func (s ProviderSnapshot) Mismatch(current ProviderSnapshot) (string, bool) {
	pairs := []struct {
		name  string
		old   Descriptor
		newer Descriptor
	}{
		{"mailbox", s.Mailbox, current.Mailbox},
		{"identity", s.Identity, current.Identity},
		{"oauth", s.OAuth, current.OAuth},
		{"email", s.Email, current.Email},
	}
	for _, p := range pairs {
		if p.old.Name == "" {
			continue
		}
		if p.old.Name != p.newer.Name || p.old.ContractVersion != p.newer.ContractVersion {
			return p.name, true
		}
	}
	return "", false
}

// NewDisallowedError is the canonical "provider disabled" error. Lookups for
// disabled providers must fail before any network call.
func NewDisallowedError(kind Kind, name, reason string) error {
	if reason == "" {
		reason = "provider is disabled by configuration"
	}
	return errs.New(errs.ClassNotAuthorized, "provider."+string(kind), name+": "+reason)
}

// NewUnknownProviderError is returned for providers outside the allowlist.
func NewUnknownProviderError(kind Kind, name string) error {
	return errs.New(errs.ClassValidation, "provider."+string(kind), "unknown provider "+name)
}

// ---------------------------------------------------------------------------
// Existing-account email management (GitHub official REST only)
// ---------------------------------------------------------------------------

// EmailAddress is one mailbox registration on an existing account.
type EmailAddress struct {
	Email      string `json:"email"`
	Verified   bool   `json:"verified"`
	Primary    bool   `json:"primary"`
	Visibility string `json:"visibility,omitempty"`
}

// EmailManager manages email addresses on an EXISTING, operator-authorized
// account through the platform's official API. Implementations must never
// create accounts or bypass human confirmation.
type EmailManager interface {
	Descriptor() Descriptor
	AddEmail(ctx context.Context, email string) (EmailAddress, error)
	ListEmails(ctx context.Context) ([]EmailAddress, error)
}
