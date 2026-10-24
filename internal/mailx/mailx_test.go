package mailx

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

// ref is the fixed clock used by every test so the suite is deterministic.
var ref = time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{
		ExpectedRecipient: "sandbox001@mailpit.local",
		SenderAllowlist:   []string{"keycloak.local"},
		SubjectPatterns:   []string{"verify your email"},
		WindowStart:       ref.Add(-5 * time.Minute),
		WindowEnd:         ref.Add(5 * time.Minute),
		ClockSkew:         30 * time.Second,
	}
}

func mustMatcher(t *testing.T, cfg Config) *Matcher {
	t.Helper()
	m, err := NewMatcher(cfg)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	return m
}

func message(id string) provider.Message {
	return provider.Message{
		ID:         id,
		Recipient:  "sandbox001@mailpit.local",
		Sender:     "Keycloak <no-reply@keycloak.local>",
		Subject:    "Verify your email",
		ReceivedAt: ref,
		Text:       "Please verify your account. Your code is 123456.",
	}
}

func TestMatchExactRecipient(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = nil
	cfg.SenderAllowlist = []string{"keycloak.local"}

	cases := []struct {
		name      string
		recipient string
		want      bool
	}{
		{"exact", "sandbox001@mailpit.local", true},
		{"display name and different case", "Sandbox One <SANDBOX001@Mailpit.Local>", true},
		{"other local part", "sandbox002@mailpit.local", false},
		{"plus tag", "sandbox001+tag@mailpit.local", false},
		{"different domain", "sandbox001@example.com", false},
		{"suffix spoof", "sandbox001@mailpit.local.evil.example", false},
		{"address list", "sandbox001@mailpit.local, other@mailpit.local", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMatcher(t, cfg)
			msg := message("m-" + tc.name)
			msg.Recipient = tc.recipient
			_, ok := m.Match([]provider.Message{msg})
			if ok != tc.want {
				t.Fatalf("Match() ok = %v, want %v (recipient %q)", ok, tc.want, tc.recipient)
			}
		})
	}
}

func TestMatchSenderAllowlistRejection(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = nil

	cases := []struct {
		name   string
		sender string
		allow  []string
		want   bool
	}{
		{"domain rule match", "Keycloak <no-reply@keycloak.local>", []string{"keycloak.local"}, true},
		{"exact address rule match", "no-reply@keycloak.local", []string{"noreply@example.com", "no-reply@keycloak.local"}, true},
		{"unlisted sender", "Keycloak <no-reply@other.example>", []string{"keycloak.local"}, false},
		{"empty allowlist denies", "no-reply@keycloak.local", nil, false},
		{"suffix confusion", "no-reply@keycloak.local.evil.example", []string{"keycloak.local"}, false},
		{"wildcard subdomain", "auth@sub.keycloak.local", []string{"*.keycloak.local"}, true},
		{"wildcard apex rejected", "auth@keycloak.local", []string{"*.keycloak.local"}, false},
		{"display name cannot vouch", `"no-reply@keycloak.local" <evil@attacker.example>`, []string{"keycloak.local"}, false},
		{"comment cannot vouch", "evil@attacker.example (no-reply@keycloak.local)", []string{"keycloak.local"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg
			c.SenderAllowlist = tc.allow
			m := mustMatcher(t, c)
			msg := message("m-" + tc.name)
			msg.Sender = tc.sender
			_, ok := m.Match([]provider.Message{msg})
			if ok != tc.want {
				t.Fatalf("Match() ok = %v, want %v (sender %q, allowlist %v)", ok, tc.want, tc.sender, tc.allow)
			}
		})
	}
}

