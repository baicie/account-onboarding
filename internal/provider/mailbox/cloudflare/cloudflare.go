// Package cloudflare adapts the self-hosted Cloudflare Temp Email worker
// (docs/08-integration-cloudflare.md) to the provider.MailboxProvider contract.
//
// Endpoints used:
//
//	POST {base}/admin/new_address      x-admin-auth (+ x-custom-auth)
//	GET  {base}/api/parsed_mails       Authorization: Bearer <address JWT>
//	GET  {base}/api/parsed_mail/{id}   Authorization: Bearer <address JWT>
//
// The address JWT is a credential: it is written through provider.SecretWriter
// under "secret://mailbox/{id}" and only the opaque reference is stored in
// provider.Mailbox. Neither the admin password, the site password nor the JWT
// is ever logged, returned in an error or embedded in a URL.
//
// Error mapping (documented because the engine branches on errs.Class):
//
//	400/422          -> VALIDATION              terminal, request was rejected
//	401/403          -> NOT_AUTHORIZED          admin/site credential rejected
//	404              -> NOT_FOUND (GetMessage)  / PROVIDER_BUG (list, create)
//	405              -> PROVIDER_BUG (list)     detail endpoint absent -> list fallback
//	409              -> CONFLICT                duplicate address (fail-closed, no retry)
//	410              -> AUTH_EXPIRED            address JWT no longer valid
//	429              -> RATE_LIMITED            Retry-After honoured
//	5xx              -> UPSTREAM_TRANSIENT
//	timeout (create) -> TIMEOUT_UNCERTAIN       a write may have landed upstream
//	timeout (reads)  -> UPSTREAM_TRANSIENT      safe to retry
//	transport error  -> UPSTREAM_TRANSIENT
//	cross-origin 3xx -> PROVIDER_BUG            credentials are never forwarded
//	non-JSON / schema-> PROVIDER_BUG
//
// Duplicate handling is fail-closed: the admin API has no lookup-by-name
// endpoint, so a retry with a random prefix would create an orphan mailbox that
// this service can neither reuse nor safely delete. A duplicate response (HTTP
// 409, or 400/422 whose JSON error says the address already exists) is reported
// as CONFLICT and requires operator reconciliation; no second address is
// created. Engine-level idempotency keys remain the primary duplicate guard.
//
// GetMessage tries the detail endpoint first and falls back to scanning
// /api/parsed_mails when the deployment reports 404/405 for the detail path.
package cloudflare

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

const (
	// DefaultBaseURL is the live worker endpoint from docs/08.
	DefaultBaseURL = "https://api.moonbyte.top"
	// DefaultDomain is the self-owned mail domain from docs/08.
	DefaultDomain = "moonbyte.top"
	// DefaultTimeout bounds every Cloudflare HTTP call.
	DefaultTimeout = 15 * time.Second
	// DefaultListLimit is the parsed_mails page size (docs/08 uses limit=20).
	DefaultListLimit = 20
	// DefaultMaxPages bounds list pagination.
	DefaultMaxPages = 3
	// DefaultMaxMessages bounds one ListMessages result.
	DefaultMaxMessages = 200
	// ContractVersion is the adapter contract version locked into runs.
	ContractVersion = "v1"
	// ProviderName is the descriptor name used for registry lookups.
	ProviderName = "cloudflare"

	// SecretRefKind is the reference namespace for mailbox credentials. The
	// secret store derives the stored kind from the reference
	// ("secret://mailbox/{id}"), so Put must be called with this kind; docs/04
	// fixes the reference as "secret://mailbox/{id}".
	SecretRefKind = "mailbox"

	opCreateAddress = "cloudflare.create_address"
	opListMessages  = "cloudflare.list_messages"
	opGetMessage    = "cloudflare.get_message"

	maxResponseBytes = 8 << 20
	maxRedirects     = 3
	maxCredentialLen = 8192
)

// defaultAllowedHosts pins the live endpoint. Tests and local deployments opt in
// explicitly through Config.AllowedHosts / Config.AllowLoopback.
var defaultAllowedHosts = []string{"api.moonbyte.top"}

// errRedirectBlocked signals a redirect that would forward credentials to a
// different origin.
var errRedirectBlocked = errors.New("cloudflare: redirect to a different origin refused")

