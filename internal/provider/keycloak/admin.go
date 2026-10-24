package keycloak

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

// userRepresentation is the subset of the Keycloak Admin REST user resource the
// adapter needs. It never contains a credential.
type userRepresentation struct {
	ID               string              `json:"id"`
	Username         string              `json:"username"`
	Email            string              `json:"email"`
	EmailVerified    bool                `json:"emailVerified"`
	Enabled          bool                `json:"enabled"`
	CreatedTimestamp int64               `json:"createdTimestamp"`
	RequiredActions  []string            `json:"requiredActions"`
	Attributes       map[string][]string `json:"attributes"`
}

// credentialRepresentation is the password credential payload.
type credentialRepresentation struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Temporary bool   `json:"temporary"`
}

// createUserRepresentation is the create payload. Usernames are stable run
// subjects; email verification always starts from false.
type createUserRepresentation struct {
	Username      string                     `json:"username"`
	Email         string                     `json:"email"`
	FirstName     string                     `json:"firstName,omitempty"`
	LastName      string                     `json:"lastName,omitempty"`
	Enabled       bool                       `json:"enabled"`
	EmailVerified bool                       `json:"emailVerified"`
	Attributes    map[string][]string        `json:"attributes,omitempty"`
	Credentials   []credentialRepresentation `json:"credentials,omitempty"`
}

// adminCall performs an authorized Admin REST request, refreshing the cached
// admin token exactly once on an unexpected 401 (never on 403).
func (a *Adapter) adminCall(ctx context.Context, op, method, rawURL string, out any, body []byte) (*http.Response, error) {
	token, err := a.admin.adminToken(ctx)
	if err != nil {
		return nil, err
	}
	header := withUserAgent(bearerAuthorized(token))
	resp, err := a.doJSON(ctx, a.api, op, method, rawURL, header, body, out)
	if err == nil {
		return resp, nil
	}
	if errs.HTTPStatusOf(err) != http.StatusUnauthorized {
		return resp, err
	}
	// The cached admin token went stale early: drop it and retry once. 401/403
	// are never retried beyond this single token-refresh attempt.
	a.admin.invalidate()
	token, tokenErr := a.admin.adminToken(ctx)
	if tokenErr != nil {
		return resp, err
	}
	header = withUserAgent(bearerAuthorized(token))
	return a.doJSON(ctx, a.api, op, method, rawURL, header, body, out)
}

// findUserByUsername performs the exact-match admin query used to make identity
// creation idempotent across step retries and process restarts.
func (a *Adapter) findUserByUsername(ctx context.Context, username string) (userRepresentation, bool, error) {
	const op = "keycloak.find_user"
	username = strings.TrimSpace(username)
	if username == "" {
		return userRepresentation{}, false, errs.New(errs.ClassValidation, op, "username is required")
	}
	query := url.Values{}
	query.Set("username", username)
	query.Set("exact", "true")
	query.Set("briefRepresentation", "false")
	var users []userRepresentation
	if _, err := a.adminCall(ctx, op, http.MethodGet, a.adminURL("/users")+"?"+query.Encode(), &users, nil); err != nil {
		return userRepresentation{}, false, err
	}
	for _, u := range users {
		if strings.EqualFold(u.Username, username) {
			return u, true, nil
		}
	}
	return userRepresentation{}, false, nil
}

// findUserByEmail performs the exact-match email query.
func (a *Adapter) findUserByEmail(ctx context.Context, email string) (userRepresentation, bool, error) {
	const op = "keycloak.find_user"
	email = strings.TrimSpace(email)
	if email == "" {
		return userRepresentation{}, false, errs.New(errs.ClassValidation, op, "email is required")
	}
	query := url.Values{}
	query.Set("email", email)
	query.Set("exact", "true")
	var users []userRepresentation
	if _, err := a.adminCall(ctx, op, http.MethodGet, a.adminURL("/users")+"?"+query.Encode(), &users, nil); err != nil {
		return userRepresentation{}, false, err
	}
	for _, u := range users {
		if strings.EqualFold(strings.TrimSpace(u.Email), email) {
			return u, true, nil
		}
	}
	return userRepresentation{}, false, nil
}

