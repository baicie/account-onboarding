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
	"net/http"
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
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12}
	return &WebhookNotifier{cfg: cfg, client: &http.Client{
		Timeout: cfg.Timeout, Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

type webhookPayload struct {
	IdempotencyKey string  `json:"idempotency_key"`
	Alerts         []Alert `json:"alerts"`
}

func (w *WebhookNotifier) Notify(ctx context.Context, alerts []Alert) error {
	w.mu.Lock()
	if w.cfg.Now().Before(w.openTill) {
		w.mu.Unlock()
		return ErrWebhookCircuitOpen
	}
	w.mu.Unlock()
	body, err := json.Marshal(webhookPayload{IdempotencyKey: deliveryKey(alerts), Alerts: alerts})
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
	req.Header.Set("X-Alert-Idempotency-Key", deliveryKey(alerts))
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
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
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
