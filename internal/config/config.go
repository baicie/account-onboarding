// Package config loads and validates the service configuration.
//
// Security invariants enforced here:
//   - live third-party providers are disabled by default;
//   - enabled live providers require an allowlisted base URL and a secret file;
//   - the real WorkBuddy adapter additionally requires an explicit permission
//     acknowledgement reference, because platform permission is unknown;
//   - the listen address defaults to loopback and public binding is opt-in;
//   - secrets are read from files, never from logged values.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Mode is the operating mode of the service.
type Mode string

const (
	ModeSandbox    Mode = "sandbox"
	ModeProduction Mode = "production"
)

// CloudflareConfig configures the Cloudflare Temp Email adapter.
type CloudflareConfig struct {
	Enabled           bool
	BaseURL           string
	Domain            string
	AdminPassword     string
	SitePassword      string
	AdminPasswordFile string
	SitePasswordFile  string
	// AllowedHosts extends the built-in host allowlist so an operator can point the
	// adapter at a self-hosted cloudflare_temp_email deployment.
	AllowedHosts []string
	Timeout      time.Duration
}

// MailpitConfig configures the local Mailpit adapter (sandbox only).
type MailpitConfig struct {
	Enabled bool
	BaseURL string
	Domain  string
	Timeout time.Duration
}

// KeycloakConfig configures the sandbox Keycloak adapter.
type KeycloakConfig struct {
	Enabled               bool
	BaseURL               string
	Realm                 string
	AdminUser             string
	AdminPassword         string
	AdminPasswordFile     string
	AdminClientID         string
	AdminClientSecret     string
	AdminClientSecretFile string
	ClientID              string
	ClientSecret          string
	ClientSecretFile      string
	RedirectURI           string
	Timeout               time.Duration
}

// GitHubConfig configures the official GitHub existing-account adapter.
type GitHubConfig struct {
	Enabled   bool
	BaseURL   string
	Token     string
	TokenFile string
	Timeout   time.Duration
	// AllowedHosts extends the built-in host allowlist (for GitHub Enterprise).
	AllowedHosts []string
}

// WorkBuddyConfig configures the permission-gated existing gateway adapter.
type WorkBuddyConfig struct {
	Enabled       bool
	BaseURL       string
	Realm         string
	Token         string
	TokenFile     string
	PermissionAck string
	Timeout       time.Duration
	// AllowRemote permits a non-loopback gateway; AllowedHosts lists the hosts that
	// opt-in accepts. The default keeps the adapter on a private network.
	AllowRemote  bool
	AllowedHosts []string
}

// OIDCConfig configures the generic sandbox OIDC test client.
type OIDCConfig struct {
	Enabled          bool
	Issuer           string
	ClientID         string
	ClientSecret     string
	ClientSecretFile string
	RedirectURI      string
	Scopes           []string
	Timeout          time.Duration
}

// Config is the fully resolved runtime configuration.
type Config struct {
	Listen         string
	DBPath         string
	Mode           Mode
	MaxConcurrency int
	LogLevel       string
	AdminToken     string
	AdminTokenFile string
	// ReadOnlyToken grants viewer access (GET endpoints only). Optional.
	ReadOnlyToken     string
	ReadOnlyTokenFile string
	MasterKey         []byte
	MasterKeyFile     string
	SessionTTL        time.Duration
	RunDeadline       time.Duration
	StepTimeout       time.Duration
	LeaseTTL          time.Duration
	PollInterval      time.Duration
	TickInterval      time.Duration
	MaxStepAttempts   int
	AllowedOrigins    []string
	UILang            string
	Domains           []string
	SenderAllowlist   []string
	AllowPublicListen bool
	// TrustProxyHeaders enables X-Forwarded-Proto handling for Secure cookies
	// when the service runs behind an authenticated TLS reverse proxy.
	TrustProxyHeaders bool
	// RateLimitPerMinute bounds general control-plane requests per client IP.
	RateLimitPerMinute int
	// LoginRateLimitPerMinute bounds console login attempts per client IP.
	LoginRateLimitPerMinute int
	// MailSnapshot keeps the sender/subject/link of each received verification mail so
	// the console can show it. Off by default; bodies are never stored either way.
	MailSnapshot bool
	// DevEphemeralKey allows running without a master key file (fake providers
	// only, local development). Secrets become undecryptable after restart, so
	// this is refused in production mode.
	DevEphemeralKey bool

	Cloudflare CloudflareConfig
	Mailpit    MailpitConfig
	Keycloak   KeycloakConfig
	GitHub     GitHubConfig
	WorkBuddy  WorkBuddyConfig
	OIDC       OIDCConfig

	// FakeProviders enables the R0 in-memory providers. It is the only way to
	// get a fully working control plane without any external dependency.
	FakeProviders bool
}

