// Package mailx implements provider-independent mailbox message matching and
// verification-secret extraction for the onboarding engine.
//
// It is the single place where the "which email belongs to this run" decision is
// made, so that the decision is testable without any network access.
//
// Safety invariants (docs/04-providers.md, docs/08-integration-cloudflare.md,
// docs/11-testing-acceptance.md):
//
//   - A message matches only when the recipient exactly equals the address this
//     run provisioned, the sender is inside the configured allowlist, the
//     received timestamp is inside the step window (plus a clock-skew
//     tolerance) and the subject matches one of the configured patterns.
//   - The sender address is parsed from the message, never substring-matched,
//     so a spoofed display name ("no-reply@good.example" <evil@bad.example>)
//     cannot satisfy the allowlist.
//   - Every provider message ID is consumed at most once, so a poll loop cannot
//     reuse the same OTP or verification link. Optional code-reuse rejection
//     additionally refuses the same code delivered in a second message.
//   - Extracted links, OTP codes, subjects and bodies live only in the transient
//     Result. Result.String, Result.SafeSummary and Result.MarshalJSON never
//     expose them, and this package neither logs nor persists anything.
//
// Durability note: deduplication state is in-memory. The workflow engine owns
// durable "already consumed" bookkeeping; it can preload that state with
// MarkSeen and mirror each returned Result.MessageID.
package mailx

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

const (
	// DefaultClockSkew is the tolerance applied on both edges of the step time
	// window. It absorbs clock disagreement between the mailbox provider and
	// this host; it is not a delivery timeout.
	DefaultClockSkew = 2 * time.Minute

	// DefaultMaxMessages caps how many provider messages one match call
	// inspects. Older messages are outside the step window anyway.
	DefaultMaxMessages = 200
)

// Config is the matching policy for one mailbox step. Zero values are safe:
// ClockSkew defaults to DefaultClockSkew, MaxMessages to DefaultMaxMessages,
// an empty SubjectPatterns means "any subject", empty WindowStart/WindowEnd
// means "unbounded on that side", and an empty SenderAllowlist denies every
// sender (fail closed).
type Config struct {
	// ExpectedRecipient is the address provisioned for this run. Comparison is
	// exact after RFC 5322 style normalization (display name, angle brackets,
	// surrounding whitespace, case).
	ExpectedRecipient string

	// SenderAllowlist holds allowed senders. Supported entries:
	//
	//	"noreply@idp.example"  exact address
	//	"idp.example"          exact domain
	//	"@idp.example"         exact domain (leading @ accepted)
	//	"*.example.com"        any subdomain of example.com (not the apex)
	//
	// An empty allowlist allows nothing.
	SenderAllowlist []string

	// SubjectPatterns are regular expressions matched case-insensitively
	// against the subject; a message matches if any pattern matches. An empty
	// list accepts any subject.
	SubjectPatterns []string

	// WindowStart and WindowEnd bound the acceptable received_at value,
	// inclusive, widened by ClockSkew on both sides. A zero value leaves that
	// side unbounded.
	WindowStart time.Time
	WindowEnd   time.Time

	// ClockSkew is the time-window tolerance. Zero selects DefaultClockSkew;
	// a negative value is rejected.
	ClockSkew time.Duration

	// LinkPatterns and OTPPatterns are optional custom extraction regular
	// expressions. When empty, the documented defaults in extract.go are used.
	// The first capture group is the extracted value, otherwise the whole match
	// is used.
	LinkPatterns []string
	OTPPatterns  []string

	// MaxMessages caps the inspected slice. Zero selects DefaultMaxMessages.
	MaxMessages int

	// RejectCodeReuse, when true, refuses to consume a message whose extracted
	// code was already consumed from a different message. Default false:
	// providers may resend the same code, and message-ID deduplication already
	// prevents reuse of a single message.
	RejectCodeReuse bool
}

// Need selects which transient secret a caller requires from a matched message.
type Need int

const (
	// NeedAny accepts a message that yields a verification link or a code.
	NeedAny Need = iota
	// NeedLink accepts only a message with at least one extracted link.
	NeedLink
	// NeedCode accepts only a message with at least one extracted code.
	NeedCode
)

