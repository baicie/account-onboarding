// Package api implements the control-plane HTTP API and the minimal operator UI.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/baicie/account-onboarding/internal/alerts"
	"github.com/baicie/account-onboarding/internal/config"
	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/secrets"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow"
)

// Version is the service version reported by /v1/meta.
// Version and Commit are injected at build time (-ldflags -X ...). The defaults
// keep `go run`/tests working without build metadata.
var Version = "0.1.0"

// Commit is the source revision the binary was built from (short SHA or
// "unknown"); exposed by `onboardd version` for release provenance.
var Commit = "unknown"

// Server wires the API to storage, the engine and providers.
type Server struct {
	Cfg           config.Config
	Store         *storage.Store
	Engine        *workflow.Engine
	Registry      *provider.Registry
	Secrets       *secrets.Service
	Log           *slog.Logger
	Signer        *security.SessionSigner
	Redactor      *security.Redactor
	AlertQueue    *alerts.Queue
	AlertNotifier alerts.Notifier
	StartedAt     time.Time
	Version       string

	pages map[string]*page

	generalLimiter *rateLimiter
	loginLimiter   *rateLimiter
}

// New builds the HTTP server.
func New(cfg config.Config, store *storage.Store, engine *workflow.Engine,
	registry *provider.Registry, sec *secrets.Service, log *slog.Logger,
	signer *security.SessionSigner, redactor *security.Redactor) (*Server, error) {
	pages, err := loadPages()
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	if redactor == nil {
		redactor = security.NewRedactor()
	}
	return &Server{
		Cfg:            cfg,
		Store:          store,
		Engine:         engine,
		Registry:       registry,
		Secrets:        sec,
		Log:            log,
		Signer:         signer,
		Redactor:       redactor,
		AlertQueue:     alerts.NewQueue(100, time.Minute),
		AlertNotifier:  &alerts.RecordingNotifier{},
		StartedAt:      time.Now().UTC(),
		Version:        Version,
		pages:          pages,
		generalLimiter: newRateLimiter(cfg.RateLimitPerMinute, time.Minute),
		loginLimiter:   newRateLimiter(cfg.LoginRateLimitPerMinute, time.Minute),
	}, nil
}