// Default returns the safe baseline configuration.
func Default() Config {
	return Config{
		Listen:                  "127.0.0.1:8788",
		DBPath:                  "./data/state.sqlite",
		Mode:                    ModeSandbox,
		MaxConcurrency:          2,
		LogLevel:                "info",
		SessionTTL:              12 * time.Hour,
		RunDeadline:             30 * time.Minute,
		StepTimeout:             30 * time.Second,
		LeaseTTL:                90 * time.Second,
		PollInterval:            5 * time.Second,
		TickInterval:            250 * time.Millisecond,
		MaxStepAttempts:         8,
		RateLimitPerMinute:      300,
		LoginRateLimitPerMinute: 10,
		Domains:                 []string{"moonbyte.top"},
		SenderAllowlist:         []string{"moonbyte.test", "idp.fake.test", "github.com", "noreply.github.com", "accounts.google.com"},
		AllowedOrigins:          nil,
		UILang:                  "zh-CN",
		Cloudflare: CloudflareConfig{
			Enabled: false,
			BaseURL: "https://api.moonbyte.top",
			Domain:  "moonbyte.top",
			Timeout: 20 * time.Second,
		},
		Mailpit: MailpitConfig{
			Enabled: false,
			BaseURL: "http://127.0.0.1:8025",
			Domain:  "moonbyte.test",
			Timeout: 10 * time.Second,
		},
		Keycloak: KeycloakConfig{
			Enabled:     false,
			BaseURL:     "http://127.0.0.1:8180",
			Realm:       "moonbyte-test",
			AdminUser:   "admin",
			ClientID:    "onboarding-test",
			RedirectURI: "http://127.0.0.1:8788/oidc/callback",
			Timeout:     20 * time.Second,
		},
		GitHub: GitHubConfig{
			Enabled: false,
			BaseURL: "https://api.github.com",
			Timeout: 20 * time.Second,
		},
		WorkBuddy: WorkBuddyConfig{
			Enabled: false,
			Realm:   "global",
			Timeout: 20 * time.Second,
		},
		OIDC: OIDCConfig{
			Enabled:     false,
			Issuer:      "http://127.0.0.1:8180/realms/moonbyte-test",
			ClientID:    "onboarding-test",
			RedirectURI: "http://127.0.0.1:8788/oidc/callback",
			Scopes:      []string{"openid", "email", "profile"},
			Timeout:     20 * time.Second,
		},
	}
}

// Load reads configuration from the environment, applying defaults and
// validating the result. Secret file contents are read into memory.
func Load() (Config, error) {
	return LoadFrom(os.Environ())
}

