package opc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Transport performs one HTTP exchange. It is the seam the quota plugin uses to
// route this package's requests through CPA's host transport policy.
type Transport interface {
	Do(*http.Request) (*http.Response, error)
}

// Client reads one account's usage over the single fixed endpoint.
//
// A Client is safe for concurrent use: it holds only immutable configuration
// and an http.Client (itself concurrency-safe).
type Client struct {
	transport    Transport
	usageURL     string
	baseURL      string // scheme://host[/path] the request must stay on
	timeout      time.Duration
	maxBodyBytes int64
	now          func() time.Time
}

// Option adjusts a Client at construction.
type Option func(*Client)

// WithTransport replaces the HTTP transport. A *http.Client handed in here is
// copied with its redirect policy replaced.
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

// WithBaseURL points the client at the provider base the credential carries.
// The usage path is appended here, so a base of "https://opencode.ai/zen/go"
// and one of "https://opencode.ai/zen/go/" both reach "/zen/go/v1/usage" once.
func WithBaseURL(base string) Option {
	return func(c *Client) {
		target, err := UsageURL(base)
		if err != nil {
			return
		}
		c.usageURL = target
		c.baseURL = originOf(target)
	}
}

// WithTimeout overrides the per-collection deadline.
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

// NewClient builds a client for the default vendor base.
func NewClient(options ...Option) *Client {
	target, err := UsageURL(DefaultBaseURL)
	if err != nil {
		target = DefaultBaseURL + UsagePath
	}
	client := &Client{
		transport:    &http.Client{CheckRedirect: refuseRedirects},
		usageURL:     target,
		baseURL:      originOf(target),
		timeout:      DefaultTimeout,
		maxBodyBytes: DefaultMaxBodyBytes,
		now:          time.Now,
	}
	for _, option := range options {
		option(client)
	}
	return client
}

// copyHTTPClient reuses the given client's transport but refuses redirects.
func copyHTTPClient(client *http.Client) Transport {
	if client == nil {
		return &http.Client{CheckRedirect: refuseRedirects}
	}
	clone := *client
	clone.CheckRedirect = refuseRedirects
	return &clone
}

// refuseRedirects stops the transport following a 3xx.
func refuseRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// originOf returns "scheme://host" for a URL.
func originOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// Fetch collects one account's usage. It never panics.
func (c *Client) Fetch(ctx context.Context, credential Credential) (snapshot Snapshot, err error) {
	observedAt := c.safeNow()
	defer func() {
		if recovered := recover(); recovered != nil {
			snapshot = Snapshot{ObservedAt: observedAt}
			err = newError(KindNetwork, 0, fmt.Sprintf("the OpenCode Go collection panicked: %T", recovered))
		}
	}()
	if credential.Empty() {
		return Snapshot{ObservedAt: observedAt}, ErrMissingCredential
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	body, errFetch := c.get(ctx, credential.APIKey)
	if errFetch != nil {
		return Snapshot{ObservedAt: observedAt}, errFetch
	}
	windows, errParse := parseUsage(body)
	if errParse != nil {
		return Snapshot{ObservedAt: observedAt}, errParse
	}
	return rejectEcho(Snapshot{Windows: windows, ObservedAt: observedAt}, credential)
}

// safeNow calls the injected clock, falling back to the wall clock if it panics.
func (c *Client) safeNow() (observedAt time.Time) {
	defer func() {
		if recovered := recover(); recovered != nil {
			observedAt = time.Now()
		}
	}()
	return c.now()
}

// get issues the authenticated GET.
func (c *Client) get(ctx context.Context, apiKey string) ([]byte, error) {
	if c.usageURL == "" || c.baseURL == "" {
		return nil, newError(KindNetwork, 0, "the OpenCode Go usage endpoint is not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.usageURL, nil)
	if err != nil {
		return nil, networkError(err)
	}
	if err := c.checkHost(request.URL); err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	request.Header.Set("Accept", "application/json")

	response, errDo := c.do(request)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = drainClose(response.Body) }()

	switch status := response.StatusCode; {
	case status == http.StatusOK:
		// fall through to read
	case status == http.StatusTooManyRequests:
		return nil, rateLimited(response.Header)
	default:
		return nil, errorForStatus(status)
	}

	body, errRead := readBounded(response.Body, c.maxBodyBytes)
	if errRead != nil {
		return nil, networkError(errRead)
	}
	return body, nil
}

// checkHost pins a request to the configured origin.
func (c *Client) checkHost(target *url.URL) error {
	origin := target.Scheme + "://" + target.Host
	if !strings.EqualFold(origin, c.baseURL) {
		return newError(KindNetwork, 0,
			fmt.Sprintf("refusing a request to %s: only %s is allowed", origin, c.baseURL))
	}
	return nil
}

// do issues a request with panic recovery.
func (c *Client) do(request *http.Request) (response *http.Response, err error) {
	if c.transport == nil {
		return nil, newError(KindNetwork, 0, "the OpenCode Go HTTP transport is not configured")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			response = nil
			err = newError(KindNetwork, 0, fmt.Sprintf("the OpenCode Go usage request panicked: %T", recovered))
		}
	}()
	response, err = c.transport.Do(request)
	if err != nil {
		return nil, networkError(err)
	}
	if response == nil {
		return nil, newError(KindNetwork, 0, "the OpenCode Go HTTP transport returned no response")
	}
	if response.Body == nil {
		response.Body = http.NoBody
	}
	if response.Request != nil && response.Request.URL != nil {
		if err := c.checkHost(response.Request.URL); err != nil {
			return nil, err
		}
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, redirectError(response.Header.Get("Location"))
	}
	return response, nil
}

// readBounded reads at most limit bytes; a truncated body is a parse problem.
func readBounded(body io.Reader, limit int64) ([]byte, error) {
	reader := io.LimitReader(body, limit+1)
	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("response body exceeded %d bytes", limit)
	}
	return content, nil
}

// drainClose consumes a bounded remainder then closes the body.
func drainClose(body io.ReadCloser) (err error) {
	if body == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("the OpenCode Go response body panicked while it was being closed")
		}
	}()
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
	return body.Close()
}

// minEchoLength is the shortest secret worth scanning a reading for. A real API
// key is far longer; a one- or two-character key would match almost any label by
// chance ("Weekly" contains "k"), and refusing an otherwise good reading over
// that coincidence would be worse than the leak it guards against. The value is
// never reported, only compared.
const minEchoLength = 8

// rejectEcho refuses a reading that contains the key it was read with.
func rejectEcho(snapshot Snapshot, credential Credential) (Snapshot, error) {
	secret := strings.TrimSpace(credential.APIKey)
	if len(secret) < minEchoLength {
		return snapshot, nil
	}
	for _, window := range snapshot.Windows {
		if strings.Contains(window.Label, secret) || strings.Contains(window.Note, secret) {
			return Snapshot{ObservedAt: snapshot.ObservedAt}, newError(KindParse, 0,
				"the OpenCode Go response echoed the credential back; the reading was discarded rather than returned")
		}
	}
	return snapshot, nil
}
