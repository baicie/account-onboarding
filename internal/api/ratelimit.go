package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
)

// rateLimiter is a small fixed-window per-key limiter. It is intentionally
// in-memory: this is a single-instance self-hosted service.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	now     func() time.Time
	buckets map[string]*rateBucket
}

type rateBucket struct {
	count int
	reset time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	if limit <= 0 {
		return nil
	}
	return &rateLimiter{
		limit:   limit,
		window:  window,
		now:     time.Now,
		buckets: map[string]*rateBucket{},
	}
}

// allow reports whether the key may proceed and, when rejected, how long the
// caller should wait before retrying.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok || now.After(b.reset) {
		l.buckets[key] = &rateBucket{count: 1, reset: now.Add(l.window)}
		l.cleanupLocked(now)
		return true, 0
	}
	if b.count >= l.limit {
		return false, b.reset.Sub(now)
	}
	b.count++
	return true, 0
}

// cleanupLocked bounds memory: at most every 1024 calls it drops expired keys.
func (l *rateLimiter) cleanupLocked(now time.Time) {
	if len(l.buckets) < 1024 {
		return
	}
	for k, b := range l.buckets {
		if now.After(b.reset) {
			delete(l.buckets, k)
		}
	}
}

// clientKey derives the limiting key. Forwarded headers are only trusted when
// the operator explicitly configured a TLS-terminating reverse proxy.
func (s *Server) clientKey(r *http.Request) string {
	if s.Cfg.TrustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, ok := strings.Cut(xff, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimit wraps the mux with a stricter bucket for console login attempts and
// a general control-plane bucket. Disabled when the configured limit is 0.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limiter := s.generalLimiter
		if r.Method == http.MethodPost && r.URL.Path == "/ui/login" {
			limiter = s.loginLimiter
		}
		if ok, retry := limiter.allow(s.clientKey(r)); !ok {
			seconds := int(retry.Round(time.Second).Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeError(w, r, errs.New(errs.ClassRateLimited, "http.rate_limit",
				"too many requests; retry after "+strconv.Itoa(seconds)+"s"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
