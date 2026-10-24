package keycloak

// Test-only fake of the Keycloak realm surface used by the adapter. It mimics
// the verified local sandbox behaviour (Admin REST, login form, action-token
// interstitial, PKCE token endpoint, userinfo) so the adapter contract can be
// tested without touching the live fixture.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeUser struct {
	ID            string
	Username      string
	Email         string
	EmailVerified bool
	Enabled       bool
	Password      string
	CreatedMS     int64
	Required      []string
}

type fakeAuthRequest struct {
	SessionCode string
	State       string
	Nonce       string
	Challenge   string
	Redirect    string
	Scope       string
	ClientID    string
}

type fakeCode struct {
	Code      string
	UserID    string
	Challenge string
	Redirect  string
	Nonce     string
	State     string
	Used      bool
}

type fakeKeycloak struct {
	t      *testing.T
	server *httptest.Server

	realm    string
	clientID string

	mu              sync.Mutex
	users           map[string]*fakeUser
	byUsername      map[string]string
	tokenRequests   int
	adminTokenForms []url.Values
	adminExpiresIn  int64
	adminStatus     int
	adminUserHits   int
	admin401Once    bool
	admin401Used    bool

	createCount        int
	resetPasswords     []string
	executeActions     []url.Values
	executeActionsBody []string
	confirmSecondHit   int
	userCreateHits     int

	pending map[string]*fakeAuthRequest
	codes   map[string]*fakeCode
	tokens  map[string]string // access token -> user id

	authRequests int
	loginHits    int
	userinfoHits int
	userinfoAuth []string
	requestPaths []string

	// behaviour switches
	userinfoStatus     int
	userinfoRetryAfter string
	userinfoMalformed  bool
	userinfoDelay      time.Duration
	userinfoCrossSite  bool
	tokenStatus        int
	tokenSecretBody    string
	forceWrongVerifier bool
	loginRejects       bool
	badNonce           bool
	actionTokenStatus  int
	badActionToken     bool
	actionTokenExpired bool
	createConflictOnce bool
	conflictUsed       bool
}

func newFakeKeycloak(t *testing.T, realm, clientID string) *fakeKeycloak {
	t.Helper()
	f := &fakeKeycloak{
		t:          t,
		realm:      realm,
		clientID:   clientID,
		users:      map[string]*fakeUser{},
		byUsername: map[string]string{},
		pending:    map[string]*fakeAuthRequest{},
		codes:      map[string]*fakeCode{},
		tokens:     map[string]string{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeKeycloak) URL() string { return f.server.URL }

func (f *fakeKeycloak) userCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users)
}

func (f *fakeKeycloak) user(id string) (fakeUser, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return fakeUser{}, false
	}
	return *u, true
}

// seedUser inserts a pre-existing sandbox user, as if a previous attempt had
// already created the identity.
func (f *fakeKeycloak) seedUser(username, email string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", len(f.users)+1)
	f.users[id] = &fakeUser{ID: id, Username: username, Email: email, Enabled: true, CreatedMS: time.Now().UnixMilli()}
	f.byUsername[strings.ToLower(username)] = id
	f.createCount++
	return id
}

func (f *fakeKeycloak) issueActionToken(userID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.server.URL + "/realms/" + f.realm + actionTokenPath + "?key=good-" + userID
}

func (f *fakeKeycloak) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requestPaths = append(f.requestPaths, r.URL.Path)
	f.mu.Unlock()

	path := r.URL.Path
	switch {
	case path == "/realms/"+DefaultAdminRealm+"/protocol/openid-connect/token":
		f.handleAdminToken(w, r)
	case strings.HasPrefix(path, "/admin/realms/"+f.realm+"/"):
		f.handleAdmin(w, r)
	case path == "/realms/"+f.realm+"/protocol/openid-connect/auth":
		f.handleAuth(w, r)
	case strings.HasPrefix(path, "/realms/"+f.realm+"/login-actions/authenticate"):
		f.handleLoginSubmit(w, r)
	case path == "/realms/"+f.realm+"/login-actions/action-token":
		f.handleActionToken(w, r)
	case path == "/realms/"+f.realm+"/protocol/openid-connect/token":
		f.handleToken(w, r)
	case path == "/realms/"+f.realm+"/protocol/openid-connect/userinfo":
		f.handleUserinfo(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeKeycloak) handleAdminToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	f.mu.Lock()
	f.tokenRequests++
	f.adminTokenForms = append(f.adminTokenForms, r.PostForm)
	status := f.adminStatus
	expires := f.adminExpiresIn
	f.mu.Unlock()
	if status != 0 {
		writeFakeJSON(w, status, map[string]any{"error": "server_error"})
		return
	}
	if expires == 0 {
		expires = 60
	}
	writeFakeJSON(w, http.StatusOK, map[string]any{
		"access_token": "admin-token",
		"expires_in":   expires,
		"token_type":   "Bearer",
	})
}