// Result is a matched message. Every field is transient: it exists only inside
// the current workflow step and must never be persisted or logged by callers.
// String, SafeSummary and MarshalJSON are deliberately redacted so accidental
// logging cannot leak the link or the code.
type Result struct {
	// MessageID is the provider message identifier.
	MessageID string
	// Recipient is the normalized recipient address of the message.
	Recipient string
	// Sender is the normalized sender address of the message.
	Sender string
	// ReceivedAt is the provider-reported receive time.
	ReceivedAt time.Time
	// Links are extracted verification link candidates, most relevant first.
	Links []string
	// Link is the best link candidate (transient).
	Link string
	// Codes are extracted OTP candidates, most relevant first.
	Codes []string
	// Code is the best OTP candidate (transient).
	Code string
}

// safeResult is the only serializable projection of Result.
type safeResult struct {
	MessageID  string    `json:"message_id"`
	ReceivedAt time.Time `json:"received_at,omitempty"`
	HasLink    bool      `json:"has_link"`
	HasCode    bool      `json:"has_code"`
}

// MarshalJSON redacts the link and the code so a Result cannot leak secrets
// through generic serialization.
func (r Result) MarshalJSON() ([]byte, error) {
	return json.Marshal(safeResult{
		MessageID:  r.MessageID,
		ReceivedAt: r.ReceivedAt,
		HasLink:    r.Link != "",
		HasCode:    r.Code != "",
	})
}

// SafeSummary returns a diagnostic string that never contains the message body,
// the verification link, the OTP code or the subject.
func (r Result) SafeSummary() string {
	return fmt.Sprintf("mailx.Result{message_id=%q received_at=%s has_link=%t has_code=%t}",
		r.MessageID, r.ReceivedAt.UTC().Format(time.RFC3339Nano), r.Link != "", r.Code != "")
}

// String implements fmt.Stringer with the redacted summary, so that %v and %+v
// cannot leak a verification secret.
func (r Result) String() string { return r.SafeSummary() }

// Matcher applies a Config to provider messages. It is safe for concurrent use.
type Matcher struct {
	recipient   string
	allowlist   []senderRule
	subject     []*regexp.Regexp
	link        []*regexp.Regexp
	otp         []otpPattern
	windowStart time.Time
	windowEnd   time.Time
	skew        time.Duration
	maxMessages int

	rejectCodeReuse bool

	mu            sync.Mutex
	seen          map[string]struct{}
	consumedCodes map[string]struct{}
}

// NewMatcher validates the configuration and precompiles every pattern. Invalid
// configurations return a VALIDATION classified error whose summary contains no
// message content and no credential.
func NewMatcher(cfg Config) (*Matcher, error) {
	m := &Matcher{
		allowlist:     make([]senderRule, 0, len(cfg.SenderAllowlist)),
		skew:          cfg.ClockSkew,
		windowStart:   cfg.WindowStart.UTC(),
		windowEnd:     cfg.WindowEnd.UTC(),
		maxMessages:   cfg.MaxMessages,
		seen:          map[string]struct{}{},
		consumedCodes: map[string]struct{}{},

		rejectCodeReuse: cfg.RejectCodeReuse,
	}

	recipient, ok := normalizeAddress(cfg.ExpectedRecipient)
	if !ok {
		return nil, configErr("expected recipient is not a single valid address")
	}
	m.recipient = recipient

	for i, entry := range cfg.SenderAllowlist {
		rule, err := parseSenderRule(entry)
		if err != nil {
			return nil, configErr("sender allowlist entry %d is invalid", i+1)
		}
		m.allowlist = append(m.allowlist, rule)
	}

	var err error
	if m.subject, err = compilePatterns(cfg.SubjectPatterns, true); err != nil {
		return nil, configErr("subject pattern is invalid: %s", regexpCode(err))
	}
	if m.link, err = compilePatterns(cfg.LinkPatterns, true); err != nil {
		return nil, configErr("link pattern is invalid: %s", regexpCode(err))
	}

	if len(cfg.LinkPatterns) == 0 {
		if m.link, err = compilePatterns(defaultLinkPatterns, true); err != nil {
			return nil, configErr("default link pattern is invalid")
		}
	}

	otpSources := cfg.OTPPatterns
	if len(otpSources) == 0 {
		m.otp = defaultOTPPatterns()
	} else {
		for i, expr := range otpSources {
			re, cerr := regexp.Compile(expr)
			if cerr != nil {
				return nil, configErr("OTP pattern %d is invalid: %s", i+1, regexpCode(cerr))
			}
			m.otp = append(m.otp, otpPattern{re: re})
		}
	}

	if m.skew == 0 {
		m.skew = DefaultClockSkew
	}
	if m.skew < 0 {
		return nil, configErr("clock skew must not be negative")
	}
	if m.maxMessages <= 0 {
		m.maxMessages = DefaultMaxMessages
	}
	if !m.windowStart.IsZero() && !m.windowEnd.IsZero() {
		if m.windowEnd.Add(m.skew).Before(m.windowStart.Add(-m.skew)) {
			return nil, configErr("time window end precedes window start")
		}
	}
	return m, nil
}