// findUser resolves a sandbox identity by either its Keycloak subject id, its
// username or its email. Subject ids are UUIDs; the other two are stable run
// inputs, so every caller can recover after a crash without extra state.
func (a *Adapter) findUser(ctx context.Context, subjectID, address string) (userRepresentation, error) {
	const op = "keycloak.find_user"
	subjectID = strings.TrimSpace(subjectID)
	if subjectID != "" && looksLikeUUID(subjectID) {
		user, found, err := a.getUser(ctx, subjectID)
		if err != nil {
			return userRepresentation{}, err
		}
		if found {
			return user, nil
		}
	}
	if subjectID != "" {
		if user, found, err := a.findUserByUsername(ctx, subjectID); err != nil {
			return userRepresentation{}, err
		} else if found {
			return user, nil
		}
	}
	if address != "" {
		if user, found, err := a.findUserByEmail(ctx, address); err != nil {
			return userRepresentation{}, err
		} else if found {
			return user, nil
		}
	}
	return userRepresentation{}, errs.New(errs.ClassNotFound, op, "sandbox identity does not exist")
}

// getUser reads one user by Keycloak id. A missing user is reported as
// found=false, not as an error.
func (a *Adapter) getUser(ctx context.Context, id string) (userRepresentation, bool, error) {
	const op = "keycloak.get_user"
	id = strings.TrimSpace(id)
	if id == "" {
		return userRepresentation{}, false, errs.New(errs.ClassValidation, op, "user id is required")
	}
	var user userRepresentation
	_, err := a.adminCall(ctx, op, http.MethodGet, a.adminURL("/users")+"/"+url.PathEscape(id), &user, nil)
	if err != nil {
		if errs.ClassOf(err) == errs.ClassNotFound {
			return userRepresentation{}, false, nil
		}
		return userRepresentation{}, false, err
	}
	if strings.TrimSpace(user.ID) == "" {
		return userRepresentation{}, false, errs.New(errs.ClassProviderBug, op, "keycloak user resource has no id")
	}
	return user, true, nil
}

// createUser posts a new user and returns its id. A 409 (already exists) is
// expected on retries and is resolved by re-reading the existing user, which is
// what makes Register idempotent.
func (a *Adapter) createUser(ctx context.Context, payload createUserRepresentation) (userRepresentation, bool, error) {
	const op = "keycloak.create_user"
	raw, err := json.Marshal(payload)
	if err != nil {
		return userRepresentation{}, false, errs.New(errs.ClassInternal, op, "could not encode the sandbox user payload")
	}
	resp, err := a.adminCall(ctx, op, http.MethodPost, a.adminURL("/users"), nil, raw)
	if err != nil {
		if errs.ClassOf(err) == errs.ClassConflict {
			user, found, findErr := a.findUserByUsername(ctx, payload.Username)
			if findErr != nil {
				return userRepresentation{}, false, findErr
			}
			if !found {
				return userRepresentation{}, false, errs.New(errs.ClassConflict, op, "keycloak reported a conflict but the sandbox identity is missing")
			}
			return user, false, nil
		}
		return userRepresentation{}, false, err
	}
	if id := userIDFromLocation(resp); id != "" {
		return userRepresentation{ID: id, Username: payload.Username, Email: payload.Email, Enabled: payload.Enabled}, true, nil
	}
	// Older proxies strip Location: fall back to the exact-match lookup.
	user, found, err := a.findUserByUsername(ctx, payload.Username)
	if err != nil {
		return userRepresentation{}, true, err
	}
	if !found {
		return userRepresentation{}, true, errs.New(errs.ClassProviderBug, op, "created sandbox user is not retrievable")
	}
	return user, true, nil
}

// updateUserEmail rewrites the email of an existing sandbox user. It is only
// used when a retry supplies a different address for the same stable subject.
func (a *Adapter) updateUserEmail(ctx context.Context, id, email string) error {
	const op = "keycloak.update_user"
	payload := map[string]any{"email": email, "emailVerified": false}
	raw, err := json.Marshal(payload)
	if err != nil {
		return errs.New(errs.ClassInternal, op, "could not encode the sandbox user payload")
	}
	_, err = a.adminCall(ctx, op, http.MethodPut, a.adminURL("/users")+"/"+url.PathEscape(id), nil, raw)
	return err
}

// resetPassword sets a non-temporary password for the sandbox user.
func (a *Adapter) resetPassword(ctx context.Context, id, password string) error {
	const op = "keycloak.reset_password"
	if strings.TrimSpace(password) == "" {
		return errs.New(errs.ClassValidation, op, "password is required")
	}
	raw, err := json.Marshal(credentialRepresentation{Type: "password", Value: password, Temporary: false})
	if err != nil {
		return errs.New(errs.ClassInternal, op, "could not encode the sandbox credential payload")
	}
	_, err = a.adminCall(ctx, op, http.MethodPut, a.adminURL("/users")+"/"+url.PathEscape(id)+"/reset-password", nil, raw)
	return err
}