func (f *fakeKeycloak) handleAdmin(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/realms/"+f.realm)
	if rest == "/users" && r.Method == http.MethodGet {
		f.mu.Lock()
		f.adminUserHits++
		if f.admin401Once && !f.admin401Used {
			f.admin401Used = true
			f.mu.Unlock()
			writeFakeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		f.mu.Unlock()
		f.searchUsers(w, r)
		return
	}
	if rest == "/users" && r.Method == http.MethodPost {
		f.createUser(w, r)
		return
	}
	if rest == "/users" {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/users/"), "/")
	id := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			u, ok := f.users[id]
			f.mu.Unlock()
			if !ok {
				writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
				return
			}
			writeFakeJSON(w, http.StatusOK, fakeUserJSON(*u))
		case http.MethodPut:
			var update map[string]any
			_ = json.NewDecoder(r.Body).Decode(&update)
			f.mu.Lock()
			if u, ok := f.users[id]; ok {
				if email, ok := update["email"].(string); ok {
					u.Email = email
				}
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			f.mu.Lock()
			f.deleteUserLocked(id)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
		return
	}
	switch parts[1] {
	case "reset-password":
		var cred credentialRepresentation
		_ = json.NewDecoder(r.Body).Decode(&cred)
		f.mu.Lock()
		if u, ok := f.users[id]; ok {
			u.Password = cred.Value
			f.resetPasswords = append(f.resetPasswords, cred.Value)
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "execute-actions-email":
		var actions []string
		_ = json.NewDecoder(r.Body).Decode(&actions)
		f.mu.Lock()
		f.executeActions = append(f.executeActions, r.URL.Query())
		f.executeActionsBody = append(f.executeActionsBody, strings.Join(actions, ","))
		f.mu.Unlock()
		if _, ok := f.user(id); !ok {
			writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeKeycloak) searchUsers(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	var out []map[string]any
	if username := q.Get("username"); username != "" {
		if id, ok := f.byUsername[strings.ToLower(username)]; ok {
			u := f.users[id]
			out = append(out, fakeUserJSON(*u))
		}
	} else if email := q.Get("email"); email != "" {
		for _, u := range f.users {
			if strings.EqualFold(u.Email, email) {
				out = append(out, fakeUserJSON(*u))
			}
		}
	} else {
		for _, u := range f.users {
			out = append(out, fakeUserJSON(*u))
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeFakeJSON(w, http.StatusOK, out)
}

func (f *fakeKeycloak) createUser(w http.ResponseWriter, r *http.Request) {
	var payload createUserRepresentation
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userCreateHits++
	if f.createConflictOnce && !f.conflictUsed {
		f.conflictUsed = true
		writeFakeJSON(w, http.StatusConflict, map[string]any{"error": "conflict"})
		return
	}
	if _, ok := f.byUsername[strings.ToLower(payload.Username)]; ok {
		writeFakeJSON(w, http.StatusConflict, map[string]any{"error": "conflict"})
		return
	}
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", len(f.users)+1)
	user := &fakeUser{
		ID:        id,
		Username:  payload.Username,
		Email:     payload.Email,
		Enabled:   payload.Enabled,
		CreatedMS: time.Now().UnixMilli(),
	}
	if len(payload.Credentials) > 0 {
		user.Password = payload.Credentials[0].Value
	}
	f.users[id] = user
	f.byUsername[strings.ToLower(payload.Username)] = id
	f.createCount++
	w.Header().Set("Location", f.server.URL+"/admin/realms/"+f.realm+"/users/"+id)
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeKeycloak) deleteUserLocked(id string) {
	if u, ok := f.users[id]; ok {
		delete(f.byUsername, strings.ToLower(u.Username))
		delete(f.users, id)
	}
}

func (f *fakeKeycloak) handleAuth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != f.clientID {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_client"})
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "error_description": "PKCE required"})
		return
	}
	session := fmt.Sprintf("session-%d", time.Now().UnixNano())
	f.mu.Lock()
	f.authRequests++
	f.pending[session] = &fakeAuthRequest{
		SessionCode: session,
		State:       q.Get("state"),
		Nonce:       q.Get("nonce"),
		Challenge:   q.Get("code_challenge"),
		Redirect:    q.Get("redirect_uri"),
		Scope:       q.Get("scope"),
		ClientID:    q.Get("client_id"),
	}
	f.mu.Unlock()
	action := "/realms/" + f.realm + "/login-actions/authenticate?session_code=" + url.QueryEscape(session) + "&amp;execution=exec-1&amp;client_id=" + url.QueryEscape(f.clientID)
	page := `<!DOCTYPE html><html><head><title>Sign in</title></head><body>` +
		`<form id="kc-form-login" onsubmit="login.disabled = true; return true;" action="` + action + `" method="post">` +
		`<input id="username" name="username" type="text"><input id="password" name="password" type="password">` +
		`<input type="hidden" name="credentialId" value=""><input name="login" type="submit" value="Sign In">` +
		`</form></body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

func (f *fakeKeycloak) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	session := r.URL.Query().Get("session_code")
	f.mu.Lock()
	f.loginHits++
	auth := f.pending[session]
	rejects := f.loginRejects
	f.mu.Unlock()
	if auth == nil {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown session"})
		return
	}
	username := r.PostForm.Get("username")
	password := r.PostForm.Get("password")
	f.mu.Lock()
	id, ok := f.byUsername[strings.ToLower(username)]
	var user *fakeUser
	if ok {
		user = f.users[id]
	}
	f.mu.Unlock()
	if rejects || user == nil || user.Password != password {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><div id="kc-error-message">Invalid username or password.</div></body></html>`))
		return
	}
	code := fmt.Sprintf("code-%d", time.Now().UnixNano())
	f.mu.Lock()
	f.codes[code] = &fakeCode{
		Code:      code,
		UserID:    user.ID,
		Challenge: auth.Challenge,
		Redirect:  auth.Redirect,
		Nonce:     auth.Nonce,
		State:     auth.State,
	}
	f.mu.Unlock()
	location := auth.Redirect + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(auth.State)
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusFound)
}

