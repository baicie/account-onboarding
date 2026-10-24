package keycloak

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

// actionTokenPath is the Keycloak endpoint that executes email action tokens
// (VERIFY_EMAIL). The driver only ever follows links on this path, so a crafted
// email can never make it fetch an arbitrary URL.
const actionTokenPath = "/login-actions/action-token"

var (
	loginFormRE       = regexp.MustCompile(`(?is)<form[^>]*\bid\s*=\s*["']kc-form-login["'][^>]*>`)
	formTagRE         = regexp.MustCompile(`(?is)<form[^>]*>`)
	formActionRE      = regexp.MustCompile(`(?i)\baction\s*=\s*["']([^"']*)["']`)
	actionTokenHrefRE = regexp.MustCompile(`(?is)href\s*=\s*["']([^"']*login-actions/action-token[^"']*)["']`)
)

// tokenEnvelope is the SecretStore payload. It only exists inside the encrypted
// store: the caller receives an opaque reference, never these values.
type tokenEnvelope struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token,omitempty"`
	TokenType   string `json:"token_type,omitempty"`
	Issuer      string `json:"issuer,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Audience    string `json:"audience,omitempty"`
	ExpiresAtMS int64  `json:"expires_at_ms,omitempty"`
	CreatedAtMS int64  `json:"created_at_ms,omitempty"`
}

// authorizeOIDC implements the full browser dance against the sandbox realm.
func (a *Adapter) authorizeOIDC(ctx context.Context, req provider.SandboxAuthorizeRequest) (provider.SandboxCredentialRef, error) {
	const op = "keycloak.authorize_oidc"
	if a.secrets == nil {
		return provider.SandboxCredentialRef{}, errs.New(errs.ClassInternal, op, "secret store is not configured")
	}
	user, err := a.findUser(ctx, req.Identity.SubjectID, strings.TrimSpace(req.Identity.Address))
	if err != nil {
		return provider.SandboxCredentialRef{}, err
	}
	redirect, err := a.resolveRedirectURI(req.RedirectURI)
	if err != nil {
		return provider.SandboxCredentialRef{}, err
	}
	scopes := normalizeScopes(req.Scopes)
	verifier, challenge, err := pkcePair()
	if err != nil {
		return provider.SandboxCredentialRef{}, errs.Wrap(errs.ClassInternal, op, "could not generate a PKCE verifier", err)
	}
	state, err := randomHex(16)
	if err != nil {
		return provider.SandboxCredentialRef{}, errs.Wrap(errs.ClassInternal, op, "could not generate the OAuth state", err)
	}
	nonce, err := randomHex(16)
	if err != nil {
		return provider.SandboxCredentialRef{}, errs.Wrap(errs.ClassInternal, op, "could not generate the OIDC nonce", err)
	}
	// The driver fully controls this test identity, so it can rotate the
	// sandbox password in-memory for the duration of the flow. No password is
	// persisted by this adapter.
	password, err := randomToken(24)
	if err != nil {
		return provider.SandboxCredentialRef{}, errs.Wrap(errs.ClassInternal, op, "could not generate the sandbox password", err)
	}
	if err := a.resetPassword(ctx, user.ID, "Aa1!"+strings.NewReplacer("+", "x", "/", "y").Replace(password)); err != nil {
		return provider.SandboxCredentialRef{}, err
	}
	loginPassword := "Aa1!" + strings.NewReplacer("+", "x", "/", "y").Replace(password)
	client, err := a.newBrowserClient(redirect)
	if err != nil {
		return provider.SandboxCredentialRef{}, err
	}
	page, current, err := a.startAuthorization(ctx, client, op, user.Username, redirect, scopes, state, nonce, challenge)
	if err != nil {
		return provider.SandboxCredentialRef{}, err
	}
	action, err := loginFormAction(page, current)
	if err != nil {
		return provider.SandboxCredentialRef{}, err
	}
	code, err := a.submitLogin(ctx, client, op, action, user.Username, loginPassword, state, redirect)
	if err != nil {
		return provider.SandboxCredentialRef{}, err
	}
	tokens, err := a.exchangeCode(ctx, op, code, redirect, verifier)
	if err != nil {
		return provider.SandboxCredentialRef{}, err
	}
	claims, err := a.verifyIDToken(tokens.IDToken, nonce)
	if err != nil {
		return provider.SandboxCredentialRef{}, err
	}
	subject, err := a.userinfoSubject(ctx, op, tokens.AccessToken)
	if err != nil {
		return provider.SandboxCredentialRef{}, err
	}
	if subject != user.ID {
		return provider.SandboxCredentialRef{}, errs.New(errs.ClassProviderBug, op, "userinfo subject does not match the sandbox identity")
	}
	issuer := strings.TrimSpace(claims.Issuer)
	if issuer == "" {
		issuer = a.Issuer()
	}
	if claims.Subject != "" && claims.Subject != user.ID {
		return provider.SandboxCredentialRef{}, errs.New(errs.ClassProviderBug, op, "id_token subject does not match the sandbox identity")
	}
	audience := claims.Audience
	if audience == "" {
		audience = a.client
	}
	expiresAt := time.Time{}
	if claims.ExpiresAt > 0 {
		expiresAt = time.Unix(claims.ExpiresAt, 0).UTC()
	} else if tokens.ExpiresIn > 0 {
		expiresAt = a.now().UTC().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	}
	envelope := tokenEnvelope{
		AccessToken: tokens.AccessToken,
		IDToken:     tokens.IDToken,
		TokenType:   tokens.TokenType,
		Issuer:      issuer,
		Subject:     user.ID,
		Audience:    audience,
		CreatedAtMS: a.now().UTC().UnixMilli(),
	}
	if !expiresAt.IsZero() {
		envelope.ExpiresAtMS = expiresAt.UnixMilli()
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return provider.SandboxCredentialRef{}, errs.Wrap(errs.ClassInternal, op, "could not encode the sandbox credential", err)
	}
	id, err := randomHex(16)
	if err != nil {
		return provider.SandboxCredentialRef{}, errs.Wrap(errs.ClassInternal, op, "could not allocate the credential reference", err)
	}
	ref := provider.SecretRefPrefix + secretKindOIDC + "/" + id
	if err := a.secrets.Put(ctx, ref, secretKindOIDC, string(raw)); err != nil {
		return provider.SandboxCredentialRef{}, errs.Wrap(errs.ClassInternal, op, "could not store the sandbox credential", err)
	}
	return provider.SandboxCredentialRef{
		Ref:       ref,
		Issuer:    issuer,
		Subject:   user.ID,
		Audience:  audience,
		ExpiresAt: expiresAt,
	}, nil
}

// startAuthorization opens the authorization endpoint and returns the login
// form page. An already-authorized redirect straight to the callback is an error
// because a fresh cookie jar must always show the login form.
func (a *Adapter) startAuthorization(ctx context.Context, client *http.Client, op, username string, redirect *url.URL, scopes []string, state, nonce, challenge string) (string, *url.URL, error) {
	query := url.Values{}
	query.Set("client_id", a.client)
	query.Set("response_type", "code")
	query.Set("scope", strings.Join(scopes, " "))
	query.Set("redirect_uri", redirect.String())
	query.Set("state", state)
	query.Set("nonce", nonce)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	authURL := a.authURL() + "?" + query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, nil)
	if err != nil {
		return "", nil, errs.New(errs.ClassInternal, op, "could not build the authorization request")
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, a.transportError(op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLoginPageBytes+1))
	if err != nil {
		return "", nil, a.transportError(op, err)
	}
	if int64(len(body)) > maxLoginPageBytes {
		return "", nil, errs.New(errs.ClassProviderBug, op, "keycloak login page exceeded the size limit").WithHTTPStatus(resp.StatusCode)
	}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// A redirect here means the browser session was already authorized;
		// with a private jar that is unexpected for the sandbox flow.
		return "", resp.Request.URL, errs.New(errs.ClassProviderBug, op, "authorization endpoint skipped the sandbox login form").WithHTTPStatus(resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return "", resp.Request.URL, statusError(op, resp, body)
	}
	return string(body), resp.Request.URL, nil
}

// loginFormAction extracts the form action of the Keycloak login page.
func loginFormAction(page string, current *url.URL) (string, error) {
	const op = "keycloak.login_form"
	tag := loginFormRE.FindString(page)
	if tag == "" {
		for _, candidate := range formTagRE.FindAllString(page, -1) {
			lower := strings.ToLower(candidate)
			if strings.Contains(lower, "method=\"post\"") || strings.Contains(lower, "method='post'") {
				tag = candidate
				break
			}
		}
	}
	if tag == "" {
		return "", errs.New(errs.ClassProviderBug, op, "sandbox login form was not found")
	}
	match := formActionRE.FindStringSubmatch(tag)
	if len(match) < 2 {
		return "", errs.New(errs.ClassProviderBug, op, "sandbox login form has no action")
	}
	raw := strings.TrimSpace(html.UnescapeString(match[1]))
	if raw == "" {
		return "", errs.New(errs.ClassProviderBug, op, "sandbox login form action is empty")
	}
	target, err := url.Parse(raw)
	if err != nil {
		return "", errs.New(errs.ClassProviderBug, op, "sandbox login form action is malformed")
	}
	if current != nil {
		target = current.ResolveReference(target)
	}
	if !isLoopbackHost(target.Hostname()) {
		return "", errs.New(errs.ClassProviderBug, op, "sandbox login form action leaves the loopback sandbox")
	}
	if !strings.Contains(target.Path, "/login-actions/") {
		return "", errs.New(errs.ClassProviderBug, op, "sandbox login form action is not a keycloak login action")
	}
	return target.String(), nil
}

// submitLogin posts the sandbox credentials and returns the authorization code
// from the callback redirect. The callback itself is not fetched: it belongs to
// the application under test, not to this orchestration process.
func (a *Adapter) submitLogin(ctx context.Context, client *http.Client, op, action, username, password, state string, callback *url.URL) (string, error) {
	form := url.Values{}
	form.Set("username", username)
	form.Set("password", password)
	form.Set("credentialId", "")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, action, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errs.New(errs.ClassInternal, op, "could not build the sandbox login request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", a.transportError(op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain a bounded amount so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	// The redirect guard stops at the application callback, so the location of
	// the code is the response Location header rather than resp.Request.URL.
	location := strings.TrimSpace(resp.Header.Get("Location"))
	if location == "" {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", statusError(op, resp, nil)
		}
		// The login was rejected or a required action intercepted the flow.
		return "", errs.New(errs.ClassAuthExpired, op,
			"sandbox login did not return an authorization code").WithHTTPStatus(resp.StatusCode)
	}
	target, err := resp.Request.URL.Parse(location)
	if err != nil {
		return "", errs.New(errs.ClassProviderBug, op, "sandbox login redirect is malformed")
	}
	if !callbackMatches(target, callback) {
		return "", errs.New(errs.ClassAuthExpired, op,
			"sandbox login redirected away from the application callback").WithHTTPStatus(resp.StatusCode)
	}
	if errParam := target.Query().Get("error"); errParam != "" {
		if errParam == "access_denied" {
			return "", errs.New(errs.ClassNotAuthorized, op, "sandbox authorization was denied")
		}
		return "", errs.New(errs.ClassAuthExpired, op, "sandbox authorization failed")
	}
	code := target.Query().Get("code")
	returnedState := target.Query().Get("state")
	if strings.TrimSpace(code) == "" {
		return "", errs.New(errs.ClassProviderBug, op, "sandbox callback redirect has no authorization code")
	}
	if !constantTimeEqual(returnedState, state) {
		return "", errs.New(errs.ClassProviderBug, op, "sandbox callback state does not match the authorization state")
	}
	return code, nil
}

// exchangeCode redeems the authorization code with the PKCE verifier.
func (a *Adapter) exchangeCode(ctx context.Context, op, code string, redirect *url.URL, verifier string) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect.String())
	form.Set("client_id", a.client)
	form.Set("code_verifier", verifier)
	if a.secret != "" {
		form.Set("client_secret", a.secret)
	}
	var tokens tokenResponse
	if _, err := a.postForm(ctx, nil, op, a.tokenURL(), form, withUserAgent(nil), &tokens); err != nil {
		return tokenResponse{}, err
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return tokenResponse{}, errs.New(errs.ClassProviderBug, op, "keycloak token response is missing an access token")
	}
	return tokens, nil
}

// tokenResponse is the OIDC token endpoint payload (never logged).
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

// tokenClaims are the non-secret id_token claims the adapter needs for
// bookkeeping. The signature is not re-verified: the token was just received
// over the loopback back-channel, is never used for authorization decisions,
// and all identity decisions go through userinfo.
type tokenClaims struct {
	Issuer    string
	Subject   string
	Audience  string
	Nonce     string
	ExpiresAt int64
}

// verifyIDToken decodes the id_token payload and enforces the nonce, audience
// and expiry checks that matter for this fixture.
func (a *Adapter) verifyIDToken(idToken, nonce string) (tokenClaims, error) {
	const op = "keycloak.id_token"
	if strings.TrimSpace(idToken) == "" {
		return tokenClaims{}, errs.New(errs.ClassProviderBug, op, "keycloak token response is missing an id_token")
	}
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return tokenClaims{}, errs.New(errs.ClassProviderBug, op, "keycloak id_token is not a compact JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return tokenClaims{}, errs.New(errs.ClassProviderBug, op, "keycloak id_token payload is not base64url")
	}
	var claims struct {
		Iss   string          `json:"iss"`
		Sub   string          `json:"sub"`
		Aud   json.RawMessage `json:"aud"`
		Azp   string          `json:"azp"`
		Nonce string          `json:"nonce"`
		Exp   int64           `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return tokenClaims{}, errs.New(errs.ClassProviderBug, op, "keycloak id_token payload is malformed")
	}
	audience := audienceValue(claims.Aud)
	if !audienceContains(audience, a.client) && !strings.EqualFold(claims.Azp, a.client) {
		return tokenClaims{}, errs.New(errs.ClassProviderBug, op, "keycloak id_token audience does not match the configured client")
	}
	if !constantTimeEqual(strings.TrimSpace(claims.Nonce), nonce) {
		return tokenClaims{}, errs.New(errs.ClassProviderBug, op, "keycloak id_token nonce does not match the authorization nonce")
	}
	if claims.Exp <= 0 {
		return tokenClaims{}, errs.New(errs.ClassProviderBug, op, "keycloak id_token has no expiry")
	}
	if a.now().UTC().After(time.Unix(claims.Exp, 0).UTC().Add(2 * time.Minute)) {
		return tokenClaims{}, errs.New(errs.ClassAuthExpired, op, "keycloak id_token is already expired")
	}
	return tokenClaims{
		Issuer:    strings.TrimSpace(claims.Iss),
		Subject:   strings.TrimSpace(claims.Sub),
		Audience:  audience,
		Nonce:     claims.Nonce,
		ExpiresAt: claims.Exp,
	}, nil
}