// Config configures the Cloudflare Temp Email adapter.
type Config struct {
	// BaseURL defaults to https://api.moonbyte.top. The host must be allowlisted
	// (or loopback with AllowLoopback=true for tests).
	BaseURL string
	// AllowedHosts overrides the default live allowlist. Entries are hostnames
	// without a port.
	AllowedHosts []string
	// AllowLoopback permits a loopback base URL (httptest / local worker).
	// It is the documented test-only override and must never be enabled in
	// production wiring.
	AllowLoopback bool
	// AdminAuth is the worker admin password (x-admin-auth). Required.
	AdminAuth string
	// SitePassword is the optional worker private-site password
	// (x-custom-auth). Stored only to set the header.
	SitePassword string
	// Domain defaults to moonbyte.top.
	Domain string
	// AllowedDomains optionally widens the requestable domains. The configured
	// Domain is always allowed.
	AllowedDomains []string
	// Timeout bounds each HTTP call. Defaults to DefaultTimeout.
	Timeout time.Duration
	// HTTPClient optionally supplies a transport; timeout and redirect policy
	// are replaced by the adapter's own.
	HTTPClient *http.Client
	// Secrets stores the address JWT. Required.
	Secrets provider.SecretWriter
	// Resolver resolves mailbox SecretRefs to address JWTs. Required.
	Resolver provider.SecretResolver
	// ListLimit is the parsed_mails page size. Defaults to DefaultListLimit.
	ListLimit int
	// MaxPages bounds pagination. Defaults to DefaultMaxPages.
	MaxPages int
	// MaxMessages bounds a single ListMessages result. Defaults to
	// DefaultMaxMessages.
	MaxMessages int
	// DisableDetailEndpoint forces GetMessage to use the list-scan fallback.
	DisableDetailEndpoint bool
	// Disabled gates the adapter: Descriptor reports Enabled=false and every
	// call fails with NOT_AUTHORIZED before any network I/O.
	Disabled bool
}

// Provider implements provider.MailboxProvider against Cloudflare Temp Email.
// It deliberately does NOT implement provider.MailboxCleaner: docs/08 forbids
// automatic deletion of real long-lived addresses.
type Provider struct {
	base           *url.URL
	domain         string
	allowedDomains []string
	adminAuth      string
	sitePassword   string
	timeout        time.Duration
	secrets        provider.SecretWriter
	resolver       provider.SecretResolver
	listLimit      int
	maxPages       int
	maxMessages    int
	disableDetail  bool
	disabled       bool
	client         *http.Client
}

var _ provider.MailboxProvider = (*Provider)(nil)

// New validates the configuration and builds the adapter. It performs no
// network I/O; every credential is checked for header safety without being
// echoed in the returned error.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.AdminAuth) == "" {
		return nil, configErr("admin credential is required")
	}
	if !headerSafe(cfg.AdminAuth) {
		return nil, configErr("admin credential contains characters that are not header safe")
	}
	if cfg.SitePassword != "" && !headerSafe(cfg.SitePassword) {
		return nil, configErr("site password contains characters that are not header safe")
	}
	if cfg.Secrets == nil {
		return nil, configErr("a secret writer is required to store the address credential")
	}
	if cfg.Resolver == nil {
		return nil, configErr("a secret resolver is required to read the address credential")
	}

	base, err := parseBaseURL(cfg)
	if err != nil {
		return nil, err
	}

	domain := strings.ToLower(strings.TrimSpace(cfg.Domain))
	if domain == "" {
		domain = DefaultDomain
	}
	if !validDomainName(domain) {
		return nil, configErr("default domain is not a valid DNS name")
	}
	allowedDomains := []string{domain}
	for _, entry := range cfg.AllowedDomains {
		d := strings.ToLower(strings.TrimSpace(entry))
		if !validDomainName(d) {
			return nil, configErr("allowed domain is not a valid DNS name")
		}
		if d != domain {
			allowedDomains = append(allowedDomains, d)
		}
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	listLimit := cfg.ListLimit
	if listLimit <= 0 {
		listLimit = DefaultListLimit
	}
	maxPages := cfg.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}
	maxMessages := cfg.MaxMessages
	if maxMessages <= 0 {
		maxMessages = DefaultMaxMessages
	}

	p := &Provider{
		base:           base,
		domain:         domain,
		allowedDomains: allowedDomains,
		adminAuth:      cfg.AdminAuth,
		sitePassword:   cfg.SitePassword,
		timeout:        timeout,
		secrets:        cfg.Secrets,
		resolver:       cfg.Resolver,
		listLimit:      listLimit,
		maxPages:       maxPages,
		maxMessages:    maxMessages,
		disableDetail:  cfg.DisableDetailEndpoint,
		disabled:       cfg.Disabled,
	}

	client := http.Client{}
	if cfg.HTTPClient != nil {
		client = *cfg.HTTPClient
	}
	client.Timeout = timeout
	client.CheckRedirect = p.checkRedirect
	p.client = &client
	return p, nil
}