func TestMatchTimeWindowRejection(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = nil
	cfg.WindowStart = ref
	cfg.WindowEnd = ref.Add(2 * time.Minute)
	cfg.ClockSkew = 30 * time.Second

	cases := []struct {
		name     string
		received time.Time
		want     bool
	}{
		{"inside window", ref.Add(time.Minute), true},
		{"start boundary inside skew", ref.Add(-29 * time.Second), true},
		{"start boundary outside skew", ref.Add(-31 * time.Second), false},
		{"end boundary inside skew", ref.Add(2*time.Minute + 29*time.Second), true},
		{"end boundary outside skew", ref.Add(2*time.Minute + 31*time.Second), false},
		{"far before", ref.Add(-2 * time.Hour), false},
		{"far after", ref.Add(2 * time.Hour), false},
		{"unknown receive time", time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMatcher(t, cfg)
			msg := message("m-" + tc.name)
			msg.ReceivedAt = tc.received
			_, ok := m.Match([]provider.Message{msg})
			if ok != tc.want {
				t.Fatalf("Match() ok = %v, want %v (received %s)", ok, tc.want, tc.received)
			}
		})
	}
}

func TestOTPReuseAcrossCalls(t *testing.T) {
	t.Run("consumed message is never returned twice", func(t *testing.T) {
		cfg := testConfig()
		cfg.SubjectPatterns = nil
		m := mustMatcher(t, cfg)
		msgs := []provider.Message{message("m-1")}

		first, ok := m.Match(msgs)
		if !ok {
			t.Fatal("first Match() = no match, want match")
		}
		if first.Code != "123456" {
			t.Fatalf("first code = %q, want 123456", first.Code)
		}
		if !m.Seen("m-1") {
			t.Fatal("message ID not recorded as seen")
		}
		if second, ok := m.Match(msgs); ok {
			t.Fatalf("second Match() returned %v, want no match (OTP must not be reusable)", second)
		}
	})

	t.Run("MarkSeen preloads durable dedupe state", func(t *testing.T) {
		cfg := testConfig()
		cfg.SubjectPatterns = nil
		m := mustMatcher(t, cfg)
		m.MarkSeen("m-1")
		if _, ok := m.Match([]provider.Message{message("m-1")}); ok {
			t.Fatal("Match() returned a message that was preloaded as seen")
		}
	})

	t.Run("duplicate IDs in one batch are consumed once", func(t *testing.T) {
		cfg := testConfig()
		cfg.SubjectPatterns = nil
		m := mustMatcher(t, cfg)
		dup := []provider.Message{message("m-1"), message("m-1")}
		if _, ok := m.Match(dup); !ok {
			t.Fatal("first Match() = no match, want match")
		}
		if _, ok := m.Match(dup); ok {
			t.Fatal("second Match() reused the same message ID")
		}
	})

	t.Run("optional code reuse rejection across distinct messages", func(t *testing.T) {
		cfg := testConfig()
		cfg.SubjectPatterns = nil
		cfg.RejectCodeReuse = true
		m := mustMatcher(t, cfg)

		first := message("m-1")
		second := message("m-2") // same code "123456", different message ID
		if res, ok := m.Match([]provider.Message{first}); !ok || res.Code != "123456" {
			t.Fatalf("first Match() = (%v, %v), want code 123456", res, ok)
		}
		if res, ok := m.Match([]provider.Message{second}); ok {
			t.Fatalf("Match() accepted reused code %v, want rejection", res.Code)
		}
	})

	t.Run("code reuse allowed by default", func(t *testing.T) {
		cfg := testConfig()
		cfg.SubjectPatterns = nil
		m := mustMatcher(t, cfg)

		if _, ok := m.Match([]provider.Message{message("m-1")}); !ok {
			t.Fatal("first Match() = no match")
		}
		if _, ok := m.Match([]provider.Message{message("m-2")}); !ok {
			t.Fatal("second Match() = no match; default policy allows a resent code")
		}
	})
}

func TestUnknownSubject(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = []string{"verify your email", `confirm your identity`}

	t.Run("unrelated subject rejected", func(t *testing.T) {
		m := mustMatcher(t, cfg)
		msg := message("m-1")
		msg.Subject = "Welcome to the service"
		if _, ok := m.Match([]provider.Message{msg}); ok {
			t.Fatal("Match() accepted a message with an unknown subject")
		}
	})

	t.Run("matching subject is case insensitive", func(t *testing.T) {
		m := mustMatcher(t, cfg)
		msg := message("m-1")
		msg.Subject = "Please CONFIRM YOUR IDENTITY now"
		if _, ok := m.Match([]provider.Message{msg}); !ok {
			t.Fatal("Match() rejected a case-insensitive subject match")
		}
	})

	t.Run("empty subject patterns accept any subject", func(t *testing.T) {
		c := cfg
		c.SubjectPatterns = nil
		m := mustMatcher(t, c)
		msg := message("m-1")
		msg.Subject = "Anything at all"
		if _, ok := m.Match([]provider.Message{msg}); !ok {
			t.Fatal("Match() rejected a message although no subject gate was configured")
		}
	})
}

