package config_test

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baicie/account-onboarding/internal/config"
)

func base() config.Config {
	c := config.Default()
	c.MasterKey = bytes.Repeat([]byte{1}, 32)
	return c
}

func TestDefaultConfigValidWithChecks(t *testing.T) {
	c := base()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	if c.Cloudflare.Enabled || c.GitHub.Enabled || c.WorkBuddy.Enabled || c.Keycloak.Enabled || c.Mailpit.Enabled {
		t.Fatal("live providers must be disabled by default")
	}
	if c.FakeProviders {
		t.Fatal("fake providers must be opt-in")
	}
	red := c.Redacted()
	if red["admin_token_set"] != false {
		t.Fatalf("unexpected redacted config: %+v", red)
	}
}

func TestValidateRejectsPublicListenWithoutFlag(t *testing.T) {
	c := base()
	c.Listen = "0.0.0.0:8788"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("expected loopback rejection, got %v", err)
	}
	c.AllowPublicListen = true
	c.AdminToken = "token"
	if err := c.Validate(); err != nil {
		t.Fatalf("public listen with token rejected: %v", err)
	}
	c.AdminToken = ""
	if err := c.Validate(); err == nil {
		t.Fatal("public listen without admin token must be rejected")
	}
}

func TestValidateRequiresMasterKey(t *testing.T) {
	c := base()
	c.MasterKey = nil
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "MASTER_KEY") {
		t.Fatalf("expected master key requirement, got %v", err)
	}
	c.DevEphemeralKey = true
	if err := c.Validate(); err != nil {
		t.Fatalf("dev ephemeral key should be allowed in sandbox: %v", err)
	}
	c.Mode = config.ModeProduction
	if err := c.Validate(); err == nil {
		t.Fatal("ephemeral key must be rejected in production")
	}
}

func TestCloudflareGate(t *testing.T) {
	c := base()
	c.Cloudflare.Enabled = true
	if err := c.Validate(); err == nil {
		t.Fatal("cloudflare without admin password must be rejected")
	}
	c.Cloudflare.AdminPassword = "secret"
	if err := c.Validate(); err != nil {
		t.Fatalf("valid cloudflare config rejected: %v", err)
	}
	c.Cloudflare.BaseURL = "https://evil.example.com"
	if err := c.Validate(); err == nil {
		t.Fatal("non-allowlisted cloudflare host must be rejected")
	}
}

func TestWorkBuddyGate(t *testing.T) {
	c := base()
	c.WorkBuddy.Enabled = true
	c.WorkBuddy.BaseURL = "http://127.0.0.1:9000"
	c.WorkBuddy.Token = "gw-token"
	if err := c.Validate(); err == nil {
		t.Fatal("workbuddy without permission acknowledgement must be rejected")
	}
	c.WorkBuddy.PermissionAck = "signed-permission-2026-01"
	if err := c.Validate(); err != nil {
		t.Fatalf("workbuddy with ack rejected: %v", err)
	}
	c.WorkBuddy.BaseURL = "https://public.example.com"
	if err := c.Validate(); err == nil {
		t.Fatal("workbuddy public base URL must be rejected")
	}
}

func TestGitHubGate(t *testing.T) {
	c := base()
	c.GitHub.Enabled = true
	if err := c.Validate(); err == nil {
		t.Fatal("github without token must be rejected")
	}
	c.GitHub.Token = "ghp_test"
	if err := c.Validate(); err != nil {
		t.Fatalf("github config rejected: %v", err)
	}
	c.GitHub.BaseURL = "https://api.example.com"
	if err := c.Validate(); err == nil {
		t.Fatal("non-allowlisted github host must be rejected")
	}
}

func TestDecodeMasterKey(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	for name, encoded := range map[string]string{
		"hex":    strings.ToUpper(string(hexOf(key))),
		"base64": base64.StdEncoding.EncodeToString(key),
	} {
		got, err := config.DecodeMasterKey(encoded)
		if err != nil || !bytes.Equal(got, key) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := config.DecodeMasterKey("short"); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestLoadFromEnv(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "admin.token")
	if err := os.WriteFile(tokenPath, []byte("admin-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom([]string{
		"ONBOARD_LISTEN=127.0.0.1:9999",
		"ONBOARD_DB_PATH=" + filepath.Join(dir, "state.sqlite"),
		"ONBOARD_MASTER_KEY_FILE=" + keyPath,
		"ONBOARD_ADMIN_TOKEN_FILE=" + tokenPath,
		"ONBOARD_FAKE_PROVIDERS=true",
		"ONBOARD_MAX_CONCURRENCY=3",
		"ONBOARD_DOMAINS=moonbyte.test,example.test",
		"ONBOARD_POLL_INTERVAL=2s",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" || cfg.MaxConcurrency != 3 || !cfg.FakeProviders {
		t.Fatalf("unexpected config: %+v", cfg.Redacted())
	}
	if cfg.AdminToken != "admin-token-value" {
		t.Fatalf("admin token not loaded: %q", cfg.AdminToken)
	}
	if len(cfg.Domains) != 2 || cfg.Domains[0] != "moonbyte.test" {
		t.Fatalf("domains: %v", cfg.Domains)
	}
	// A secret value must not appear in the log-safe summary.
	for _, v := range cfg.Redacted() {
		if s, ok := v.(string); ok && strings.Contains(s, "admin-token-value") {
			t.Fatal("redacted summary leaked admin token")
		}
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	_, err := config.LoadFrom([]string{"ONBOARD_MAX_CONCURRENCY=99", "ONBOARD_DEV_EPHEMERAL_KEY=1"})
	if err == nil {
		t.Fatal("expected concurrency validation error")
	}
	_, err = config.LoadFrom([]string{"ONBOARD_MODE=invalid", "ONBOARD_DEV_EPHEMERAL_KEY=1"})
	if err == nil {
		t.Fatal("expected mode validation error")
	}
}

func hexOf(b []byte) []byte {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, v := range b {
		out = append(out, digits[v>>4], digits[v&0x0f])
	}
	return out
}

func TestRaw32ByteMasterKeyWithWhitespaceBytes(t *testing.T) {
	dir := t.TempDir()
	// Raw binary keys may contain bytes that TrimSpace would strip; the loader
	// must accept the 32-byte file verbatim.
	key := bytes.Repeat([]byte{' '}, 32)
	copy(key, []byte("k"))
	keyPath := filepath.Join(dir, "raw.key")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom([]string{
		"ONBOARD_MASTER_KEY_FILE=" + keyPath,
		"ONBOARD_DB_PATH=" + filepath.Join(dir, "state.sqlite"),
	})
	if err != nil {
		t.Fatalf("raw 32-byte key rejected: %v", err)
	}
	if !bytes.Equal(cfg.MasterKey, key) {
		t.Fatalf("raw key was altered: %x", cfg.MasterKey)
	}
}
