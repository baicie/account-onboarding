package api

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

//go:embed templates/*.html templates/app.css
var uiFS embed.FS

const sessionCookieName = "onboard_session"

type page struct{ tmpl *template.Template }

func loadPages(lang string) (map[string]*page, error) {
	funcs := template.FuncMap{
		"T": func(key string) string { return localize(lang, key) },
		"fmtMS": func(ms *int64) string {
			if ms == nil || *ms == 0 {
				return "—"
			}
			return time.UnixMilli(*ms).UTC().Format("2006-01-02 15:04:05Z")
		},
		"fmtUnix": func(ms int64) string {
			if ms == 0 {
				return "—"
			}
			return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05Z")
		},
		"stateClass": func(s string) string {
			return strings.ToLower(strings.ReplaceAll(s, "_", "-"))
		},
		"short": func(id string) string {
			if len(id) > 8 {
				return id[:8]
			}
			return id
		},
		"list": func(v ...string) []string { return v },
		"json": func(v any) string {
			raw, err := json.Marshal(v)
			if err != nil {
				return "{}"
			}
			return string(raw)
		},
	}
	layout, err := template.New("layout").Funcs(funcs).ParseFS(uiFS, "templates/layout.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*page{}
	for _, name := range []string{"login", "dashboard", "run", "ops_external", "ops_external_detail", "alerts", "alerts_detail"} {
		clone, err := layout.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := clone.ParseFS(uiFS, "templates/"+name+".html"); err != nil {
			return nil, fmt.Errorf("parse %s page: %w", name, err)
		}
		pages[name] = &page{tmpl: clone}
	}
	return pages, nil
}

func (p *page) render(w http.ResponseWriter, status int, data any) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := p.tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		// The response may already be partially written; the handler logs the
		// underlying cause so a broken template is visible in operations.
		_, _ = w.Write([]byte("template error"))
		return err
	}
	return nil
}

type pageData struct {
	Title     string
	Version   string
	CSRFToken string
	SignedIn  bool
	Actor     string
	Error     string
	Notice    string
}

type statRow struct {
	Status string
	Count  int
}

type runRow struct {
	ID             string
	Status         string
	Template       string
	Mode           string
	CurrentStep    string
	ActionRequired bool
	ErrorClass     string
	CreatedAt      int64
	UpdatedAt      int64
}

type templateRow struct {
	Name  string
	Mode  string
	Steps int
}

type dashboardPage struct {
	pageData
	Runs           []runRow
	Providers      []provider.Descriptor
	Templates      []templateRow
	Stats          []statRow
	IdempotencyKey string
	Form           createForm
	StatusFilter   string
	TemplateFilter string
	Cursor         string
	NextCursor     string
	HasNext        bool
}

type createForm struct {
	Template            string
	MailboxProvider     string
	IdentityProvider    string
	OAuthProvider       string
	EmailProvider       string
	CredentialProvider  string
	SandboxProvider     string
	ExistingIdentityRef string
}

type stepRow struct {
	Name          string
	Status        string
	Attempts      int64
	Failures      int64
	ErrorClass    string
	ErrorSummary  string
	WaitingReason string
	NextRetryAt   *int64
}

type eventRow struct {
	Seq       int64
	Type      string
	Step      string
	Payload   string
	CreatedAt int64
}

