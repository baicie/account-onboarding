package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

// Compile-time proof that the adapter satisfies the frozen contracts.
var (
	_ provider.EmailManager       = (*Adapter)(nil)
	_ provider.CredentialVerifier = (*Adapter)(nil)
)

const defaultDisabledReason = "GitHub email manager is not configured for this deployment"

// errRedirectRefused aborts a redirect before the Authorization header can be
// replayed against a host outside the allowlist.
var errRedirectRefused = errors.New("github: redirect refused")

// Adapter manages email addresses on an existing, operator-authorized GitHub
// account. It holds no credential material: only the opaque secret reference
// and a resolver. The token is fetched per call and stays inside the request
// that needs it.
type Adapter struct {
	cfg     Config
	base    *url.URL
	secrets provider.SecretResolver
	client  *http.Client

	// disabledReason non-empty turns every capability into NOT_AUTHORIZED with
	// zero network calls (registry gate: Descriptor().Enabled == false).
	disabledReason string

	// now is injectable for deterministic Retry-After tests.
	now func() time.Time
}

// New builds a live-capable adapter. Configuration errors are reported as
// VALIDATION classified errors; the adapter is never constructed half-valid.
func New(cfg Config, secrets provider.SecretResolver) (*Adapter, error) {
	if secrets == nil {
		return nil, errs.New(errs.ClassValidation, "github.config",
			"a secret resolver is required to read the token reference")
	}
	effective, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(effective.BaseURL)
	if err != nil {
		return nil, errs.New(errs.ClassValidation, "github.config", "base URL is not a valid URL")
	}
	a := &Adapter{
		cfg:     effective,
		base:    base,
		secrets: secrets,
		now:     time.Now,
	}
	a.client = &http.Client{
		Timeout: effective.Timeout,
		// A cross-host redirect could replay the Authorization header against a
		// host the operator never allowed. Refuse before the header is written.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errRedirectRefused
			}
			if req.URL.Scheme != base.Scheme || !strings.EqualFold(req.URL.Host, base.Host) {
				return errRedirectRefused
			}
			return nil
		},
	}
	return a, nil
}

// NewDisabled returns an adapter that is present in the provider catalog with
// Enabled=false and an explicit restriction reason. Every capability call
// fails as NOT_AUTHORIZED before touching the network.
func NewDisabled(reason string) *Adapter {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = defaultDisabledReason
	}
	return &Adapter{disabledReason: reason, now: time.Now}
}

// Descriptor implements provider.EmailManager and provider.CredentialVerifier.
// The kind is email because that is the registry slot the control plane
// resolves (provider.Registry.AddEmailManager); the credential self-check is an
// optional capability of the same adapter.
func (a *Adapter) Descriptor() provider.Descriptor {
	caps := []string{"email.add", "email.list", "credential.verify"}
	if a == nil || a.disabledReason != "" {
		reason := defaultDisabledReason
		if a != nil {
			reason = a.disabledReason
		}
		return provider.Descriptor{
			Name:              ProviderName,
			Kind:              provider.KindEmail,
			ContractVersion:   ContractVersion,
			Enabled:           false,
			Live:              false,
			Capabilities:      caps,
			RestrictionReason: reason,
		}
	}
	return provider.Descriptor{
		Name:            ProviderName,
		Kind:            provider.KindEmail,
		ContractVersion: ContractVersion,
		Enabled:         true,
		Live:            true,
		Capabilities:    caps,
	}
}

// requireEnabled enforces the activation gate before any network call.
func (a *Adapter) requireEnabled() error {
	reason := defaultDisabledReason
	if a != nil && a.disabledReason != "" {
		reason = a.disabledReason
	}
	if a == nil || a.disabledReason != "" {
		return provider.NewDisallowedError(provider.KindEmail, ProviderName, reason)
	}
	return nil
}