// Descriptor implements provider.MailboxProvider.
func (p *Provider) Descriptor() provider.Descriptor {
	d := provider.Descriptor{
		Name:            ProviderName,
		Kind:            provider.KindMailbox,
		ContractVersion: ContractVersion,
		Enabled:         !p.disabled,
		Live:            true,
		Capabilities: []string{
			"address.create",
			"message.list",
			"message.get",
		},
	}
	if p.disabled {
		d.RestrictionReason = "cloudflare adapter is disabled by configuration"
	}
	return d
}

// CreateAddress provisions an address and stores its JWT in the secret store.
// Only the opaque "secret://mailbox/{id}" reference leaves the adapter.
func (p *Provider) CreateAddress(ctx context.Context, req provider.CreateAddressRequest) (provider.Mailbox, error) {
	const op = opCreateAddress
	if err := p.ensureEnabled(op); err != nil {
		return provider.Mailbox{}, err
	}
	if err := ctx.Err(); err != nil {
		return provider.Mailbox{}, errs.New(errs.ClassCancelled, op, "request cancelled")
	}

	name, err := p.localPart(req)
	if err != nil {
		return provider.Mailbox{}, err
	}
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	if domain == "" {
		domain = p.domain
	}
	if !validDomainName(domain) {
		return provider.Mailbox{}, errs.New(errs.ClassValidation, op, "requested mailbox domain is not a valid DNS name")
	}
	if !p.domainAllowed(domain) {
		return provider.Mailbox{}, errs.New(errs.ClassValidation, op, "requested mailbox domain is not allowed for this provider")
	}

	mb, duplicate, err := p.createOnce(ctx, op, name, domain, req.EnablePrefix)
	if err != nil {
		return provider.Mailbox{}, err
	}
	if duplicate {
		// Never silently create a second address: the worker has no
		// lookup-by-name API, so a retry with a random prefix would leave an
		// orphan mailbox that this service can neither reuse nor safely delete.
		// Fail closed and require an operator to reconcile.
		return provider.Mailbox{}, errs.New(errs.ClassConflict, op,
			"cloudflare reports this address already exists; refusing to create a second address (manual reconciliation required)")
	}
	return mb, nil
}