func TestHTMLLinkExtraction(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = nil
	cfg.SenderAllowlist = []string{"keycloak.local"}

	msg := message("m-1")
	msg.Text = ""
	msg.HTML = `<html><body>` +
		`<a href="https://id.example/logo.png">Logo</a>` +
		`<a href="https://id.example/verify?token=abc123&amp;state=xyz">Verify</a>` +
		`</body></html>`

	m := mustMatcher(t, cfg)
	res, ok := m.Match([]provider.Message{msg})
	if !ok {
		t.Fatal("Match() = no match, want HTML link extraction")
	}
	if res.Link != "https://id.example/verify?token=abc123&state=xyz" {
		t.Fatalf("Link = %q, want verification URL with decoded entity", res.Link)
	}
	if len(res.Links) != 2 || res.Links[0] != res.Link {
		t.Fatalf("Links = %v, want verification link ranked first", res.Links)
	}

	t.Run("plain text url", func(t *testing.T) {
		c := cfg
		m := mustMatcher(t, c)
		msg := message("m-2")
		msg.Text = "Open https://id.example/confirm?token=plain to continue."
		msg.HTML = ""
		res, ok := m.Match([]provider.Message{msg})
		if !ok {
			t.Fatal("Match() = no match for plain-text URL")
		}
		if res.Link != "https://id.example/confirm?token=plain" {
			t.Fatalf("Link = %q, want trailing punctuation trimmed", res.Link)
		}
	})

	t.Run("non-http link ignored", func(t *testing.T) {
		c := cfg
		m := mustMatcher(t, c)
		msg := message("m-3")
		msg.Text = "mailto:noreply@keycloak.local"
		msg.HTML = `<a href="#top">Top</a>`
		if _, ok := m.Match([]provider.Message{msg}); ok {
			t.Fatal("Match() accepted a message without an absolute http(s) link")
		}
	})
}

func TestMultipleMatches(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = nil
	m := mustMatcher(t, cfg)

	unrelated := message("m-x")
	unrelated.Recipient = "someone-else@mailpit.local"
	msgs := []provider.Message{unrelated, message("m-1"), message("m-2")}

	first, ok := m.Match(msgs)
	if !ok || first.MessageID != "m-1" {
		t.Fatalf("first Match() = (%v, %v), want m-1", first.MessageID, ok)
	}
	second, ok := m.Match(msgs)
	if !ok || second.MessageID != "m-2" {
		t.Fatalf("second Match() = (%v, %v), want m-2", second.MessageID, ok)
	}
	if res, ok := m.Match(msgs); ok {
		t.Fatalf("third Match() = %v, want no match", res)
	}
}

func TestSpoofedFrom(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = nil
	cfg.SenderAllowlist = []string{"keycloak.local"}

	spoofs := []string{
		`"no-reply@keycloak.local" <attacker@evil.example>`,
		`no-reply@keycloak.local <attacker@evil.example>`,
		`attacker@evil.example (no-reply@keycloak.local)`,
		`no-reply@keycloak.local; attacker@evil.example`,
		`attacker@evil.example, no-reply@keycloak.local`,
		`keycloak.local`,
	}
	for i, sender := range spoofs {
		t.Run("spoof-"+string(rune('a'+i)), func(t *testing.T) {
			m := mustMatcher(t, cfg)
			msg := message("m-spoof")
			msg.Sender = sender
			if _, ok := m.Match([]provider.Message{msg}); ok {
				t.Fatalf("Match() accepted spoofed From %q", sender)
			}
		})
	}

	// The allowlisted address inside a display name is not enough, but a real
	// allowlisted angle-addr must pass.
	t.Run("legitimate display name accepted", func(t *testing.T) {
		m := mustMatcher(t, cfg)
		msg := message("m-ok")
		msg.Sender = "Keycloak <no-reply@keycloak.local>"
		res, ok := m.Match([]provider.Message{msg})
		if !ok {
			t.Fatal("Match() rejected a legitimate allowlisted sender")
		}
		if res.Sender != "no-reply@keycloak.local" {
			t.Fatalf("Sender = %q, want normalized bare address", res.Sender)
		}
	})
}

