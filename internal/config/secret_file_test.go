package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMode(t *testing.T, mode os.FileMode, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSecureSecretFileModes(t *testing.T) {
	for _, tc := range []struct {
		mode os.FileMode
		ok   bool
	}{
		{0o400, true}, {0o600, true},
		{0o644, false}, {0o660, false}, {0o666, false}, {0o640, false},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			p := writeMode(t, tc.mode, "secret-value")
			_, err := readSecureSecretFile(p, "test", secretPolicy{})
			if tc.ok && err != nil {
				t.Fatalf("mode %o rejected: %v", tc.mode, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("mode %o accepted", tc.mode)
			}
			if err != nil && strings.Contains(err.Error(), "secret-value") {
				t.Fatal("error leaked secret value")
			}
		})
	}
}

func TestSecureSecretFilePathSafety(t *testing.T) {
	dir := t.TempDir()
	if _, err := readSecureSecretFile(dir, "test", secretPolicy{}); err == nil {
		t.Fatal("directory accepted as secret file")
	}
	if _, err := readSecureSecretFile(filepath.Join(dir, "missing"), "test", secretPolicy{}); err == nil {
		t.Fatal("missing file accepted")
	}
	empty := writeMode(t, 0o600, "")
	if _, err := readSecureSecretFile(empty, "test", secretPolicy{}); err == nil {
		t.Fatal("empty file accepted")
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, make([]byte, maxSecretFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecureSecretFile(large, "test", secretPolicy{}); err == nil {
		t.Fatal("oversized file accepted")
	}
	target := writeMode(t, 0o600, "target-secret")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecureSecretFile(link, "test", secretPolicy{}); err == nil {
		t.Fatal("symlink accepted; projected secrets must be copied")
	}
}

func TestSecureSecretFileDetectsReplacement(t *testing.T) {
	p := writeMode(t, 0o600, "original")
	other := writeMode(t, 0o600, "different-inode")
	origOpen := openSecretFile
	t.Cleanup(func() { openSecretFile = origOpen })
	openSecretFile = func(string) (*os.File, error) {
		// Open a different owner-only file than the one that was Lstat'ed.
		return os.Open(other)
	}
	if _, err := readSecureSecretFile(p, "test", secretPolicy{}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("replacement not detected: %v", err)
	}
}

func TestAllProviderSecretFilesUseSecureReader(t *testing.T) {
	bad := writeMode(t, 0o644, "leaky")
	cases := []struct {
		name  string
		build func(*Config)
	}{
		{"cloudflare-site-password", func(c *Config) { c.Cloudflare.SitePasswordFile = bad }},
		{"keycloak-admin-password", func(c *Config) { c.Keycloak.AdminPasswordFile = bad }},
		{"keycloak-admin-client-secret", func(c *Config) { c.Keycloak.AdminClientSecretFile = bad }},
		{"keycloak-client-secret", func(c *Config) { c.Keycloak.ClientSecretFile = bad }},
		{"github-token", func(c *Config) { c.GitHub.TokenFile = bad }},
		{"workbuddy-token", func(c *Config) { c.WorkBuddy.TokenFile = bad }},
		{"cloudflare-admin-password", func(c *Config) { c.Cloudflare.AdminPasswordFile = bad }},
		{"oidc-client-secret", func(c *Config) { c.OIDC.ClientSecretFile = bad }},
		{"admin-token", func(c *Config) { c.AdminTokenFile = bad }},
		{"readonly-token", func(c *Config) { c.ReadOnlyTokenFile = bad }},
		{"master-key", func(c *Config) { c.MasterKeyFile = bad }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			tc.build(c)
			if err := c.resolveSecrets(); err == nil || !strings.Contains(err.Error(), "permissions") {
				t.Fatalf("field did not use secure reader: %v", err)
			}
		})
	}
}