// audienceValue flattens a JWT aud claim that may be a string or an array.
func audienceValue(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return strings.Join(list, ",")
	}
	return ""
}

func audienceContains(audience, want string) bool {
	for _, part := range strings.Split(audience, ",") {
		if strings.EqualFold(strings.TrimSpace(part), want) {
			return true
		}
	}
	return false
}

// userinfoSubject reads the authoritative subject of an access token.
func (a *Adapter) userinfoSubject(ctx context.Context, op, accessToken string) (string, error) {
	var info struct {
		Sub string `json:"sub"`
	}
	if _, err := a.doJSON(ctx, nil, op, http.MethodGet, a.userinfoURL(), withUserAgent(bearerAuthorized(accessToken)), nil, &info); err != nil {
		return "", err
	}
	if strings.TrimSpace(info.Sub) == "" {
		return "", errs.New(errs.ClassProviderBug, op, "keycloak userinfo has no subject")
	}
	return info.Sub, nil
}

// validateRef resolves an opaque credential reference and asks userinfo whether
// the stored access token is still accepted.
func (a *Adapter) validateRef(ctx context.Context, ref, op string) (provider.CredentialResult, error) {
	kind, _, ok := splitRef(ref)
	if !ok || kind != secretKindOIDC {
		return provider.CredentialResult{}, errs.New(errs.ClassValidation, op, "credential reference is not a sandbox OIDC reference")
	}
	raw, err := a.secrets.Resolve(ctx, ref)
	if err != nil {
		return provider.CredentialResult{}, errs.New(errs.ClassProviderBug, op, "stored sandbox credential is not resolvable")
	}
	var envelope tokenEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return provider.CredentialResult{}, errs.New(errs.ClassProviderBug, op, "stored sandbox credential is malformed")
	}
	if strings.TrimSpace(envelope.AccessToken) == "" {
		return provider.CredentialResult{State: provider.CredentialInvalid, Detail: "stored sandbox credential has no access token"}, nil
	}
	var info struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	_, err = a.doJSON(ctx, nil, op, http.MethodGet, a.userinfoURL(), withUserAgent(bearerAuthorized(envelope.AccessToken)), nil, &info)
	if err != nil {
		if errs.ClassOf(err) == errs.ClassNotAuthorized {
			// userinfo rejects expired tokens with 401; distinguish an expected
			// expiry from a credential that was revoked or tampered with.
			if envelope.ExpiresAtMS > 0 && !a.now().UTC().Before(time.UnixMilli(envelope.ExpiresAtMS)) {
				return provider.CredentialResult{State: provider.CredentialExpired, SubjectID: envelope.Subject,
					Issuer: envelope.Issuer, Detail: "sandbox access token expired"}, nil
			}
			return provider.CredentialResult{State: provider.CredentialInvalid, SubjectID: envelope.Subject,
				Issuer: envelope.Issuer, Detail: "userinfo rejected the sandbox access token"}, nil
		}
		return provider.CredentialResult{}, err
	}
	if strings.TrimSpace(info.Sub) == "" {
		return provider.CredentialResult{}, errs.New(errs.ClassProviderBug, op, "keycloak userinfo has no subject")
	}
	if envelope.Subject != "" && info.Sub != envelope.Subject {
		return provider.CredentialResult{State: provider.CredentialInvalid, SubjectID: info.Sub,
			Issuer: envelope.Issuer, Detail: "userinfo subject does not match the stored credential"}, nil
	}
	return provider.CredentialResult{
		State:     provider.CredentialValid,
		SubjectID: info.Sub,
		Issuer:    envelope.Issuer,
		Detail:    "userinfo accepted the sandbox access token",
	}, nil
}