// LoadFrom is the testable loader.
func LoadFrom(environ []string) (Config, error) {
	cfg := Default()
	env := map[string]string{}
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	get := func(key string) string { return strings.TrimSpace(env[key]) }
	getBool := func(key string, def bool) (bool, error) {
		v := get(key)
		if v == "" {
			return def, nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return def, fmt.Errorf("%s: %w", key, err)
		}
		return b, nil
	}
	getInt := func(key string, def int) (int, error) {
		v := get(key)
		if v == "" {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return def, fmt.Errorf("%s: %w", key, err)
		}
		return n, nil
	}
	getDur := func(key string, def time.Duration) (time.Duration, error) {
		v := get(key)
		if v == "" {
			return def, nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return def, fmt.Errorf("%s: %w", key, err)
		}
		return d, nil
	}

	if v := get("ONBOARD_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := get("ONBOARD_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := strings.ToLower(get("ONBOARD_MODE")); v != "" {
		cfg.Mode = Mode(v)
	}
	if v := get("ONBOARD_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	var err error
	if cfg.MaxConcurrency, err = getInt("ONBOARD_MAX_CONCURRENCY", cfg.MaxConcurrency); err != nil {
		return cfg, err
	}
	if cfg.SessionTTL, err = getDur("ONBOARD_SESSION_TTL", cfg.SessionTTL); err != nil {
		return cfg, err
	}
	if cfg.RunDeadline, err = getDur("ONBOARD_RUN_DEADLINE", cfg.RunDeadline); err != nil {
		return cfg, err
	}
	if cfg.StepTimeout, err = getDur("ONBOARD_STEP_TIMEOUT", cfg.StepTimeout); err != nil {
		return cfg, err
	}
	if cfg.LeaseTTL, err = getDur("ONBOARD_LEASE_TTL", cfg.LeaseTTL); err != nil {
		return cfg, err
	}
	if cfg.PollInterval, err = getDur("ONBOARD_POLL_INTERVAL", cfg.PollInterval); err != nil {
		return cfg, err
	}
	if cfg.TickInterval, err = getDur("ONBOARD_TICK_INTERVAL", cfg.TickInterval); err != nil {
		return cfg, err
	}
	if cfg.MaxStepAttempts, err = getInt("ONBOARD_MAX_STEP_ATTEMPTS", cfg.MaxStepAttempts); err != nil {
		return cfg, err
	}
	if cfg.FakeProviders, err = getBool("ONBOARD_FAKE_PROVIDERS", cfg.FakeProviders); err != nil {
		return cfg, err
	}
	if cfg.AllowPublicListen, err = getBool("ONBOARD_ALLOW_PUBLIC_LISTEN", cfg.AllowPublicListen); err != nil {
		return cfg, err
	}
	if cfg.TrustProxyHeaders, err = getBool("ONBOARD_TRUST_PROXY_HEADERS", cfg.TrustProxyHeaders); err != nil {
		return cfg, err
	}
	if cfg.RateLimitPerMinute, err = getInt("ONBOARD_RATE_LIMIT_PER_MINUTE", cfg.RateLimitPerMinute); err != nil {
		return cfg, err
	}
	if cfg.MailSnapshot, err = getBool("ONBOARD_MAIL_SNAPSHOT", cfg.MailSnapshot); err != nil {
		return cfg, err
	}
	if cfg.LoginRateLimitPerMinute, err = getInt("ONBOARD_LOGIN_RATE_LIMIT_PER_MINUTE", cfg.LoginRateLimitPerMinute); err != nil {
		return cfg, err
	}
	if cfg.DevEphemeralKey, err = getBool("ONBOARD_DEV_EPHEMERAL_KEY", cfg.DevEphemeralKey); err != nil {
		return cfg, err
	}

	cfg.AdminTokenFile = get("ONBOARD_ADMIN_TOKEN_FILE")
	cfg.ReadOnlyTokenFile = get("ONBOARD_READONLY_TOKEN_FILE")
	cfg.MasterKeyFile = get("ONBOARD_MASTER_KEY_FILE")
	cfg.AdminToken = get("ONBOARD_ADMIN_TOKEN")
	cfg.ReadOnlyToken = get("ONBOARD_READONLY_TOKEN")

	if v := get("ONBOARD_DOMAINS"); v != "" {
		cfg.Domains = splitCSV(v)
	}
	if v := get("ONBOARD_SENDER_ALLOWLIST"); v != "" {
		cfg.SenderAllowlist = splitCSV(v)
	}
	if v := get("ONBOARD_UI_LANG"); v != "" {
		cfg.UILang = v
	}
	if v := get("ONBOARD_ALLOWED_ORIGINS"); v != "" {
		cfg.AllowedOrigins = splitCSV(v)
	}

	cfg.Cloudflare.BaseURL = firstNonEmpty(get("ONBOARD_CLOUDFLARE_BASE_URL"), cfg.Cloudflare.BaseURL)
	cfg.Cloudflare.Domain = firstNonEmpty(get("ONBOARD_CLOUDFLARE_DOMAIN"), cfg.Cloudflare.Domain)
	cfg.Cloudflare.AdminPasswordFile = get("ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE")
	cfg.Cloudflare.SitePasswordFile = get("ONBOARD_CLOUDFLARE_SITE_PASSWORD_FILE")
	if v := get("ONBOARD_CLOUDFLARE_ALLOWED_HOSTS"); v != "" {
		cfg.Cloudflare.AllowedHosts = splitCSV(v)
	}
	if cfg.Cloudflare.Enabled, err = getBool("ONBOARD_CLOUDFLARE_ENABLED", cfg.Cloudflare.Enabled); err != nil {
		return cfg, err
	}
	if cfg.Cloudflare.Timeout, err = getDur("ONBOARD_CLOUDFLARE_TIMEOUT", cfg.Cloudflare.Timeout); err != nil {
		return cfg, err
	}

	cfg.Mailpit.BaseURL = firstNonEmpty(get("ONBOARD_MAILPIT_BASE_URL"), cfg.Mailpit.BaseURL)
	cfg.Mailpit.Domain = firstNonEmpty(get("ONBOARD_MAILPIT_DOMAIN"), cfg.Mailpit.Domain)
	if cfg.Mailpit.Enabled, err = getBool("ONBOARD_MAILPIT_ENABLED", cfg.Mailpit.Enabled); err != nil {
		return cfg, err
	}

	cfg.Keycloak.BaseURL = firstNonEmpty(get("ONBOARD_KEYCLOAK_BASE_URL"), cfg.Keycloak.BaseURL)
	cfg.Keycloak.Realm = firstNonEmpty(get("ONBOARD_KEYCLOAK_REALM"), cfg.Keycloak.Realm)
	cfg.Keycloak.AdminUser = firstNonEmpty(get("ONBOARD_KEYCLOAK_ADMIN_USER"), cfg.Keycloak.AdminUser)
	cfg.Keycloak.ClientID = firstNonEmpty(get("ONBOARD_KEYCLOAK_CLIENT_ID"), cfg.Keycloak.ClientID)
	cfg.Keycloak.RedirectURI = firstNonEmpty(get("ONBOARD_KEYCLOAK_REDIRECT_URI"), cfg.Keycloak.RedirectURI)
	cfg.Keycloak.AdminPasswordFile = get("ONBOARD_KEYCLOAK_ADMIN_PASSWORD_FILE")
	cfg.Keycloak.AdminClientID = firstNonEmpty(get("ONBOARD_KEYCLOAK_ADMIN_CLIENT_ID"), cfg.Keycloak.AdminClientID)
	cfg.Keycloak.AdminClientSecretFile = get("ONBOARD_KEYCLOAK_ADMIN_CLIENT_SECRET_FILE")
	cfg.Keycloak.ClientSecretFile = get("ONBOARD_KEYCLOAK_CLIENT_SECRET_FILE")
	if cfg.Keycloak.Enabled, err = getBool("ONBOARD_KEYCLOAK_ENABLED", cfg.Keycloak.Enabled); err != nil {
		return cfg, err
	}

	cfg.GitHub.BaseURL = firstNonEmpty(get("ONBOARD_GITHUB_BASE_URL"), cfg.GitHub.BaseURL)
	cfg.GitHub.TokenFile = get("ONBOARD_GITHUB_TOKEN_FILE")
	if v := get("ONBOARD_GITHUB_ALLOWED_HOSTS"); v != "" {
		cfg.GitHub.AllowedHosts = splitCSV(v)
	}
	if cfg.GitHub.Enabled, err = getBool("ONBOARD_GITHUB_ENABLED", cfg.GitHub.Enabled); err != nil {
		return cfg, err
	}

	cfg.WorkBuddy.BaseURL = firstNonEmpty(get("ONBOARD_WORKBUDDY_BASE_URL"), cfg.WorkBuddy.BaseURL)
	cfg.WorkBuddy.Realm = firstNonEmpty(get("ONBOARD_WORKBUDDY_REALM"), cfg.WorkBuddy.Realm)
	cfg.WorkBuddy.TokenFile = get("ONBOARD_WORKBUDDY_TOKEN_FILE")
	cfg.WorkBuddy.PermissionAck = get("ONBOARD_WORKBUDDY_PERMISSION_ACK")
	if v := get("ONBOARD_WORKBUDDY_ALLOW_REMOTE"); v != "" {
		if cfg.WorkBuddy.AllowRemote, err = getBool("ONBOARD_WORKBUDDY_ALLOW_REMOTE", cfg.WorkBuddy.AllowRemote); err != nil {
			return cfg, err
		}
	}
	if v := get("ONBOARD_WORKBUDDY_ALLOWED_HOSTS"); v != "" {
		cfg.WorkBuddy.AllowedHosts = splitCSV(v)
	}
	if cfg.WorkBuddy.Enabled, err = getBool("ONBOARD_WORKBUDDY_ENABLED", cfg.WorkBuddy.Enabled); err != nil {
		return cfg, err
	}

	cfg.OIDC.Issuer = firstNonEmpty(get("ONBOARD_OIDC_ISSUER"), cfg.OIDC.Issuer)
	cfg.OIDC.ClientID = firstNonEmpty(get("ONBOARD_OIDC_CLIENT_ID"), cfg.OIDC.ClientID)
	cfg.OIDC.RedirectURI = firstNonEmpty(get("ONBOARD_OIDC_REDIRECT_URI"), cfg.OIDC.RedirectURI)
	cfg.OIDC.ClientSecretFile = get("ONBOARD_OIDC_CLIENT_SECRET_FILE")
	if cfg.OIDC.Enabled, err = getBool("ONBOARD_OIDC_ENABLED", cfg.OIDC.Enabled); err != nil {
		return cfg, err
	}

	if err := cfg.resolveSecrets(); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) resolveSecrets() error {
	var err error
	read := func(path, label string) (string, error) {
		raw, err := readSecureSecretFile(path, label, secretPolicy{MaxBytes: 64 * 1024})
		if err != nil {
			return "", err
		}
		v := strings.TrimSpace(string(raw))
		if v == "" {
			return "", fmt.Errorf("%s file %q is empty", label, path)
		}
		return v, nil
	}
	if c.AdminToken == "" && c.AdminTokenFile != "" {
		if c.AdminToken, err = read(c.AdminTokenFile, "ONBOARD_ADMIN_TOKEN_FILE"); err != nil {
			return err
		}
	}
	if c.ReadOnlyToken == "" && c.ReadOnlyTokenFile != "" {
		if c.ReadOnlyToken, err = read(c.ReadOnlyTokenFile, "ONBOARD_READONLY_TOKEN_FILE"); err != nil {
			return err
		}
	}
	if c.MasterKey == nil && c.MasterKeyFile != "" {
		raw, err := readSecureSecretFile(c.MasterKeyFile, "ONBOARD_MASTER_KEY_FILE", secretPolicy{MaxBytes: 64 * 1024})
		if err != nil {
			return err
		}
		if len(raw) == 32 {
			c.MasterKey = append([]byte(nil), raw...)
		} else {
			key, err := DecodeMasterKey(strings.TrimSpace(string(raw)))
			if err != nil {
				return fmt.Errorf("master key file: %w", err)
			}
			c.MasterKey = key
		}
	}
	if c.Cloudflare.AdminPassword == "" && c.Cloudflare.AdminPasswordFile != "" {
		if c.Cloudflare.AdminPassword, err = read(c.Cloudflare.AdminPasswordFile, "cloudflare admin password"); err != nil {
			return err
		}
	}
	if c.Cloudflare.SitePassword == "" && c.Cloudflare.SitePasswordFile != "" {
		if c.Cloudflare.SitePassword, err = read(c.Cloudflare.SitePasswordFile, "cloudflare site password"); err != nil {
			return err
		}
	}
	if c.Keycloak.AdminPassword == "" && c.Keycloak.AdminPasswordFile != "" {
		if c.Keycloak.AdminPassword, err = read(c.Keycloak.AdminPasswordFile, "keycloak admin password"); err != nil {
			return err
		}
	}
	if c.Keycloak.AdminClientSecret == "" && c.Keycloak.AdminClientSecretFile != "" {
		if c.Keycloak.AdminClientSecret, err = read(c.Keycloak.AdminClientSecretFile, "keycloak admin client secret"); err != nil {
			return err
		}
	}
	if c.Keycloak.ClientSecret == "" && c.Keycloak.ClientSecretFile != "" {
		if c.Keycloak.ClientSecret, err = read(c.Keycloak.ClientSecretFile, "keycloak client secret"); err != nil {
			return err
		}
	}
	if c.GitHub.Token == "" && c.GitHub.TokenFile != "" {
		if c.GitHub.Token, err = read(c.GitHub.TokenFile, "github token"); err != nil {
			return err
		}
	}
	if c.WorkBuddy.Token == "" && c.WorkBuddy.TokenFile != "" {
		if c.WorkBuddy.Token, err = read(c.WorkBuddy.TokenFile, "workbuddy gateway token"); err != nil {
			return err
		}
	}
	if c.OIDC.ClientSecret == "" && c.OIDC.ClientSecretFile != "" {
		if c.OIDC.ClientSecret, err = read(c.OIDC.ClientSecretFile, "oidc client secret"); err != nil {
			return err
		}
	}
	return nil
}

// secretPolicy is intentionally strict. Docker/Kubernetes secret projections
// must copy to an owner-only regular file (0400/0600) or configure defaultMode
// accordingly; do not silently accept 0444.
type secretPolicy struct {
	MaxBytes int64
}

const maxSecretFileBytes = 64 * 1024

// openSecretFile is a test seam for the Lstat/Open replacement race test.
var openSecretFile = os.Open

// readSecureSecretFile is the single secret-file entry point. It rejects
// non-regular files, symlinks (TOCTOU/redirect risk), group/other permissions,
// empty files and oversized files. It returns bytes read from the already-open
// file descriptor; no secret value is included in errors.
func readSecureSecretFile(path, label string, policy secretPolicy) ([]byte, error) {
	if policy.MaxBytes <= 0 || policy.MaxBytes > maxSecretFileBytes {
		policy.MaxBytes = maxSecretFileBytes
	}
	lst, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%s file %q is not readable: %w", label, path, err)
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s file %q is a symlink; projected secrets must be copied to an owner-only regular file", label, path)
	}
	f, err := openSecretFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s file %q is not readable: %w", label, path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s file: %w", label, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s path is not a regular file", label)
	}
	if !os.SameFile(lst, st) {
		return nil, fmt.Errorf("%s file changed while being opened", label)
	}
	perm := st.Mode().Perm()
	if perm != 0o400 && perm != 0o600 {
		return nil, fmt.Errorf("%s file %q permissions %04o are not allowed; require exactly 0400 or 0600", label, path, perm)
	}
	if st.Size() <= 0 {
		return nil, fmt.Errorf("%s file %q is empty", label, path)
	}
	if st.Size() > policy.MaxBytes {
		return nil, fmt.Errorf("%s file is too large (max %d bytes)", label, policy.MaxBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(f, policy.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s file: %w", label, err)
	}
	if int64(len(raw)) > policy.MaxBytes {
		return nil, fmt.Errorf("%s file is too large (max %d bytes)", label, policy.MaxBytes)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s file %q is empty", label, path)
	}
	return raw, nil
}

func readSecretFile(path, label string) (string, error) {
	raw, err := readSecureSecretFile(path, label, secretPolicy{MaxBytes: maxSecretFileBytes})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// DecodeMasterKey accepts raw 32 bytes, hex, base64 or a JSON keyring envelope.
func DecodeMasterKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty master key")
	}
	// A raw 32-byte key wins over JSON detection: binary keys may legitimately
	// start with '{' and must never be parsed as a keyring envelope.
	if len(s) == 32 && !isPrintableHex(s) && !isBase64Only(s) {
		return []byte(s), nil
	}
	if key, ok, err := decodeKeyringEnvelope(s); ok {
		return key, err
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, errors.New("master key must be 32 bytes encoded as raw/hex/base64")
}

// decodeKeyringEnvelope resolves the current key from a rotation keyring:
//
//	{"current":1,"keys":{"1":"<base64 of 32 bytes>", ...}}
//
// The second return value reports whether the input looked like a JSON envelope
// (so malformed envelopes fail closed instead of being tried as raw keys).
func decodeKeyringEnvelope(s string) ([]byte, bool, error) {
	if !strings.HasPrefix(s, "{") {
		return nil, false, nil
	}
	var envelope struct {
		Current int               `json:"current"`
		Keys    map[string]string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(s), &envelope); err != nil {
		return nil, true, errors.New("master key JSON keyring is not valid JSON")
	}
	if envelope.Current <= 0 || len(envelope.Keys) == 0 {
		return nil, true, errors.New("master key JSON keyring needs current>=1 and at least one key")
	}
	encoded, ok := envelope.Keys[strconv.Itoa(envelope.Current)]
	if !ok {
		return nil, true, fmt.Errorf("master key JSON keyring has no key version %d", envelope.Current)
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(encoded); err == nil && len(b) == 32 {
			return b, true, nil
		}
	}
	return nil, true, fmt.Errorf("master key JSON keyring version %d must be base64 of 32 bytes", envelope.Current)
}

func isPrintableHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func isBase64Only(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '+' || r == '/' || r == '=' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// Validate enforces the security invariants.
func (c *Config) Validate() error {
	if c.Mode != ModeSandbox && c.Mode != ModeProduction {
		return fmt.Errorf("ONBOARD_MODE must be %q or %q", ModeSandbox, ModeProduction)
	}
	if c.MaxConcurrency < 1 || c.MaxConcurrency > 5 {
		return fmt.Errorf("ONBOARD_MAX_CONCURRENCY must be between 1 and 5")
	}
	if c.Listen == "" {
		return errors.New("ONBOARD_LISTEN must not be empty")
	}
	host, _, err := splitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("ONBOARD_LISTEN: %w", err)
	}
	if !c.AllowPublicListen && !isLoopbackHost(host) {
		return fmt.Errorf("ONBOARD_LISTEN host %q is not loopback; set ONBOARD_ALLOW_PUBLIC_LISTEN=1 only behind a private network proxy", host)
	}
	if c.DBPath == "" {
		return errors.New("ONBOARD_DB_PATH must not be empty")
	}
	if c.MasterKey != nil && len(c.MasterKey) != 32 {
		return fmt.Errorf("master key must be 32 bytes, got %d", len(c.MasterKey))
	}
	if c.MasterKey == nil && !c.DevEphemeralKey {
		return errors.New("ONBOARD_MASTER_KEY_FILE (or ONBOARD_MASTER_KEY) is required; set ONBOARD_DEV_EPHEMERAL_KEY=1 only for local fake-provider development")
	}
	if c.DevEphemeralKey && c.Mode == ModeProduction {
		return errors.New("ONBOARD_DEV_EPHEMERAL_KEY is not allowed in production mode")
	}
	if c.StepTimeout <= 0 || c.RunDeadline <= 0 || c.LeaseTTL <= 0 || c.PollInterval <= 0 || c.TickInterval <= 0 {
		return errors.New("timeouts must be positive")
	}
	if c.LeaseTTL <= c.StepTimeout {
		return errors.New("ONBOARD_LEASE_TTL must be greater than ONBOARD_STEP_TIMEOUT")
	}
	if c.MaxStepAttempts < 1 || c.MaxStepAttempts > 50 {
		return errors.New("ONBOARD_MAX_STEP_ATTEMPTS must be between 1 and 50")
	}
	if c.AllowPublicListen && c.AdminToken == "" && c.AdminTokenFile == "" {
		return errors.New("public listen requires an admin token")
	}
	if c.ReadOnlyToken != "" && c.AdminToken != "" && c.ReadOnlyToken == c.AdminToken {
		return errors.New("ONBOARD_READONLY_TOKEN must differ from the admin token")
	}
	if c.RateLimitPerMinute < 0 || c.RateLimitPerMinute > 100000 {
		return errors.New("ONBOARD_RATE_LIMIT_PER_MINUTE must be between 0 (disabled) and 100000")
	}
	if c.LoginRateLimitPerMinute < 0 || c.LoginRateLimitPerMinute > 100000 {
		return errors.New("ONBOARD_LOGIN_RATE_LIMIT_PER_MINUTE must be between 0 (disabled) and 100000")
	}
	if c.Cloudflare.Enabled {
		if err := requireAllowlistedURL("ONBOARD_CLOUDFLARE_BASE_URL", c.Cloudflare.BaseURL, append([]string{"api.moonbyte.top"}, c.Cloudflare.AllowedHosts...)); err != nil {
			return err
		}
		if c.Cloudflare.AdminPassword == "" {
			return errors.New("cloudflare enabled but no admin password configured")
		}
		if c.Cloudflare.Domain == "" {
			return errors.New("cloudflare enabled but no domain configured")
		}
	}
	if c.Mailpit.Enabled {
		if err := requireAllowlistedURL("ONBOARD_MAILPIT_BASE_URL", c.Mailpit.BaseURL, []string{"127.0.0.1", "localhost", "::1"}); err != nil {
			return err
		}
	}
	if c.Keycloak.Enabled {
		if err := requireAllowlistedURL("ONBOARD_KEYCLOAK_BASE_URL", c.Keycloak.BaseURL, []string{"127.0.0.1", "localhost", "::1"}); err != nil {
			return err
		}
		hasAdminPassword := c.Keycloak.AdminPassword != ""
		hasServiceAccount := c.Keycloak.AdminClientID != "" && c.Keycloak.AdminClientSecret != ""
		if !hasAdminPassword && !hasServiceAccount {
			return errors.New("keycloak enabled but no admin password or admin service-account client configured")
		}
	}
	if c.GitHub.Enabled {
		if err := requireAllowlistedURL("ONBOARD_GITHUB_BASE_URL", c.GitHub.BaseURL, append([]string{"api.github.com"}, c.GitHub.AllowedHosts...)); err != nil {
			return err
		}
		if c.GitHub.Token == "" {
			return errors.New("github enabled but no token configured")
		}
	}
	if c.WorkBuddy.Enabled {
		if c.WorkBuddy.AllowRemote {
			if err := requireAllowlistedURL("ONBOARD_WORKBUDDY_BASE_URL", c.WorkBuddy.BaseURL, c.WorkBuddy.AllowedHosts); err != nil {
				return fmt.Errorf("%w (set ONBOARD_WORKBUDDY_ALLOWED_HOSTS for a remote gateway)", err)
			}
		} else if err := requireLocalURL("ONBOARD_WORKBUDDY_BASE_URL", c.WorkBuddy.BaseURL); err != nil {
			return fmt.Errorf("%w (the WorkBuddy adapter must target a private gateway only)", err)
		}
		if c.WorkBuddy.PermissionAck == "" {
			return errors.New("workbuddy enabled but ONBOARD_WORKBUDDY_PERMISSION_ACK is empty; written platform permission is required")
		}
		if c.WorkBuddy.Token == "" {
			return errors.New("workbuddy enabled but no gateway token configured")
		}
	}
	if c.OIDC.Enabled {
		if err := requireLocalURL("ONBOARD_OIDC_ISSUER", c.OIDC.Issuer); err != nil {
			return err
		}
	}
	if c.Mode == ModeProduction && !c.FakeProviders && c.Cloudflare.Enabled == false && c.GitHub.Enabled == false && c.Mailpit.Enabled == false && c.Keycloak.Enabled == false {
		return errors.New("production mode requires at least one provider configured")
	}
	return nil
}

func requireAllowlistedURL(env, raw string, hosts []string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL", env)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("%s must be http(s)", env)
	}
	for _, h := range hosts {
		if strings.EqualFold(u.Hostname(), h) {
			return nil
		}
	}
	return fmt.Errorf("%s host %q is not allowlisted", env, u.Hostname())
}

func requireLocalURL(env, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL", env)
	}
	if !isLoopbackHost(u.Hostname()) && !isPrivateHost(u.Hostname()) {
		return fmt.Errorf("%s host %q must be loopback or private address", env, u.Hostname())
	}
	return nil
}

