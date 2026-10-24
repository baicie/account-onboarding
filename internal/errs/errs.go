// Package errs defines the domain error taxonomy used across providers, the
// workflow engine and the control-plane API.
//
// The class is the only thing the engine may branch on. Error summaries must be
// safe to publish: no tokens, OTPs, mailbox JWTs, OAuth state or message bodies.
package errs

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Class is the stable, public failure taxonomy defined by docs/04-providers.md.
type Class string

const (
	ClassValidation        Class = "VALIDATION"
	ClassNotAuthorized     Class = "NOT_AUTHORIZED"
	ClassAuthRequired      Class = "AUTH_REQUIRED"
	ClassAuthExpired       Class = "AUTH_EXPIRED"
	ClassRateLimited       Class = "RATE_LIMITED"
	ClassUpstreamTransient Class = "UPSTREAM_TRANSIENT"
	ClassTimeoutUncertain  Class = "TIMEOUT_UNCERTAIN"
	ClassProviderBug       Class = "PROVIDER_BUG"
	ClassCancelled         Class = "CANCELLED"

	// Control-plane only classes. They never drive workflow transitions.
	ClassNotFound Class = "NOT_FOUND"
	ClassConflict Class = "CONFLICT"
	ClassInternal Class = "INTERNAL"
)

// Error is a classified error. Cause is kept for local diagnostics only and is
// never serialized into API responses or events.
type Error struct {
	Class      Class
	Op         string
	Summary    string
	RetryAfter time.Duration
	HTTPStatus int
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Op == "" {
		return fmt.Sprintf("%s: %s", e.Class, e.Summary)
	}
	return fmt.Sprintf("%s: %s: %s", e.Class, e.Op, e.Summary)
}

func (e *Error) Unwrap() error { return e.Cause }

// New builds a classified error with a public-safe summary.
func New(class Class, op, summary string) *Error {
	return &Error{Class: class, Op: op, Summary: summary}
}

// Wrap adds classification and a safe summary around an existing cause. The
// cause is intentionally not rendered by Error() callers publishing to the API.
func Wrap(class Class, op, summary string, cause error) *Error {
	return &Error{Class: class, Op: op, Summary: summary, Cause: cause}
}

// WithRetryAfter returns a copy carrying the provider supplied Retry-After.
func (e *Error) WithRetryAfter(d time.Duration) *Error {
	cp := *e
	cp.RetryAfter = d
	return &cp
}

// WithHTTPStatus returns a copy recording the upstream status code.
func (e *Error) WithHTTPStatus(code int) *Error {
	cp := *e
	cp.HTTPStatus = code
	return &cp
}

// ClassOf classifies any error. Unclassified errors are ProviderBug: an
// unexpected adapter result must not be retried blindly.
func ClassOf(err error) Class {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ClassProviderBug
}

// SummaryOf returns a public-safe summary for any error.
func SummaryOf(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		if e.Summary != "" {
			return e.Summary
		}
		return string(e.Class)
	}
	return "unexpected provider failure"
}

// RetryAfterOf extracts a provider-supplied retry hint, if any.
func RetryAfterOf(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

// HTTPStatusOf returns the upstream HTTP status carried by the error, if any.
func HTTPStatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.HTTPStatus
	}
	return 0
}

// IsRetryable reports whether the class can be retried with backoff. AUTH_*,
// VALIDATION, NOT_AUTHORIZED and PROVIDER_BUG are never retried blindly.
func IsRetryable(class Class) bool {
	switch class {
	case ClassRateLimited, ClassUpstreamTransient, ClassTimeoutUncertain:
		return true
	default:
		return false
	}
}

// Terminal reports whether the class must end the run as FAILED_TERMINAL.
func Terminal(class Class) bool {
	switch class {
	case ClassValidation, ClassNotAuthorized, ClassProviderBug:
		return true
	default:
		return false
	}
}

// FromHTTPStatus maps an upstream HTTP status to a domain class. Unknown status
// codes become PROVIDER_BUG so that unexpected schemas are observable.
func FromHTTPStatus(status int) Class {
	switch {
	case status == http.StatusTooManyRequests:
		return ClassRateLimited
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ClassNotAuthorized
	case status == http.StatusGone:
		return ClassAuthExpired
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return ClassTimeoutUncertain
	case status >= 500:
		return ClassUpstreamTransient
	case status >= 400:
		return ClassValidation
	default:
		return ClassProviderBug
	}
}
