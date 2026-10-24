// Package logging provides structured JSON logging with mandatory redaction.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/baicie/account-onboarding/internal/security"
)

type redactingHandler struct {
	next     slog.Handler
	redactor *security.Redactor
}

func (h *redactingHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.next.Enabled(ctx, lvl)
}

func (h *redactingHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.redactor.Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactingHandler) redactAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.redactor.Redact(a.Value.String()))
	case slog.KindGroup:
		group := a.Value.Group()
		out := make([]slog.Attr, 0, len(group))
		for _, g := range group {
			out = append(out, h.redactAttr(g))
		}
		return slog.Group(a.Key, attrsToAny(out)...)
	default:
		return a
	}
}

func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, len(attrs))
	for i, a := range attrs {
		out[i] = a
	}
	return out
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		redacted = append(redacted, h.redactAttr(a))
	}
	return &redactingHandler{next: h.next.WithAttrs(redacted), redactor: h.redactor}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name), redactor: h.redactor}
}

// ParseLevel maps a config string to a slog level.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New builds a JSON logger whose output is redacted.
func New(level string, redactor *security.Redactor) *slog.Logger {
	if redactor == nil {
		redactor = security.NewRedactor()
	}
	base := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: ParseLevel(level)})
	return slog.New(&redactingHandler{next: base, redactor: redactor})
}

// NewWithWriter is used by tests.
func NewWithWriter(w io.Writer, level string, redactor *security.Redactor) *slog.Logger {
	if redactor == nil {
		redactor = security.NewRedactor()
	}
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: ParseLevel(level)})
	return slog.New(&redactingHandler{next: base, redactor: redactor})
}
