package api

import (
	"embed"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/security"
)

const sessionCookieName = "onboard_session"

//go:embed all:webdist
var webDistFS embed.FS

// handleNotFound answers unknown paths with the standard JSON error envelope.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, errs.New(errs.ClassNotFound, "http.not_found", "unknown path"))
}

// handleRoot sends a bare origin to the console.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// loginRedirect sends the operator back to the console login with a message. The console
// renders the message itself, so a failed attempt never needs a server-rendered page.
func (s *Server) loginRedirect(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/ui/login?error="+url.QueryEscape(message), http.StatusSeeOther)
}

// handleLoginSubmit verifies the operator token, issues the session cookie and redirects to
// the console.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.loginRedirect(w, r, s.t("error.form"))
		return
	}
	if !s.originAllowed(r) {
		s.loginRedirect(w, r, s.t("error.origin"))
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if s.Cfg.AdminToken == "" {
		s.loginRedirect(w, r, s.t("error.token_missing"))
		return
	}
	if token == "" || !security.ConstantTimeEqual(token, s.Cfg.AdminToken) {
		_ = s.Store.Audit(r.Context(), "anonymous", "auth.login_failed", "", nil)
		s.loginRedirect(w, r, s.t("error.invalid_token"))
		return
	}
	value, _, sessionID, err := s.Signer.IssueWithID(time.Now().Unix())
	if err != nil {
		s.loginRedirect(w, r, s.t("error.session"))
		return
	}
	// Opportunistically prune expired revocations so the table stays bounded
	// without a separate maintenance process.
	if _, err := s.Store.PruneSessionRevocations(r.Context(), time.Now()); err != nil {
		s.Log.Warn("prune session revocations failed", "error_class", "storage")
	}
	_ = sessionID
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.Cfg.SessionTTL.Seconds()),
	})
	_ = s.Store.Audit(r.Context(), "operator", "auth.login", "", nil)
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Revoke the session durably before clearing the cookie: a copied cookie
	// must not be re-authorised after logout, even across a process restart.
	info, _ := authInfoFrom(r)
	if info.SessionID == "" {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	if s.Store == nil {
		http.Error(w, "session store unavailable", http.StatusServiceUnavailable)
		return
	}
	now := time.Now()
	// Bind the revocation to the cookie's own expiry, not just the current
	// configuration: an operator may lower ONBOARD_SESSION_TTL after the cookie
	// was issued, and pruning must never resurrect an already-logged-out cookie.
	revokeUntil := now.Add(s.Cfg.SessionTTL)
	if info.SessionExpires > 0 {
		if cookieExpiry := time.Unix(info.SessionExpires, 0).UTC(); cookieExpiry.After(revokeUntil) {
			revokeUntil = cookieExpiry
		}
	}
	if err := s.Store.RevokeSession(r.Context(), info.SessionID, now, revokeUntil); err != nil {
		s.Log.Error("revoke session failed; logout aborted", "error_class", "storage")
		http.Error(w, "could not revoke the session; retry the logout", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
		SameSite: http.SameSiteStrictMode,
	})
	_ = s.Store.Audit(r.Context(), "operator", "auth.logout", "", nil)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// handleUISession lets the console bootstrap: who am I, which CSRF token to send on
// mutations, which build and language the server runs.
func (s *Server) handleUISession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if info, ok := s.authenticate(r); ok {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authenticated": true,
			"actor":         info.Actor,
			"role":          info.Role,
			"csrf":          info.CSRF,
			"version":       s.Version,
			"lang":          s.Cfg.UILang,
		})
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{"authenticated": false})
}

// handleSPA serves the embedded Vite build and falls back to index.html so client-side routes
// survive a reload.
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/ui")
	if rel == "" || rel == "/" {
		rel = "/index.html"
	}
	if data, err := webDistFS.ReadFile("webdist" + rel); err == nil {
		if ct := mime.TypeByExtension(filepath.Ext(rel)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		_, _ = w.Write(data)
		return
	}
	index, err := webDistFS.ReadFile("webdist/index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(index)
}
