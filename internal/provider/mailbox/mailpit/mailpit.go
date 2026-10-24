// Package mailpit adapts the Mailpit inbox sink (https://mailpit.axllent.org)
// to the provider.MailboxProvider contract. It is intended for local
// development and sandbox verification only: Mailpit is a shared test sink, not
// a production mailbox provider.
//
// The adapter speaks the Mailpit v1 HTTP API:
//
//	GET    /api/v1/messages?limit=N        list newest messages
//	GET    /api/v1/message/{id}            read one message (including body)
//	DELETE /api/v1/message/{id}            delete one message
//
// CreateAddress performs no network call: the address is generated locally from
// the configured domain. ListMessages filters the shared sink by recipient
// because Mailpit has no per-recipient list endpoint.
//
// Security: the base URL is pinned at construction time to a loopback host, so
// the adapter can never be pointed at an arbitrary upstream. Redirects are
// restricted to the same host. The adapter holds no credentials beyond the
// optional Mailpit HTTP basic auth userinfo, which is rejected in the URL and
// therefore never stored.
package mailpit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

const (
	// DefaultBaseURL is the loopback Mailpit UI/API endpoint from the design
	// docs and the local sandbox compose file.
	DefaultBaseURL = "http://127.0.0.1:8025"
	// DefaultDomain is the local test domain used when no domain is configured.
	DefaultDomain = "mailpit.local"
	// DefaultTimeout bounds every Mailpit HTTP call.
	DefaultTimeout = 10 * time.Second
	// DefaultMaxMessages caps a list/cleanup scan.
	DefaultMaxMessages = 500
	// ContractVersion is the Mailpit adapter contract version locked into runs.
	ContractVersion = "v1"
	// ProviderName is the descriptor name used for registry lookups.
	ProviderName = "mailpit"

	maxResponseBytes = 8 << 20
	maxRedirects     = 3
)

// errRedirectBlocked is returned by the redirect guard when a response tries to
// move the request to a different host or downgrade the scheme.
var errRedirectBlocked = errors.New("mailpit: redirect to a different origin refused")

// Config configures the Mailpit adapter. The zero value is usable.
type Config struct {
	// BaseURL must point at a loopback host (127.0.0.0/8, ::1 or localhost).
	// Defaults to DefaultBaseURL.
	BaseURL string
	// Domain is the sink domain used to synthesize addresses. Defaults to
	// DefaultDomain.
	Domain string
	// AllowedDomains optionally narrows which domains CreateAddress accepts.
	// When empty, only Domain is allowed.
	AllowedDomains []string
	// Timeout bounds each HTTP request. Defaults to DefaultTimeout.
	Timeout time.Duration
	// MaxMessages caps list and cleanup scans. Defaults to DefaultMaxMessages.
	MaxMessages int
	// HTTPClient optionally supplies a transport. Its timeout and redirect
	// policy are replaced by the adapter's own so the loopback pin cannot be
	// bypassed.
	HTTPClient *http.Client
	// Disabled gates the adapter: the descriptor reports Enabled=false and
	// every call fails with NOT_AUTHORIZED before doing any work.
	Disabled bool
}

// Provider implements provider.MailboxProvider and provider.MailboxCleaner
// against a local Mailpit instance.
type Provider struct {
	base        *url.URL
	domain      string
	allowed     []string
	timeout     time.Duration
	maxMessages int
	client      *http.Client
	disabled    bool
}

var (
	_ provider.MailboxProvider = (*Provider)(nil)
	_ provider.MailboxCleaner  = (*Provider)(nil)
)

// New validates the configuration and builds the adapter. It performs no
// network I/O.
func New(cfg Config) (*Provider, error) {
	base, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}

	domain := strings.ToLower(strings.TrimSpace(cfg.Domain))
	if domain == "" {
		domain = DefaultDomain
	}
	if !validDomainName(domain) {
		return nil, configErr("default domain is not a valid DNS name")
	}

	allowed := make([]string, 0, len(cfg.AllowedDomains)+1)
	allowed = append(allowed, domain) // the configured default is always allowed
	for _, entry := range cfg.AllowedDomains {
		d := strings.ToLower(strings.TrimSpace(entry))
		if !validDomainName(d) {
			return nil, configErr("allowed domain is not a valid DNS name")
		}
		if d == domain {
			continue
		}
		allowed = append(allowed, d)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxMessages := cfg.MaxMessages
	if maxMessages <= 0 {
		maxMessages = DefaultMaxMessages
	}

	p := &Provider{
		base:        base,
		domain:      domain,
		allowed:     allowed,
		timeout:     timeout,
		maxMessages: maxMessages,
		disabled:    cfg.Disabled,
	}

	client := http.Client{}
	if cfg.HTTPClient != nil {
		client = *cfg.HTTPClient
	}
	client.Timeout = timeout
	client.CheckRedirect = p.checkRedirect
	p.client = &client
	return p, nil
}

