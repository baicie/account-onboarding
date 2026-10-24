// Command onboardd runs the account-onboarding control plane.
//
// Subcommands:
//
//	onboardd            serve the API and console UI
//	onboardd migrate    apply database migrations and exit
//	onboardd healthcheck  probe /healthz over loopback (Docker HEALTHCHECK)
//	onboardd version    print the version
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/baicie/account-onboarding/internal/api"
	"github.com/baicie/account-onboarding/internal/config"
	"github.com/baicie/account-onboarding/internal/logging"
	"github.com/baicie/account-onboarding/internal/provider/boot"
	"github.com/baicie/account-onboarding/internal/secrets"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "version":
			fmt.Println("onboardd " + api.Version + " commit " + api.Commit)
			return
		case "configcheck":
			if _, err := config.Load(); err != nil {
				fmt.Fprintln(os.Stderr, "configcheck failed:", err)
				os.Exit(1)
			}
			fmt.Println("config ok")
			return
		case "migrate":
			if err := runMigrate(); err != nil {
				fmt.Fprintln(os.Stderr, "migrate failed:", err)
				os.Exit(1)
			}
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
			os.Exit(2)
		}
	}
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func buildRuntime() (config.Config, *security.Redactor, *security.KeyRing, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, nil, nil, err
	}
	redactor := security.NewRedactor(
		cfg.AdminToken,
		cfg.Cloudflare.AdminPassword,
		cfg.Cloudflare.SitePassword,
		cfg.Keycloak.AdminPassword,
		cfg.Keycloak.ClientSecret,
		cfg.GitHub.Token,
		cfg.WorkBuddy.Token,
		cfg.OIDC.ClientSecret,
	)
	master := cfg.MasterKey
	if len(master) != 32 {
		if !cfg.DevEphemeralKey {
			return cfg, redactor, nil, errors.New("master key missing; refuse to start")
		}
		master = make([]byte, 32)
		if _, err := rand.Read(master); err != nil {
			return cfg, redactor, nil, err
		}
		fmt.Fprintln(os.Stderr, "WARNING: using an ephemeral master key (ONBOARD_DEV_EPHEMERAL_KEY=1); stored secrets will not survive a restart")
		cfg.MasterKey = master
	}
	ring, err := security.NewKeyRing(master)
	if err != nil {
		return cfg, redactor, nil, err
	}
	return cfg, redactor, ring, nil
}

func serve() error {
	cfg, redactor, ring, err := buildRuntime()
	if err != nil {
		return err
	}
	log := logging.New(cfg.LogLevel, redactor)
	log.Info("starting onboardd", "config", cfg.Redacted())
	if cfg.AdminToken == "" {
		log.Warn("no admin token configured: every authenticated endpoint will reject requests")
	}
	if cfg.WorkBuddy.Enabled {
		log.Warn("WorkBuddy live adapter is enabled with a permission acknowledgement", "ack", cfg.WorkBuddy.PermissionAck)
	}

	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	hmacKey := security.DeriveKey(cfg.MasterKey, "storage-hmac")
	store, err := storage.Open(ctx, storage.Options{
		Path:                cfg.DBPath,
		HMACKey:             hmacKey,
		Redactor:            redactor,
		TransitionValidator: func(from, to string) error { return state.CanTransition(state.RunState(from), state.RunState(to)) },
	})
	if err != nil {
		return err
	}
	defer store.Close()
	health, err := store.Health(ctx)
	if err != nil {
		return err
	}
	log.Info("database ready", "schema_version", health.SchemaVersion, "wal", health.WALMode, "path", cfg.DBPath)

	sec := secrets.New(store, security.NewBox(ring))
	registry, err := boot.Build(ctx, cfg, sec, log)
	if err != nil {
		return err
	}
	engine := workflow.New(store, registry, sec, log, workflow.Config{
		MaxConcurrency:  cfg.MaxConcurrency,
		TickInterval:    cfg.TickInterval,
		LeaseTTL:        cfg.LeaseTTL,
		StepTimeout:     cfg.StepTimeout,
		PollInterval:    cfg.PollInterval,
		MaxAttempts:     cfg.MaxStepAttempts,
		RunDeadline:     cfg.RunDeadline,
		Domains:         cfg.Domains,
		SenderAllowlist: cfg.SenderAllowlist,
		FingerprintKey:  security.DeriveKey(cfg.MasterKey, "run-fingerprint"),
		OIDCScopes:      cfg.OIDC.Scopes,
		OIDCRedirectURI: cfg.OIDC.RedirectURI,
	})
	signer := security.NewSessionSigner(cfg.MasterKey, int64(cfg.SessionTTL.Seconds()))
	srv, err := api.New(cfg, store, engine, registry, sec, log, signer, redactor)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    32 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	engine.Start(ctx)
	_ = store.Audit(ctx, "system", "service.start", "", map[string]any{"version": api.Version})

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		engine.Stop()
		return err
	case <-ctx.Done():
		log.Info("shutdown requested")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown failed", "error", err)
	}
	engine.Stop()
	_ = store.Audit(context.Background(), "system", "service.stop", "", nil)
	log.Info("stopped")
	return nil
}

func runMigrate() error {
	cfg, redactor, ring, err := buildRuntime()
	if err != nil {
		return err
	}
	_ = logging.New(cfg.LogLevel, redactor)
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	store, err := storage.Open(context.Background(), storage.Options{
		Path:    cfg.DBPath,
		HMACKey: security.DeriveKey(cfg.MasterKey, "storage-hmac"),
	})
	if err != nil {
		return err
	}
	defer store.Close()
	health, err := store.Health(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("migrations applied: schema_version=%d wal=%s writable=%t\n", health.SchemaVersion, health.WALMode, health.Writable)
	_ = ring
	return nil
}

func healthcheck() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck config:", err)
		return 1
	}
	token := cfg.AdminToken
	if token == "" {
		token = cfg.ReadOnlyToken
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "healthcheck: no admin/readonly token configured; /readyz requires authentication")
		return 1
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: invalid ONBOARD_LISTEN:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := probeReadiness(ctx, "http://"+net.JoinHostPort(host, port)+"/readyz", token); err != nil {
		// Never include the token or response body in the diagnostic.
		fmt.Fprintln(os.Stderr, "healthcheck not ready:", err)
		return 1
	}
	return 0
}

// probeReadiness checks the authenticated /readyz endpoint. Docker HEALTHCHECK
// uses this so the container is only healthy when the database, schema,
// revocation table, WAL mode and master key are all usable, not merely when the
// HTTP listener accepts connections.
func probeReadiness(ctx context.Context, rawURL, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return errors.New("invalid readiness URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("readiness request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness status %d", resp.StatusCode)
	}
	return nil
}