// ExpectedRecipient returns the normalized address this matcher accepts.
func (m *Matcher) ExpectedRecipient() string { return m.recipient }

// Match returns the first unseen message that satisfies every gate and yields a
// verification link or a code. It marks that message as consumed. Input order is
// preserved, so callers that want the oldest message first must sort the slice.
func (m *Matcher) Match(messages []provider.Message) (Result, bool) {
	return m.MatchNeed(messages, NeedAny)
}

// MatchNeed is Match with an explicit extraction requirement.
func (m *Matcher) MatchNeed(messages []provider.Message, need Need) (Result, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(messages) > m.maxMessages {
		messages = messages[:m.maxMessages]
	}
	for _, msg := range messages {
		id := strings.TrimSpace(msg.ID)
		if id == "" {
			// Without a stable provider ID the message cannot be deduplicated,
			// so it must never be consumed (fail closed).
			continue
		}
		if _, ok := m.seen[id]; ok {
			continue
		}
		if !m.matches(msg) {
			continue
		}
		res := m.extract(msg)
		switch need {
		case NeedLink:
			if res.Link == "" {
				continue
			}
		case NeedCode:
			if res.Code == "" {
				continue
			}
		default:
			if res.Link == "" && res.Code == "" {
				continue
			}
		}
		if m.rejectCodeReuse && res.Code != "" {
			if _, ok := m.consumedCodes[res.Code]; ok {
				continue
			}
		}
		m.seen[id] = struct{}{}
		if m.rejectCodeReuse && res.Code != "" {
			m.consumedCodes[res.Code] = struct{}{}
		}
		return res, true
	}
	return Result{}, false
}

// MarkSeen records a provider message ID as consumed. The engine uses it to
// restore durable deduplication state after a process restart.
func (m *Matcher) MarkSeen(messageID string) {
	id := strings.TrimSpace(messageID)
	if id == "" {
		return
	}
	m.mu.Lock()
	m.seen[id] = struct{}{}
	m.mu.Unlock()
}

// Seen reports whether a provider message ID was already consumed.
func (m *Matcher) Seen(messageID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.seen[strings.TrimSpace(messageID)]
	return ok && strings.TrimSpace(messageID) != ""
}

// matches applies the recipient, sender, time-window and subject gates.
func (m *Matcher) matches(msg provider.Message) bool {
	if !equalAddress(msg.Recipient, m.recipient) {
		return false
	}
	if !m.senderAllowed(msg.Sender) {
		return false
	}
	if !m.inWindow(msg.ReceivedAt) {
		return false
	}
	if len(m.subject) > 0 && !matchAny(m.subject, msg.Subject) {
		return false
	}
	return true
}

// inWindow applies the step time window with the configured clock skew. A zero
// receive time is never trusted.
func (m *Matcher) inWindow(received time.Time) bool {
	if received.IsZero() {
		return false
	}
	received = received.UTC()
	if !m.windowStart.IsZero() && received.Before(m.windowStart.Add(-m.skew)) {
		return false
	}
	if !m.windowEnd.IsZero() && received.After(m.windowEnd.Add(m.skew)) {
		return false
	}
	return true
}

// senderAllowed matches the parsed sender address against the allowlist.
func (m *Matcher) senderAllowed(sender string) bool {
	addr, ok := normalizeAddress(sender)
	if !ok {
		return false
	}
	_, host, _ := strings.Cut(addr, "@")
	if host == "" {
		return false
	}
	for _, rule := range m.allowlist {
		switch rule.kind {
		case senderAddress:
			if addr == rule.value {
				return true
			}
		case senderDomain:
			if host == rule.value {
				return true
			}
		case senderSubdomain:
			// *.example.com matches a.example.com and b.a.example.com but not
			// example.com itself.
			if host != rule.value && strings.HasSuffix(host, "."+rule.value) {
				return true
			}
		}
	}
	return false
}