func TestResultRedaction(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = nil
	m := mustMatcher(t, cfg)
	msg := message("m-1")
	msg.Text = "Verify at https://id.example/verify?token=abc123 or use code 123456"
	res, ok := m.Match([]provider.Message{msg})
	if !ok {
		t.Fatal("Match() = no match")
	}
	if res.Link == "" || res.Code == "" {
		t.Fatalf("expected link and code, got link=%q code=%q", res.Link, res.Code)
	}

	for _, rendered := range []string{res.String(), res.SafeSummary()} {
		if strings.Contains(rendered, res.Code) {
			t.Fatalf("summary leaked OTP: %s", rendered)
		}
		if strings.Contains(rendered, res.Link) {
			t.Fatalf("summary leaked link: %s", rendered)
		}
	}

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}
	encoded := string(raw)
	if strings.Contains(encoded, res.Code) || strings.Contains(encoded, res.Link) {
		t.Fatalf("JSON leaked transient secret: %s", encoded)
	}
	if !strings.Contains(encoded, `"has_link":true`) || !strings.Contains(encoded, `"has_code":true`) {
		t.Fatalf("JSON = %s, want has_link/has_code flags", encoded)
	}
}

func TestCustomExtractionPatterns(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = nil
	cfg.LinkPatterns = []string{`https://id\.example/\S+`}
	cfg.OTPPatterns = []string{`CODE-([0-9]{6})`}

	msg := message("m-1")
	msg.Text = "Use CODE-778899 at https://id.example/confirm/xyz to finish."
	msg.HTML = ""

	m := mustMatcher(t, cfg)
	res, ok := m.Match([]provider.Message{msg})
	if !ok {
		t.Fatal("Match() = no match with custom patterns")
	}
	if res.Code != "778899" {
		t.Fatalf("Code = %q, want 778899", res.Code)
	}
	if res.Link != "https://id.example/confirm/xyz" {
		t.Fatalf("Link = %q, want https://id.example/confirm/xyz", res.Link)
	}
}

func TestCodeExtractionHeuristics(t *testing.T) {
	base := testConfig()
	base.SubjectPatterns = nil

	cases := []struct {
		name string
		text string
		html string
		want string
	}{
		{name: "numeric six digits", text: "Your code is 654321.", want: "654321"},
		{name: "numeric four digits in context", text: "Verification code: 4321", want: "4321"},
		{name: "alphanumeric code", text: "Your verification code is AB12CD", want: "AB12CD"},
		{name: "standalone numeric run", text: "123456 is your login token", want: "123456"},
		{name: "html body", html: "<p>Your code is <b>908172</b></p>", want: "908172"},
		{name: "url digits are not a code", text: "Visit https://id.example/t/123456 now", want: ""},
		{name: "script content ignored", html: `<script>var x="code 111111"</script><p>hello</p>`, want: ""},
		{name: "no digits", text: "No code in this message.", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMatcher(t, base)
			msg := message("m-" + tc.name)
			msg.Text = tc.text
			msg.HTML = tc.html
			res, ok := m.MatchNeed([]provider.Message{msg}, NeedCode)
			if tc.want == "" {
				if ok {
					t.Fatalf("Match() = %v, want no match", res)
				}
				return
			}
			if !ok {
				t.Fatalf("Match() = no match, want code %q", tc.want)
			}
			if res.Code != tc.want {
				t.Fatalf("Code = %q, want %q", res.Code, tc.want)
			}
		})
	}
}

