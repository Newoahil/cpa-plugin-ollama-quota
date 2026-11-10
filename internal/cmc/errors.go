package cmc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Kind classifies why a collection attempt did not produce a reading.
type Kind string

const (
	// KindAuth: the key was rejected. Re-entering it is the only fix.
	KindAuth Kind = "auth"
	// KindRateLimit: upstream asked us to slow down. Wait and retry.
	KindRateLimit Kind = "rate_limit"
	// KindParse: the response arrived but could not be understood.
	KindParse Kind = "parse"
	// KindNetwork: a timeout, refused connection, DNS failure or a redirect.
	KindNetwork Kind = "network"
)

// Error is the only error type this package returns for an upstream failure.
// Its message never contains a key, an Authorization header or a request URL
// that could embed one.
type Error struct {
	Kind       Kind
	Message    string
	StatusCode int
	// Endpoint names the fixed path that failed: "credits" or "subscriptions".
	Endpoint string
	// RetryAfter is how long the caller should wait before trying again.
	RetryAfter time.Duration
	// HasRetryAfter reports whether RetryAfter was communicated upstream.
	HasRetryAfter bool

	// sentinel is the only thing Unwrap chains to: a context sentinel or nil.
	// The transport's own error is never kept, because it may embed the key.
	sentinel error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Message
}

// Unwrap exposes the class sentinel and, when the failure was a cancellation
// or a deadline, that standard sentinel too.
func (e *Error) Unwrap() []error {
	if e == nil {
		return nil
	}
	errs := []error{e.Kind.sentinel()}
	if e.sentinel != nil {
		errs = append(errs, e.sentinel)
	}
	return errs
}

func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e.Kind == other.Kind
}

func (k Kind) sentinel() error {
	switch k {
	case KindAuth:
		return ErrAuth
	case KindRateLimit:
		return ErrRateLimit
	case KindParse:
		return ErrParse
	case KindNetwork:
		return ErrNetwork
	default:
		return ErrUpstream
	}
}

// ErrUpstream is the base of the failure hierarchy.
var ErrUpstream = errors.New("Command Code upstream failure")

var (
	ErrAuth      = fmt.Errorf("%w: authentication failed", ErrUpstream)
	ErrRateLimit = fmt.Errorf("%w: rate limited", ErrUpstream)
	ErrParse     = fmt.Errorf("%w: response could not be parsed", ErrUpstream)
	ErrNetwork   = fmt.Errorf("%w: network error", ErrUpstream)
)

// ErrMissingCredential reports an attempt with no usable key.
var ErrMissingCredential = fmt.Errorf("%w: no API key is configured", ErrAuth)

// KindOf returns the failure class of err, or "" for a nil or foreign error.
//
// The sentinel fallback covers the errors that are not *Error values: a
// credential that is missing outright never reached upstream, so it has no
// status or endpoint, but it is still an auth-class failure and callers must
// see it as one.
func KindOf(err error) Kind {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Kind
	}
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrAuth):
		return KindAuth
	case errors.Is(err, ErrRateLimit):
		return KindRateLimit
	case errors.Is(err, ErrParse):
		return KindParse
	case errors.Is(err, ErrNetwork):
		return KindNetwork
	default:
		return ""
	}
}

// RetryAfterOf returns the upstream-communicated backoff for err.
func RetryAfterOf(err error) (time.Duration, bool) {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.RetryAfter, failure.HasRetryAfter
	}
	return 0, false
}

// defaultRateLimitBackoff is used when upstream sends 429 without Retry-After.
const defaultRateLimitBackoff = 30 * time.Second

func newError(kind Kind, endpoint string, status int, message string) *Error {
	return &Error{Kind: kind, Endpoint: endpoint, StatusCode: status, Message: message}
}

func errorForStatus(endpoint string, status int) *Error {
	switch {
	case status == 401 || status == 403:
		return newError(KindAuth, endpoint, status,
			fmt.Sprintf("the Command Code API key was rejected by the %s endpoint (HTTP %d); re-enter a valid key", endpoint, status))
	case status == 429:
		failure := newError(KindRateLimit, endpoint, status,
			fmt.Sprintf("the Command Code %s endpoint rate limited the request (HTTP 429)", endpoint))
		failure.RetryAfter = defaultRateLimitBackoff
		return failure
	default:
		return newError(KindNetwork, endpoint, status,
			fmt.Sprintf("the Command Code %s endpoint returned HTTP %d", endpoint, status))
	}
}

// rateLimited builds a 429 error, reading Retry-After when present.
func rateLimited(endpoint string, header map[string][]string) *Error {
	failure := newError(KindRateLimit, endpoint, 429,
		fmt.Sprintf("the Command Code %s endpoint rate limited the request (HTTP 429)", endpoint))
	failure.RetryAfter = defaultRateLimitBackoff
	if wait, ok := parseRetryAfter(header); ok {
		failure.RetryAfter = wait
		failure.HasRetryAfter = true
	}
	return failure
}

// maxRetryAfterSeconds bounds a delay-seconds value before it is multiplied, so
// a hostile value cannot overflow into a negative duration.
const maxRetryAfterSeconds = int64(24 * 60 * 60)

func parseRetryAfter(header map[string][]string) (time.Duration, bool) {
	if header == nil {
		return 0, false
	}
	for name, values := range header {
		if !strings.EqualFold(name, "Retry-After") {
			continue
		}
		for _, raw := range values {
			value := strings.TrimSpace(raw)
			if value == "" {
				continue
			}
			if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
				if seconds < 0 {
					continue
				}
				if seconds > maxRetryAfterSeconds {
					return 24 * time.Hour, true
				}
				return time.Duration(seconds) * time.Second, true
			}
			if when, err := http.ParseTime(value); err == nil {
				return clampBackoff(time.Until(when)), true
			}
		}
	}
	return 0, false
}

func clampBackoff(wait time.Duration) time.Duration {
	if wait < 0 {
		return 0
	}
	if wait > 24*time.Hour {
		return 24 * time.Hour
	}
	return wait
}

// redirectError classifies an observed 3xx. The client never follows one: a
// redirect could carry the key to another host.
func redirectError(endpoint, location string) *Error {
	target, err := url.Parse(location)
	if err == nil && isLoginPath(target.Path) {
		return newError(KindAuth, endpoint, 0,
			"the Command Code API key was rejected (upstream redirected to the login page); re-enter a valid key")
	}
	return newError(KindNetwork, endpoint, 0,
		fmt.Sprintf("the Command Code %s endpoint answered with a redirect, which is never followed", endpoint))
}

func isLoginPath(path string) bool {
	for _, segment := range strings.Split(strings.ToLower(path), "/") {
		switch segment {
		case "signin", "login", "sign-in", "log-in", "auth":
			return true
		}
	}
	return false
}

// networkError classifies a transport failure without keeping the transport's
// own error, which may embed the key.
func networkError(endpoint string, err error) *Error {
	failure := &Error{Kind: KindNetwork, Endpoint: endpoint, Message: networkMessage(endpoint, err)}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		failure.sentinel = context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		failure.sentinel = context.Canceled
	}
	return failure
}

func networkMessage(endpoint string, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("the Command Code %s endpoint did not respond before the deadline", endpoint)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Sprintf("the request to the Command Code %s endpoint was cancelled", endpoint)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the Command Code endpoint name could not be resolved"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Sprintf("the Command Code %s endpoint did not respond before the deadline", endpoint)
	}
	return fmt.Sprintf("the request to the Command Code %s endpoint failed", endpoint)
}