// Handler builds the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Liveness is the only unauthenticated endpoint.
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Control-plane API. Mutations require CSRF when authenticated by cookie.
	mux.HandleFunc("GET /readyz", s.apiAuth(false, false, s.handleReadyz))
	mux.HandleFunc("GET /v1/meta", s.apiAuth(false, false, s.handleMeta))
	mux.HandleFunc("GET /v1/providers", s.apiAuth(false, false, s.handleProviders))
	mux.HandleFunc("POST /v1/runs", s.apiAuth(true, true, s.handleCreateRun))
	mux.HandleFunc("GET /v1/runs", s.apiAuth(false, false, s.handleListRuns))
	mux.HandleFunc("GET /v1/runs/{id}", s.apiAuth(false, false, s.handleGetRun))
	mux.HandleFunc("GET /v1/runs/{id}/events", s.apiAuth(false, false, s.handleListEvents))
	mux.HandleFunc("POST /v1/runs/{id}/resume", s.apiAuth(true, true, s.handleResumeRun))
	mux.HandleFunc("POST /v1/runs/{id}/cancel", s.apiAuth(true, true, s.handleCancelRun))
	mux.HandleFunc("GET /v1/audit", s.apiAuth(false, false, s.handleListAudit))
	mux.HandleFunc("GET /v1/ops/external", s.apiAuth(false, true, s.handleListExternalOperations))
	mux.HandleFunc("POST /v1/ops/external/{id}/resolve", s.apiAuth(true, true, s.handleResolveExternalOperation))
	mux.HandleFunc("GET /v1/ops/external/{id}/audit", s.apiAuth(false, true, s.handleExternalOperationAudit))
	mux.HandleFunc("GET /v1/ops/metrics", s.apiAuth(false, true, s.handleOpsMetrics))
	mux.HandleFunc("GET /v1/ops/alerts", s.apiAuth(false, true, s.handleOpsAlerts))
	mux.HandleFunc("GET /v1/ops/alerts/queue", s.apiAuth(false, true, s.handleOpsAlertQueue))
	mux.HandleFunc("POST /v1/ops/alerts/drain", s.apiAuth(true, true, s.handleOpsAlertDrain))
	mux.HandleFunc("POST /v1/ops/alerts/resolve", s.apiAuth(true, true, s.handleOpsAlertResolve))
	mux.HandleFunc("POST /v1/ops/alerts/reconcile", s.apiAuth(true, true, s.handleOpsAlertReconcile))

	// Minimal operator UI.
	mux.HandleFunc("GET /", s.handleRoot)
	mux.HandleFunc("GET /ui/login", s.handleLoginPage)
	mux.HandleFunc("POST /ui/login", s.handleLoginSubmit)
	mux.HandleFunc("POST /ui/logout", s.uiAuth(s.handleLogout))
	mux.HandleFunc("GET /ui/", s.uiAuth(s.handleDashboard))
	mux.HandleFunc("GET /ui/runs/{id}", s.uiAuth(s.handleRunPage))
	mux.HandleFunc("GET /ui/ops/external", s.uiAuth(s.handleOpsExternalPage))
	mux.HandleFunc("POST /ui/ops/external/{id}/resolve", s.uiAuth(s.handleOpsExternalResolveUI))
	mux.HandleFunc("GET /ui/ops/external/{id}", s.uiAuth(s.handleOpsExternalDetail))
	mux.HandleFunc("POST /ui/runs", s.uiAuth(s.handleCreateRunUI))
	mux.HandleFunc("POST /ui/runs/{id}/resume", s.uiAuth(s.handleResumeUI))
	mux.HandleFunc("POST /ui/runs/{id}/cancel", s.uiAuth(s.handleCancelUI))
	mux.HandleFunc("GET /ui/static/app.css", s.handleCSS)

	return s.securityHeaders(s.requestLogger(s.recoverer(s.rateLimit(mux))))
}

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

type requestIDKey struct{}

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("panic in handler", "path", r.URL.Path, "recover", fmt.Sprint(rec))
				writeError(w, r, errs.New(errs.ClassInternal, "http", "internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

var requestCounter atomic.Int64

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), requestCounter.Add(1))
		}
		ctx := context.WithValue(r.Context(), requestIDKey{}, rid)
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-ID", rid)

		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		if !strings.HasPrefix(r.URL.Path, "/ui/static") {
			s.Log.Info("http request",
				"request_id", rid,
				"method", r.Method,
				"path", security.SanitizeURL(r.URL.Path),
				"status", status,
				"latency_ms", time.Since(start).Milliseconds(),
			)
		}
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if strings.HasPrefix(r.URL.Path, "/ui/") || r.URL.Path == "/" {
			h.Set("Content-Security-Policy",
				"default-src 'none'; style-src 'self'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// auth
// ---------------------------------------------------------------------------

type authInfo struct {
	Actor          string
	Role           string // "admin" or "viewer" (read-only)
	Via            string
	CSRF           string
	SessionID      string
	SessionExpires int64 // unix seconds from the signed cookie payload
}

type authKeyType struct{}

var authKey = authKeyType{}

func authInfoFrom(r *http.Request) (authInfo, bool) {
	v, ok := r.Context().Value(authKey).(authInfo)
	return v, ok
}

// apiAuth accepts a bearer token or a session cookie. Cookie-authenticated
// mutations additionally require a CSRF token and a same-origin request.
func (s *Server) apiAuth(requireCSRF bool, adminOnly bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, ok := s.authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="onboard"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{
					"class":      string(errs.ClassAuthRequired),
					"message":    "authentication required",
					"request_id": requestID(r),
				},
			})
			return
		}
		if adminOnly && info.Role != "admin" {
			writeError(w, r, errs.New(errs.ClassNotAuthorized, "http.auth",
				"viewer role is read-only; a mutating request requires the admin token"))
			return
		}
		if info.Via == "cookie" && requireCSRF {
			if !s.csrfValid(r, info) {
				writeError(w, r, errs.New(errs.ClassNotAuthorized, "http.csrf", "CSRF or Origin check failed"))
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authKey, info)))
	}
}

