package ollama

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Transport performs one HTTP exchange. It is the seam #3 uses to route this
// package's requests through CPA's host transport policy.
//
// A *http.Client satisfies the default adapter, and anything that can issue a
// *http.Request and return an *http.Response satisfies this interface. #3 wraps
// pluginapi.HostHTTPClient (QuotaFetchRequest.HTTPClient) in a small adapter
// and passes it with WithTransport; this package stays free of pluginapi.
type Transport interface {
	Do(*http.Request) (*http.Response, error)
}

// Credential is what one collection attempt authenticates with. At least one
// field should be set; Client picks the path from what is present (SPEC 3.2).
//
// Neither field is ever logged, formatted into an error, or written anywhere.
// It is deliberately a plain struct so a caller can read it straight from
// CPA's StorageJSON without this package knowing that shape.
type Credential struct {
	// APIKey is sent as "Authorization: Bearer <APIKey>" to the balance
	// endpoint. It is never accepted by the settings endpoint.
	APIKey string
	// Cookie is sent as the Cookie header to the settings endpoint. A value
	// that already contains '=' is used verbatim; anything else is wrapped as
	// the __Secure-session cookie, which is the name Ollama sets.
	Cookie string
}

// Empty reports whether the credential carries nothing usable.
func (c Credential) Empty() bool {
	return strings.TrimSpace(c.APIKey) == "" && strings.TrimSpace(c.Cookie) == ""
}

// Redacted returns a copy safe to log or serialize: both fields are blanked.
// It exists so a caller cannot accidentally keep a secret by copying the
// struct into a log line.
func (c Credential) Redacted() Credential { return Credential{} }

// String redacts both secrets so a stray "%v" or log call cannot leak them.
func (c Credential) String() string {
	return fmt.Sprintf("Credential{apiKey:%s, cookie:%s}", presence(c.APIKey), presence(c.Cookie))
}

// GoString keeps a "%#v" from printing the struct fields verbatim.
func (c Credential) GoString() string { return c.String() }

func presence(secret string) string {
	if strings.TrimSpace(secret) == "" {
		return "absent"
	}
	return "set"
}

// Client collects one account's quota over the two fixed upstream endpoints.
//
// A Client is safe for concurrent use: it holds only immutable configuration
// and an http.Client (itself concurrency-safe).
type Client struct {
	transport    Transport
	baseURL      string // scheme://host of the fixed origin, no trailing slash
	balanceURL   string
	settingsURL  string
	timeout      time.Duration
	maxBodyBytes int64
	now          func() time.Time
}

// Option adjusts a Client at construction.
type Option func(*Client)

// WithTransport replaces the HTTP transport. #3 passes an adapter over
// pluginapi.HostHTTPClient here. The redirect policy, host pinning and body
// limit stay in force regardless of what transport is supplied: a *http.Client
// handed in here (it satisfies Transport directly) is copied with its redirect
// policy replaced, so it can never follow a redirect and carry a credential
// off-origin.
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

// WithTimeout overrides the per-collection deadline (SPEC 4.4, default 20s).
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

// WithClock injects the clock used to anchor relative reset hints and to stamp
// ObservedAt. Only tests set it.
func WithClock(now func() time.Time) Option {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
	}
}

// endpoints points a client at a fixture server. It is unexported and only
// reachable from this package's tests, because a production caller must never
// be able to change the origin: the credential is only ever sent to the fixed
// ollama.com host (SPEC 4.2). Both URLs must share one host, and that host
// becomes the only one a request may reach.
func endpoints(balanceURL, settingsURL string) Option {
	return func(c *Client) {
		balance, err := url.Parse(strings.TrimSpace(balanceURL))
		if err != nil || balance.Host == "" {
			return
		}
		settings, err := url.Parse(strings.TrimSpace(settingsURL))
		if err != nil || settings.Host == "" {
			return
		}
		if !strings.EqualFold(balance.Host, settings.Host) {
			return
		}
		c.balanceURL = balance.String()
		c.settingsURL = settings.String()
		c.baseURL = balance.Scheme + "://" + balance.Host
	}
}

