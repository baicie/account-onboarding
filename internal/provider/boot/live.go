package boot

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/baicie/account-onboarding/internal/config"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/provider/github"
	"github.com/baicie/account-onboarding/internal/provider/keycloak"
	"github.com/baicie/account-onboarding/internal/provider/mailbox/cloudflare"
	"github.com/baicie/account-onboarding/internal/provider/mailbox/mailpit"
	"github.com/baicie/account-onboarding/internal/provider/workbuddy"
	"github.com/baicie/account-onboarding/internal/secrets"
	"github.com/baicie/account-onboarding/internal/security"
)

// buildLive wires the real adapters. Each constructor re-validates its own URL
// allowlist and gating flag; a disabled adapter is still registered so that
// GET /v1/providers explains the activation gate and lookups fail closed with
// NOT_AUTHORIZED.
func buildLive(ctx context.Context, cfg config.Config, sec *secrets.Service, reg *provider.Registry, log *slog.Logger) error {
	if cfg.Mailpit.Enabled {
		domains := append([]string{cfg.Mailpit.Domain}, cfg.Domains...)
		p, err := mailpit.New(mailpit.Config{
			BaseURL:        cfg.Mailpit.BaseURL,
			Domain:         cfg.Mailpit.Domain,
			AllowedDomains: domains,
			Timeout:        cfg.Mailpit.Timeout,
		})
		if err != nil {
			return fmt.Errorf("mailpit mailbox provider: %w", err)
		}
		reg.AddMailbox(p)
		log.Info("mailpit mailbox provider enabled", "base_url", cfg.Mailpit.BaseURL, "domain", cfg.Mailpit.Domain)
	}

	if cfg.Cloudflare.Enabled {
		p, err := cloudflare.New(cloudflare.Config{
			BaseURL:        cfg.Cloudflare.BaseURL,
			Domain:         cfg.Cloudflare.Domain,
			AllowedDomains: cfg.Domains,
			Timeout:        cfg.Cloudflare.Timeout,
			AdminAuth:      cfg.Cloudflare.AdminPassword,
			SitePassword:   cfg.Cloudflare.SitePassword,
			Secrets:        sec,
			Resolver:       sec,
		})
		if err != nil {
			return fmt.Errorf("cloudflare mailbox provider: %w", err)
		}
		reg.AddMailbox(p)
		log.Info("cloudflare mailbox provider enabled", "base_url", cfg.Cloudflare.BaseURL, "domain", cfg.Cloudflare.Domain)
	}

	// GitHub existing-account adapter. The token is stored as an opaque secret
	// reference; the adapter resolves it per call and never keeps the plaintext.
	if cfg.GitHub.Enabled {
		ref := stableSecretRef("github", "token", cfg.GitHub.Token)
		if err := sec.Put(ctx, ref, "github", cfg.GitHub.Token); err != nil {
			return fmt.Errorf("store github token reference: %w", err)
		}
		adapter, err := github.New(github.Config{
			TokenRef:       ref,
			Timeout:        cfg.GitHub.Timeout,
			AllowedDomains: cfg.Domains,
		}, sec)
		if err != nil {
			return fmt.Errorf("github email provider: %w", err)
		}
		reg.AddEmailManager(adapter)
		log.Info("github existing-account email adapter enabled")
	} else {
		reg.AddEmailManager(github.NewDisabled(
			"GitHub adapter disabled by configuration (ONBOARD_GITHUB_ENABLED=false); existing-account email flow needs an operator-authorized token"))
	}

	// Keycloak is the LOCAL SANDBOX identity fixture. One shared client serves
	// the identity, sandbox-driver and credential-verifier registry roles, and
	// the adapter itself rejects any non-loopback base URL. When disabled we
	// still register an inert identity descriptor so GET /v1/providers explains
	// the activation gate and lookups fail closed with NOT_AUTHORIZED.
	if cfg.Keycloak.Enabled {
		adapter, err := keycloak.New(keycloak.Config{
			BaseURL:           cfg.Keycloak.BaseURL,
			Realm:             cfg.Keycloak.Realm,
			AdminUser:         cfg.Keycloak.AdminUser,
			AdminPassword:     cfg.Keycloak.AdminPassword,
			AdminClientID:     cfg.Keycloak.AdminClientID,
			AdminClientSecret: cfg.Keycloak.AdminClientSecret,
			ClientID:          cfg.Keycloak.ClientID,
			ClientSecret:      cfg.Keycloak.ClientSecret,
			RedirectURI:       cfg.Keycloak.RedirectURI,
			Timeout:           cfg.Keycloak.Timeout,
			// The local fixture serves plain HTTP on 127.0.0.1 only; the
			// adapter rejects every non-loopback host regardless.
			AllowLoopbackHTTP: true,
		}, sec)
		if err != nil {
			return fmt.Errorf("keycloak sandbox provider: %w", err)
		}
		reg.AddIdentity(adapter.Identity())
		reg.AddSandbox(adapter.Sandbox())
		reg.AddCredential(adapter.Credential())
		log.Info("keycloak sandbox provider enabled",
			"base_url", cfg.Keycloak.BaseURL, "realm", cfg.Keycloak.Realm, "client_id", cfg.Keycloak.ClientID)
	} else {
		reg.AddIdentity(keycloak.NewDisabled(
			"Keycloak sandbox adapter disabled by configuration (ONBOARD_KEYCLOAK_ENABLED=false); the sandbox-email-verify-oidc template needs the local Keycloak + Mailpit fixture").Identity())
	}

	// WorkBuddy is permission-gated: the real adapter is only constructed when
	// the operator explicitly enables it and acknowledges written permission.
	if cfg.WorkBuddy.Enabled {
		tokenRef := ""
		if cfg.WorkBuddy.Token != "" {
			tokenRef = stableSecretRef("workbuddy", "gateway", cfg.WorkBuddy.Token)
			if err := sec.Put(ctx, tokenRef, "workbuddy", cfg.WorkBuddy.Token); err != nil {
				return fmt.Errorf("store workbuddy gateway token reference: %w", err)
			}
		}
		adapter, err := workbuddy.New(workbuddy.Config{
			Enabled:         true,
			BaseURL:         cfg.WorkBuddy.BaseURL,
			GatewayTokenRef: tokenRef,
			Realm:           cfg.WorkBuddy.Realm,
			Timeout:         cfg.WorkBuddy.Timeout,
		}, sec)
		if err != nil {
			return fmt.Errorf("workbuddy oauth provider: %w", err)
		}
		reg.AddOAuth(adapter)
		log.Warn("workbuddy live adapter enabled with permission acknowledgement", "ack", cfg.WorkBuddy.PermissionAck)
	} else {
		reg.AddOAuth(workbuddy.NewDisabled(workbuddy.DefaultRestrictionReason))
	}
	return nil
}

// stableSecretRef derives a deterministic reference so restarting the service
// upserts the same secret row instead of accumulating new ones.
func stableSecretRef(kind, label, value string) string {
	id := security.HMACHex([]byte("account-onboarding-boot"), []byte(kind+"|"+label+"|"+value))
	return secrets.Ref(kind, id[:32])
}
