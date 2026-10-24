// Package boot wires configuration into the provider registry. Every adapter is
// explicitly gated: a disabled provider is registered but marked disabled so
// lookups fail with NOT_AUTHORIZED before any network call.
package boot

import (
	"context"
	"log/slog"

	"github.com/baicie/account-onboarding/internal/config"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/provider/fake"
	"github.com/baicie/account-onboarding/internal/secrets"
)

// Build constructs the provider registry for the given configuration.
func Build(ctx context.Context, cfg config.Config, sec *secrets.Service, log *slog.Logger) (*provider.Registry, error) {
	reg := provider.NewRegistry()

	if cfg.FakeProviders {
		world := fake.NewWorld(sec)
		reg.AddMailbox(fake.NewMailbox(world))
		reg.AddIdentity(fake.NewIdentity(world))
		reg.AddOAuth(fake.NewOAuth(world))
		reg.AddCredential(fake.NewCredential(world))
		reg.AddSandbox(fake.NewSandbox(world))
		reg.AddEmailManager(fake.NewEmailManager(world))
		log.Warn("fake providers enabled: no external calls will be made")
	}

	// Real adapters are wired in R1/R3 once their packages exist. They are
	// constructed only when enabled by config, and each adapter independently
	// re-checks its URL allowlist. See docs/acceptance/R1.md and R3.md.
	if err := buildLive(ctx, cfg, sec, reg, log); err != nil {
		return nil, err
	}
	return reg, nil
}