// ---------------------------------------------------------------------------
// Verification-link helpers
// ---------------------------------------------------------------------------

// parseActionTokenURL validates an extracted verification link. Only Keycloak
// action-token URLs on the configured loopback service are accepted, which
// prevents the mailbox content from steering this driver at another host.
func (a *Adapter) parseActionTokenURL(raw string) (*url.URL, error) {
	const op = "keycloak.confirm_email"
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errs.New(errs.ClassValidation, op, "verification link is empty")
	}
	if len(raw) > 4096 {
		return nil, errs.New(errs.ClassValidation, op, "verification link is too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errs.New(errs.ClassValidation, op, "verification link is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errs.New(errs.ClassValidation, op, "verification link must use http or https")
	}
	if u.User != nil {
		return nil, errs.New(errs.ClassValidation, op, "verification link must not embed credentials")
	}
	if u.Host == "" {
		return nil, errs.New(errs.ClassValidation, op, "verification link has no host")
	}
	if !strings.Contains(u.Path, actionTokenPath) {
		return nil, errs.New(errs.ClassValidation, op, "verification link is not a keycloak action-token link")
	}
	if !a.sameLoopbackService(u) {
		return nil, errs.New(errs.ClassValidation, op, "verification link does not belong to the local keycloak sandbox")
	}
	return u, nil
}

