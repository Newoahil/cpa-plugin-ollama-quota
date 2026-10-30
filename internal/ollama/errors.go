package ollama

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Kind classifies why a collection attempt did not produce a reading. The four
// values are the four behaviours a caller needs to distinguish (issue #2):
//
//   - KindAuth: the credential was rejected. Ask the operator to re-enter it;
//     retrying with the same credential cannot succeed.
//   - KindRateLimit: upstream asked us to slow down. Wait RetryAfter, then
//     retry; nothing is wrong with the credential.
//   - KindParse: the response arrived but could not be understood — likely a
//     page redesign. Report error, keep the previous reading marked stale, and
//     do not substitute a number (SPEC 4.3).
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
// the HTTP status, and never contains an API key, a cookie, an Authorization
// header or a request URL that could embed a secret. Detail about a malformed
// payload is deliberately reduced to its class, so a decode error cannot echo
// credential material back through the message.
type Error struct {
	// Kind is the failure class.
	Kind Kind `json:"kind"`
	// Message is a scrubbed, human-readable explanation.
	Message string `json:"message"`
	// StatusCode is the upstream HTTP status, or 0 when no response arrived.
	StatusCode int `json:"status_code,omitempty"`
	// Endpoint names which fixed path failed: "balance" or "settings".
	Endpoint string `json:"endpoint,omitempty"`
	// RetryAfter is how long the caller should wait before trying again. It is
	// set for KindRateLimit (from the Retry-After header when present, else a
	// conservative default) and may be set on other kinds when upstream said.
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	// HasRetryAfter reports whether RetryAfter was communicated upstream.
	HasRetryAfter bool `json:"has_retry_after,omitempty"`

	// sentinel is the only error an Error chains to: context.Canceled or
	// context.DeadlineExceeded, or nil. The raw transport error is deliberately
	// never kept — a *url.Error carries the request URL and any error an
	// injected transport returns may carry the credential verbatim, so exposing
	// it through Unwrap would make the secret reachable even though the message
	// is clean (SPEC 4.2).
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
//	errors.Is(err, ollama.ErrRateLimit)
//	errors.Is(err, context.DeadlineExceeded)
//	var failure *ollama.Error; errors.As(err, &failure)
//
// Nothing from the transport is chained. Every element Unwrap can return is a
// package constant or a context sentinel, so walking the chain can never yield
// credential material (SPEC 4.2).
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

// sentinel maps a Kind to its package-level sentinel error.
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
var ErrUpstream = errors.New("ollama upstream failure")

// Sentinel errors, one per class. Both styles work:
//
//	errors.Is(err, ollama.ErrRateLimit)
//	var failure *ollama.Error; errors.As(err, &failure); failure.Kind
var (
	// ErrAuth reports a rejected or missing credential.
	ErrAuth = fmt.Errorf("%w: authentication failed", ErrUpstream)
	// ErrRateLimit reports an upstream 429.
	ErrRateLimit = fmt.Errorf("%w: rate limited", ErrUpstream)
	// ErrParse reports a response that arrived but could not be understood.
	ErrParse = fmt.Errorf("%w: response could not be parsed", ErrUpstream)
	// ErrNetwork reports a transport-level failure or an unsafe redirect.
	ErrNetwork = fmt.Errorf("%w: network error", ErrUpstream)
)

// ErrMissingCredential reports a collection attempt with no usable credential.
// It is an auth-class failure, so callers already handling KindAuth need no
// special case for "nothing configured yet".
var ErrMissingCredential = fmt.Errorf("%w: no credential is configured", ErrAuth)

// KindOf returns the failure class of err, or "" for a nil error or one this
// package did not produce.
//
// When the two credential paths both failed, Fetch joins their errors. In that
// case the primary (balance) class is reported, matching the order in the
// message: the class a caller branches on should be the one from the path it
// would have used anyway.
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
// HasRetryAfter is false, so a caller can tell "upstream said 30s" from
// "nobody said, use a default".
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
// callers pass constant strings or values that cannot contain a credential.
func newError(kind Kind, endpoint string, status int, message string) *Error {
	return &Error{Kind: kind, Endpoint: endpoint, StatusCode: status, Message: message}
}

// errorForStatus classifies a non-2xx status. 401/403 is an auth failure, 429 a
// rate limit, and anything else a network-level failure the caller may retry.
func errorForStatus(endpoint string, status int) *Error {
	switch {
	case status == 401 || status == 403:
		return newError(KindAuth, endpoint, status, authMessage(endpoint, status))
	case status == 429:
		failure := newError(KindRateLimit, endpoint, status, rateLimitMessage(endpoint))
		failure.RetryAfter = defaultRateLimitBackoff
		return failure
	default:
		return newError(KindNetwork, endpoint, status,
			fmt.Sprintf("the Ollama %s endpoint returned HTTP %d", endpoint, status))
	}
}

func authMessage(endpoint string, status int) string {
	// The credential never appears here; only what the operator must do.
	if endpoint == "balance" {
		return fmt.Sprintf("the Ollama API key was rejected by the balance endpoint (HTTP %d); re-enter a valid key", status)
	}
	return "the Ollama session cookie was rejected or has expired; re-enter a valid cookie"
}

func rateLimitMessage(endpoint string) string {
	return fmt.Sprintf("the Ollama %s endpoint rate limited the request (HTTP 429)", endpoint)
}

// redirectError classifies an observed 3xx response. The client never follows
// one: a redirect could carry the credential to another host, and a redirect to
// the login page means the credential expired. Only the Location host and path
// are inspected — never echoed into the message.
func redirectError(endpoint, location string) *Error {
	target, err := url.Parse(location)
	if err == nil && isLoginPath(target.Path) {
		return newError(KindAuth, endpoint, 0,
			authMessage(endpoint, 0)+" (upstream redirected to the login page)")
	}
	return newError(KindNetwork, endpoint, 0,
		fmt.Sprintf("the Ollama %s endpoint answered with a redirect, which is never followed", endpoint))
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
// transport that puts the credential in its error text cannot leak it through
// the returned chain (SPEC 4.2).
func networkError(endpoint string, err error) *Error {
	failure := &Error{
		Kind:     KindNetwork,
		Endpoint: endpoint,
		Message:  networkMessage(endpoint, err),
	}
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
		return fmt.Sprintf("the Ollama %s endpoint did not respond before the deadline", endpoint)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Sprintf("the request to the Ollama %s endpoint was cancelled", endpoint)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the Ollama endpoint name could not be resolved"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Sprintf("the Ollama %s endpoint did not respond before the deadline", endpoint)
	}
	return fmt.Sprintf("the request to the Ollama %s endpoint failed", endpoint)
}