// createOnce performs one POST /admin/new_address call. duplicate=true reports a
// duplicate-address response and leaves the classification to CreateAddress.
func (p *Provider) createOnce(ctx context.Context, op, name, domain string, enablePrefix bool) (provider.Mailbox, bool, error) {
	payload, err := json.Marshal(newAddressRequest{
		EnablePrefix: enablePrefix,
		Name:         name,
		Domain:       domain,
	})
	if err != nil {
		return provider.Mailbox{}, false, errs.New(errs.ClassInternal, op, "could not encode cloudflare request")
	}
	req, err := p.adminRequest(ctx, http.MethodPost, "/admin/new_address", nil, payload)
	if err != nil {
		return provider.Mailbox{}, false, err
	}
	res, err := p.execute(ctx, op, req)
	if err != nil {
		return provider.Mailbox{}, false, err
	}
	if res.status < 200 || res.status > 299 {
		if isDuplicateResponse(res) {
			return provider.Mailbox{}, true, nil
		}
		if res.status == http.StatusNotFound {
			return provider.Mailbox{}, false, errs.New(errs.ClassProviderBug, op, "cloudflare address endpoint is missing").WithHTTPStatus(res.status)
		}
		return provider.Mailbox{}, false, p.statusError(op, res)
	}

	var parsed newAddressResponse
	if err := json.Unmarshal(res.body, &parsed); err != nil {
		return provider.Mailbox{}, false, errs.New(errs.ClassProviderBug, op, "cloudflare returned a non-JSON address response").WithHTTPStatus(res.status)
	}
	jwt := strings.TrimSpace(parsed.JWT)
	if jwt == "" {
		return provider.Mailbox{}, false, errs.New(errs.ClassProviderBug, op, "cloudflare address response is missing the address credential").WithHTTPStatus(res.status)
	}
	if !headerSafe(jwt) {
		return provider.Mailbox{}, false, errs.New(errs.ClassProviderBug, op, "cloudflare returned an unusable address credential").WithHTTPStatus(res.status)
	}
	address := strings.TrimSpace(parsed.Address)
	if _, ok := canonicalMailbox(address); !ok {
		return provider.Mailbox{}, false, errs.New(errs.ClassProviderBug, op, "cloudflare address response is missing a valid address").WithHTTPStatus(res.status)
	}

	id := decodeAddressID(parsed.AddressID)
	if id == "" {
		id = address
	}
	ref := secretRef(id)
	kind, ok := refKind(ref)
	if !ok {
		return provider.Mailbox{}, false, errs.New(errs.ClassInternal, op, "could not build a mailbox secret reference")
	}
	if err := p.secrets.Put(ctx, ref, kind, jwt); err != nil {
		// The cause is replaced on purpose: a secret store error must never
		// echo the plaintext credential.
		return provider.Mailbox{}, false, errs.Wrap(errs.ClassInternal, op, "cloudflare address credential could not be stored", errors.New("secret store write failed"))
	}

	return provider.Mailbox{
		ID:        id,
		Address:   address,
		Provider:  ProviderName,
		SecretRef: ref,
		CreatedAt: time.Now().UTC(),
	}, false, nil
}

// ListMessages lists parsed mails for the mailbox, newest page first as the
// worker reports them.
func (p *Provider) ListMessages(ctx context.Context, mailbox provider.Mailbox, since time.Time) ([]provider.Message, error) {
	const op = opListMessages
	if err := p.ensureEnabled(op); err != nil {
		return nil, err
	}
	jwt, err := p.addressJWT(ctx, op, mailbox)
	if err != nil {
		return nil, err
	}
	return p.listWithJWT(ctx, op, mailbox, jwt, since)
}

// GetMessage reads one parsed mail. It uses GET /api/parsed_mail/{id} when the
// deployment exposes it and falls back to scanning /api/parsed_mails on 404/405.
func (p *Provider) GetMessage(ctx context.Context, mailbox provider.Mailbox, id string) (provider.Message, error) {
	const op = opGetMessage
	if err := p.ensureEnabled(op); err != nil {
		return provider.Message{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return provider.Message{}, errs.New(errs.ClassValidation, op, "message id is required")
	}
	jwt, err := p.addressJWT(ctx, op, mailbox)
	if err != nil {
		return provider.Message{}, err
	}

	if !p.disableDetail {
		msg, err := p.detailWithJWT(ctx, op, mailbox, jwt, id)
		if err == nil {
			return msg, nil
		}
		status := errs.HTTPStatusOf(err)
		if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			return provider.Message{}, err
		}
		// Fall through to the documented list-scan fallback.
	}

	msgs, err := p.listWithJWT(ctx, op, mailbox, jwt, time.Time{})
	if err != nil {
		return provider.Message{}, err
	}
	for _, msg := range msgs {
		if msg.ID == id {
			return msg, nil
		}
	}
	return provider.Message{}, errs.New(errs.ClassNotFound, op, "message not found in the mailbox")
}

