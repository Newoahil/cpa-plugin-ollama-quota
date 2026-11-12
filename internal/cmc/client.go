package cmc

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

// Client collects one account's quota over the two fixed account endpoints.
//
// A Client is safe for concurrent use: it holds only immutable configuration
// and an http.Client (itself concurrency-safe).
type Client struct {
	transport    Transport
	baseURL      string // scheme://authority[/path], no trailing slash
	timeout      time.Duration
	maxBodyBytes int64
	now          func() time.Time
}

// Option adjusts a Client at construction.
type Option func(*Client)

// WithTransport replaces the HTTP transport. A *http.Client handed in here is
// copied with its redirect policy replaced, so a credential can never follow a
// redirect off-origin.
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

// NewClient builds a client whose account root is DefaultBaseURL.
func NewClient(options ...Option) *Client {
	base, err := AccountAPIBase(DefaultBaseURL)
	if err != nil {
		base = DefaultBaseURL
	}
	client := &Client{
		transport:    &http.Client{CheckRedirect: refuseRedirects},
		baseURL:      base,
		timeout:      DefaultTimeout,
		maxBodyBytes: DefaultMaxBodyBytes,
		now:          time.Now,
	}
	for _, option := range options {
		option(client)
	}
	return client
}

// WithBaseURL points the client at the account root derived from base. An
// unusable base leaves the client on the default, which the next Fetch reports
// as host_unavailable-class rather than silently using a wrong host.
func WithBaseURL(base string) Option {
	return func(c *Client) {
		root, err := AccountAPIBase(base)
		if err != nil {
			return
		}
		c.baseURL = root
	}
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

// Fetch collects one account's quota. It never panics: a panic anywhere below
// is turned into a network-class failure with no figures.
//
// The credits call is required; the subscriptions call is best effort, so a
// plan lookup that fails still yields the balance and the windows. The monthly
// remaining figure is attached only after a successful subscription read: the
// credits endpoint states a remaining amount but not what it is an allowance
// of, and a figure presented as a plan's monthly credit should come from an
// account confirmed to be on a plan.
func (c *Client) Fetch(ctx context.Context, credential Credential) (snapshot Snapshot, err error) {
	observedAt := c.safeNow()
	defer func() {
		if recovered := recover(); recovered != nil {
			snapshot = Snapshot{ObservedAt: observedAt}
			err = newError(KindNetwork, "", 0, fmt.Sprintf("the Command Code collection panicked: %T", recovered))
		}
	}()
	if credential.Empty() {
		return Snapshot{ObservedAt: observedAt}, ErrMissingCredential
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	body, errCredits := c.get(ctx, credential.APIKey, CreditsPath, "credits")
	if errCredits != nil {
		return Snapshot{ObservedAt: observedAt}, errCredits
	}
	parsed, errParse := parseCredits(body)
	if errParse != nil {
		return Snapshot{ObservedAt: observedAt}, errParse
	}
	snapshot = Snapshot{Balance: parsed.Balance, Windows: parsed.Windows, ObservedAt: observedAt}

	// Best effort: a subscription lookup that fails costs the plan name and the
	// monthly figure, nothing more.
	if body, errSub := c.get(ctx, credential.APIKey, SubscriptionPath, "subscriptions"); errSub == nil {
		if sub, ok := parseSubscription(body); ok {
			snapshot.Plan = sub.Plan
			if parsed.MonthlyCredits != nil {
				snapshot.Monthly = &MonthlyCredit{
					Remaining: roundCents(*parsed.MonthlyCredits),
					ResetAt:   sub.CurrentPeriodEnd,
				}
			}
		}
	}
	return rejectEcho(snapshot, credential)
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

// get issues one authenticated GET against a fixed path on the account root.
func (c *Client) get(ctx context.Context, apiKey, path, endpoint string) ([]byte, error) {
	if c.baseURL == "" {
		return nil, newError(KindNetwork, endpoint, 0, "the Command Code account API base is not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, networkError(endpoint, err)
	}
	if err := c.checkHost(request.URL); err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	request.Header.Set("Accept", "application/json")

	response, errDo := c.do(ctx, request, endpoint)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = drainClose(response.Body) }()

	switch status := response.StatusCode; {
	case status == http.StatusOK:
		// fall through to read
	case status == http.StatusTooManyRequests:
		return nil, rateLimited(endpoint, response.Header)
	default:
		return nil, errorForStatus(endpoint, status)
	}

	body, errRead := readBounded(response.Body, c.maxBodyBytes)
	if errRead != nil {
		return nil, networkError(endpoint, errRead)
	}
	return body, nil
}

// checkHost pins a request to the configured account origin.
func (c *Client) checkHost(target *url.URL) error {
	origin := target.Scheme + "://" + target.Host
	expected, err := url.Parse(c.baseURL)
	if err != nil || expected.Host == "" {
		return newError(KindNetwork, "", 0, "the Command Code account API base is not configured")
	}
	expectedOrigin := expected.Scheme + "://" + expected.Host
	if !strings.EqualFold(origin, expectedOrigin) {
		return newError(KindNetwork, "", 0,
			fmt.Sprintf("refusing a request to %s: only %s is allowed", origin, expectedOrigin))
	}
	return nil
}

// do issues a request with panic recovery.
func (c *Client) do(ctx context.Context, request *http.Request, endpoint string) (response *http.Response, err error) {
	if c.transport == nil {
		return nil, newError(KindNetwork, endpoint, 0, "the Command Code HTTP transport is not configured")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			response = nil
			err = newError(KindNetwork, endpoint, 0,
				fmt.Sprintf("the Command Code %s request panicked: %T", endpoint, recovered))
		}
	}()
	response, err = c.transport.Do(request)
	if err != nil {
		return nil, networkError(endpoint, err)
	}
	if response == nil {
		return nil, newError(KindNetwork, endpoint, 0, "the Command Code HTTP transport returned no response")
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
		return nil, redirectError(endpoint, response.Header.Get("Location"))
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

// drainClose consumes a bounded remainder so the connection can be reused, then
// closes the body. A panic in either step is swallowed: the caller has already
// produced its result by the time this runs.
func drainClose(body io.ReadCloser) (err error) {
	if body == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("the Command Code response body panicked while it was being closed")
		}
	}()
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
	return body.Close()
}

// minEchoLength is the shortest secret worth scanning a reading for. A real
// API key is far longer; a one- or two-character key would match almost any
// label by chance ("Weekly" contains "k"), and refusing an otherwise good
// reading over that coincidence would be worse than the leak it guards against.
// The value is never reported, only compared.
const minEchoLength = 8

// rejectEcho refuses a reading that contains the key it was read with.
func rejectEcho(snapshot Snapshot, credential Credential) (Snapshot, error) {
	secret := strings.TrimSpace(credential.APIKey)
	if len(secret) < minEchoLength {
		return snapshot, nil
	}
	values := []string{snapshot.Plan}
	for _, window := range snapshot.Windows {
		values = append(values, window.Label, window.Note)
	}
	for _, value := range values {
		if strings.Contains(value, secret) {
			return Snapshot{ObservedAt: snapshot.ObservedAt}, newError(KindParse, "credits", 0,
				"the Command Code response echoed the credential back; the reading was discarded rather than returned")
		}
	}
	return snapshot, nil
}
