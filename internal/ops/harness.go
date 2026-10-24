package ops

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"time"

	"github.com/baicie/account-onboarding/internal/api"
	"github.com/baicie/account-onboarding/internal/config"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/provider/fake"
	"github.com/baicie/account-onboarding/internal/secrets"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

// StackOptions configures an in-process, fully offline account-onboarding stack.
type StackOptions struct {
	// Dir hosts the SQLite database. The caller owns it.
	Dir string
	// AdminToken defaults to a random test token in Dir-local tests.
	AdminToken string
	// MasterKey defaults to 32 random bytes. Tests may pin it to decrypt.
	MasterKey []byte
	// MaxConcurrency defaults to 2 (engine cap: 5).
	MaxConcurrency int
	TickInterval   time.Duration
	LeaseTTL       time.Duration
	StepTimeout    time.Duration
	PollInterval   time.Duration
	MaxAttempts    int
	RunDeadline    time.Duration
	// LogWriter receives redacted JSON logs. Defaults to io.Discard.
	LogWriter io.Writer
	// AddProviders registers extra adapters (for example a disabled stub used by
	// the HTTP authorization tests) after the fake set.
	AddProviders func(*provider.Registry)
	// ReadOnlyToken enables the minimal viewer role (GET only) for HTTP audit
	// tests. Empty disables it.
	ReadOnlyToken string
	// RateLimitPerMinute / LoginRateLimitPerMinute override the per-client-IP
	// fixed-window limits. Zero means "high test default" so unrelated tests do
	// not trip; HTTP audit tests set explicit low values.
	RateLimitPerMinute      int
	LoginRateLimitPerMinute int
	// TrustProxyHeaders enables X-Forwarded-* handling (Secure cookie test).
	TrustProxyHeaders bool
}

// Stack is an offline account-onboarding service instance: real api.Server,
// real storage, real workflow engine, fake providers and a loopback httptest
// server. No external network call is possible.
type Stack struct {
	Cfg        config.Config
	Dir        string
	DBPath     string
	Store      *storage.Store
	Engine     *workflow.Engine
	Registry   *provider.Registry
	Secrets    *secrets.Service
	Signer     *security.SessionSigner
	Redactor   *security.Redactor
	KeyRing    *security.KeyRing
	MasterKey  []byte
	AdminToken string
	Log        *slog.Logger
	Server     *api.Server

	httpServer *httptest.Server
	cancel     context.CancelFunc
	started    bool
}