// listWithJWT performs the paged GET /api/parsed_mails read.
func (p *Provider) listWithJWT(ctx context.Context, op string, mailbox provider.Mailbox, jwt string, since time.Time) ([]provider.Message, error) {
	out := make([]provider.Message, 0)
	offset := 0
	for page := 0; page < p.maxPages; page++ {
		items, err := p.parsedMails(ctx, op, jwt, p.listLimit, offset)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			break
		}
		for _, raw := range items {
			msg, ok, err := normalizeListItem(raw)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			msg.MailboxID = mailbox.ID
			if !since.IsZero() && !msg.ReceivedAt.IsZero() && msg.ReceivedAt.Before(since.UTC()) {
				continue
			}
			out = append(out, msg)
			if len(out) >= p.maxMessages {
				return out, nil
			}
		}
		if len(items) < p.listLimit {
			break
		}
		offset += len(items)
	}
	return out, nil
}

// parsedMails requests one page and returns the raw mail items.
func (p *Provider) parsedMails(ctx context.Context, op, jwt string, limit, offset int) ([]json.RawMessage, error) {
	query := url.Values{}
	query.Set("limit", strconv.Itoa(limit))
	query.Set("offset", strconv.Itoa(offset))
	req, err := p.userRequest(ctx, http.MethodGet, "/api/parsed_mails", query, nil, jwt)
	if err != nil {
		return nil, err
	}
	res, err := p.execute(ctx, op, req)
	if err != nil {
		return nil, err
	}
	if res.status < 200 || res.status > 299 {
		if res.status == http.StatusNotFound || res.status == http.StatusMethodNotAllowed {
			return nil, errs.New(errs.ClassProviderBug, op, "cloudflare parsed_mails endpoint is missing").WithHTTPStatus(res.status)
		}
		return nil, p.statusError(op, res)
	}
	items, err := parseMailList(res.body, op)
	if err != nil {
		return nil, err
	}
	return items, nil
}

