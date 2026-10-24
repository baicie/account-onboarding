package workbuddy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

// Compile-time proof that the adapter satisfies the frozen contract.
var _ provider.OAuthProvider = (*Adapter)(nil)

// errRedirectRefused aborts a redirect before the gateway credential can be
// replayed against a host outside the allowlist.
var errRedirectRefused = errors.New("workbuddy: redirect refused")

// Adapter drives the existing gateway's device-authorization control plane. It
// stores no platform credentials: only an opaque session reference and, after
// success, an opaque pointer to the gateway-owned credential.
type Adapter struct {
	cfg     Config
	base    *url.URL
	secrets provider.SecretStore
	client  *http.Client

	// now is injectable so the local 15 minute deadline and the terminal cache
	// are deterministically testable.
	now func() time.Time

	mu        sync.Mutex
	flights   map[string]*sync.Mutex
	terminals map[string]terminalState
}

// terminalState memoizes the outcome of a session reference so a duplicate poll
// can never replay a single-use state against the gateway.
type terminalState struct {
	status  provider.AuthorizationStatus
	expired bool
	at      time.Time
}

// sessionRecord is what the adapter stores under the opaque session reference.
// It contains the gateway state (a short-lived in-memory value) and no token.
type sessionRecord struct {
	State     string    `json:"state"`
	Realm     string    `json:"realm"`
	SubjectID string    `json:"subject_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// credentialPointer is the only thing recorded for a gateway-owned credential:
// where it lives, never what it is.
type credentialPointer struct {
	UID         string `json:"uid"`
	Realm       string `json:"realm"`
	GatewayBase string `json:"gateway_base"`
	Provider    string `json:"provider"`
	Note        string `json:"note"`
}

// New builds the adapter. With Enabled false (the default) it is a fully inert
// gate: no transport is created and no request can be issued. With Enabled true
// the configuration must name a loopback/private gateway base URL and a secret
// store.
func New(cfg Config, secrets provider.SecretStore) (*Adapter, error) {
	effective, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	a := &Adapter{
		cfg:       effective,
		secrets:   secrets,
		now:       time.Now,
		flights:   make(map[string]*sync.Mutex),
		terminals: make(map[string]terminalState),
	}
	if !effective.Enabled {
		return a, nil
	}
	if secrets == nil {
		return nil, errs.New(errs.ClassValidation, "workbuddy.config",
			"a secret store is required when the adapter is enabled")
	}
	base, err := validateBaseURL(effective.BaseURL, effective.AllowTestHTTP)
	if err != nil {
		return nil, err
	}
	a.base = base
	a.client = &http.Client{
		Timeout: effective.Timeout,
		// A cross-host redirect could replay the gateway panel credential
		// against a host the operator never allowed.
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

// NewDisabled returns an inert adapter with an explicit restriction reason. It
// exists so wiring can always register the provider and let Descriptor().Enabled
// reject lookups before any network call.
func NewDisabled(reason string) *Adapter {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = DefaultRestrictionReason
	}
	cfg, err := Config{RestrictionReason: reason}.withDefaults()
	if err != nil {
		// withDefaults cannot fail for a zero Config with a custom reason.
		cfg = Config{RestrictionReason: reason, Realm: DefaultRealm, Timeout: DefaultTimeout, SessionTTL: DefaultSessionTTL}
	}
	return &Adapter{
		cfg:       cfg,
		now:       time.Now,
		flights:   make(map[string]*sync.Mutex),
		terminals: make(map[string]terminalState),
	}
}

// Descriptor implements provider.OAuthProvider and carries the activation gate.
func (a *Adapter) Descriptor() provider.Descriptor {
	caps := []string{"oauth.device.start", "oauth.device.poll", "credential.ref"}
	if a == nil || !a.cfg.Enabled {
		reason := DefaultRestrictionReason
		if a != nil && a.cfg.RestrictionReason != "" {
			reason = a.cfg.RestrictionReason
		}
		return provider.Descriptor{
			Name:              ProviderName,
			Kind:              provider.KindOAuth,
			ContractVersion:   ContractVersion,
			Enabled:           false,
			Live:              false,
			Capabilities:      caps,
			RestrictionReason: reason,
		}
	}
	return provider.Descriptor{
		Name:            ProviderName,
		Kind:            provider.KindOAuth,
		ContractVersion: ContractVersion,
		Enabled:         true,
		Live:            true,
		Capabilities:    caps,
	}
}

// requireEnabled enforces the permission gate before any network call.
func (a *Adapter) requireEnabled() error {
	reason := DefaultRestrictionReason
	if a != nil && a.cfg.RestrictionReason != "" {
		reason = a.cfg.RestrictionReason
	}
	if a == nil || !a.cfg.Enabled {
		return provider.NewDisallowedError(provider.KindOAuth, ProviderName, reason)
	}
	return nil
}

// Start opens an authorization session through POST /panel/api/login/start and
// returns the official authorization URL plus an opaque session reference. The
// gateway state itself is stored in the SecretStore; it never appears in the
// reference, in an error or in a log line.
func (a *Adapter) Start(ctx context.Context, req provider.StartAuthRequest) (provider.AuthorizationSession, error) {
	const op = "workbuddy.oauth.start"

	if err := a.requireEnabled(); err != nil {
		return provider.AuthorizationSession{}, err
	}
	a.pruneTerminals()

	token, err := a.gatewayToken(ctx)
	if err != nil {
		return provider.AuthorizationSession{}, err
	}
	payload, err := json.Marshal(struct {
		Realm string `json:"realm"`
	}{Realm: a.cfg.Realm})
	if err != nil {
		return provider.AuthorizationSession{}, errs.Wrap(errs.ClassProviderBug, op,
			"cannot encode the gateway request", err)
	}
	resp, err := a.do(ctx, op, http.MethodPost, pathLoginStart, nil, payload, token)
	if err != nil {
		return provider.AuthorizationSession{}, err
	}
	if resp.status != http.StatusOK {
		return provider.AuthorizationSession{}, a.classifyStatus(op, resp)
	}

	var body struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		State string `json:"state"`
		Realm string `json:"realm"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(resp.body, &body); err != nil {
		return provider.AuthorizationSession{}, errs.New(errs.ClassProviderBug, op,
			"gateway returned a malformed start response").WithHTTPStatus(resp.status)
	}
	if !body.OK {
		return provider.AuthorizationSession{}, errs.New(errs.ClassProviderBug, op,
			"gateway refused to start the authorization").WithHTTPStatus(resp.status)
	}
	authURL, err := validateAuthorizationURL(body.URL)
	if err != nil {
		return provider.AuthorizationSession{}, errs.Wrap(errs.ClassProviderBug, op,
			"gateway returned an unusable authorization URL", err)
	}
	if err := validateGatewayState(body.State); err != nil {
		return provider.AuthorizationSession{}, errs.Wrap(errs.ClassProviderBug, op,
			"gateway returned an unusable authorization state", err)
	}
	realm := a.cfg.Realm
	if body.Realm != "" {
		if body.Realm != a.cfg.Realm {
			return provider.AuthorizationSession{}, errs.New(errs.ClassProviderBug, op,
				"gateway returned an unexpected realm").WithHTTPStatus(resp.status)
		}
		realm = body.Realm
	}

	now := a.now().UTC()
	expiresAt := now.Add(a.cfg.SessionTTL)
	ref, err := newSessionRef()
	if err != nil {
		return provider.AuthorizationSession{}, errs.Wrap(errs.ClassProviderBug, op,
			"cannot allocate an authorization session reference", err)
	}
	record, err := json.Marshal(sessionRecord{
		State:     body.State,
		Realm:     realm,
		SubjectID: req.SubjectID,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return provider.AuthorizationSession{}, errs.Wrap(errs.ClassProviderBug, op,
			"cannot encode the authorization session record", err)
	}
	if err := a.secrets.Put(ctx, ref, sessionSecretKind, string(record)); err != nil {
		return provider.AuthorizationSession{}, errs.Wrap(errs.ClassUpstreamTransient, op,
			"the authorization session could not be persisted locally", err)
	}
	return provider.AuthorizationSession{
		Ref:              ref,
		AuthorizationURL: authURL,
		ExpiresAt:        expiresAt,
	}, nil
}

// Poll advances one authorization session through
// GET /panel/api/login/poll?state=<stored state>.
//
// Semantics:
//   - done=false  -> PENDING (poll again later, within the deadline);
//   - done=true   -> AUTHORIZED with a CredentialRef that points at
//     gateway-owned storage. No token is copied: the gateway keeps the
//     credential and this adapter records only a pointer;
//   - 410         -> AUTH_EXPIRED. The state is consumed, the local record is
//     deleted and a later Poll never replays it;
//   - a terminal result is memoized per reference, so a duplicate poll returns
//     the same outcome with zero extra gateway calls (single-flight).
//
// Poll never retries an expired session automatically.
func (a *Adapter) Poll(ctx context.Context, sessionRef string) (provider.AuthorizationStatus, error) {
	const op = "workbuddy.oauth.poll"

	if err := a.requireEnabled(); err != nil {
		return provider.AuthorizationStatus{}, err
	}
	ref := strings.TrimSpace(sessionRef)
	if ref == "" {
		return provider.AuthorizationStatus{}, errs.New(errs.ClassValidation, op,
			"an authorization session reference is required")
	}
	if status, ok := a.cachedAuthorized(ref); ok {
		return status, nil
	}
	if a.cachedExpired(ref) {
		return provider.AuthorizationStatus{}, authExpiredError(op)
	}

	// Single-flight: loginPoll has side effects inside the gateway, so two
	// concurrent polls of one state must never both reach it.
	lock := a.flightLock(ref)
	lock.Lock()
	defer lock.Unlock()

	// Re-check after acquiring the flight lock: a concurrent poll may have
	// completed the exchange and consumed the state.
	if status, ok := a.cachedAuthorized(ref); ok {
		return status, nil
	}
	if a.cachedExpired(ref) {
		return provider.AuthorizationStatus{}, authExpiredError(op)
	}

	session, err := a.loadSession(ctx, ref)
	if err != nil {
		return provider.AuthorizationStatus{}, err
	}
	if !session.ExpiresAt.IsZero() && !a.now().Before(session.ExpiresAt) {
		a.dropSession(ctx, ref)
		a.markExpired(ref)
		return provider.AuthorizationStatus{}, authExpiredError(op)
	}
	token, err := a.gatewayToken(ctx)
	if err != nil {
		return provider.AuthorizationStatus{}, err
	}
	query := url.Values{"state": []string{session.State}}
	resp, err := a.do(ctx, op, http.MethodGet, pathLoginPoll, query, nil, token)
	if err != nil {
		return provider.AuthorizationStatus{}, err
	}
	switch {
	case resp.status == http.StatusGone:
		// The gateway no longer knows the state (expired or process restart).
		a.dropSession(ctx, ref)
		a.markExpired(ref)
		return provider.AuthorizationStatus{}, authExpiredError(op)
	case resp.status != http.StatusOK:
		return provider.AuthorizationStatus{}, a.classifyStatus(op, resp)
	}

	var body struct {
		Done     *bool  `json:"done"`
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		Realm    string `json:"realm"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(resp.body, &body); err != nil || body.Done == nil {
		return provider.AuthorizationStatus{}, errs.New(errs.ClassProviderBug, op,
			"gateway returned a malformed poll response").WithHTTPStatus(resp.status)
	}
	if !*body.Done {
		return provider.AuthorizationStatus{
			State:     provider.AuthPending,
			SubjectID: session.SubjectID,
			Detail:    "waiting for the account holder to finish authorization in the browser",
		}, nil
	}

	uid := strings.TrimSpace(body.UID)
	if !validGatewayUID(uid) {
		return provider.AuthorizationStatus{}, errs.New(errs.ClassProviderBug, op,
			"gateway reported completion without a usable account identifier").WithHTTPStatus(resp.status)
	}
	// The gateway already persisted the credential and hot-loaded the account.
	// Record only where it lives: no token material is read from this response.
	pointer, err := json.Marshal(credentialPointer{
		UID:         uid,
		Realm:       session.Realm,
		GatewayBase: a.base.String(),
		Provider:    ProviderName,
		Note:        "credential is owned and stored by the gateway; no token material is copied",
	})
	if err != nil {
		return provider.AuthorizationStatus{}, errs.Wrap(errs.ClassProviderBug, op,
			"cannot encode the credential pointer", err)
	}
	credentialRef := credentialRefPrefix + uid
	if err := a.secrets.Put(ctx, credentialRef, credentialPointerSecretKind, string(pointer)); err != nil {
		return provider.AuthorizationStatus{}, errs.Wrap(errs.ClassUpstreamTransient, op,
			"the gateway completed the authorization but the credential reference could not be persisted locally", err)
	}
	a.dropSession(ctx, ref)
	status := provider.AuthorizationStatus{
		State:         provider.AuthAuthorized,
		SubjectID:     session.SubjectID,
		CredentialRef: credentialRef,
		Detail:        "gateway authorization complete; the gateway owns the stored credential",
	}
	a.markAuthorized(ref, status)
	return status, nil
}

// loadSession resolves and decodes the stored session record.
func (a *Adapter) loadSession(ctx context.Context, ref string) (sessionRecord, error) {
	const op = "workbuddy.oauth.poll"
	if a.secrets == nil {
		return sessionRecord{}, errs.New(errs.ClassAuthExpired, op,
			"authorization session is no longer available; start a new authorization")
	}
	raw, err := a.secrets.Resolve(ctx, ref)
	if err != nil {
		return sessionRecord{}, errs.Wrap(errs.ClassAuthExpired, op,
			"authorization session is no longer available; start a new authorization", err)
	}
	var session sessionRecord
	if err := json.Unmarshal([]byte(raw), &session); err != nil || strings.TrimSpace(session.State) == "" {
		return sessionRecord{}, errs.New(errs.ClassProviderBug, op,
			"the stored authorization session is malformed; start a new authorization")
	}
	if session.Realm == "" {
		session.Realm = a.cfg.Realm
	}
	if session.ExpiresAt.IsZero() {
		session.ExpiresAt = session.CreatedAt.Add(a.cfg.SessionTTL)
	}
	return session, nil
}

// gatewayToken resolves the gateway panel credential at call time. An empty
// reference is legitimate: the gateway's withAuth allows anonymous access when
// no api key is configured.
func (a *Adapter) gatewayToken(ctx context.Context) (string, error) {
	const op = "workbuddy.auth"
	ref := strings.TrimSpace(a.cfg.GatewayTokenRef)
	if ref == "" {
		return "", nil
	}
	if a.secrets == nil {
		return "", errs.New(errs.ClassNotAuthorized, op,
			"the gateway credential reference is configured but no secret store is available")
	}
	token, err := a.secrets.Resolve(ctx, ref)
	if err != nil {
		return "", errs.Wrap(errs.ClassNotAuthorized, op,
			"the gateway control-plane credential is unavailable", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errs.New(errs.ClassNotAuthorized, op,
			"the gateway control-plane credential resolved to an empty secret")
	}
	return token, nil
}

// apiResponse is the raw gateway answer. The body never reaches an error or DTO.
type apiResponse struct {
	status int
	header http.Header
	body   []byte
}

// do performs one allowlisted control-plane request. Transport faults are
// classified here; HTTP status mapping is left to the call site so poll can
// intercept 410 (AUTH_EXPIRED) before the generic mapping.
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
		return apiResponse{}, errs.Wrap(errs.ClassProviderBug, op, "cannot build the gateway request", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", a.cfg.UserAgent)
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
			"gateway response exceeded the size limit").WithHTTPStatus(resp.StatusCode)
	}
	return apiResponse{status: resp.StatusCode, header: resp.Header.Clone(), body: raw}, nil
}

// transportError maps transport faults onto the domain taxonomy. A timeout is
// TIMEOUT_UNCERTAIN because the gateway may already have performed the
// authorization exchange.
func (a *Adapter) transportError(op string, err error) error {
	if errors.Is(err, errRedirectRefused) {
		return errs.Wrap(errs.ClassProviderBug, op,
			"the gateway redirected the request to an unexpected host; refusing to forward the credential", err)
	}
	if errors.Is(err, context.Canceled) {
		return errs.Wrap(errs.ClassCancelled, op, "the request was cancelled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.ClassTimeoutUncertain, op,
			"the gateway did not answer before the deadline; the outcome is unknown", err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errs.Wrap(errs.ClassTimeoutUncertain, op,
			"the gateway did not answer before the deadline; the outcome is unknown", err)
	}
	return errs.Wrap(errs.ClassUpstreamTransient, op, "the gateway control plane is unreachable", err)
}

// classifyStatus maps a non-2xx gateway status onto the domain taxonomy.
func (a *Adapter) classifyStatus(op string, resp apiResponse) error {
	status := resp.status
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errs.New(errs.ClassNotAuthorized, op,
			"the gateway rejected the control-plane credential or the operator lacks panel permission").
			WithHTTPStatus(status)
	case http.StatusTooManyRequests:
		e := errs.New(errs.ClassRateLimited, op,
			"the gateway rate limited the control-plane request (HTTP 429)").WithHTTPStatus(status)
		if d, ok := retryAfterFromHeader(resp.header, a.now()); ok {
			e = e.WithRetryAfter(d)
		}
		return e
	}
	if status >= 500 {
		return errs.New(errs.ClassUpstreamTransient, op,
			"the gateway control plane returned a transient server error").WithHTTPStatus(status)
	}
	if status >= 200 && status <= 299 {
		return errs.New(errs.ClassProviderBug, op,
			"internal error: a success status reached the error classifier").WithHTTPStatus(status)
	}
	return errs.New(errs.FromHTTPStatus(status), op,
		"the gateway returned an unexpected HTTP status").WithHTTPStatus(status)
}

// retryAfterFromHeader extracts Retry-After (seconds or HTTP date).
func retryAfterFromHeader(header http.Header, now time.Time) (time.Duration, bool) {
	if header == nil {
		return 0, false
	}
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if seconds, err := time.ParseDuration(raw + "s"); err == nil && !strings.ContainsAny(raw, " ") {
		if d := seconds; d >= 0 {
			return d, true
		}
	}
	if at, err := http.ParseTime(raw); err == nil {
		d := at.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// session reference / state handling
// ---------------------------------------------------------------------------

// newSessionRef mints an opaque reference. Randomness keeps references
// unguessable and guarantees they never encode the state.
func newSessionRef() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return sessionRefPrefix + hex.EncodeToString(buf), nil
}

// validateAuthorizationURL accepts the official authorization URL shown to the
// human operator. The adapter never fetches it.
func validateAuthorizationURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4096 {
		return "", errors.New("empty or oversized URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" {
		return "", errors.New("authorization URL must be https")
	}
	if u.Host == "" || u.User != nil {
		return "", errors.New("authorization URL must have a host and no userinfo")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("authorization URL contains control characters")
		}
	}
	return raw, nil
}

// validateGatewayState rejects a state that cannot be safely transported in a
// query string. It is never logged or embedded in a reference.
func validateGatewayState(state string) error {
	if state == "" || len(state) > 512 {
		return errors.New("state must be 1..512 bytes")
	}
	for _, r := range state {
		if r < 0x21 || r > 0x7e {
			return errors.New("state must be printable ASCII without spaces")
		}
	}
	return nil
}

// validGatewayUID mirrors the gateway's own path-traversal guard: the uid is
// used to build a credential reference (and the gateway uses it in a filename),
// so only [A-Za-z0-9_-] is accepted.
func validGatewayUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// dropSession deletes the stored state once it can never be used again.
// Best-effort: a delete failure must not resurrect the session.
func (a *Adapter) dropSession(ctx context.Context, ref string) {
	if a.secrets == nil {
		return
	}
	_ = a.secrets.Delete(ctx, ref)
}

// flightLock returns the per-reference single-flight mutex.
func (a *Adapter) flightLock(ref string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.flights == nil {
		a.flights = make(map[string]*sync.Mutex)
	}
	lock, ok := a.flights[ref]
	if !ok {
		lock = &sync.Mutex{}
		a.flights[ref] = lock
	}
	return lock
}

// markAuthorized memoizes a completed authorization.
func (a *Adapter) markAuthorized(ref string, status provider.AuthorizationStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminals == nil {
		a.terminals = make(map[string]terminalState)
	}
	a.terminals[ref] = terminalState{status: status, at: a.now()}
}

// markExpired memoizes an expired/consumed session so it is never retried.
func (a *Adapter) markExpired(ref string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminals == nil {
		a.terminals = make(map[string]terminalState)
	}
	a.terminals[ref] = terminalState{expired: true, at: a.now()}
}

// cachedAuthorized returns a previously completed authorization.
func (a *Adapter) cachedAuthorized(ref string) (provider.AuthorizationStatus, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.terminals[ref]
	if !ok || entry.expired {
		return provider.AuthorizationStatus{}, false
	}
	if a.now().Sub(entry.at) > terminalCacheTTL {
		delete(a.terminals, ref)
		return provider.AuthorizationStatus{}, false
	}
	return entry.status, true
}

// cachedExpired reports a previously expired/consumed session.
func (a *Adapter) cachedExpired(ref string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.terminals[ref]
	if !ok || !entry.expired {
		return false
	}
	if a.now().Sub(entry.at) > terminalCacheTTL {
		delete(a.terminals, ref)
		return false
	}
	return true
}

// pruneTerminals keeps the memo bounded. Called from Start, which is a natural
// low-frequency entry point.
func (a *Adapter) pruneTerminals() {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for ref, entry := range a.terminals {
		if now.Sub(entry.at) > terminalCacheTTL {
			delete(a.terminals, ref)
		}
	}
	for len(a.terminals) > terminalCacheLimit {
		var oldestRef string
		var oldest time.Time
		for ref, entry := range a.terminals {
			if oldestRef == "" || entry.at.Before(oldest) {
				oldestRef, oldest = ref, entry.at
			}
		}
		delete(a.terminals, oldestRef)
	}
}

// authExpiredError is the canonical AUTH_EXPIRED failure: the engine maps it to
// REAUTHORIZE_REQUIRED and the operator starts a new authorization.
func authExpiredError(op string) error {
	return errs.New(errs.ClassAuthExpired, op,
		"authorization session expired; start a new authorization")
}