type runPage struct {
	pageData
	Run              workflow.RunSummary
	Steps            []stepRow
	Events           []eventRow
	CanResume        bool
	CanCancel        bool
	IsTerminal       bool
	AuthorizationURL string
	TemplateSteps    []string
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		notFoundResponse(w, r)
		return
	}
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if info, ok := s.authenticate(r); ok && info.Via == "cookie" {
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, http.StatusOK, "")
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, http.StatusBadRequest, s.t("error.form"))
		return
	}
	if !s.originAllowed(r) {
		s.renderLogin(w, http.StatusForbidden, s.t("error.origin"))
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if s.Cfg.AdminToken == "" {
		s.renderLogin(w, http.StatusServiceUnavailable, s.t("error.token_missing"))
		return
	}
	if token == "" || !security.ConstantTimeEqual(token, s.Cfg.AdminToken) {
		_ = s.Store.Audit(r.Context(), "anonymous", "auth.login_failed", "", nil)
		s.renderLogin(w, http.StatusUnauthorized, s.t("error.invalid_token"))
		return
	}
	value, _, sessionID, err := s.Signer.IssueWithID(time.Now().Unix())
	if err != nil {
		s.renderLogin(w, http.StatusInternalServerError, s.t("error.session"))
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

func (s *Server) renderLogin(w http.ResponseWriter, status int, message string) {
	if err := s.pages["login"].render(w, status, pageData{
		Title:   s.t("page.signin"),
		Version: s.Version,
		Error:   message,
	}); err != nil {
		s.Log.Error("render login page", "error", err)
	}
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

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	if runID := strings.TrimSpace(r.URL.Query().Get("run")); runID != "" {
		if _, err := s.Store.GetRun(r.Context(), runID); err != nil {
			http.Redirect(w, r, "/ui/?error="+url.QueryEscape("run not found"), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/ui/runs/"+url.PathEscape(runID), http.StatusSeeOther)
		return
	}
	statusFilter := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	templateFilter := strings.TrimSpace(r.URL.Query().Get("template"))
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if msg := s.validateRunFilters(statusFilter, templateFilter, cursor); msg != "" {
		s.renderDashboardError(w, http.StatusBadRequest, msg)
		return
	}
	runs, err := s.Store.ListRuns(r.Context(), storage.RunListFilter{Status: statusFilter, Template: templateFilter, Limit: 50, Cursor: cursor})
	if err != nil {
		if errors.Is(err, storage.ErrConflict) {
			s.renderDashboardError(w, http.StatusBadRequest, "invalid pagination cursor")
			return
		}
		http.Error(w, "could not load runs", http.StatusInternalServerError)
		return
	}
	counts, err := s.Store.CountByStatus(r.Context())
	if err != nil {
		counts = map[string]int{}
	}
	catalog := s.Registry.Catalog()
	rows := make([]runRow, 0, len(runs.Items))
	for _, run := range runs.Items {
		rows = append(rows, runRow{
			ID: run.ID, Status: run.Status, Template: run.Template, Mode: run.Mode,
			CurrentStep: run.CurrentStep, ActionRequired: run.ActionRequired,
			ErrorClass: run.LastErrorClass, CreatedAt: run.CreatedAtMS, UpdatedAt: run.UpdatedAtMS,
		})
	}
	stats := make([]statRow, 0, len(state.AllRunStates))
	for _, st := range state.AllRunStates {
		count := counts[string(st)]
		if count == 0 {
			continue
		}
		stats = append(stats, statRow{Status: string(st), Count: count})
	}
	templates := make([]templateRow, 0, 4)
	for _, t := range s.Engine.Templates() {
		templates = append(templates, templateRow{Name: t.Name, Mode: t.Mode, Steps: len(t.Steps)})
	}
	form := createForm{
		Template:           "sandbox-email-verify-oidc",
		MailboxProvider:    pickEnabled(catalog, provider.KindMailbox),
		IdentityProvider:   pickEnabled(catalog, provider.KindIdentity),
		OAuthProvider:      pickEnabled(catalog, provider.KindOAuth),
		EmailProvider:      pickEnabled(catalog, provider.KindEmail),
		CredentialProvider: pickEnabled(catalog, provider.KindCredential),
		SandboxProvider:    pickEnabled(catalog, provider.KindSandbox),
	}
	if err := s.pages["dashboard"].render(w, http.StatusOK, dashboardPage{
		pageData: pageData{
			Title:     s.t("page.runs"),
			Version:   s.Version,
			CSRFToken: info.CSRF,
			SignedIn:  true,
			Actor:     info.Actor,
			Error:     r.URL.Query().Get("error"),
			Notice:    r.URL.Query().Get("notice"),
		},
		Runs: rows, Providers: catalog, Templates: templates, Stats: stats,
		IdempotencyKey: security.NewID(), Form: form, StatusFilter: statusFilter,
		TemplateFilter: templateFilter, Cursor: cursor, NextCursor: runs.NextCursor,
		HasNext: runs.NextCursor != "",
	}); err != nil {
		s.Log.Error("render dashboard page", "error", err)
	}
}

func pickEnabled(catalog []provider.Descriptor, kind provider.Kind) string {
	for _, d := range catalog {
		if d.Kind == kind && d.Enabled {
			return d.Name
		}
	}
	return ""
}

func (s *Server) validateRunFilters(status, template, cursor string) string {
	if len(status) > 64 || len(template) > 128 || len(cursor) > 512 {
		return "filter or cursor is too long"
	}
	if status != "" {
		valid := false
		for _, st := range state.AllRunStates {
			if string(st) == status {
				valid = true
				break
			}
		}
		if !valid {
			return "unknown run status filter"
		}
	}
	if template != "" {
		if _, ok := s.Engine.Template(template); !ok {
			return "unknown template filter"
		}
	}
	return ""
}

func (s *Server) renderDashboardError(w http.ResponseWriter, status int, message string) {
	info, _ := authInfoFrom(&http.Request{})
	_ = info
	templates := make([]templateRow, 0, 4)
	for _, t := range s.Engine.Templates() {
		templates = append(templates, templateRow{Name: t.Name, Mode: t.Mode, Steps: len(t.Steps)})
	}
	if err := s.pages["dashboard"].render(w, status, dashboardPage{
		pageData:  pageData{Title: s.t("page.runs"), Version: s.Version, Error: message},
		Templates: templates, Providers: s.Registry.Catalog(),
	}); err != nil {
		s.Log.Error("render dashboard error", "error", err)
	}
}

func (s *Server) handleRunPage(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	id := r.PathValue("id")
	run, err := s.Store.GetRun(r.Context(), id)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	steps, err := s.Store.ListSteps(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load steps", http.StatusInternalServerError)
		return
	}
	events, err := s.Store.ListEvents(r.Context(), id, 0, 200)
	if err != nil {
		http.Error(w, "could not load events", http.StatusInternalServerError)
		return
	}
	summary := s.Engine.Summarize(r.Context(), run)
	stepRows := make([]stepRow, 0, len(steps))
	for _, st := range steps {
		stepRows = append(stepRows, stepRow{
			Name: st.Name, Status: st.Status, Attempts: st.AttemptCount, Failures: st.FailureCount,
			ErrorClass: st.ErrorClass, ErrorSummary: st.ErrorSummary,
			WaitingReason: st.WaitingReason, NextRetryAt: st.NextRetryAtMS,
		})
	}
	eventRows := make([]eventRow, 0, len(events))
	for _, ev := range events {
		payload, _ := json.Marshal(ev.Payload)
		eventRows = append(eventRows, eventRow{
			Seq: ev.Seq, Type: ev.Type, Step: ev.Step,
			Payload: string(payload), CreatedAt: ev.CreatedAtMS,
		})
	}
	var templateSteps []string
	if t, ok := s.Engine.Template(run.Template); ok {
		templateSteps = t.StepNames()
	}
	status := state.RunState(run.Status)
	if err := s.pages["run"].render(w, http.StatusOK, runPage{
		pageData: pageData{
			Title:     s.t("page.run_prefix") + shortID(run.ID),
			Version:   s.Version,
			CSRFToken: info.CSRF,
			SignedIn:  true,
			Actor:     info.Actor,
			Error:     r.URL.Query().Get("error"),
			Notice:    r.URL.Query().Get("notice"),
		},
		Run:              summary,
		Steps:            stepRows,
		Events:           eventRows,
		CanResume:        status.NeedsManualResume(),
		CanCancel:        !status.IsTerminal() && run.Status != "CANCEL_REQUESTED",
		IsTerminal:       status.IsTerminal(),
		AuthorizationURL: summary.AuthorizationURL,
		TemplateSteps:    templateSteps,
	}); err != nil {
		s.Log.Error("render run page", "error", err, "run_id", id)
	}
}

func (s *Server) handleCreateRunUI(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/?error="+url.QueryEscape("invalid form"), http.StatusSeeOther)
		return
	}
	info, _ := authInfoFrom(r)
	idem := strings.TrimSpace(r.PostFormValue("idempotency_key"))
	if idem == "" {
		idem = security.NewID()
	}
	req := workflow.CreateRunRequest{
		Template:            strings.TrimSpace(r.PostFormValue("template")),
		Mode:                templateMode(s.Engine, r.PostFormValue("template")),
		MailboxProvider:     strings.TrimSpace(r.PostFormValue("mailbox_provider")),
		IdentityProvider:    strings.TrimSpace(r.PostFormValue("identity_provider")),
		OAuthProvider:       strings.TrimSpace(r.PostFormValue("oauth_provider")),
		EmailProvider:       strings.TrimSpace(r.PostFormValue("email_provider")),
		CredentialProvider:  strings.TrimSpace(r.PostFormValue("credential_provider")),
		SandboxProvider:     strings.TrimSpace(r.PostFormValue("sandbox_provider")),
		ExistingIdentityRef: strings.TrimSpace(r.PostFormValue("existing_identity_ref")),
	}
	sum, _, err := s.createRun(r.Context(), req, idem, info.Actor)
	if err != nil {
		http.Redirect(w, r, "/ui/?error="+url.QueryEscape(errs.SummaryOf(err)), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/runs/"+sum.ID, http.StatusSeeOther)
}

func templateMode(e *workflow.Engine, name string) string {
	if t, ok := e.Template(strings.TrimSpace(name)); ok {
		return t.Mode
	}
	return ""
}

func (s *Server) handleResumeUI(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	id := r.PathValue("id")
	if _, err := s.Store.RequestResume(r.Context(), id); err != nil {
		http.Redirect(w, r, "/ui/runs/"+id+"?error="+url.QueryEscape(errs.SummaryOf(err)), http.StatusSeeOther)
		return
	}
	_ = s.Store.Audit(r.Context(), info.Actor, "run.resume", id, nil)
	http.Redirect(w, r, "/ui/runs/"+id+"?notice=resume+requested", http.StatusSeeOther)
}

func (s *Server) handleCancelUI(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	id := r.PathValue("id")
	if _, err := s.Store.RequestCancel(r.Context(), id); err != nil {
		http.Redirect(w, r, "/ui/runs/"+id+"?error="+url.QueryEscape(errs.SummaryOf(err)), http.StatusSeeOther)
		return
	}
	_ = s.Store.Audit(r.Context(), info.Actor, "run.cancel", id, nil)
	http.Redirect(w, r, "/ui/runs/"+id+"?notice=cancellation+requested", http.StatusSeeOther)
}

func (s *Server) handleCSS(w http.ResponseWriter, r *http.Request) {
	raw, err := uiFS.ReadFile("templates/app.css")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(raw)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// handleUISession lets the single-page console bootstrap: who am I, which CSRF token to
// send on mutations, which build and language the server runs.
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

//go:embed all:webdist
var webDistFS embed.FS

// handleSPA serves the Vite build mounted at /ui/app and falls back to index.html so
// client-side routes survive a reload. Assets are embedded, so the binary stays single-file.
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/ui/app")
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