// detailWithJWT requests GET /api/parsed_mail/{id}.
func (p *Provider) detailWithJWT(ctx context.Context, op string, mailbox provider.Mailbox, jwt, id string) (provider.Message, error) {
	req, err := p.userRequest(ctx, http.MethodGet, "/api/parsed_mail/"+url.PathEscape(id), nil, nil, jwt)
	if err != nil {
		return provider.Message{}, err
	}
	res, err := p.execute(ctx, op, req)
	if err != nil {
		return provider.Message{}, err
	}
	if res.status < 200 || res.status > 299 {
		return provider.Message{}, p.statusError(op, res)
	}
	msg, ok, err := normalizeDetail(res.body)
	if err != nil {
		return provider.Message{}, err
	}
	if !ok {
		return provider.Message{}, errs.New(errs.ClassProviderBug, op, "cloudflare detail response has an unrecognized schema").WithHTTPStatus(res.status)
	}
	if strings.TrimSpace(msg.ID) == "" {
		msg.ID = id
	}
	msg.MailboxID = mailbox.ID
	return msg, nil
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// newAddressRequest is the documented POST /admin/new_address body
// (docs/08-integration-cloudflare.md).
type newAddressRequest struct {
	EnablePrefix bool   `json:"enablePrefix"`
	Name         string `json:"name"`
	Domain       string `json:"domain"`
}

// newAddressResponse is the documented success response. address_id is decoded
// tolerantly because deployments return it as a number or as a string.
type newAddressResponse struct {
	JWT       string          `json:"jwt"`
	Address   string          `json:"address"`
	AddressID json.RawMessage `json:"address_id"`
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

type httpResult struct {
	status int
	header http.Header
	body   []byte
}

func (p *Provider) adminRequest(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Request, error) {
	req, err := p.newRequest(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-admin-auth", p.adminAuth)
	if p.sitePassword != "" {
		req.Header.Set("x-custom-auth", p.sitePassword)
	}
	return req, nil
}

func (p *Provider) userRequest(ctx context.Context, method, path string, query url.Values, body []byte, jwt string) (*http.Request, error) {
	req, err := p.newRequest(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	if p.sitePassword != "" {
		req.Header.Set("x-custom-auth", p.sitePassword)
	}
	return req, nil
}

func (p *Provider) newRequest(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Request, error) {
	u := *p.base
	u.Path = joinPath(u.Path, path)
	if query != nil {
		u.RawQuery = query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, errs.New(errs.ClassInternal, "cloudflare.request", "could not build cloudflare request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// execute applies the per-call timeout, runs the request and reads a bounded
// response body. Transport failures are already classified.
func (p *Provider) execute(ctx context.Context, op string, req *http.Request) (*httpResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req = req.WithContext(reqCtx)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, p.transportError(op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, p.transportError(op, err)
	}
	if len(body) > maxResponseBytes {
		return nil, errs.New(errs.ClassProviderBug, op, "cloudflare response exceeded the size limit")
	}
	return &httpResult{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// transportError classifies transport failures. A create timeout may have
// landed upstream (TIMEOUT_UNCERTAIN); reads are safe to retry
// (UPSTREAM_TRANSIENT).
func (p *Provider) transportError(op string, err error) error {
	if errors.Is(err, context.Canceled) {
		return errs.Wrap(errs.ClassCancelled, op, "request cancelled", err)
	}
	if errors.Is(err, errRedirectBlocked) {
		return errs.Wrap(errs.ClassProviderBug, op, "cloudflare redirect to a different origin refused", err)
	}
	if isTimeout(err) {
		return errs.Wrap(p.timeoutClass(op), op, "cloudflare request timed out", err)
	}
	return errs.Wrap(errs.ClassUpstreamTransient, op, "cloudflare request failed", err)
}

func (p *Provider) timeoutClass(op string) errs.Class {
	if op == opCreateAddress {
		return errs.ClassTimeoutUncertain
	}
	return errs.ClassUpstreamTransient
}

// statusError maps a non-2xx response to the taxonomy. The response body is
// never part of the error text.
func (p *Provider) statusError(op string, res *httpResult) error {
	class := errs.FromHTTPStatus(res.status)
	switch res.status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		class = errs.ClassValidation
	case http.StatusUnauthorized, http.StatusForbidden:
		class = errs.ClassNotAuthorized
	case http.StatusNotFound:
		class = errs.ClassNotFound
	case http.StatusConflict:
		class = errs.ClassConflict
	case http.StatusGone:
		class = errs.ClassAuthExpired
	case http.StatusTooManyRequests:
		class = errs.ClassRateLimited
	}
	err := errs.New(class, op, fmt.Sprintf("cloudflare returned HTTP %d", res.status)).WithHTTPStatus(res.status)
	if retryAfter := parseRetryAfter(res.header.Get("Retry-After"), time.Now()); retryAfter > 0 {
		err = err.WithRetryAfter(retryAfter)
	}
	return err
}

// checkRedirect guarantees credentials are never forwarded to a different
// origin (scheme + host + port).
func (p *Provider) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= maxRedirects {
		return errors.New("cloudflare: too many redirects")
	}
	origin := via[0].URL
	if !strings.EqualFold(req.URL.Scheme, origin.Scheme) || !strings.EqualFold(req.URL.Host, origin.Host) {
		return errRedirectBlocked
	}
	return nil
}

// addressJWT resolves the opaque reference to the address JWT. Resolver
// failures are reported as NOT_AUTHORIZED without the underlying cause, because
// the cause could contain credential material.
func (p *Provider) addressJWT(ctx context.Context, op string, mailbox provider.Mailbox) (string, error) {
	ref := strings.TrimSpace(mailbox.SecretRef)
	if _, ok := refKind(ref); !ok {
		return "", errs.New(errs.ClassNotAuthorized, op, "mailbox address credential reference is missing")
	}
	jwt, err := p.resolver.Resolve(ctx, ref)
	if err != nil {
		return "", errs.New(errs.ClassNotAuthorized, op, "mailbox address credential could not be resolved")
	}
	jwt = strings.TrimSpace(jwt)
	if jwt == "" || len(jwt) > maxCredentialLen || !headerSafe(jwt) {
		return "", errs.New(errs.ClassNotAuthorized, op, "mailbox address credential is unusable")
	}
	return jwt, nil
}

// secretRef builds the canonical opaque mailbox reference.
func secretRef(id string) string {
	return provider.SecretRefPrefix + SecretRefKind + "/" + sanitizeRefID(id)
}

// refKind extracts the kind segment of an opaque reference. The secret store
// requires Put's kind argument to equal the reference kind, so adapters must
// pass the value returned here instead of an independent label.
func refKind(ref string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(ref), provider.SecretRefPrefix)
	if !ok {
		return "", false
	}
	kind, id, found := strings.Cut(rest, "/")
	if !found || kind == "" || id == "" {
		return "", false
	}
	return kind, true
}

func (p *Provider) ensureEnabled(op string) error {
	if p.disabled {
		return provider.NewDisallowedError(provider.KindMailbox, ProviderName, "cloudflare adapter is disabled by configuration")
	}
	return nil
}

func (p *Provider) domainAllowed(domain string) bool {
	for _, allowed := range p.allowedDomains {
		if domain == allowed {
			return true
		}
	}
	return false
}

// localPart chooses the request local part: explicit name, stable key
// derivation, or random suffix.
func (p *Provider) localPart(req provider.CreateAddressRequest) (string, error) {
	const op = opCreateAddress
	if name := strings.ToLower(strings.TrimSpace(req.Name)); name != "" {
		if !validLocalPart(name) {
			return "", errs.New(errs.ClassValidation, op, "requested mailbox name is not a safe local part")
		}
		return name, nil
	}
	if key := strings.TrimSpace(req.Key); key != "" {
		sum := sha256.Sum256([]byte(key))
		return "k" + hex.EncodeToString(sum[:6]), nil
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	return "r" + hex.EncodeToString(sum[:6]), nil
}

// ---------------------------------------------------------------------------
// Configuration helpers
// ---------------------------------------------------------------------------

func parseBaseURL(cfg Config) (*url.URL, error) {
	raw := strings.TrimSpace(cfg.BaseURL)
	if raw == "" {
		raw = DefaultBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, configErr("base url is not a valid URL")
	}
	if u.User != nil {
		return nil, configErr("base url must not embed credentials")
	}
	if u.Host == "" {
		return nil, configErr("base url must include a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, configErr("base url must not include a query or fragment")
	}

	host := strings.ToLower(u.Hostname())
	loopback := isLoopbackHost(host)
	if loopback && !cfg.AllowLoopback {
		return nil, configErr("loopback base url requires the explicit AllowLoopback test override")
	}
	if !loopback {
		allowed := cfg.AllowedHosts
		if len(allowed) == 0 {
			allowed = defaultAllowedHosts
		}
		if !hostAllowed(host, allowed) {
			return nil, configErr("base url host is not in the cloudflare allowlist")
		}
		if u.Scheme != "https" {
			return nil, configErr("base url must use https outside the loopback test override")
		}
		return u, nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, configErr("base url must use http or https")
	}
	return u, nil
}

func hostAllowed(host string, allowed []string) bool {
	for _, entry := range allowed {
		if h := strings.ToLower(strings.TrimSpace(entry)); h != "" && h == host {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func configErr(summary string) error {
	return errs.New(errs.ClassValidation, "cloudflare.config", summary)
}

// headerSafe reports whether a value can be sent as an HTTP header value
// without control characters (which would also be a header-injection vector).
func headerSafe(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func canonicalMailbox(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if lt := strings.LastIndexByte(s, '<'); lt >= 0 {
		if gt := strings.IndexByte(s[lt:], '>'); gt > 0 {
			s = s[lt+1 : lt+gt]
		} else {
			return "", false
		}
	}
	s = strings.ToLower(strings.TrimSpace(strings.Trim(s, `"'`)))
	if s == "" || strings.Count(s, "@") != 1 || strings.ContainsAny(s, " \t\r\n,;<>") {
		return "", false
	}
	return s, true
}

func validLocalPart(local string) bool {
	if local == "" || len(local) > 64 {
		return false
	}
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("._-", r):
		default:
			return false
		}
	}
	return true
}

func validDomainName(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

// sanitizeRefID keeps a provider identifier safe for use inside a secret
// reference: a bounded character set, no path separators.
func sanitizeRefID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_', r == '@':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= 128 {
			break
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		sum := sha256.Sum256([]byte(id))
		return "id-" + hex.EncodeToString(sum[:8])
	}
	return out
}

func joinPath(base, suffix string) string {
	base = strings.TrimSuffix(base, "/")
	if suffix == "" {
		return base
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return base + suffix
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delta := when.Sub(now); delta > 0 {
			return delta
		}
	}
	return 0
}
