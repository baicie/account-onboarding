package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"
)

var (
	ErrWebhookDisabled    = errors.New("alerts: webhook disabled")
	ErrWebhookPermanent   = errors.New("alerts: webhook permanent rejection")
	ErrWebhookCircuitOpen = errors.New("alerts: webhook circuit open")
)

// WebhookConfig is deliberately opt-in. The URL must be HTTPS and its exact
// host:port must be allowlisted; redirects are never followed. RootCAs is used
// by tests/operators with a private CA; production should use system roots.
type WebhookConfig struct {
	URL              string
	AllowedHosts     []string
	Secret           []byte
	Timeout          time.Duration
	MaxResponseByte  int64
	FailureThreshold int
	OpenFor          time.Duration
	RootCAs          *x509.CertPool
	Now              func() time.Time
	// AllowLoopback is for loopback TLS tests only; production leaves it false.
	AllowLoopback bool
	// ResolveIP is a deterministic DNS test seam; nil uses the system resolver.
	ResolveIP func(context.Context, string) ([]net.IP, error)
}

type WebhookNotifier struct {
	cfg      WebhookConfig
	client   *http.Client
	mu       sync.Mutex
	fails    int
	openTill time.Time
}

func NewWebhookNotifier(cfg WebhookConfig) (*WebhookNotifier, error) {
	if cfg.URL == "" {
		return nil, ErrWebhookDisabled
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("alerts: webhook URL must be HTTPS without userinfo")
	}
	allowed := false
	for _, h := range cfg.AllowedHosts {
		if h == u.Host {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("alerts: webhook host %q is not allowlisted", u.Host)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxResponseByte <= 0 {
		cfg.MaxResponseByte = 64 * 1024
	}
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.OpenFor <= 0 {
		cfg.OpenFor = time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ResolveIP == nil {
		cfg.ResolveIP = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			ips := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
			return ips, nil
		}
	}
	tr := &http.Transport{
		Proxy:                 nil, // never inherit HTTP(S)_PROXY
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   cfg.Timeout,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12},
	}
	w := &WebhookNotifier{cfg: cfg, client: &http.Client{
		Timeout: cfg.Timeout, Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := w.cfg.ResolveIP(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("alerts: webhook DNS returned no addresses")
		}
		for _, ip := range ips {
			if err := validateEgressIP(ip, w.cfg.AllowLoopback); err != nil {
				return nil, fmt.Errorf("alerts: webhook egress blocked: %w", err)
			}
		}
		d := net.Dialer{Timeout: w.cfg.Timeout}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
	return w, nil
}

type webhookPayload struct {
	IdempotencyKey string  `json:"idempotency_key"`
	DeliveryID     string  `json:"delivery_id,omitempty"`
	AlertID        string  `json:"alert_id,omitempty"`
	Alerts         []Alert `json:"alerts"`
}

// DeliveryEnvelope distinguishes a durable alert (logical identity) from one
// delivery attempt. Retries keep AlertID stable while DeliveryID changes.
type DeliveryEnvelope struct {
	DeliveryID string
	AlertID    string
	Alerts     []Alert
}

// DeliveryNotifier is implemented by transports that can surface stable
// delivery/logical identities to the receiver.
type DeliveryNotifier interface {
	NotifyDelivery(context.Context, DeliveryEnvelope) error
}

func (w *WebhookNotifier) Notify(ctx context.Context, alerts []Alert) error {
	return w.send(ctx, alerts, "", "")
}

func (w *WebhookNotifier) NotifyDelivery(ctx context.Context, env DeliveryEnvelope) error {
	return w.send(ctx, env.Alerts, env.DeliveryID, env.AlertID)
}

func (w *WebhookNotifier) send(ctx context.Context, alerts []Alert, deliveryID, alertID string) error {
	w.mu.Lock()
	if w.cfg.Now().Before(w.openTill) {
		w.mu.Unlock()
		return ErrWebhookCircuitOpen
	}
	w.mu.Unlock()
	key := alertID
	if key == "" {
		key = deliveryKey(alerts)
	}
	body, err := json.Marshal(webhookPayload{IdempotencyKey: key, DeliveryID: deliveryID, AlertID: alertID, Alerts: alerts})
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Alert-Idempotency-Key", key)
	if deliveryID != "" {
		req.Header.Set("X-Alert-Delivery-ID", deliveryID)
	}
	if alertID != "" {
		req.Header.Set("X-Alert-Logical-ID", alertID)
	}
	if len(w.cfg.Secret) > 0 {
		mac := hmac.New(sha256.New, w.cfg.Secret)
		_, _ = mac.Write(body)
		req.Header.Set("X-Onboard-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		w.failure()
		return err
	}
	defer resp.Body.Close()
	n, copyErr := io.Copy(io.Discard, io.LimitReader(resp.Body, w.cfg.MaxResponseByte+1))
	if copyErr != nil {
		w.failure()
		return copyErr
	}
	if n > w.cfg.MaxResponseByte {
		w.failure()
		return errors.New("alerts: webhook response too large")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		w.mu.Lock()
		w.fails = 0
		w.openTill = time.Time{}
		w.mu.Unlock()
		return nil
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 &&
		resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
		return fmt.Errorf("%w: HTTP %d", ErrWebhookPermanent, resp.StatusCode)
	}
	w.failure()
	return fmt.Errorf("alerts: webhook HTTP %d", resp.StatusCode)
}

func (w *WebhookNotifier) failure() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fails++
	if w.fails >= w.cfg.FailureThreshold {
		w.openTill = w.cfg.Now().Add(w.cfg.OpenFor)
	}
}

func deliveryKey(alerts []Alert) string {
	h := sha256.New()
	for _, a := range alerts {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", a.Rule, a.Severity, a.Message)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validateEgressIP(ip net.IP, allowLoopback bool) error {
	if ip == nil {
		return errors.New("nil IP")
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return fmt.Errorf("invalid IP %v", ip)
	}
	addr = addr.Unmap()
	if addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return fmt.Errorf("non-routable address %s", addr)
	}
	if addr.IsLoopback() {
		if allowLoopback {
			return nil
		}
		return fmt.Errorf("loopback address %s", addr)
	}
	if addr.IsPrivate() {
		return fmt.Errorf("private address %s", addr)
	}
	if addr.Is4() && netip.MustParsePrefix("100.64.0.0/10").Contains(addr) {
		return fmt.Errorf("carrier-grade NAT address %s", addr)
	}
	// Explicit metadata/RFC1918 special cases are covered by IsPrivate, but
	// keep 169.254.169.254 protected if a platform reports non-standard flags.
	if addr == netip.MustParseAddr("169.254.169.254") {
		return errors.New("metadata address blocked")
	}
	return nil
}

// IsPermanentDeliveryError distinguishes protocol-level rejection (no useful
// retry, e.g. 401/403/404) from transient/network/circuit/timeout errors.
func IsPermanentDeliveryError(err error) bool {
	return errors.Is(err, ErrWebhookPermanent)
}