// executeActionsEmail triggers the VERIFY_EMAIL action email through the realm
// SMTP (Mailpit in the sandbox).
func (a *Adapter) executeActionsEmail(ctx context.Context, id string, actions []string, clientID string) error {
	const op = "keycloak.execute_actions_email"
	if len(actions) == 0 {
		return errs.New(errs.ClassValidation, op, "at least one action is required")
	}
	query := url.Values{}
	if strings.TrimSpace(clientID) != "" {
		query.Set("client_id", clientID)
	}
	query.Set("lifespan", strconv.Itoa(actionLifespanSecs))
	raw, err := json.Marshal(actions)
	if err != nil {
		return errs.New(errs.ClassInternal, op, "could not encode the action list")
	}
	_, err = a.adminCall(ctx, op, http.MethodPut, a.adminURL("/users")+"/"+url.PathEscape(id)+"/execute-actions-email?"+query.Encode(), nil, raw)
	return err
}

// deleteUser removes a sandbox identity. It is exposed only so tests and
// teardown can clean up controlled fixture users; nothing else calls it.
func (a *Adapter) deleteUser(ctx context.Context, id string) error {
	const op = "keycloak.delete_user"
	_, err := a.adminCall(ctx, op, http.MethodDelete, a.adminURL("/users")+"/"+url.PathEscape(id), nil, nil)
	if err != nil && errs.ClassOf(err) == errs.ClassNotFound {
		return nil
	}
	return err
}

// listUsers returns up to max realm users (brief representation). Test teardown
// uses it to remove every sandbox identity created during a run window.
func (a *Adapter) listUsers(ctx context.Context, max int) ([]userRepresentation, error) {
	const op = "keycloak.list_users"
	if max <= 0 || max > 1000 {
		max = 200
	}
	query := url.Values{}
	query.Set("max", strconv.Itoa(max))
	query.Set("briefRepresentation", "false")
	var users []userRepresentation
	if _, err := a.adminCall(ctx, op, http.MethodGet, a.adminURL("/users")+"?"+query.Encode(), &users, nil); err != nil {
		return nil, err
	}
	return users, nil
}

// DeleteSandboxUser is the only mutation exported for test teardown. It deletes
// one controlled sandbox identity and treats an already-absent user as success.
func (a *Adapter) DeleteSandboxUser(ctx context.Context, subjectID string) error {
	if err := a.ensureEnabled(provider.KindSandbox); err != nil {
		return err
	}
	user, err := a.findUser(ctx, subjectID, "")
	if err != nil {
		if errs.ClassOf(err) == errs.ClassNotFound {
			return nil
		}
		return err
	}
	return a.deleteUser(ctx, user.ID)
}

// ListSandboxUsers lists controlled sandbox identities (brief attributes).
func (a *Adapter) ListSandboxUsers(ctx context.Context, max int) ([]SandboxUser, error) {
	if err := a.ensureEnabled(provider.KindIdentity); err != nil {
		return nil, err
	}
	users, err := a.listUsers(ctx, max)
	if err != nil {
		return nil, err
	}
	out := make([]SandboxUser, 0, len(users))
	for _, u := range users {
		out = append(out, SandboxUser{
			ID:            u.ID,
			Username:      u.Username,
			Email:         u.Email,
			EmailVerified: u.EmailVerified,
			CreatedAtMS:   u.CreatedTimestamp,
		})
	}
	return out, nil
}

// SandboxUser is the non-secret projection used by E2E assertions and teardown.
type SandboxUser struct {
	ID            string
	Username      string
	Email         string
	EmailVerified bool
	CreatedAtMS   int64
}

// userIDFromLocation extracts the trailing id of a 201 Location header.
func userIDFromLocation(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	loc := strings.TrimSpace(resp.Header.Get("Location"))
	if loc == "" {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	path := strings.TrimSuffix(u.Path, "/")
	idx := strings.LastIndexByte(path, '/')
	if idx < 0 {
		return ""
	}
	return path[idx+1:]
}

// looksLikeUUID reports whether s has the canonical UUID shape Keycloak uses
// for subject ids. Usernames never match it.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}