// extract collects link and code candidates from the transient body fields.
func (m *Matcher) extract(msg provider.Message) Result {
	res := Result{
		MessageID:  strings.TrimSpace(msg.ID),
		ReceivedAt: msg.ReceivedAt.UTC(),
	}
	if addr, ok := normalizeAddress(msg.Recipient); ok {
		res.Recipient = addr
	} else {
		res.Recipient = strings.TrimSpace(msg.Recipient)
	}
	if addr, ok := normalizeAddress(msg.Sender); ok {
		res.Sender = addr
	} else {
		res.Sender = strings.TrimSpace(msg.Sender)
	}
	res.Links = m.extractLinks(msg.Text, msg.HTML)
	if len(res.Links) > 0 {
		res.Link = res.Links[0]
	}
	res.Codes = m.extractCodes(msg.Text, msg.HTML)
	if len(res.Codes) > 0 {
		res.Code = res.Codes[0]
	}
	return res
}

// matchAny reports whether any compiled pattern matches s.
func matchAny(patterns []*regexp.Regexp, s string) bool {
	for _, re := range patterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// compilePatterns compiles user patterns. caseInsensitive prefixes the regular
// expression with an inline case-insensitive group, preserving capture-group
// numbering.
func compilePatterns(patterns []string, caseInsensitive bool) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		expr := p
		if caseInsensitive {
			expr = "(?i)(?:" + p + ")"
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, err
		}
		out = append(out, re)
	}
	return out, nil
}

// regexpCode returns only the parser's short error code (never the expression).
func regexpCode(err error) string {
	var se *syntax.Error
	if errors.As(err, &se) {
		return string(se.Code)
	}
	return "invalid syntax"
}

// configErr builds a VALIDATION error without message content or credentials.
func configErr(format string, args ...any) error {
	return errs.New(errs.ClassValidation, "mailx.config", fmt.Sprintf(format, args...))
}

// ---------------------------------------------------------------------------
// Address canonicalization and allowlist parsing
// ---------------------------------------------------------------------------

type senderKind int

const (
	senderDomain senderKind = iota
	senderAddress
	senderSubdomain
)

type senderRule struct {
	kind  senderKind
	value string
}

// normalizeAddress converts an RFC 5322 style mailbox value into a lowercase
// bare address. It rejects ambiguous values such as address lists, because a
// sender field that carries two addresses must never satisfy an allowlist.
func normalizeAddress(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	// "Name <a@b>" / "<a@b>" -> a@b
	if lt := strings.LastIndexByte(s, '<'); lt >= 0 {
		if gt := strings.IndexByte(s[lt:], '>'); gt > 0 {
			s = s[lt+1 : lt+gt]
		} else {
			return "", false
		}
	} else if i := strings.IndexByte(s, '('); i > 0 {
		// "a@b (Name)" -> a@b
		s = s[:i]
	}
	s = strings.TrimSpace(strings.Trim(s, `"'`))
	s = strings.ToLower(s)
	if s == "" || strings.ContainsAny(s, " \t\r\n<>,;:()[]") {
		return "", false
	}
	if strings.Count(s, "@") != 1 {
		return "", false
	}
	local, domain, _ := strings.Cut(s, "@")
	if !validLocalPart(local) || !validDomain(domain) {
		return "", false
	}
	return s, true
}

func validLocalPart(local string) bool {
	if local == "" || len(local) > 64 {
		return false
	}
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+/=?^_`{|}~.-", r):
		default:
			return false
		}
	}
	return true
}

func validDomain(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

// equalAddress reports whether two mailbox values denote the same address.
func equalAddress(a, b string) bool {
	na, ok := normalizeAddress(a)
	if !ok {
		return false
	}
	nb, ok := normalizeAddress(b)
	if !ok {
		return false
	}
	return na == nb
}

// parseSenderRule parses one allowlist entry.
func parseSenderRule(entry string) (senderRule, error) {
	e := strings.ToLower(strings.TrimSpace(entry))
	if e == "" {
		return senderRule{}, fmt.Errorf("empty entry")
	}
	if strings.ContainsAny(e, " \t\r\n<>,;") {
		return senderRule{}, fmt.Errorf("ambiguous entry")
	}
	if strings.HasPrefix(e, "*.") {
		host := e[2:]
		if !validDomain(host) {
			return senderRule{}, fmt.Errorf("invalid wildcard domain")
		}
		return senderRule{kind: senderSubdomain, value: host}, nil
	}
	if strings.HasPrefix(e, "@") {
		e = e[1:]
	}
	if strings.Contains(e, "@") {
		addr, ok := normalizeAddress(e)
		if !ok {
			return senderRule{}, fmt.Errorf("invalid address")
		}
		return senderRule{kind: senderAddress, value: addr}, nil
	}
	if !validDomain(e) {
		return senderRule{}, fmt.Errorf("invalid domain")
	}
	return senderRule{kind: senderDomain, value: e}, nil
}
