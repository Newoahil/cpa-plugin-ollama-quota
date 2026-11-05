package copilot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Kind classifies why a collection attempt did not produce a reading.
//
//   - KindAuth: the token was rejected, or the account has no Copilot access.
//     Ask the operator to sign the account in again; retrying with the same
//     token cannot succeed.
//   - KindRateLimit: upstream asked us to slow down. Wait RetryAfter, then
//     retry; nothing is wrong with the credential.
//   - KindParse: the response arrived but could not be understood — likely a
//     change to the endpoint. Report error, keep the previous reading marked
//     stale, and do not substitute a number.
//   - KindNetwork: a timeout, refused connection, DNS failure or an unsafe
//     redirect. Retrying later is the right response.
//
// The class is carried in a typed Error so a caller branches with errors.As
// rather than matching message text.
type Kind string

const (
	KindAuth      Kind = "auth"
	KindRateLimit Kind = "rate_limit"
	KindParse     Kind = "parse"
	KindNetwork   Kind = "network"
)

// Error is the only error type this package returns for an upstream failure.
//
// Its message is safe to log and to show: it names the class and, when useful,
// the HTTP status, and never contains a token or an Authorization header.
// Detail about a malformed payload is deliberately reduced to its class, so a
// decode error cannot echo credential material back through the message.
type Error struct {
	// Kind is the failure class.
	Kind Kind `json:"kind"`
	// Message is a scrubbed, human-readable explanation.
	Message string `json:"message"`
	// StatusCode is the upstream HTTP status, or 0 when no response arrived.
	StatusCode int `json:"status_code,omitempty"`
	// RetryAfter is how long the caller should wait before trying again. It is
	// set for KindRateLimit (from the Retry-After header when present, else a
	// conservative default).
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	// HasRetryAfter reports whether RetryAfter was communicated upstream.
	HasRetryAfter bool `json:"has_retry_after,omitempty"`

	// sentinel is the only error an Error chains to: context.Canceled or
	// context.DeadlineExceeded, or nil. The raw transport error is deliberately
	// never kept — a *url.Error carries the request URL and an injected
	// transport's error may carry the token verbatim, so exposing it through
	// Unwrap would make the secret reachable even though the message is clean.
	sentinel error
}

// Error implements error.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Message
}

// Unwrap exposes the class sentinel and, when the failure was a cancellation or
// a deadline, that standard sentinel too — so all of these work:
//
//	errors.Is(err, copilot.ErrRateLimit)
//	errors.Is(err, context.DeadlineExceeded)
//	var failure *copilot.Error; errors.As(err, &failure)
//
// Nothing from the transport is chained. Every element Unwrap can return is a
// package constant or a context sentinel, so walking the chain can never yield
// credential material.
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

// Is also compares two *Error values by class, so errors.Is(err, other) holds
// when both describe the same failure kind.
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

// ErrUpstream is the base of the failure hierarchy: every Error from this
// package matches it.
var ErrUpstream = errors.New("copilot upstream failure")

// Sentinel errors, one per class.
var (
	ErrAuth      = fmt.Errorf("%w: authentication failed", ErrUpstream)
	ErrRateLimit = fmt.Errorf("%w: rate limited", ErrUpstream)
	ErrParse     = fmt.Errorf("%w: response could not be parsed", ErrUpstream)
	ErrNetwork   = fmt.Errorf("%w: network error", ErrUpstream)
)

// ErrMissingCredential reports a collection attempt with no usable credential.
// It is an auth-class failure, so callers already handling KindAuth need no
// special case for "nothing configured yet".
var ErrMissingCredential = fmt.Errorf("%w: no GitHub access token is configured", ErrAuth)

// KindOf returns the failure class of err, or "" for a nil error or one this
// package did not produce.
func KindOf(err error) Kind {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Kind
	}
	return ""
}

// RetryAfterOf returns the upstream-communicated backoff for err and whether it
// was communicated. The common case for a rate limit is a duration and true;
// without a Retry-After header the Error still carries a default backoff but
// HasRetryAfter is false.
func RetryAfterOf(err error) (time.Duration, bool) {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.RetryAfter, failure.HasRetryAfter
	}
	return 0, false
}

// defaultRateLimitBackoff is used when upstream sends 429 without Retry-After.
const defaultRateLimitBackoff = 30 * time.Second

// newError builds a classified error. The message must already be scrubbed;
// callers pass constant strings.
func newError(kind Kind, status int, message string) *Error {
	return &Error{Kind: kind, StatusCode: status, Message: message}
}

// errorForStatus classifies a non-2xx status. 401/403 is an auth failure, 429 a
// rate limit, 404 means the account has no Copilot access, and anything else is
// a network-level failure the caller may retry.
func errorForStatus(status int, retryAfter time.Duration, hasRetryAfter bool) *Error {
	switch {
	case status == 401 || status == 403:
		return newError(KindAuth, status,
			fmt.Sprintf("the GitHub token was rejected by the Copilot endpoint (HTTP %d); sign the account in again", status))
	case status == 429:
		failure := newError(KindRateLimit, status,
			fmt.Sprintf("the Copilot endpoint rate limited the request (HTTP %d)", status))
		failure.RetryAfter = retryAfter
		failure.HasRetryAfter = hasRetryAfter
		if failure.RetryAfter <= 0 {
			failure.RetryAfter = defaultRateLimitBackoff
		}
		return failure
	case status == 404:
		return newError(KindAuth, status,
			"the Copilot endpoint reported no Copilot access for this account (HTTP 404)")
	default:
		return newError(KindNetwork, status,
			fmt.Sprintf("the Copilot endpoint returned HTTP %d", status))
	}
}

// redirectError classifies an observed 3xx response. The client never follows
// one: a redirect could carry the token to another host, and a redirect to the
// login page means the credential expired. Only the Location path is inspected
// — never echoed into the message.
func redirectError(status int, location string) *Error {
	target, err := url.Parse(location)
	if err == nil && isLoginPath(target.Path) {
		return newError(KindAuth, status,
			"the Copilot endpoint redirected to the sign-in page; sign the account in again")
	}
	return newError(KindNetwork, status,
		"the Copilot endpoint answered with a redirect, which is never followed")
}

// isLoginPath reports whether a redirect target is a sign-in page.
func isLoginPath(path string) bool {
	for _, segment := range strings.Split(strings.ToLower(path), "/") {
		switch segment {
		case "signin", "login", "sign-in", "log-in", "auth":
			return true
		}
	}
	return false
}

// networkError classifies a transport failure. The message is a constant chosen
// from the error's shape, and the only thing kept for errors.Is is the
// cancellation or deadline sentinel — the error value itself is dropped, so a
// transport that puts the token in its error text cannot leak it through the
// returned chain.
func networkError(err error) *Error {
	failure := &Error{Kind: KindNetwork, Message: networkMessage(err)}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		failure.sentinel = context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		failure.sentinel = context.Canceled
	}
	return failure
}

func networkMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "the Copilot endpoint did not respond before the deadline"
	}
	if errors.Is(err, context.Canceled) {
		return "the request to the Copilot endpoint was cancelled"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the Copilot endpoint name could not be resolved"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "the Copilot endpoint did not respond before the deadline"
	}
	return "the request to the Copilot endpoint failed"
}

// parseError reports a payload this package cannot understand.
//
// The detail is a constant chosen by the parser, never a decoder message or a
// fragment of the body: the body is upstream data and the decoder's error can
// quote it.
func parseError(detail string) *Error {
	if strings.TrimSpace(detail) == "" {
		detail = "the Copilot quota response could not be understood"
	}
	return newError(KindParse, 0, detail)
}