func TestMatchNeed(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = nil

	codeOnly := message("m-code")
	codeOnly.Text = "Your code is 123456"
	codeOnly.HTML = ""

	linkOnly := message("m-link")
	linkOnly.Text = "Open https://id.example/verify?token=abc"
	linkOnly.HTML = ""

	m := mustMatcher(t, cfg)
	if res, ok := m.MatchNeed([]provider.Message{codeOnly, linkOnly}, NeedLink); !ok || res.MessageID != "m-link" {
		t.Fatalf("MatchNeed(NeedLink) = (%v, %v), want m-link", res.MessageID, ok)
	}
	if res, ok := m.MatchNeed([]provider.Message{codeOnly}, NeedCode); !ok || res.Code != "123456" {
		t.Fatalf("MatchNeed(NeedCode) = (%v, %v), want 123456", res.Code, ok)
	}
	if _, ok := m.MatchNeed([]provider.Message{linkOnly}, NeedCode); ok {
		t.Fatal("MatchNeed(NeedCode) accepted a link-only message")
	}
}

func TestConfigValidation(t *testing.T) {
	valid := testConfig()
	cases := []struct {
		name string
		cfg  func() Config
	}{
		{"invalid recipient", func() Config { c := valid; c.ExpectedRecipient = "not-an-address"; return c }},
		{"empty recipient", func() Config { c := valid; c.ExpectedRecipient = ""; return c }},
		{"recipient list", func() Config { c := valid; c.ExpectedRecipient = "a@b.example, c@d.example"; return c }},
		{"invalid sender entry", func() Config { c := valid; c.SenderAllowlist = []string{"bad entry"}; return c }},
		{"invalid sender wildcard", func() Config { c := valid; c.SenderAllowlist = []string{"*."}; return c }},
		{"invalid subject pattern", func() Config { c := valid; c.SubjectPatterns = []string{"("}; return c }},
		{"invalid link pattern", func() Config { c := valid; c.LinkPatterns = []string{"[a-"}; return c }},
		{"invalid otp pattern", func() Config { c := valid; c.OTPPatterns = []string{`(?P<`}; return c }},
		{"negative skew", func() Config { c := valid; c.ClockSkew = -time.Second; return c }},
		{"inverted window", func() Config {
			c := valid
			c.WindowStart = ref.Add(time.Hour)
			c.WindowEnd = ref
			c.ClockSkew = time.Second
			return c
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewMatcher(tc.cfg()); err == nil {
				t.Fatal("NewMatcher() = nil error, want validation error")
			} else if class := errs.ClassOf(err); class != errs.ClassValidation {
				t.Fatalf("error class = %s, want VALIDATION (err=%v)", class, err)
			}
		})
	}

	t.Run("valid config compiles", func(t *testing.T) {
		if _, err := NewMatcher(valid); err != nil {
			t.Fatalf("NewMatcher() error = %v", err)
		}
	})
}

func TestMatchConcurrentDedupe(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = nil
	m := mustMatcher(t, cfg)
	msgs := []provider.Message{message("m-1")}

	const goroutines = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	matches := 0
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := m.Match(msgs); ok {
				mu.Lock()
				matches++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if matches != 1 {
		t.Fatalf("concurrent matches = %d, want exactly 1", matches)
	}
}

func TestMatcherMissingIDsFailClosed(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = nil
	m := mustMatcher(t, cfg)
	msg := message("")
	if _, ok := m.Match([]provider.Message{msg}); ok {
		t.Fatal("Match() consumed a message without a provider ID")
	}
}

func TestNewMatcherErrorHasNoSecrets(t *testing.T) {
	cfg := testConfig()
	cfg.SubjectPatterns = []string{"secret-body-token-123456"}
	cfg.OTPPatterns = []string{"(unclosed"}
	_, err := NewMatcher(cfg)
	if err == nil {
		t.Fatal("NewMatcher() = nil error, want invalid pattern error")
	}
	text := err.Error() + " " + errs.SummaryOf(err)
	for _, secret := range []string{"secret-body-token-123456", "123456"} {
		if strings.Contains(text, secret) {
			t.Fatalf("config error leaked pattern content %q: %s", secret, text)
		}
	}
}