func (f *fakeKeycloak) handleActionToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	status := f.actionTokenStatus
	expired := f.actionTokenExpired
	f.mu.Unlock()
	if status != 0 {
		writeFakeJSON(w, status, map[string]any{"error": "action token failed"})
		return
	}
	if expired {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><body><p>We are sorry... The action token is expired.</p></body></html>`))
		return
	}
	key := r.URL.Query().Get("key")
	if !strings.HasPrefix(key, "good-") {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid token"})
		return
	}
	if r.URL.Query().Get("do") == "1" {
		userID := strings.TrimPrefix(key, "good-")
		f.mu.Lock()
		if u, ok := f.users[userID]; ok {
			u.EmailVerified = true
		}
		f.confirmSecondHit++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><p>Your email address has been verified.</p></body></html>`))
		return
	}
	anchor := "/realms/" + f.realm + actionTokenPath + "?key=" + url.QueryEscape(key) + "&amp;do=1"
	page := `<html><body><div id="kc-info-message"><p>Perform the following action(s):</p>` +
		`<p><a href="` + anchor + `">Click here to proceed</a></p></div></body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

func (f *fakeKeycloak) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	if r.PostForm.Get("grant_type") == "client_credentials" {
		// Service-account clients authenticate on the sandbox realm endpoint.
		f.mu.Lock()
		f.tokenRequests++
		f.adminTokenForms = append(f.adminTokenForms, r.PostForm)
		status := f.adminStatus
		expires := f.adminExpiresIn
		f.mu.Unlock()
		if status != 0 {
			writeFakeJSON(w, status, map[string]any{"error": "server_error"})
			return
		}
		if expires == 0 {
			expires = 60
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"access_token": "admin-token",
			"expires_in":   expires,
			"token_type":   "Bearer",
		})
		return
	}
	f.mu.Lock()
	status := f.tokenStatus
	secretBody := f.tokenSecretBody
	forceWrong := f.forceWrongVerifier
	badNonce := f.badNonce
	f.mu.Unlock()
	if status != 0 {
		if secretBody != "" {
			writeFakeJSON(w, status, map[string]any{"error": "invalid_grant", "error_description": secretBody})
			return
		}
		writeFakeJSON(w, status, map[string]any{"error": "invalid_grant"})
		return
	}
	codeValue := r.PostForm.Get("code")
	f.mu.Lock()
	code, ok := f.codes[codeValue]
	f.mu.Unlock()
	if !ok || code.Used {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "code is invalid or already used"})
		return
	}
	verifier := r.PostForm.Get("code_verifier")
	sum := sha256.Sum256([]byte(verifier))
	if forceWrong || base64.RawURLEncoding.EncodeToString(sum[:]) != code.Challenge {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "PKCE verification failed"})
		return
	}
	if r.PostForm.Get("redirect_uri") != code.Redirect || r.PostForm.Get("client_id") != f.clientID {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "redirect mismatch"})
		return
	}
	nonce := code.Nonce
	if badNonce {
		nonce = "not-the-nonce"
	}
	accessToken := fmt.Sprintf("access-token-%d", time.Now().UnixNano())
	idToken := makeIDToken(f.server.URL+"/realms/"+f.realm, f.clientID, code.UserID, nonce, time.Now().Add(5*time.Minute).Unix())
	f.mu.Lock()
	code.Used = true
	f.tokens[accessToken] = code.UserID
	f.mu.Unlock()
	writeFakeJSON(w, http.StatusOK, map[string]any{
		"access_token": accessToken,
		"id_token":     idToken,
		"token_type":   "Bearer",
		"expires_in":   300,
		"scope":        "openid email profile",
	})
}

func (f *fakeKeycloak) handleUserinfo(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.userinfoHits++
	f.userinfoAuth = append(f.userinfoAuth, r.Header.Get("Authorization"))
	status := f.userinfoStatus
	retryAfter := f.userinfoRetryAfter
	malformed := f.userinfoMalformed
	delay := f.userinfoDelay
	crossSite := f.userinfoCrossSite
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if crossSite {
		w.Header().Set("Location", "http://cross-origin.invalid/userinfo")
		w.WriteHeader(http.StatusFound)
		return
	}
	if status != 0 {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		writeFakeJSON(w, status, map[string]any{"error": "request failed"})
		return
	}
	if malformed {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not-json"))
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	userID, ok := f.tokens[token]
	var user *fakeUser
	if ok {
		user = f.users[userID]
	}
	f.mu.Unlock()
	if !ok || user == nil {
		writeFakeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_token"})
		return
	}
	writeFakeJSON(w, http.StatusOK, map[string]any{
		"sub":                user.ID,
		"email":              user.Email,
		"email_verified":     user.EmailVerified,
		"preferred_username": user.Username,
	})
}

func writeFakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fakeUserJSON(u fakeUser) map[string]any {
	return map[string]any{
		"id":               u.ID,
		"username":         u.Username,
		"email":            u.Email,
		"emailVerified":    u.EmailVerified,
		"enabled":          u.Enabled,
		"createdTimestamp": u.CreatedMS,
	}
}

func makeIDToken(issuer, clientID, subject, nonce string, exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := fmt.Sprintf(`{"iss":%q,"sub":%q,"aud":%q,"azp":%q,"nonce":%q,"exp":%d}`,
		issuer, subject, clientID, clientID, nonce, exp)
	return header + "." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// ---------------------------------------------------------------------------
// In-memory SecretStore double
// ---------------------------------------------------------------------------

type memSecrets struct {
	mu   sync.Mutex
	data map[string]string
	puts int
}

func newMemSecrets() *memSecrets { return &memSecrets{data: map[string]string{}} }

func (m *memSecrets) Put(_ context.Context, ref, kind, plaintext string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rKind, _, ok := splitRef(ref)
	if !ok || rKind != kind {
		return fmt.Errorf("invalid ref %q for kind %q", ref, kind)
	}
	m.data[ref] = plaintext
	m.puts++
	return nil
}

func (m *memSecrets) Resolve(_ context.Context, ref string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[ref]
	if !ok {
		return "", fmt.Errorf("secret not found")
	}
	return v, nil
}

func (m *memSecrets) Delete(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, ref)
	return nil
}

func (m *memSecrets) all() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.data))
	for k, v := range m.data {
		out[k] = v
	}
	return out
}