// AddEmail registers a self-owned address on the existing account through
// POST /user/emails.
//
// Idempotency: GitHub answers 409/422 when the address is already registered.
// That is treated as success-after-reconcile, not as a terminal failure: the
// adapter re-reads GET /user/emails and returns the authoritative record. The
// address is never marked verified by this call; a human must confirm GitHub's
// verification email and the caller polls ListEmails afterwards.
func (a *Adapter) AddEmail(ctx context.Context, email string) (provider.EmailAddress, error) {
	const op = "github.email.add"

	if err := a.requireEnabled(); err != nil {
		return provider.EmailAddress{}, err
	}
	addr, err := a.normalizeEmail(email, op)
	if err != nil {
		return provider.EmailAddress{}, err
	}
	token, err := a.resolveToken(ctx, a.cfg.TokenRef)
	if err != nil {
		return provider.EmailAddress{}, err
	}
	payload, err := json.Marshal(struct {
		Emails []string `json:"emails"`
	}{Emails: []string{addr}})
	if err != nil {
		return provider.EmailAddress{}, errs.Wrap(errs.ClassProviderBug, op, "cannot encode the request body", err)
	}

	resp, err := a.do(ctx, op, http.MethodPost, pathUserEmails, nil, payload, token)
	if err != nil {
		return provider.EmailAddress{}, err
	}

	if !is2xx(resp.status) {
		if isDuplicateResponse(resp) {
			found, ok, rerr := a.reconcile(ctx, token, addr)
			if rerr != nil {
				return provider.EmailAddress{}, rerr
			}
			if ok {
				return found, nil
			}
			return provider.EmailAddress{}, errs.New(errs.ClassValidation, op,
				"github reported the address as already present but it is not visible on the account; reconcile manually").
				WithHTTPStatus(resp.status)
		}
		return provider.EmailAddress{}, a.classifyStatus(op, resp)
	}

	added, parseErr := parseEmailArray(resp.body)
	if parseErr == nil {
		for _, candidate := range added {
			if strings.EqualFold(candidate.Email, addr) {
				return candidate, nil
			}
		}
	}

	// 2xx but the body did not echo the address (or was empty): the list is the
	// source of truth. Reconcile before reporting an unverified registration.
	found, ok, rerr := a.reconcile(ctx, token, addr)
	if rerr != nil {
		return provider.EmailAddress{}, rerr
	}
	if ok {
		return found, nil
	}
	if parseErr != nil {
		return provider.EmailAddress{}, errs.New(errs.ClassProviderBug, op,
			"github accepted the address but returned an unexpected response body").WithHTTPStatus(resp.status)
	}
	// Registration succeeded, propagation to the list may lag. Report the
	// address as unverified so no caller can mistake it for a verified one.
	return provider.EmailAddress{Email: addr}, nil
}

// ListEmails returns the addresses registered on the existing account through
// GET /user/emails, including the provider-reported verified/primary state.
func (a *Adapter) ListEmails(ctx context.Context) ([]provider.EmailAddress, error) {
	if err := a.requireEnabled(); err != nil {
		return nil, err
	}
	token, err := a.resolveToken(ctx, a.cfg.TokenRef)
	if err != nil {
		return nil, err
	}
	return a.listEmails(ctx, token)
}