// Descriptor implements provider.MailboxProvider.
func (p *Provider) Descriptor() provider.Descriptor {
	d := provider.Descriptor{
		Name:            ProviderName,
		Kind:            provider.KindMailbox,
		ContractVersion: ContractVersion,
		Enabled:         !p.disabled,
		Live:            false,
		Capabilities: []string{
			"address.create",
			"message.list",
			"message.get",
			"address.cleanup",
		},
	}
	if p.disabled {
		d.RestrictionReason = "mailpit adapter is disabled by configuration"
	}
	return d
}

// CreateAddress synthesizes an address from the configured domain. The Mailpit
// API has no address concept, so this is a local operation by design.
func (p *Provider) CreateAddress(ctx context.Context, req provider.CreateAddressRequest) (provider.Mailbox, error) {
	const op = "mailpit.create_address"
	if err := p.ensureEnabled(op); err != nil {
		return provider.Mailbox{}, err
	}
	if err := ctx.Err(); err != nil {
		return provider.Mailbox{}, ctxError(op)
	}

	name, err := p.localPart(req)
	if err != nil {
		return provider.Mailbox{}, err
	}
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	if domain == "" {
		domain = p.domain
	}
	if !validDomainName(domain) {
		return provider.Mailbox{}, errs.New(errs.ClassValidation, op, "requested mailbox domain is not a valid DNS name")
	}
	if !p.domainAllowed(domain) {
		return provider.Mailbox{}, errs.New(errs.ClassValidation, op, "requested mailbox domain is not allowed for this provider")
	}

	address := name + "@" + domain
	return provider.Mailbox{
		ID:        address,
		Address:   address,
		Provider:  ProviderName,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// ListMessages returns messages delivered to mailbox, newest first as Mailpit
// reports them. The shared sink is filtered by recipient so another run's mail
// never leaks into this mailbox view.
func (p *Provider) ListMessages(ctx context.Context, mailbox provider.Mailbox, since time.Time) ([]provider.Message, error) {
	const op = "mailpit.list_messages"
	if err := p.ensureEnabled(op); err != nil {
		return nil, err
	}
	address, err := requireMailboxAddress(op, mailbox)
	if err != nil {
		return nil, err
	}

	items, err := p.list(ctx, op)
	if err != nil {
		if errs.ClassOf(err) == errs.ClassNotFound {
			// The list endpoint is fixed for the adapter; a 404 means a wrong
			// base path, not a missing message.
			return nil, errs.New(errs.ClassProviderBug, op, "mailpit messages endpoint is missing").WithHTTPStatus(http.StatusNotFound)
		}
		return nil, err
	}

	out := make([]provider.Message, 0, len(items))
	for _, item := range items {
		matched, ok := item.recipientMatch(address)
		if !ok {
			continue
		}
		received, err := item.createdTime(op)
		if err != nil {
			return nil, err
		}
		if !since.IsZero() && !received.IsZero() && received.Before(since.UTC()) {
			continue
		}
		msg, err := item.toMessage(op, matched)
		if err != nil {
			return nil, err
		}
		msg.MailboxID = mailbox.ID
		out = append(out, msg)
	}
	return out, nil
}

// GetMessage reads one message, including its transient body fields.
func (p *Provider) GetMessage(ctx context.Context, mailbox provider.Mailbox, id string) (provider.Message, error) {
	const op = "mailpit.get_message"
	if err := p.ensureEnabled(op); err != nil {
		return provider.Message{}, err
	}
	address, err := requireMailboxAddress(op, mailbox)
	if err != nil {
		return provider.Message{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return provider.Message{}, errs.New(errs.ClassValidation, op, "message id is required")
	}

	item, err := p.get(ctx, op, id)
	if err != nil {
		return provider.Message{}, err
	}
	recipient, _ := item.recipientMatch(address)
	if recipient == "" {
		recipient = item.firstRecipient()
	}
	msg, err := item.toMessage(op, recipient)
	if err != nil {
		return provider.Message{}, err
	}
	msg.MailboxID = mailbox.ID
	return msg, nil
}

// DeleteAddress clears the messages addressed to mailbox from the Mailpit sink.
// Mailpit has no delete-by-recipient endpoint, so the adapter enumerates the
// sink and deletes matching messages one by one. A message that is already gone
// is not an error, which makes retries idempotent.
func (p *Provider) DeleteAddress(ctx context.Context, mailbox provider.Mailbox) error {
	const op = "mailpit.delete_address"
	if err := p.ensureEnabled(op); err != nil {
		return err
	}
	address, err := requireMailboxAddress(op, mailbox)
	if err != nil {
		return err
	}
	items, err := p.list(ctx, op)
	if err != nil {
		if errs.ClassOf(err) == errs.ClassNotFound {
			return errs.New(errs.ClassProviderBug, op, "mailpit messages endpoint is missing").WithHTTPStatus(http.StatusNotFound)
		}
		return err
	}
	for _, item := range items {
		if _, ok := item.recipientMatch(address); !ok {
			continue
		}
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		if err := p.deleteMessage(ctx, op, item.ID); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Internal operations
// ---------------------------------------------------------------------------

func (p *Provider) list(ctx context.Context, op string) ([]messageItem, error) {
	query := url.Values{}
	query.Set("limit", strconv.Itoa(p.maxMessages))

	var body json.RawMessage
	if err := p.do(ctx, op, http.MethodGet, "/api/v1/messages", query, &body); err != nil {
		return nil, err
	}
	return parseMessageList(body, op)
}

func (p *Provider) get(ctx context.Context, op, id string) (messageItem, error) {
	var item messageItem
	path := "/api/v1/message/" + url.PathEscape(id)
	if err := p.do(ctx, op, http.MethodGet, path, nil, &item); err != nil {
		return messageItem{}, err
	}
	return item, nil
}

func (p *Provider) deleteMessage(ctx context.Context, op, id string) error {
	if err := p.do(ctx, op, http.MethodDelete, "/api/v1/message/"+url.PathEscape(id), nil, nil); err != nil {
		if errs.ClassOf(err) == errs.ClassNotFound {
			return nil // already deleted
		}
		return err
	}
	return nil
}

// do performs one HTTP request and decodes the JSON response into out.
func (p *Provider) do(ctx context.Context, op, method, path string, query url.Values, out any) error {
	if err := ctx.Err(); err != nil {
		return ctxError(op)
	}
	reqCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	u := *p.base
	u.Path = joinPath(u.Path, path)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(reqCtx, method, u.String(), nil)
	if err != nil {
		return errs.New(errs.ClassInternal, op, "could not build mailpit request")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return p.transportError(op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return p.transportError(op, err)
	}
	if len(body) > maxResponseBytes {
		return errs.New(errs.ClassProviderBug, op, "mailpit response exceeded the size limit")
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(op, resp)
	}
	if out == nil {
		return nil
	}
	if len(body) == 0 {
		return errs.New(errs.ClassProviderBug, op, "mailpit returned an empty response body").WithHTTPStatus(resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errs.New(errs.ClassProviderBug, op, "mailpit returned a non-JSON body").WithHTTPStatus(resp.StatusCode)
	}
	return nil
}

// transportError classifies transport failures. Mailpit operations are read
// mostly and the delete is idempotent, so a timeout is retryable
// (UPSTREAM_TRANSIENT) rather than ambiguous.
func (p *Provider) transportError(op string, err error) error {
	if errors.Is(err, context.Canceled) {
		return errs.Wrap(errs.ClassCancelled, op, "mailpit request cancelled", err)
	}
	if errors.Is(err, errRedirectBlocked) {
		return errs.Wrap(errs.ClassProviderBug, op, "mailpit attempted a cross-origin redirect", err)
	}
	if isTimeout(err) {
		return errs.Wrap(errs.ClassUpstreamTransient, op, "mailpit request timed out", err)
	}
	return errs.Wrap(errs.ClassUpstreamTransient, op, "mailpit request failed", err)
}

// statusError maps an upstream HTTP status to the domain taxonomy. Error text
// never includes the response body.
func statusError(op string, resp *http.Response) error {
	class := errs.FromHTTPStatus(resp.StatusCode)
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		class = errs.ClassValidation
	case http.StatusNotFound:
		class = errs.ClassNotFound
	case http.StatusConflict:
		class = errs.ClassConflict
	case http.StatusGone:
		class = errs.ClassAuthExpired
	}
	err := errs.New(class, op, fmt.Sprintf("mailpit returned HTTP %d", resp.StatusCode)).WithHTTPStatus(resp.StatusCode)
	if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); retryAfter > 0 {
		err = err.WithRetryAfter(retryAfter)
	}
	return err
}

// checkRedirect confines redirects to the configured origin so a malicious or
// misconfigured sink cannot bounce the adapter elsewhere.
func (p *Provider) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= maxRedirects {
		return errors.New("mailpit: too many redirects")
	}
	origin := via[0].URL
	if !strings.EqualFold(req.URL.Scheme, origin.Scheme) || !strings.EqualFold(req.URL.Host, origin.Host) {
		return errRedirectBlocked
	}
	return nil
}

func (p *Provider) ensureEnabled(op string) error {
	if p.disabled {
		return provider.NewDisallowedError(provider.KindMailbox, ProviderName, "mailpit adapter is disabled by configuration")
	}
	return nil
}

func (p *Provider) domainAllowed(domain string) bool {
	for _, allowed := range p.allowed {
		if domain == allowed {
			return true
		}
	}
	return false
}

// localPart chooses the generated address local part: the explicit name, a
// stable derivation from the idempotency key, or a random local part.
func (p *Provider) localPart(req provider.CreateAddressRequest) (string, error) {
	const op = "mailpit.create_address"
	if name := strings.ToLower(strings.TrimSpace(req.Name)); name != "" {
		if !validLocalPart(name) {
			return "", errs.New(errs.ClassValidation, op, "requested mailbox name is not a safe local part")
		}
		return name, nil
	}
	if key := strings.TrimSpace(req.Key); key != "" {
		sum := sha256.Sum256([]byte(key))
		return "k" + hex.EncodeToString(sum[:6]), nil
	}
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", errs.New(errs.ClassInternal, op, "could not generate a mailbox local part")
	}
	return "r" + hex.EncodeToString(buf), nil
}

// ---------------------------------------------------------------------------
// Wire types and normalization
// ---------------------------------------------------------------------------

// messageItem mirrors a Mailpit message. Mailpit uses capitalized JSON keys;
// encoding/json matches keys case-insensitively, so both spellings decode.
type messageItem struct {
	ID        string       `json:"ID"`
	MessageID string       `json:"MessageID"`
	From      addressValue `json:"From"`
	To        addressList  `json:"To"`
	Subject   string       `json:"Subject"`
	Created   string       `json:"Created"`
	// Date is present on the single-message detail endpoint instead of Created.
	Date string `json:"Date"`
	Text string `json:"Text"`
	HTML string `json:"HTML"`
}

// addressValue decodes a Mailpit address that may be an object
// {"Name","Address"} or a plain string.
type addressValue struct {
	Name    string
	Address string
}

func (a *addressValue) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*a = addressValue{}
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*a = addressValue{Address: s}
		return nil
	}
	var obj struct {
		Name    string `json:"Name"`
		Address string `json:"Address"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*a = addressValue{Name: obj.Name, Address: obj.Address}
	return nil
}

// String renders the address in RFC 5322 style so mailx can parse it.
func (a addressValue) String() string {
	name := strings.TrimSpace(a.Name)
	address := strings.TrimSpace(a.Address)
	switch {
	case address == "":
		return name
	case name == "":
		return address
	default:
		return name + " <" + address + ">"
	}
}

// addressList decodes a list of Mailpit addresses from an array, a single
// object or a bare string.
type addressList []addressValue

func (l *addressList) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*l = nil
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var items []addressValue
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		*l = items
		return nil
	}
	var single addressValue
	if err := single.UnmarshalJSON(data); err != nil {
		return err
	}
	*l = addressList{single}
	return nil
}

// recipientMatch returns the canonical address of mailbox when the message was
// addressed to it.
func (m messageItem) recipientMatch(address string) (string, bool) {
	for _, candidate := range m.To {
		canonical, ok := canonicalMailbox(candidate.Address)
		if !ok {
			if canonical, ok = canonicalMailbox(candidate.Name); !ok {
				continue
			}
		}
		if canonical == address {
			return canonical, true
		}
	}
	return "", false
}

func (m messageItem) firstRecipient() string {
	for _, candidate := range m.To {
		if canonical, ok := canonicalMailbox(candidate.Address); ok {
			return canonical
		}
	}
	return ""
}

func (m messageItem) createdTime(op string) (time.Time, error) {
	// The list endpoint returns Created; the detail endpoint returns Date.
	// Prefer Created and fall back to Date so a hydrated message keeps its
	// receive time for the matching time window.
	raw := strings.TrimSpace(m.Created)
	if raw == "" {
		raw = strings.TrimSpace(m.Date)
	}
	if raw == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", time.RFC1123Z, time.RFC1123} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, errs.New(errs.ClassProviderBug, op, "mailpit returned an unexpected Created timestamp")
}

func (m messageItem) toMessage(op, recipient string) (provider.Message, error) {
	received, err := m.createdTime(op)
	if err != nil {
		return provider.Message{}, err
	}
	return provider.Message{
		ID:         m.effectiveID(),
		Recipient:  recipient,
		Sender:     m.From.String(),
		Subject:    m.Subject,
		ReceivedAt: received,
		Text:       m.Text,
		HTML:       m.HTML,
	}, nil
}

// effectiveID prefers the Mailpit API identifier (used in paths) and falls back
// to the RFC 5322 Message-ID so deduplication always has a stable key.
func (m messageItem) effectiveID() string {
	if id := strings.TrimSpace(m.ID); id != "" {
		return id
	}
	return strings.TrimSpace(m.MessageID)
}

// parseMessageList accepts both the documented wrapper object and a bare array.
func parseMessageList(body json.RawMessage, op string) ([]messageItem, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, errs.New(errs.ClassProviderBug, op, "mailpit returned an empty response body")
	}
	if strings.HasPrefix(trimmed, "[") {
		var items []messageItem
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, errs.New(errs.ClassProviderBug, op, "mailpit returned malformed messages JSON")
		}
		return items, nil
	}
	var wrapper struct {
		Messages []messageItem `json:"messages"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, errs.New(errs.ClassProviderBug, op, "mailpit returned malformed messages JSON")
	}
	if wrapper.Messages == nil {
		return nil, errs.New(errs.ClassProviderBug, op, "mailpit messages response has no messages field")
	}
	return wrapper.Messages, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseBaseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, configErr("base url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, configErr("base url must use http or https")
	}
	if u.User != nil {
		return nil, configErr("base url must not embed credentials")
	}
	if u.Host == "" {
		return nil, configErr("base url must include a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, configErr("base url must not include a query or fragment")
	}
	if !isLoopbackHost(u.Hostname()) {
		return nil, configErr("base url must point at a loopback host")
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func configErr(summary string) error {
	return errs.New(errs.ClassValidation, "mailpit.config", summary)
}

func ctxError(op string) error {
	return errs.New(errs.ClassCancelled, op, "request cancelled")
}

func requireMailboxAddress(op string, mailbox provider.Mailbox) (string, error) {
	address, ok := canonicalMailbox(mailbox.Address)
	if !ok {
		return "", errs.New(errs.ClassValidation, op, "mailbox address is missing or invalid")
	}
	return address, nil
}

// canonicalMailbox lowercases an address and strips a display name.
func canonicalMailbox(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if lt := strings.LastIndexByte(s, '<'); lt >= 0 {
		if gt := strings.IndexByte(s[lt:], '>'); gt > 0 {
			s = s[lt+1 : lt+gt]
		} else {
			return "", false
		}
	}
	s = strings.ToLower(strings.TrimSpace(strings.Trim(s, `"'`)))
	if s == "" || strings.Count(s, "@") != 1 || strings.ContainsAny(s, " \t\r\n,;<>") {
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
		case strings.ContainsRune("._-", r):
		default:
			return false
		}
	}
	return true
}

func validDomainName(domain string) bool {
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

func joinPath(base, suffix string) string {
	base = strings.TrimSuffix(base, "/")
	if suffix == "" {
		return base
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return base + suffix
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delta := when.Sub(now); delta > 0 {
			return delta
		}
	}
	return 0
}