// NewStack builds the offline stack. The caller must call Close.
func NewStack(opts StackOptions) (*Stack, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("ops stack: Dir is required")
	}
	master := opts.MasterKey
	if len(master) == 0 {
		master = make([]byte, 32)
		if _, err := rand.Read(master); err != nil {
			return nil, fmt.Errorf("ops stack: generate master key: %w", err)
		}
	}
	if len(master) != 32 {
		return nil, fmt.Errorf("ops stack: master key must be 32 bytes")
	}
	adminToken := opts.AdminToken
	if adminToken == "" {
		adminToken = "ops-test-" + security.RandomToken(18)
	}
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = 2
	}
	if opts.TickInterval <= 0 {
		opts.TickInterval = 10 * time.Millisecond
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 30 * time.Second
	}
	if opts.StepTimeout <= 0 {
		opts.StepTimeout = 5 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 20 * time.Millisecond
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 4
	}
	if opts.RunDeadline <= 0 {
		opts.RunDeadline = 2 * time.Minute
	}
	if opts.LogWriter == nil {
		opts.LogWriter = io.Discard
	}

	redactor := security.NewRedactor(adminToken, opts.ReadOnlyToken)
	log := newRedactedLogger(opts.LogWriter, redactor)

	cfg := config.Default()
	cfg.DBPath = filepath.Join(opts.Dir, "state.sqlite")
	cfg.Mode = config.ModeSandbox
	cfg.Listen = "127.0.0.1:0"
	cfg.AdminToken = adminToken
	cfg.MasterKey = master
	cfg.FakeProviders = true
	cfg.MaxConcurrency = opts.MaxConcurrency
	cfg.SessionTTL = time.Hour
	cfg.LeaseTTL = opts.LeaseTTL
	cfg.TickInterval = opts.TickInterval
	cfg.StepTimeout = opts.StepTimeout
	cfg.PollInterval = opts.PollInterval
	cfg.MaxStepAttempts = opts.MaxAttempts
	cfg.RunDeadline = opts.RunDeadline
	cfg.ReadOnlyToken = opts.ReadOnlyToken
	cfg.TrustProxyHeaders = opts.TrustProxyHeaders
	cfg.RateLimitPerMinute = opts.RateLimitPerMinute
	cfg.LoginRateLimitPerMinute = opts.LoginRateLimitPerMinute
	if cfg.RateLimitPerMinute == 0 {
		cfg.RateLimitPerMinute = 100000
	}
	if cfg.LoginRateLimitPerMinute == 0 {
		cfg.LoginRateLimitPerMinute = 100000
	}

	store, err := storage.Open(context.Background(), storage.Options{
		Path:     cfg.DBPath,
		HMACKey:  security.DeriveKey(master, "storage-hmac"),
		Redactor: redactor,
		TransitionValidator: func(from, to string) error {
			return state.CanTransition(state.RunState(from), state.RunState(to))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ops stack: open store: %w", err)
	}
	ring, err := security.NewKeyRing(master)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("ops stack: keyring: %w", err)
	}
	sec := secrets.New(store, security.NewBox(ring))
	world := fake.NewWorld(sec)
	reg := provider.NewRegistry()
	reg.AddMailbox(fake.NewMailbox(world))
	reg.AddIdentity(fake.NewIdentity(world))
	reg.AddOAuth(fake.NewOAuth(world))
	reg.AddCredential(fake.NewCredential(world))
	reg.AddSandbox(fake.NewSandbox(world))
	reg.AddEmailManager(fake.NewEmailManager(world))
	if opts.AddProviders != nil {
		opts.AddProviders(reg)
	}

	engine := workflow.New(store, reg, sec, log, workflow.Config{
		MaxConcurrency:  opts.MaxConcurrency,
		TickInterval:    opts.TickInterval,
		LeaseTTL:        opts.LeaseTTL,
		StepTimeout:     opts.StepTimeout,
		PollInterval:    opts.PollInterval,
		MaxAttempts:     opts.MaxAttempts,
		RunDeadline:     opts.RunDeadline,
		Domains:         []string{"moonbyte.test"},
		SenderAllowlist: []string{"no-reply", "idp.fake.test"},
		FingerprintKey:  security.DeriveKey(master, "run-fingerprint"),
		OIDCScopes:      []string{"openid", "email", "profile"},
		OIDCRedirectURI: "http://127.0.0.1:8788/oidc/callback",
	})
	signer := security.NewSessionSigner(master, int64(cfg.SessionTTL.Seconds()))
	srv, err := api.New(cfg, store, engine, reg, sec, log, signer, redactor)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("ops stack: build api server: %w", err)
	}
	return &Stack{
		Cfg:        cfg,
		Dir:        opts.Dir,
		DBPath:     cfg.DBPath,
		Store:      store,
		Engine:     engine,
		Registry:   reg,
		Secrets:    sec,
		Signer:     signer,
		Redactor:   redactor,
		KeyRing:    ring,
		MasterKey:  master,
		AdminToken: adminToken,
		Log:        log,
		Server:     srv,
	}, nil
}

// newRedactedLogger builds a JSON slog logger backed by the redacting handler.
func newRedactedLogger(w io.Writer, redactor *security.Redactor) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(&redactedHandler{next: base, redactor: redactor})
}

// redactedHandler mirrors internal/logging without exporting test-only helpers.
type redactedHandler struct {
	next     slog.Handler
	redactor *security.Redactor
}

func (h *redactedHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.next.Enabled(ctx, lvl)
}

func (h *redactedHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.redactor.Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactedHandler) redactAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.redactor.Redact(a.Value.String()))
	case slog.KindGroup:
		group := a.Value.Group()
		attrs := make([]any, 0, len(group))
		for _, g := range group {
			attrs = append(attrs, h.redactAttr(g))
		}
		return slog.Group(a.Key, attrs...)
	default:
		return a
	}
}

func (h *redactedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		redacted = append(redacted, h.redactAttr(a))
	}
	return &redactedHandler{next: h.next.WithAttrs(redacted), redactor: h.redactor}
}

func (h *redactedHandler) WithGroup(name string) slog.Handler {
	return &redactedHandler{next: h.next.WithGroup(name), redactor: h.redactor}
}

// Start launches the worker pool bound to ctx.
func (s *Stack) Start(ctx context.Context) {
	if s.started {
		return
	}
	engineCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.Engine.Start(engineCtx)
	s.started = true
}

// HTTPBaseURL starts (once) the loopback httptest server and returns its URL.
func (s *Stack) HTTPBaseURL() string {
	if s.httpServer == nil {
		s.httpServer = httptest.NewServer(s.Server.Handler())
	}
	return s.httpServer.URL
}

// Close stops the HTTP server, the engine and storage.
func (s *Stack) Close() {
	if s.httpServer != nil {
		s.httpServer.Close()
		s.httpServer = nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.started {
		s.Engine.Stop()
		s.started = false
	}
	_ = s.Store.Close()
}

// BearerAuth returns the Authorization header value for the test admin token.
func (s *Stack) BearerAuth() string { return "Bearer " + s.AdminToken }

// NewSessionCookie issues a signed browser session plus its CSRF token.
func (s *Stack) NewSessionCookie(now time.Time) (cookieValue, csrf string, err error) {
	return s.Signer.Issue(now.Unix())
}
