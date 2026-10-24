package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/provider"
)

// TestLiveSmokeCloudflare is the optional, explicitly opt-in live smoke test
// from docs/11-testing-acceptance.md. It performs REAL calls against the
// configured worker, including ONE address creation. It is skipped by default
// and must never be run from CI without an operator decision.
//
// Required environment:
//
//	ONBOARD_CLOUDFLARE_LIVE=1
//	ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE=/path/to/admin-password-file
//
// Optional environment:
//
//	ONBOARD_CLOUDFLARE_SITE_PASSWORD_FILE=/path/to/site-password-file
//	ONBOARD_CLOUDFLARE_BASE_URL=https://api.moonbyte.top
//	ONBOARD_CLOUDFLARE_DOMAIN=moonbyte.top
//
// Credentials are read from files (chmod 600) and are never printed, echoed in
// an assertion message or required on the command line. The created address is
// intentionally NOT deleted: docs/08 leaves lifecycle cleanup to the operator's
// worker cleanup policy, and real long-lived addresses are never auto-deleted.
//
// This test does not send mail. End-to-end delivery (SMTP sender -> worker ->
// parsed_mails) requires a separate authorized SMTP relay and remains a manual
// step documented in docs/acceptance/R1.md.
func TestLiveSmokeCloudflare(t *testing.T) {
	if os.Getenv("ONBOARD_CLOUDFLARE_LIVE") != "1" {
		t.Skip("live Cloudflare smoke is opt-in; set ONBOARD_CLOUDFLARE_LIVE=1 and credential file paths (see docs/acceptance/R1.md)")
	}
	adminFile := strings.TrimSpace(os.Getenv("ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE"))
	if adminFile == "" {
		t.Skip("ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE is not set; refusing to run live smoke without a credential file")
	}
	admin, err := readLiveSecretFile(adminFile)
	if err != nil {
		t.Fatalf("could not read admin credential file: %v", err)
	}
	site := ""
	if siteFile := strings.TrimSpace(os.Getenv("ONBOARD_CLOUDFLARE_SITE_PASSWORD_FILE")); siteFile != "" {
		site, err = readLiveSecretFile(siteFile)
		if err != nil {
			t.Fatalf("could not read site credential file: %v", err)
		}
	}

	domain := strings.TrimSpace(os.Getenv("ONBOARD_CLOUDFLARE_DOMAIN"))
	if domain == "" {
		domain = DefaultDomain
	}
	secrets := newFakeSecrets()
	cfg := Config{
		BaseURL:      strings.TrimSpace(os.Getenv("ONBOARD_CLOUDFLARE_BASE_URL")),
		AdminAuth:    admin,
		SitePassword: site,
		Domain:       domain,
		Timeout:      20 * time.Second,
		Secrets:      secrets,
		Resolver:     secrets,
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("live configuration rejected: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	name := fmt.Sprintf("onboard-smoke-%d", time.Now().UnixNano()%10_000_000)
	mb, err := p.CreateAddress(ctx, provider.CreateAddressRequest{Name: name, Domain: domain})
	if err != nil {
		t.Fatalf("live CreateAddress failed: %v", err)
	}
	t.Logf("live smoke: created address for domain %s with provider id %s", domain, mb.ID)
	if mb.SecretRef == "" {
		t.Fatal("live smoke: no secret reference was returned")
	}
	if _, ok := secrets.get(mb.SecretRef); !ok {
		t.Fatal("live smoke: address credential was not stored in the secret store")
	}

	msgs, err := p.ListMessages(ctx, mb, time.Time{})
	if err != nil {
		t.Fatalf("live ListMessages failed: %v", err)
	}
	t.Logf("live smoke: parsed_mails returned %d message(s)", len(msgs))
	for _, msg := range msgs {
		if msg.ID == "" {
			t.Fatalf("live smoke: message without an identifier")
		}
		if _, err := p.GetMessage(ctx, mb, msg.ID); err != nil {
			t.Fatalf("live GetMessage failed for provider message %s: %v", msg.ID, err)
		}
	}
}

// readLiveSecretFile reads a credential from a file and removes the trailing
// newline an editor may add. The value is never included in an error.
func readLiveSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("credential file is unreadable")
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("credential file is empty")
	}
	return value, nil
}