func splitHostPort(addr string) (string, string, error) {
	if strings.HasPrefix(addr, "[") {
		i := strings.LastIndex(addr, "]:")
		if i < 0 {
			return "", "", fmt.Errorf("invalid address %q", addr)
		}
		return addr[1:i], addr[i+2:], nil
	}
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("invalid address %q", addr)
	}
	return addr[:i], addr[i+1:], nil
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == "127.0.0.1" || h == "localhost" || h == "::1" || h == ""
}

func isPrivateHost(host string) bool {
	h := strings.ToLower(host)
	if isLoopbackHost(h) {
		return true
	}
	ip := parseIPv4(h)
	if ip == nil {
		return false
	}
	return ip[0] == 10 || (ip[0] == 192 && ip[1] == 168) || (ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31)
}

func parseIPv4(s string) []byte {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return nil
	}
	out := make([]byte, 4)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return nil
		}
		out[i] = byte(n)
	}
	return out
}

// EnsureDataDir creates the parent directory of the database path.
func (c *Config) EnsureDataDir() error {
	dir := filepath.Dir(c.DBPath)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o750)
}

// Redacted returns a log-safe summary of the configuration.
func (c *Config) Redacted() map[string]any {
	return map[string]any{
		"listen":                   c.Listen,
		"db_path":                  c.DBPath,
		"mode":                     string(c.Mode),
		"max_concurrency":          c.MaxConcurrency,
		"cloudflare_enabled":       c.Cloudflare.Enabled,
		"github_enabled":           c.GitHub.Enabled,
		"workbuddy_enabled":        c.WorkBuddy.Enabled,
		"mailpit_enabled":          c.Mailpit.Enabled,
		"keycloak_enabled":         c.Keycloak.Enabled,
		"fake_providers":           c.FakeProviders,
		"admin_token_set":          c.AdminToken != "",
		"master_key_set":           c.MasterKey != nil,
		"allow_public_listen":      c.AllowPublicListen,
		"dev_ephemeral_key":        c.DevEphemeralKey,
		"readonly_token_set":       c.ReadOnlyToken != "",
		"rate_limit_per_min":       c.RateLimitPerMinute,
		"login_rate_limit_per_min": c.LoginRateLimitPerMinute,
		"trust_proxy_headers":      c.TrustProxyHeaders,
	}
}

func firstNonEmpty(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