// Verify implements provider.CredentialVerifier. It checks that the referenced
// token is still accepted by GitHub (GET /user) without ever returning the
// token. An empty credentialRef verifies the configured token reference.
func (a *Adapter) Verify(ctx context.Context, credentialRef string) (provider.CredentialResult, error) {
	const op = "github.credential.verify"

	if err := a.requireEnabled(); err != nil {
		return provider.CredentialResult{}, err
	}
	ref := strings.TrimSpace(credentialRef)
	if ref == "" {
		ref = a.cfg.TokenRef
	}
	token, err := a.resolveToken(ctx, ref)
	if err != nil {
		return provider.CredentialResult{}, err
	}
	resp, err := a.do(ctx, op, http.MethodGet, pathUser, nil, nil, token)
	if err != nil {
		return provider.CredentialResult{}, err
	}
	switch {
	case resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden:
		// A rejected token is a verification result, not a transport failure.
		return provider.CredentialResult{
			State:  provider.CredentialInvalid,
			Issuer: "github.com",
			Detail: "github rejected the token; it is invalid, expired or missing email permission",
		}, nil
	case !is2xx(resp.status):
		return provider.CredentialResult{}, a.classifyStatus(op, resp)
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.Unmarshal(resp.body, &user); err != nil || user.ID <= 0 {
		return provider.CredentialResult{}, errs.New(errs.ClassProviderBug, op,
			"github returned an unexpected user schema").WithHTTPStatus(resp.status)
	}
	return provider.CredentialResult{
		State:     provider.CredentialValid,
		SubjectID: strconv.FormatInt(user.ID, 10),
		Issuer:    "github.com",
	}, nil
}

// listEmails is the internal read used both by ListEmails and by duplicate
// reconciliation.
func (a *Adapter) listEmails(ctx context.Context, token string) ([]provider.EmailAddress, error) {
	const op = "github.email.list"

	query := url.Values{"per_page": []string{"100"}}
	resp, err := a.do(ctx, op, http.MethodGet, pathUserEmails, query, nil, token)
	if err != nil {
		return nil, err
	}
	if !is2xx(resp.status) {
		return nil, a.classifyStatus(op, resp)
	}
	list, parseErr := parseEmailArray(resp.body)
	if parseErr != nil {
		return nil, errs.New(errs.ClassProviderBug, op,
			"github returned an unexpected email list schema").WithHTTPStatus(resp.status)
	}
	return list, nil
}

// reconcile looks the address up in the authoritative list.
func (a *Adapter) reconcile(ctx context.Context, token, addr string) (provider.EmailAddress, bool, error) {
	list, err := a.listEmails(ctx, token)
	if err != nil {
		return provider.EmailAddress{}, false, err
	}
	for _, candidate := range list {
		if strings.EqualFold(candidate.Email, addr) {
			return candidate, true, nil
		}
	}
	return provider.EmailAddress{}, false, nil
}

// resolveToken reads the opaque reference at call time. The plaintext is never
// stored on the adapter and never embedded in an error summary.
func (a *Adapter) resolveToken(ctx context.Context, ref string) (string, error) {
	if a == nil || a.secrets == nil || strings.TrimSpace(ref) == "" {
		return "", errs.New(errs.ClassNotAuthorized, "github.auth",
			"the github token reference is not configured")
	}
	token, err := a.secrets.Resolve(ctx, ref)
	if err != nil {
		return "", errs.Wrap(errs.ClassNotAuthorized, "github.auth",
			"the configured github token reference cannot be resolved", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errs.New(errs.ClassNotAuthorized, "github.auth",
			"the configured github token reference resolved to an empty secret")
	}
	return token, nil
}

// normalizeEmail validates and normalizes an address before any network call.
func (a *Adapter) normalizeEmail(raw, op string) (string, error) {
	email := strings.TrimSpace(raw)
	if email == "" {
		return "", errs.New(errs.ClassValidation, op, "an email address is required")
	}
	if len(email) > 254 {
		return "", errs.New(errs.ClassValidation, op, "the email address is too long")
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Name != "" || parsed.Address != email {
		return "", errs.New(errs.ClassValidation, op, "the email address is not a plain address")
	}
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return "", errs.New(errs.ClassValidation, op, "the email address has no valid domain")
	}
	domain := strings.ToLower(email[at+1:])
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", errs.New(errs.ClassValidation, op, "the email address domain is not valid")
	}
	if len(a.cfg.AllowedDomains) > 0 {
		allowed := false
		for _, candidate := range a.cfg.AllowedDomains {
			if strings.EqualFold(candidate, domain) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", errs.New(errs.ClassValidation, op,
				"the email address domain is not in the configured allowlist")
		}
	}
	return email, nil
}

// apiResponse is the raw upstream answer used for status/schema classification.
// The body is intentionally not carried into any error or DTO.
type apiResponse struct {
	status int
	header http.Header
	body   []byte
}