// credentialActor identifies the credential subject. A shared Bearer token
// cannot identify a natural person, so it is explicitly labelled as the token
// credential (stable truncated fingerprint, never the token itself).
func credentialActor(kind, token string) string {
	sum := sha256.Sum256([]byte(token))
	return kind + ":sha256:" + hex.EncodeToString(sum[:6])
}

func (s *Server) authenticate(r *http.Request) (authInfo, bool) {
	// Bearer token path (API clients).
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if s.Cfg.AdminToken != "" && security.ConstantTimeEqual(token, s.Cfg.AdminToken) {
			return authInfo{Actor: credentialActor("bearer:admin", token), Role: "admin", Via: "bearer"}, true
		}
		if s.Cfg.ReadOnlyToken != "" && security.ConstantTimeEqual(token, s.Cfg.ReadOnlyToken) {
			return authInfo{Actor: credentialActor("bearer:viewer", token), Role: "viewer", Via: "bearer"}, true
		}
		return authInfo{}, false
	}
	// Session cookie path (browser UI).
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return authInfo{}, false
	}
	csrf, sessionID, expiresAt, ok := s.Signer.VerifySession(cookie.Value, time.Now().Unix())
	if !ok {
		return authInfo{}, false
	}
	if s.Store == nil {
		return authInfo{}, false
	}
	revoked, err := s.Store.SessionRevoked(r.Context(), sessionID, time.Now())
	if err != nil {
		// Fail closed: a session must never be accepted when the durable
		// revocation state cannot be read.
		s.Log.Warn("session revocation lookup failed; rejecting session",
			"error_class", "storage_unavailable")
		return authInfo{}, false
	}
	if revoked {
		return authInfo{}, false
	}
	return authInfo{Actor: "cookie:operator", Role: "admin", Via: "cookie", CSRF: csrf,
		SessionID: sessionID, SessionExpires: expiresAt}, true
}

func (s *Server) csrfValid(r *http.Request, info authInfo) bool {
	if !s.sameOriginMutation(r) {
		return false
	}
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		token = r.FormValue("csrf")
	}
	if token == "" {
		return false
	}
	return security.ConstantTimeEqual(token, info.CSRF)
}

func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	return s.originHostAllowed(origin, r.Host)
}

// sameOriginMutation is the stricter check for cookie-authenticated mutations:
// an Origin or same-host Referer is mandatory. A browser always sends one; a
// cross-site form or a synthetic request without either is rejected.
func (s *Server) sameOriginMutation(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		return s.originHostAllowed(origin, r.Host)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		return err == nil && u.Host != "" && sameHost(u.Host, r.Host)
	}
	return false
}

func (s *Server) originHostAllowed(origin, requestHost string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if sameHost(u.Host, requestHost) {
		return true
	}
	for _, allowed := range s.Cfg.AllowedOrigins {
		if strings.EqualFold(strings.TrimRight(allowed, "/"), strings.TrimRight(origin, "/")) {
			return true
		}
	}
	// Loopback aliases are equivalent for a self-hosted service.
	return sameHost(u.Host, normalizeLoopback(requestHost))
}