// NewClient builds a client for the fixed Ollama origin.
func NewClient(options ...Option) *Client {
	client := &Client{
		transport:    &http.Client{CheckRedirect: refuseRedirects},
		baseURL:      originOf(BalanceURL),
		balanceURL:   BalanceURL,
		settingsURL:  SettingsURL,
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
// transport but refuses redirects, so the credential can never follow one.
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
// must never receive the credential.
func refuseRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// originOf returns "scheme://host" for a fixed endpoint URL.
func originOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// Fetch collects one account's quota. It never panics: this code runs inside
// the CPA process, where a panic on a collection goroutine can take the gateway
// down (SPEC 4.4). The recovery covers the whole collection — transport, body
// read, body close and parsing — not just the transport call.
//
// The returned Snapshot is always usable: on failure it has StatusUnavailable
// and no figures, and the error classifies why. Order of paths (SPEC 3.2):
//
//  1. balance endpoint, when an API key is present;
//  2. settings page, when a cookie is present, or when the key path failed and
//     a cookie offers a second opinion.
//
// A settings reading that only yields a plan and a balance is not a quota read
// and is not reported as success (see ParseSettings).
//
// Whatever the path produced is checked against the credential one last time: a
// page that echoes the key or the cookie is refused rather than returned, since
// SPEC 4.2 forbids the secret reaching a caller through a value as much as
// through an error string.
func (c *Client) Fetch(ctx context.Context, credential Credential) (snapshot Snapshot, err error) {
	// The timestamp is read behind its own recovery so a broken clock still
	// yields a snapshot an operator can place in time.
	observedAt := c.safeNow()
	// The recovery converts a panic anywhere below — the transport, Body.Read,
	// Body.Close, the parser — into a failure with no figures. Returning the
	// zero Snapshot instead would carry Status "" and no ObservedAt, which a
	// display layer cannot tell from a reading.
	defer func() {
		if recovered := recover(); recovered != nil {
			snapshot = unavailable(observedAt, SourceNone)
			err = newError(KindNetwork, "", 0,
				fmt.Sprintf("the Ollama collection panicked: %T", recovered))
		}
	}()

	snapshot, err = c.fetch(ctx, credential, observedAt)
	return rejectEcho(snapshot, err, credential)
}

// safeNow calls the injected clock, falling back to the wall clock if it panics,
// so ObservedAt is always a real instant.
func (c *Client) safeNow() (observedAt time.Time) {
	defer func() {
		if recovered := recover(); recovered != nil {
			observedAt = time.Now()
		}
	}()
	return c.now()
}

// fetch is the body of Fetch; Fetch adds the panic boundary around it.
func (c *Client) fetch(ctx context.Context, credential Credential, now time.Time) (snapshot Snapshot, err error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if credential.Empty() {
		return unavailable(now, SourceNone), ErrMissingCredential
	}

	apiKey := strings.TrimSpace(credential.APIKey)
	cookie := strings.TrimSpace(credential.Cookie)

	var firstErr error
	if apiKey != "" {
		snapshot, err := c.fetchBalance(ctx, apiKey, now)
		if err == nil && snapshot.HasData() {
			snapshot.Status = snapshot.deriveStatus(DefaultWarningPercent)
			return snapshot, nil
		}
		if err == nil {
			err = newError(KindParse, "balance", 0,
				"the Ollama balance endpoint returned no usable balance")
		}
		firstErr = err
		// The settings path cannot help a rejected key, and there may be no
		// cookie; fall through either way because a cookie is an independent
		// credential that may still be valid.
		if cookie == "" {
			return unavailable(now, SourceBalance), firstErr
		}
	}

	if cookie != "" {
		snapshot, err := c.fetchSettings(ctx, cookie, now)
		if err == nil {
			snapshot.Status = snapshot.deriveStatus(DefaultWarningPercent)
			return snapshot, nil
		}
		if firstErr != nil {
			// Both credentials failed. Keep both classes so a caller can see
			// that, say, the key was rejected and the cookie had expired,
			// instead of hearing only about the last attempt. The balance
			// error comes first because it is the primary path.
			return unavailable(now, SourceSettings), errors.Join(firstErr, err)
		}
		return unavailable(now, SourceSettings), err
	}

	if firstErr != nil {
		return unavailable(now, SourceBalance), firstErr
	}
	return unavailable(now, SourceNone), ErrMissingCredential
}

// rejectEcho is the last check before a reading leaves the package. Any field
// that can carry upstream text — the reset wording, the plan — is scanned for
// the credential this attempt used; a hit turns the whole reading into a parse
// failure with no figures. Substituting an empty string would leave a reading
// the page did not verify, so the result is refused instead (SPEC 4.2, 4.3).
func rejectEcho(snapshot Snapshot, err error, credential Credential) (Snapshot, error) {
	if err != nil {
		return snapshot, err
	}
	if echoesCredential(snapshot, credential) {
		return unavailable(snapshot.ObservedAt, snapshot.Source), newError(KindParse, endpointOf(snapshot.Source), 0,
			"the Ollama response echoed the credential back; the reading was discarded rather than returned")
	}
	return snapshot, nil
}

// echoesCredential reports whether any echoable field of the snapshot contains
// a credential fragment.
func echoesCredential(snapshot Snapshot, credential Credential) bool {
	secrets := []string{
		strings.TrimSpace(credential.APIKey),
		strings.TrimSpace(credential.Cookie),
		cookieValue(credential.Cookie),
	}
	values := []string{snapshot.Plan}
	for _, window := range snapshot.Windows {
		values = append(values, window.Label, window.ResetText)
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, value := range values {
			if strings.Contains(value, secret) {
				return true
			}
		}
	}
	return false
}

// cookieValue is the value half of a cookie credential, so a page that echoes
// only the session token (not the full "name=value" pair) is still caught.
func cookieValue(cookie string) string {
	trimmed := strings.TrimSpace(cookie)
	if trimmed == "" {
		return ""
	}
	if at := strings.IndexByte(trimmed, '='); at >= 0 {
		return strings.TrimSpace(trimmed[at+1:])
	}
	return trimmed
}

func endpointOf(source Source) string {
	if source == SourceSettings {
		return "settings"
	}
	return "balance"
}

// fetchBalance reads the balance endpoint. Only the documented fields are
// consumed; included.balance_usd is the remaining included credit and
// included.allowance_usd is the credit the period started with. Legacy plans
// return included.session/included.weekly instead, which are read as windows.
func (c *Client) fetchBalance(ctx context.Context, apiKey string, now time.Time) (Snapshot, error) {
	snapshot := Snapshot{ObservedAt: now, Source: SourceBalance}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.balanceURL, nil)
	if err != nil {
		return snapshot, networkError("balance", err)
	}
	if err := c.checkHost(request.URL); err != nil {
		return snapshot, err
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Accept", "application/json")

	response, err := c.do(ctx, request, "balance")
	if err != nil {
		return snapshot, err
	}
	defer drainClose(response.Body)

	switch status := response.StatusCode; {
	case status == http.StatusOK:
		// fall through to parse
	case status >= 300 && status < 400:
		return snapshot, redirectError("balance", response.Header.Get("Location"))
	case status == http.StatusTooManyRequests:
		return snapshot, rateLimited("balance", response.Header)
	default:
		return snapshot, errorForStatus("balance", status)
	}

	body, err := readBounded(response.Body, c.maxBodyBytes)
	if err != nil {
		return snapshot, networkError("balance", err)
	}
	parsed, err := parseBalance(body)
	if err != nil {
		return snapshot, err
	}
	snapshot.Plan = parsed.Plan
	snapshot.Balance = parsed.Balance
	snapshot.Windows = parsed.Windows
	return snapshot, nil
}

// fetchSettings reads and parses the settings page (SPEC 3.2 path two).
func (c *Client) fetchSettings(ctx context.Context, cookie string, now time.Time) (Snapshot, error) {
	snapshot := Snapshot{ObservedAt: now, Source: SourceSettings}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.settingsURL, nil)
	if err != nil {
		return snapshot, networkError("settings", err)
	}
	if err := c.checkHost(request.URL); err != nil {
		return snapshot, err
	}
	request.Header.Set("Cookie", cookieHeader(cookie))
	request.Header.Set("Accept", "text/html,application/xhtml+xml")

	response, err := c.do(ctx, request, "settings")
	if err != nil {
		return snapshot, err
	}
	defer drainClose(response.Body)

	switch status := response.StatusCode; {
	case status == http.StatusOK:
		// fall through to parse
	case status >= 300 && status < 400:
		return snapshot, redirectError("settings", response.Header.Get("Location"))
	case status == http.StatusTooManyRequests:
		return snapshot, rateLimited("settings", response.Header)
	default:
		return snapshot, errorForStatus("settings", status)
	}

	body, err := readBounded(response.Body, c.maxBodyBytes)
	if err != nil {
		return snapshot, networkError("settings", err)
	}
	document := string(body)
	if LooksLikeLoginPage(document) {
		return snapshot, newError(KindAuth, "settings", 0,
			"the Ollama session cookie has expired; upstream served the sign-in page")
	}
	parsed, err := ParseSettings(document, now)
	if err != nil {
		return snapshot, err
	}
	snapshot.Plan = parsed.Plan
	snapshot.Balance = parsed.Balance
	snapshot.Windows = parsed.Windows
	return snapshot, nil
}

// checkHost pins a request to the fixed origin. It is the last line of defence
// behind the URL being a constant: even a future refactor that introduced a
// configurable URL could not point a credential at another host.
func (c *Client) checkHost(target *url.URL) error {
	if c.baseURL == "" {
		return newError(KindNetwork, "", 0, "the Ollama endpoint origin is not configured")
	}
	origin := target.Scheme + "://" + target.Host
	if !strings.EqualFold(origin, c.baseURL) {
		// The origin is a constant of this package, so this path is only
		// reachable through a programming error; naming the origin (not the
		// credential) is enough to diagnose it.
		return newError(KindNetwork, "", 0,
			fmt.Sprintf("refusing a request to %s: only %s is allowed", origin, c.baseURL))
	}
	return nil
}

// do issues a request with panic recovery. A panic inside an injected
// transport is turned into a network-class error instead of taking down the
// host process (SPEC 4.4).
func (c *Client) do(ctx context.Context, request *http.Request, endpoint string) (response *http.Response, err error) {
	if c.transport == nil {
		return nil, newError(KindNetwork, endpoint, 0, "the Ollama HTTP transport is not configured")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			response = nil
			err = newError(KindNetwork, endpoint, 0,
				fmt.Sprintf("the Ollama %s request panicked: %T", endpoint, recovered))
		}
	}()
	response, err = c.transport.Do(request)
	if err != nil {
		return nil, networkError(endpoint, err)
	}
	if response == nil {
		return nil, newError(KindNetwork, endpoint, 0, "the Ollama HTTP transport returned no response")
	}
	// A transport is expected to leave Body open for us; a nil body would panic
	// the caller, so guard it.
	if response.Body == nil {
		response.Body = http.NoBody
	}
	// If the transport reports which request produced this response, that
	// request must still be on the pinned origin. A transport that follows a
	// redirect (rather than the refusal policy) would otherwise hand back the
	// body of another host as if it were a reading. A 3xx status is refused
	// here too, so no caller can mistake a followed redirect for success.
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

// rateLimited builds a 429 error, reading Retry-After when the response
// carries it. A 429 without the header keeps the default backoff but reports
// HasRetryAfter false.
func rateLimited(endpoint string, header http.Header) *Error {
	failure := newError(KindRateLimit, endpoint, http.StatusTooManyRequests, rateLimitMessage(endpoint))
	// Start from the default so a 429 with no header still tells the caller
	// how long to wait; HasRetryAfter stays false so it can tell the two apart.
	failure.RetryAfter = defaultRateLimitBackoff
	if wait, ok := parseRetryAfter(header); ok {
		failure.RetryAfter = wait
		failure.HasRetryAfter = true
	}
	return failure
}

// maxRetryAfterSeconds bounds a delay-seconds value before it is multiplied.
// Multiplying an Atoi-sized integer by time.Second overflows int64 into a
// negative duration, and a negative clamp means "retry immediately" — the exact
// opposite of what upstream asked for.
const maxRetryAfterSeconds = int64(24 * 60 * 60)

// parseRetryAfter reads the Retry-After header, accepting delay-seconds and an
// HTTP date, bounded to one day so a hostile value cannot park an account
// indefinitely.
func parseRetryAfter(header http.Header) (time.Duration, bool) {
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

// cookieHeader builds the Cookie header. A value that already contains '=' is
// treated as a full Cookie header; anything else is wrapped as the
// __Secure-session cookie Ollama sets.
func cookieHeader(cookie string) string {
	if strings.Contains(cookie, "=") {
		return cookie
	}
	return "__Secure-session=" + cookie
}

// readBounded reads at most limit bytes and reports whether the body was
// truncated. A truncated body is a parse problem, not a silent partial read.
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
// closes the body. Fetch's recovery already covers this, but a panicking Read
// would otherwise be caught only at the top and lose the endpoint context, so
// the drain swallows its own panic.
func drainClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	defer func() { _ = recover() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
	_ = body.Close()
}