// do performs one allowlisted request. Transport problems are classified here;
// HTTP status classification is left to the caller so AddEmail can intercept
// duplicate responses before they become terminal errors.
func (a *Adapter) do(ctx context.Context, op, method, path string, query url.Values, payload []byte, token string) (apiResponse, error) {
	if _, ok := allowedPaths[path]; !ok {
		return apiResponse{}, errs.New(errs.ClassProviderBug, op,
			"internal endpoint allowlist violation")
	}
	u := *a.base
	u.Path = path
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return apiResponse{}, errs.Wrap(errs.ClassProviderBug, op, "cannot build the upstream request", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", a.cfg.UserAgent)
	req.Header.Set("X-GitHub-Api-Version", a.cfg.APIVersion)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return apiResponse{}, a.transportError(op, err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		return apiResponse{}, a.transportError(op, readErr)
	}
	if len(raw) > maxResponseBytes {
		return apiResponse{}, errs.New(errs.ClassProviderBug, op,
			"github response exceeded the size limit").WithHTTPStatus(resp.StatusCode)
	}
	return apiResponse{status: resp.StatusCode, header: resp.Header.Clone(), body: raw}, nil
}

// transportError maps transport faults onto the domain taxonomy. A timeout means
// the request may have been applied upstream, so it is TIMEOUT_UNCERTAIN and the
// engine must reconcile instead of blindly retrying.
func (a *Adapter) transportError(op string, err error) error {
	if errors.Is(err, errRedirectRefused) {
		return errs.Wrap(errs.ClassProviderBug, op,
			"github redirected the request to an unexpected host; refusing to forward the credential", err)
	}
	if errors.Is(err, context.Canceled) {
		return errs.Wrap(errs.ClassCancelled, op, "the request was cancelled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.ClassTimeoutUncertain, op,
			"github did not answer before the deadline; the outcome is unknown", err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errs.Wrap(errs.ClassTimeoutUncertain, op,
			"github did not answer before the deadline; the outcome is unknown", err)
	}
	return errs.Wrap(errs.ClassUpstreamTransient, op, "github API is unreachable", err)
}

// classifyStatus maps a non-2xx HTTP status onto the domain taxonomy.
func (a *Adapter) classifyStatus(op string, resp apiResponse) error {
	status := resp.status
	switch status {
	case http.StatusUnauthorized:
		return errs.New(errs.ClassNotAuthorized, op,
			"github rejected the credential (HTTP 401); check that the token is valid and has email access").
			WithHTTPStatus(status)
	case http.StatusForbidden:
		// GitHub reports primary/secondary rate limits as 403 with an exhausted
		// budget. That is retryable, not an authorization failure.
		if d, ok := retryAfterFromHeader(resp.header, a.now()); ok {
			return errs.New(errs.ClassRateLimited, op,
				"github rate limit is exhausted (HTTP 403)").
				WithRetryAfter(d).WithHTTPStatus(status)
		}
		return errs.New(errs.ClassNotAuthorized, op,
			"github refused the operation (HTTP 403); the token lacks email permission").
			WithHTTPStatus(status)
	case http.StatusNotFound:
		return errs.New(errs.ClassValidation, op,
			"github reported the resource as not found (HTTP 404)").WithHTTPStatus(status)
	case http.StatusConflict:
		return errs.New(errs.ClassValidation, op,
			"github reported a conflict for the address (HTTP 409)").WithHTTPStatus(status)
	case http.StatusUnprocessableEntity:
		return errs.New(errs.ClassValidation, op,
			"github rejected the request as unprocessable (HTTP 422)").WithHTTPStatus(status)
	case http.StatusTooManyRequests:
		e := errs.New(errs.ClassRateLimited, op,
			"github rate limit reached (HTTP 429)").WithHTTPStatus(status)
		if d, ok := retryAfterFromHeader(resp.header, a.now()); ok {
			e = e.WithRetryAfter(d)
		}
		return e
	case http.StatusRequestTimeout:
		return errs.New(errs.ClassTimeoutUncertain, op,
			"github reported a request timeout (HTTP 408)").WithHTTPStatus(status)
	}
	if status >= 500 {
		return errs.New(errs.ClassUpstreamTransient, op,
			"github API returned a transient server error").WithHTTPStatus(status)
	}
	if is2xx(status) {
		return errs.New(errs.ClassProviderBug, op,
			"internal error: a success status reached the error classifier").WithHTTPStatus(status)
	}
	return errs.New(errs.FromHTTPStatus(status), op,
		"github returned an unexpected HTTP status").WithHTTPStatus(status)
}

// retryAfterFromHeader extracts a retry hint from Retry-After or GitHub's
// rate-limit reset headers.
func retryAfterFromHeader(header http.Header, now time.Time) (time.Duration, bool) {
	if header == nil {
		return 0, false
	}
	if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
			return time.Duration(seconds) * time.Second, true
		}
		if at, err := http.ParseTime(raw); err == nil {
			return nonNegative(at.Sub(now)), true
		}
	}
	if strings.TrimSpace(header.Get("X-RateLimit-Remaining")) == "0" {
		if reset, err := strconv.ParseInt(strings.TrimSpace(header.Get("X-RateLimit-Reset")), 10, 64); err == nil {
			return nonNegative(time.Unix(reset, 0).Sub(now)), true
		}
	}
	return 0, false
}

func nonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

// rawEmail mirrors the GitHub email object. Only documented fields are parsed.
type rawEmail struct {
	Email      string `json:"email"`
	Verified   bool   `json:"verified"`
	Primary    bool   `json:"primary"`
	Visibility string `json:"visibility"`
}

// parseEmailArray parses the JSON array returned by POST/GET /user/emails.
func parseEmailArray(body []byte) ([]provider.EmailAddress, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("empty response body")
	}
	var raw []rawEmail
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return nil, err
	}
	out := make([]provider.EmailAddress, 0, len(raw))
	for _, item := range raw {
		if strings.TrimSpace(item.Email) == "" {
			return nil, errors.New("email entry without an address")
		}
		out = append(out, provider.EmailAddress{
			Email:      item.Email,
			Verified:   item.Verified,
			Primary:    item.Primary,
			Visibility: item.Visibility,
		})
	}
	return out, nil
}

// isDuplicateResponse reports whether an add-email response means "the address
// already exists". GitHub uses 422 with code already_exists for that case and
// 409 for conflicting registrations.
func isDuplicateResponse(resp apiResponse) bool {
	switch resp.status {
	case http.StatusConflict:
		return true
	case http.StatusUnprocessableEntity:
		body := strings.ToLower(string(resp.body))
		return strings.Contains(body, "already") ||
			strings.Contains(body, "duplicate") ||
			strings.Contains(body, "exists")
	default:
		return false
	}
}

func is2xx(status int) bool { return status >= 200 && status <= 299 }