// sameLoopbackService reports whether u targets the configured Keycloak service:
// same scheme and port, loopback host (localhost and 127.0.0.1 are equivalent
// for the sandbox), and a path below the base path.
func (a *Adapter) sameLoopbackService(u *url.URL) bool {
	if a == nil || a.base == nil || u == nil {
		return false
	}
	if !strings.EqualFold(u.Scheme, a.base.Scheme) {
		return false
	}
	if !isLoopbackHost(u.Hostname()) || !isLoopbackHost(a.base.Hostname()) {
		return false
	}
	if portOf(u) != portOf(a.base) {
		return false
	}
	basePath := strings.TrimSuffix(a.base.Path, "/")
	return basePath == "" || strings.HasPrefix(u.Path, basePath+"/")
}

func portOf(u *url.URL) string {
	if u == nil {
		return ""
	}
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// findActionTokenHref extracts the "Click here to proceed" target from the
// interstitial confirmation page and resolves it against the page URL.
func findActionTokenHref(page string, resp *http.Response) (string, bool) {
	match := actionTokenHrefRE.FindStringSubmatch(page)
	if len(match) < 2 {
		return "", false
	}
	href := strings.TrimSpace(html.UnescapeString(match[1]))
	if href == "" {
		return "", false
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		ref = resp.Request.URL.ResolveReference(ref)
	}
	return ref.String(), true
}

// pageMentionsExpiry is a conservative heuristic for an expired action token;
// the authoritative check is always the Admin API emailVerified state.
func pageMentionsExpiry(page string) bool {
	return strings.Contains(strings.ToLower(page), "expired")
}

// resolveRedirectURI prefers the workflow-provided callback and validates it
// against the loopback sandbox policy.
func (a *Adapter) resolveRedirectURI(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if a.redir == nil {
			return nil, errs.New(errs.ClassValidation, "keycloak.redirect_uri", "redirect uri is required")
		}
		return a.redir, nil
	}
	u, err := parseRedirectURI(raw, a.cfg.AllowLoopbackHTTP)
	if err != nil {
		return nil, errs.New(errs.ClassValidation, "keycloak.redirect_uri", "redirect uri is not a loopback sandbox callback")
	}
	return u, nil
}

func normalizeScopes(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	seen := map[string]struct{}{}
	for _, scope := range scopes {
		for _, part := range strings.Fields(scope) {
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return []string{"openid", "email", "profile"}
	}
	hasOpenID := false
	for _, scope := range out {
		if scope == "openid" {
			hasOpenID = true
		}
	}
	if !hasOpenID {
		out = append([]string{"openid"}, out...)
	}
	return out
}

// splitRef parses secret://<kind>/<id>.
func splitRef(ref string) (kind, id string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(ref), provider.SecretRefPrefix)
	if !found {
		return "", "", false
	}
	kind, id, found = strings.Cut(rest, "/")
	if !found || kind == "" || id == "" {
		return "", "", false
	}
	if strings.ContainsAny(kind+id, " /?#") {
		return "", "", false
	}
	return kind, id, true
}