// secureCookie reports whether the session cookie must carry the Secure flag.
// It is true under TLS and, when explicitly trusted, behind a TLS proxy.
func (s *Server) secureCookie(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.Cfg.TrustProxyHeaders && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func sameHost(a, b string) bool {
	return strings.EqualFold(stripDefaultPort(a), stripDefaultPort(b))
}

func stripDefaultPort(host string) string {
	if strings.HasSuffix(host, ":80") {
		return strings.TrimSuffix(host, ":80")
	}
	if strings.HasSuffix(host, ":443") {
		return strings.TrimSuffix(host, ":443")
	}
	return host
}

func normalizeLoopback(host string) string {
	h := stripDefaultPort(host)
	parts := strings.Split(h, ":")
	if len(parts) != 2 {
		return h
	}
	switch strings.ToLower(parts[0]) {
	case "localhost", "0.0.0.0", "::1", "[::1]":
		return "127.0.0.1:" + parts[1]
	}
	return h
}

// uiAuth requires a browser session and redirects to the login page.
func (s *Server) uiAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, ok := s.authenticate(r)
		if !ok || info.Via != "cookie" {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !s.csrfValid(r, info) {
			http.Error(w, "CSRF check failed", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authKey, info)))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	class := errs.ClassOf(err)
	status := statusForClass(class)
	body := map[string]any{
		"error": map[string]any{
			"class":      string(class),
			"message":    errs.SummaryOf(err),
			"request_id": requestID(r),
		},
	}
	writeJSON(w, status, body)
}

func statusForClass(class errs.Class) int {
	switch class {
	case errs.ClassValidation:
		return http.StatusBadRequest
	case errs.ClassNotAuthorized:
		return http.StatusForbidden
	case errs.ClassAuthRequired:
		return http.StatusUnauthorized
	case errs.ClassAuthExpired:
		return http.StatusGone
	case errs.ClassRateLimited:
		return http.StatusTooManyRequests
	case errs.ClassUpstreamTransient:
		return http.StatusBadGateway
	case errs.ClassTimeoutUncertain:
		return http.StatusGatewayTimeout
	case errs.ClassNotFound:
		return http.StatusNotFound
	case errs.ClassConflict:
		return http.StatusConflict
	case errs.ClassCancelled:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// decodeJSON enforces a small body limit and rejects unknown fields, which is
// how "no account_count / batch registration" is enforced structurally.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return errs.New(errs.ClassValidation, "http.decode", "Content-Type must be application/json")
	}
	body := http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errs.New(errs.ClassValidation, "http.decode", "invalid JSON body")
	}
	if dec.More() {
		return errs.New(errs.ClassValidation, "http.decode", "unexpected trailing JSON")
	}
	return nil
}

// summarizeFor applies role-based redaction: the viewer role must not see the
// vendor authorization URL, which embeds a single-use OAuth state value.
func (s *Server) summarizeFor(ctx context.Context, run storage.Run, role string) workflow.RunSummary {
	sum := s.Engine.Summarize(ctx, run)
	if role != "admin" {
		sum.AuthorizationURL = ""
	}
	return sum
}

// handleHealthz is intentionally unauthenticated and secret-free.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.Version})
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	templates := make([]map[string]string, 0, 4)
	for _, t := range s.Engine.Templates() {
		templates = append(templates, map[string]string{"name": t.Name, "mode": t.Mode})
	}
	health, _ := s.Store.Health(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        s.Version,
		"mode":           string(s.Cfg.Mode),
		"schema_version": health.SchemaVersion,
		"templates":      templates,
		"started_at":     s.StartedAt.Format(time.RFC3339),
	})
}

// handleReadyz verifies DB writability, schema version, WAL mode and key setup.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	health, err := s.Store.Health(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "reason": "database unavailable"})
		return
	}
	ready := health.Writable && health.SchemaVersion >= 5 && health.SessionRevocationsReady &&
		health.ExternalOperationsReady &&
		strings.EqualFold(health.WALMode, "wal")
	if len(s.Cfg.MasterKey) != 32 {
		ready = false
	}
	if !ready {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready": false, "schema_version": health.SchemaVersion, "wal": health.WALMode,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ready": true, "schema_version": health.SchemaVersion, "wal": health.WALMode,
	})
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"items": s.Registry.Catalog(),
		"mode":  string(s.Cfg.Mode),
	})
}

// notFoundResponse centralizes 404 JSON.
func notFoundResponse(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, errs.New(errs.ClassNotFound, "http", "not found"))
}
