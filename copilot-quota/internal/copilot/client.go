package copilot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Transport performs one HTTP exchange. It is the seam the plugin uses to route
// this package's requests through CPA's host transport policy.
type Transport interface {
	Do(*http.Request) (*http.Response, error)
}

// Client collects one account's Copilot quota over the fixed user endpoint.
//
// A Client is safe for concurrent use: it holds only immutable configuration
// and a transport.
type Client struct {
	transport    Transport
	userURL      string
	origin       string
	timeout      time.Duration
	maxBodyBytes int64
	now          func() time.Time
}

// Option adjusts a Client at construction.
type Option func(*Client)

// WithTransport replaces the HTTP transport. The plugin passes an adapter over
// the host's HTTP callback here. Redirect policy stays in force regardless: a
// *http.Client handed in is copied with its redirect policy replaced, so it can
// never follow a redirect and carry the token off-origin.
func WithTransport(transport Transport) Option {
	return func(c *Client) {
		if transport == nil {
			return
		}
		if client, ok := transport.(*http.Client); ok {
			transport = copyHTTPClient(client)
		}
		c.transport = transport
	}
}

// WithHTTPClient is the explicit *http.Client form of WithTransport.
func WithHTTPClient(client *http.Client) Option {
	return WithTransport(copyHTTPClient(client))
}

// WithTimeout overrides the per-collection deadline (default 20s).
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		if timeout > 0 {
			c.timeout = timeout
		}
	}
}

// WithMaxBodyBytes overrides the response body cap.
func WithMaxBodyBytes(limit int64) Option {
	return func(c *Client) {
		if limit > 0 {
			c.maxBodyBytes = limit
		}
	}
}

// WithClock injects the clock used to stamp ObservedAt. Only tests set it.
func WithClock(now func() time.Time) Option {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
	}
}

// endpoints points a client at a fixture server. It is unexported and only
// reachable from this package's tests, because a production caller must never
// be able to change the origin: the token is only ever sent to api.github.com.
func endpoints(rawURL string) Option {
	return func(c *Client) {
		parsed, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil || parsed.Host == "" {
			return
		}
		parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/copilot_internal/user"
		c.userURL = parsed.String()
		c.origin = parsed.Scheme + "://" + parsed.Host
	}
}

// NewClient builds a client for the fixed GitHub API origin.
func NewClient(options ...Option) *Client {
	client := &Client{
		transport:    &http.Client{CheckRedirect: refuseRedirects},
		userURL:      UserURL,
		origin:       GitHubOrigin,
		timeout:      DefaultTimeout,
		maxBodyBytes: DefaultMaxBodyBytes,
		now:          time.Now,
	}
	for _, option := range options {
		option(client)
	}
	return client
}

// copyHTTPClient returns an *http.Client that reuses the given client's
// transport but refuses redirects, so the token can never follow one.
func copyHTTPClient(client *http.Client) Transport {
	if client == nil {
		return &http.Client{CheckRedirect: refuseRedirects}
	}
	clone := *client
	clone.CheckRedirect = refuseRedirects
	return &clone
}

// refuseRedirects stops the transport following a 3xx. A redirect to the login
// page must be observed as an auth failure, and a redirect off the fixed origin
// must never receive the token.
func refuseRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Fetch collects one account's quota. It never panics: this code runs inside
// the CPA process, where a panic on a collection goroutine can take the gateway
// down. The recovery covers the whole collection — transport, body read, body
// close and parsing.
//
// The returned Snapshot is always usable: on failure it has StatusUnavailable
// and no figures, and the error classifies why.
//
// Whatever the path produced is checked against the credential one last time: a
// body that echoes the token is refused rather than returned, since the secret
// must not reach a caller through a value as much as through an error string.
func (c *Client) Fetch(ctx context.Context, credential Credential) (snapshot Snapshot, err error) {
	now := c.now()
	defer func() {
		if recovered := recover(); recovered != nil {
			// The recovered value is never formatted into the message: it can
			// be anything the code that panicked chose.
			snapshot = unavailable(now, SourceUser)
			err = parseError("the Copilot quota collection crashed")
		}
	}()

	if credential.Empty() {
		return unavailable(now, SourceUser), ErrMissingCredential
	}
	if ctx == nil {
		ctx = context.Background()
	}

	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	request, errRequest := http.NewRequestWithContext(requestCtx, http.MethodGet, c.userURL, nil)
	if errRequest != nil {
		return unavailable(now, SourceUser), parseError("the Copilot request could not be built")
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(credential.Token))
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "cpa-copilot-quota")

	response, errDo := c.transport.Do(request)
	if errDo != nil {
		return unavailable(now, SourceUser), networkError(errDo)
	}
	if response == nil {
		return unavailable(now, SourceUser), networkError(errors.New("no response"))
	}
	defer func() {
		if response.Body != nil {
			_ = response.Body.Close()
		}
	}()

	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return unavailable(now, SourceUser), redirectError(response.StatusCode, response.Header.Get("Location"))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return unavailable(now, SourceUser), errorForStatus(
			response.StatusCode,
			parseRetryAfter(response.Header.Get("Retry-After")),
			strings.TrimSpace(response.Header.Get("Retry-After")) != "",
		)
	}
	if response.Body == nil {
		return unavailable(now, SourceUser), parseError("the Copilot endpoint returned no body")
	}

	body, errRead := io.ReadAll(io.LimitReader(response.Body, c.maxBodyBytes+1))
	if errRead != nil {
		return unavailable(now, SourceUser), networkError(errRead)
	}
	if int64(len(body)) > c.maxBodyBytes {
		return unavailable(now, SourceUser), parseError("the Copilot endpoint returned an unexpectedly large body")
	}
	if strings.TrimSpace(credential.Token) != "" && strings.Contains(string(body), credential.Token) {
		// Upstream should never echo the credential. If it does, refusing the
		// reading keeps it from being stored or served.
		return unavailable(now, SourceUser), parseError("the Copilot endpoint returned the credential in its body")
	}

	return parseUserPayload(body, now)
}

// parseRetryAfter reads a Retry-After value in either supported form: seconds
// or an HTTP date.
func parseRetryAfter(value string) time.Duration {
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
		wait := time.Until(when)
		if wait <= 0 {
			return 0
		}
		return wait
	}
	return 0
}

// endpointFor returns the URL this client issues, for diagnostics in tests.
func (c *Client) endpointFor() string { return c.userURL }
