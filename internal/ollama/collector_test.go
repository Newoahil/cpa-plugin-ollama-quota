package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// canary values: if any of these appear in a returned error, message, or
// snapshot, the redaction requirement (SPEC 4.2) is broken. They are
// distinctive strings so a substring scan is meaningful.
const (
	canaryAPIKey = "sk-CANARY-api-key-DO-NOT-LEAK-9f3a2b"
	canaryCookie = "CANARY-cookie-DO-NOT-LEAK-7c1d4e"
)

// fixture is a loopback server that stands in for ollama.com. Each test wires
// the handlers it needs; the client is pointed at it with the endpoints option.
type fixture struct {
	server *httptest.Server
	mu     sync.Mutex
	// balanceHandler and settingsHandler answer the two paths. A nil handler
	// yields 404, so an unexpected request fails loudly.
	balanceHandler  http.HandlerFunc
	settingsHandler http.HandlerFunc
	// lastBalanceRequest and lastSettingsRequest capture what arrived, so a
	// test can assert on headers without the server needing to know the test.
	lastBalanceRequest  *http.Request
	lastSettingsRequest *http.Request
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		switch r.URL.Path {
		case "/api/balance":
			f.lastBalanceRequest = cloneRequest(r)
			handler := f.balanceHandler
			f.mu.Unlock()
			if handler == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			handler(w, r)
		case "/settings":
			f.lastSettingsRequest = cloneRequest(r)
			handler := f.settingsHandler
			f.mu.Unlock()
			if handler == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			handler(w, r)
		default:
			f.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func cloneRequest(r *http.Request) *http.Request {
	clone := r.Clone(r.Context())
	return clone
}

func (f *fixture) balance(fn http.HandlerFunc)  { f.mu.Lock(); f.balanceHandler = fn; f.mu.Unlock() }
func (f *fixture) settings(fn http.HandlerFunc) { f.mu.Lock(); f.settingsHandler = fn; f.mu.Unlock() }

// client builds a production client pointed at the fixture. The fixture origin
// is the only one the client will reach, exactly like ollama.com is in
// production.
func (f *fixture) client(options ...Option) *Client {
	base := f.server.URL
	all := append([]Option{
		endpoints(base+"/api/balance", base+"/settings"),
	}, options...)
	return NewClient(all...)
}

// assertNoSecrets fails the test if any canary appears in any of the values.
func assertNoSecrets(t *testing.T, label string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(value, canaryAPIKey) {
			t.Fatalf("%s leaked the API key: %q", label, value)
		}
		if strings.Contains(value, canaryCookie) {
			t.Fatalf("%s leaked the cookie: %q", label, value)
		}
	}
}

// snapshotDump is a JSON view of a snapshot, used to scan for secrets in
// everything a snapshot exposes.
func snapshotDump(t *testing.T, snapshot Snapshot) string {
	t.Helper()
	dumped, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return string(dumped)
}

// TestFetchBalanceNormalJSON covers SPEC AC-2: a well-formed balance response
// yields the plan and the exact dollar amount.
func TestFetchBalanceNormalJSON(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+canaryAPIKey {
			t.Errorf("Authorization = %q, want the bearer key", got)
		}
		if r.Method != http.MethodGet {
			// The documented endpoint is GET; the task brief said POST, but the
			// upstream schema wins. Asserting the method keeps the two from
			// drifting apart silently.
			t.Errorf("balance method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"included":{"balance_usd":12.4,"allowance_usd":50,"period":{"from":"2026-10-01T00:00:00Z","until":"2026-11-01T00:00:00Z"}},"purchased":{"balance_usd":0}}`))
	})
	// The settings path must not be consulted when the balance path succeeds.
	f.client()

	snapshot, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.Balance == nil {
		t.Fatal("balance is nil")
	}
	if snapshot.Balance.Amount != 12.4 {
		t.Errorf("balance = %v, want 12.4", snapshot.Balance.Amount)
	}
	if snapshot.Balance.Currency != "USD" {
		t.Errorf("currency = %q, want USD", snapshot.Balance.Currency)
	}
	if snapshot.Source != SourceBalance {
		t.Errorf("source = %q, want %q", snapshot.Source, SourceBalance)
	}
	if snapshot.Status != StatusOK {
		t.Errorf("status = %q, want %q", snapshot.Status, StatusOK)
	}
	if snapshot.ObservedAt.IsZero() {
		t.Error("ObservedAt is zero")
	}
	assertNoSecrets(t, "balance snapshot", snapshotDump(t, snapshot))
}

// TestFetchBalanceZeroIsExhausted pins that a zero balance is exhausted, not
// ok, and not mistaken for a missing figure.
func TestFetchBalanceZeroIsExhausted(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"included":{"balance_usd":0,"allowance_usd":50}}`))
	})
	snapshot, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != 0 {
		t.Fatalf("balance = %+v, want a present zero", snapshot.Balance)
	}
	if snapshot.Status != StatusExhausted {
		t.Errorf("status = %q, want %q", snapshot.Status, StatusExhausted)
	}
}

// TestFetchBalanceMissingFieldIsParseError pins that a present-but-empty
// payload is a failure, never "$0.00".
func TestFetchBalanceMissingFieldIsParseError(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"included":{}}`))
	})
	snapshot, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err == nil {
		t.Fatalf("Fetch err = nil, want parse failure; snapshot = %+v", snapshot)
	}
	if KindOf(err) != KindParse {
		t.Errorf("kind = %q, want %q", KindOf(err), KindParse)
	}
	if snapshot.HasData() {
		t.Errorf("failed snapshot must carry no data: %+v", snapshot)
	}
	if snapshot.Status != StatusUnavailable {
		t.Errorf("status = %q, want %q", snapshot.Status, StatusUnavailable)
	}
}

// TestFetchBalanceLegacyPlanWindows covers the legacy balance shape, which
// carries the session and weekly windows as remaining percentages.
func TestFetchBalanceLegacyPlanWindows(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"included":{"session":{"remaining_percent":70,"resets_at":"2026-10-30T18:00:00Z"},"weekly":{"remaining_percent":25,"resets_at":"2026-11-02T00:00:00Z"}},"plan":"Pro"}`))
	})
	snapshot, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.Plan != "Pro" {
		t.Errorf("plan = %q, want Pro", snapshot.Plan)
	}
	if len(snapshot.Windows) != 2 {
		t.Fatalf("windows = %+v, want 2", snapshot.Windows)
	}
	fiveHour, ok := snapshot.Window(WindowFiveHour)
	if !ok {
		t.Fatal("no 5h window")
	}
	if fiveHour.UsedPercent != 30 {
		t.Errorf("5h used = %v, want 30 (70 remaining)", fiveHour.UsedPercent)
	}
	if fiveHour.ResetAt == nil || !fiveHour.ResetAt.Equal(time.Date(2026, 10, 30, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("5h reset = %v", fiveHour.ResetAt)
	}
	weekly, ok := snapshot.Window(WindowWeekly)
	if !ok {
		t.Fatal("no weekly window")
	}
	if weekly.UsedPercent != 75 {
		t.Errorf("weekly used = %v, want 75 (25 remaining)", weekly.UsedPercent)
	}
	if snapshot.Status != StatusWarning {
		t.Errorf("status = %q, want %q", snapshot.Status, StatusWarning)
	}
}

// TestSettingsHTMLNormal covers SPEC AC-3: the settings page yields the two
// quota windows with percentages and reset times.
func TestSettingsHTMLNormal(t *testing.T) {
	const page = `<html><body>
<h2>Cloud usage</h2><p>Plan: Pro</p><p>Balance $12.40</p>
<div><span>Session usage</span><span>30% used</span>
  <span>Resets <time datetime="2026-10-30T18:00:00Z">in 3 hours</time></span></div>
<div><span>Weekly usage</span><span>65% used</span>
  <span>Resets <local-time data-time="2026-11-02T00:00:00Z">Nov 2</local-time></span></div>
</body></html>`

	f := newFixture(t)
	f.settings(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") == "" {
			t.Error("settings request carried no cookie header")
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	})

	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	snapshot, err := f.client(WithClock(func() time.Time { return now })).
		Fetch(t.Context(), Credential{Cookie: canaryCookie})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.Source != SourceSettings {
		t.Errorf("source = %q, want %q", snapshot.Source, SourceSettings)
	}
	if snapshot.Plan != "Pro" {
		t.Errorf("plan = %q, want Pro", snapshot.Plan)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != 12.4 {
		t.Errorf("balance = %+v, want $12.40", snapshot.Balance)
	}
	fiveHour, ok := snapshot.Window(WindowFiveHour)
	if !ok || fiveHour.UsedPercent != 30 {
		t.Fatalf("5h window = %+v", fiveHour)
	}
	weekly, ok := snapshot.Window(WindowWeekly)
	if !ok || weekly.UsedPercent != 65 {
		t.Fatalf("weekly window = %+v", weekly)
	}
	if snapshot.Status != StatusOK {
		t.Errorf("status = %q, want %q", snapshot.Status, StatusOK)
	}
	if snapshot.Balance.String() != "$12.40" {
		t.Errorf("balance string = %q, want $12.40", snapshot.Balance.String())
	}
	assertNoSecrets(t, "settings snapshot", snapshotDump(t, snapshot))
}

// TestSettingsHTMLRedesigned is the SPEC 4.3 and AC-4 test: a page whose
// structure no longer matches must be a parse failure, not a zero and not a
// wrong number.
func TestSettingsHTMLRedesigned(t *testing.T) {
	// A plausible redesign: the percentages moved into a chart element and the
	// labels changed wording. Nothing the parser looks for is present.
	const redesigned = `<html><body>
<div class="usage-chart" data-series="[40,60]"></div>
<div class="meter"><svg aria-label="cloud capacity 88"></svg></div>
<p class="tier">Pro member since 2025</p>
</body></html>`

	f := newFixture(t)
	f.settings(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(redesigned))
	})
	snapshot, err := f.client().Fetch(t.Context(), Credential{Cookie: canaryCookie})
	if err == nil {
		t.Fatalf("Fetch err = nil for a redesigned page; snapshot = %+v", snapshot)
	}
	if KindOf(err) != KindParse {
		t.Errorf("kind = %q, want %q", KindOf(err), KindParse)
	}
	if !errors.Is(err, ErrParse) {
		t.Errorf("errors.Is(err, ErrParse) = false; err = %v", err)
	}
	if snapshot.HasData() {
		t.Errorf("a parse failure must expose no figures: %+v", snapshot)
	}
	if len(snapshot.Windows) != 0 || snapshot.Balance != nil {
		t.Errorf("parse failure carried data: %+v", snapshot)
	}
	if snapshot.Status != StatusUnavailable {
		t.Errorf("status = %q, want %q", snapshot.Status, StatusUnavailable)
	}
	assertNoSecrets(t, "parse error", err.Error(), snapshotDump(t, snapshot))
}

// TestParseSettingsPureFunction exercises the exported pure entry point
// directly, including the relative-reset derivation.
func TestParseSettingsPureFunction(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	parsed, err := ParseSettings(`<section><h3>Weekly usage</h3><span>88% used</span><p>Resets in 6 days</p></section>`, now)
	if err != nil {
		t.Fatalf("ParseSettings: %v", err)
	}
	if len(parsed.Windows) != 1 {
		t.Fatalf("windows = %+v", parsed.Windows)
	}
	window := parsed.Windows[0]
	if window.UsedPercent != 88 {
		t.Errorf("used = %v, want 88", window.UsedPercent)
	}
	if window.ResetAt == nil || !window.ResetAt.Equal(now.Add(6*24*time.Hour)) {
		t.Fatalf("relative reset = %v", window.ResetAt)
	}
	if !window.ResetDerived {
		t.Error("a relative reset must be marked derived")
	}
}

// TestParseSettingsRemainingConverted pins that a page showing what is left is
// not read as usage.
func TestParseSettingsRemainingConverted(t *testing.T) {
	parsed, err := ParseSettings(`<div><p>Session usage</p><p>70% remaining</p></div>`, time.Now())
	if err != nil {
		t.Fatalf("ParseSettings: %v", err)
	}
	if len(parsed.Windows) != 1 || parsed.Windows[0].UsedPercent != 30 {
		t.Fatalf("remaining not converted to used: %+v", parsed.Windows)
	}
}

// TestParseSettingsOutOfRangeRefused pins that an impossible percentage is
// refused rather than clamped into a believable value.
func TestParseSettingsOutOfRangeRefused(t *testing.T) {
	_, err := ParseSettings(`<div><p>Session usage</p><p>140% used</p></div>`, time.Now())
	if err == nil || KindOf(err) != KindParse {
		t.Fatalf("err = %v, want a parse failure", err)
	}
}

// TestParseSettingsNoCrossWindowLeak pins that a percentage shared by two
// labels is refused rather than attributed to one of them: either attribution
// would display a real-but-wrong number for one window, which SPEC 4.3 forbids.
//
// This replaces the previous expectation, which accepted the reading when the
// weekly window (named on the page) silently disappeared with the five-hour
// window's figure. That was the partial-success bug R6 reports, so the page now
// has to fail as a whole.
func TestParseSettingsNoCrossWindowLeak(t *testing.T) {
	// The 5h label, one percentage, and the weekly label sit in three sibling
	// divs. The percentage is under no label's own element, so it is not claimed
	// by either window; the weekly window is named but unreadable.
	page := `<div><span>Session usage</span></div><div><span>40% used</span></div><div><span>Weekly usage</span></div>`
	parsed, err := ParseSettings(page, time.Now())
	if err == nil {
		t.Fatalf("ParseSettings err = nil; a shared percentage must not be assigned to either window: %+v", parsed)
	}
	if KindOf(err) != KindParse {
		t.Errorf("kind = %q, want %q", KindOf(err), KindParse)
	}
	if len(parsed.Windows) != 0 || parsed.Balance != nil || parsed.Plan != "" {
		t.Errorf("a refused parse must expose nothing: %+v", parsed)
	}
}

// TestFetchBothCredentialsFailKeepsBothClasses pins that when the key is
// rejected and the cookie has also expired, the caller hears both classes and
// the reported primary class is the balance path's.
func TestFetchBothCredentialsFailKeepsBothClasses(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	f.settings(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://ollama.com/signin", http.StatusFound)
	})
	_, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey, Cookie: canaryCookie})
	if err == nil {
		t.Fatal("Fetch err = nil, want a failure")
	}
	if !errors.Is(err, ErrAuth) {
		t.Errorf("errors.Is(err, ErrAuth) = false; err = %v", err)
	}
	// The primary class is the balance path's (auth), and the message names both
	// attempts so an operator knows both credentials are stale.
	if KindOf(err) != KindAuth {
		t.Errorf("kind = %q, want %q", KindOf(err), KindAuth)
	}
	if !strings.Contains(err.Error(), "API key") || !strings.Contains(err.Error(), "cookie") {
		t.Errorf("joined error should mention both paths; got %q", err.Error())
	}
	assertNoSecrets(t, "joined error", err.Error())
}

// TestFetchRateLimited covers the 429 path: classification plus Retry-After.
func TestFetchRateLimited(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	snapshot, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err == nil {
		t.Fatalf("Fetch err = nil, want rate limit; snapshot = %+v", snapshot)
	}
	if KindOf(err) != KindRateLimit {
		t.Fatalf("kind = %q, want %q", KindOf(err), KindRateLimit)
	}
	if !errors.Is(err, ErrRateLimit) {
		t.Errorf("errors.Is(err, ErrRateLimit) = false")
	}
	wait, ok := RetryAfterOf(err)
	if !ok {
		t.Fatal("RetryAfterOf reported no upstream backoff")
	}
	if wait != 45*time.Second {
		t.Errorf("retry after = %v, want 45s", wait)
	}
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatal("errors.As did not find *Error")
	}
	if failure.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", failure.StatusCode)
	}
	if snapshot.Status != StatusUnavailable {
		t.Errorf("status = %q, want %q", snapshot.Status, StatusUnavailable)
	}
	assertNoSecrets(t, "429 error", err.Error())
}

// TestFetchRateLimitedWithoutRetryAfter pins that a 429 with no header still
// carries a default backoff but reports that upstream did not specify one.
func TestFetchRateLimitedWithoutRetryAfter(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if KindOf(err) != KindRateLimit {
		t.Fatalf("kind = %q, want %q", KindOf(err), KindRateLimit)
	}
	wait, ok := RetryAfterOf(err)
	if ok {
		t.Error("HasRetryAfter = true for a response with no Retry-After header")
	}
	if wait != defaultRateLimitBackoff {
		t.Errorf("default backoff = %v, want %v", wait, defaultRateLimitBackoff)
	}
}

// TestFetchTimeout covers SPEC 4.4 and AC-7: an upstream that never answers
// returns a failure after the deadline instead of hanging.
func TestFetchTimeout(t *testing.T) {
	f := newFixture(t)
	// A handler that blocks until the client gives up. The test's own context
	// cancellation ends it, so the server does not leak past the test.
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	start := time.Now()
	snapshot, err := f.client(WithTimeout(150*time.Millisecond)).
		Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Fetch err = nil, want a timeout; snapshot = %+v", snapshot)
	}
	if KindOf(err) != KindNetwork {
		t.Errorf("kind = %q, want %q", KindOf(err), KindNetwork)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false; err = %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Fetch took %v, want roughly the 150ms deadline", elapsed)
	}
	if snapshot.Status != StatusUnavailable {
		t.Errorf("status = %q, want %q", snapshot.Status, StatusUnavailable)
	}
	assertNoSecrets(t, "timeout error", err.Error())
}

// context_DeadlineExceeded was a placeholder indirection and is gone; the test
// imports context directly.

// TestFetchAuthFailureScrubbed covers SPEC AC-6 and 4.2: a 401 must produce an
// auth-class error whose text contains neither the API key nor the cookie.
func TestFetchAuthFailureScrubbed(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	f.settings(func(w http.ResponseWriter, r *http.Request) {
		// The cookie path is tried only when it is present; give it a 401 too
		// so both credential paths are exercised for leaks.
		w.WriteHeader(http.StatusUnauthorized)
	})
	// A credential with both secrets present, so whichever path runs, a leak
	// would carry a canary.
	snapshot, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey, Cookie: canaryCookie})
	if err == nil {
		t.Fatalf("Fetch err = nil, want auth failure; snapshot = %+v", snapshot)
	}
	if KindOf(err) != KindAuth {
		t.Fatalf("kind = %q, want %q", KindOf(err), KindAuth)
	}
	if !errors.Is(err, ErrAuth) {
		t.Errorf("errors.Is(err, ErrAuth) = false")
	}
	assertNoSecrets(t, "auth error", err.Error(), snapshotDump(t, snapshot), string(snapshot.Status))
}

// TestFetchCookieLeakThroughMalformedBody is a stronger redaction test: the
// upstream returns a body crafted to echo the credential if any code path
// ever copied a decode error into the message. The assertion is that the error
// still does not contain it.
func TestFetchCookieLeakThroughMalformedBody(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		// Invalid JSON that itself contains the key, so a decoder that echoed
		// its input would leak it.
		_, _ = w.Write([]byte(`{"included": {"balance_usd": ` + canaryAPIKey + `}}`))
	})
	_, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err == nil {
		t.Fatal("Fetch err = nil, want a parse failure")
	}
	assertNoSecrets(t, "malformed-body error", err.Error())
}

// TestFetchRejectsOffOriginRedirect covers SPEC 4.2 and the redirect
// requirement: a 302 to evil.example is refused, never followed.
func TestFetchRejectsOffOriginRedirect(t *testing.T) {
	for _, path := range []string{"balance", "settings"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t)
			target := "https://evil.example/steal"
			handler := func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target, http.StatusFound)
			}
			if path == "balance" {
				f.balance(handler)
			} else {
				f.settings(handler)
			}
			credential := Credential{APIKey: canaryAPIKey}
			if path == "settings" {
				credential = Credential{Cookie: canaryCookie}
			}
			snapshot, err := f.client().Fetch(t.Context(), credential)
			if err == nil {
				t.Fatalf("Fetch err = nil, want the redirect refused; snapshot = %+v", snapshot)
			}
			if KindOf(err) != KindNetwork {
				t.Errorf("kind = %q, want %q for an off-origin redirect", KindOf(err), KindNetwork)
			}
			if snapshot.HasData() {
				t.Errorf("redirect failure carried data: %+v", snapshot)
			}
			// The target host must not appear as something the request was sent
			// to; the message may say a redirect was refused but must not carry
			// a credential.
			assertNoSecrets(t, "redirect error", err.Error())
		})
	}
}

// TestFetchLoginRedirectIsAuth pins that a redirect to the login page is an
// expired credential, not a generic network error.
func TestFetchLoginRedirectIsAuth(t *testing.T) {
	f := newFixture(t)
	f.settings(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://ollama.com/signin", http.StatusFound)
	})
	_, err := f.client().Fetch(t.Context(), Credential{Cookie: canaryCookie})
	if err == nil {
		t.Fatal("Fetch err = nil, want an auth failure")
	}
	if KindOf(err) != KindAuth {
		t.Errorf("kind = %q, want %q", KindOf(err), KindAuth)
	}
}

// TestFetchSettingsLoginPageIsAuth pins that a 200 login page means an expired
// cookie, not a redesigned settings page.
func TestFetchSettingsLoginPageIsAuth(t *testing.T) {
	f := newFixture(t)
	f.settings(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>Sign in - Ollama</title></head><body><form><input type="password" name="password"></form></body></html>`))
	})
	_, err := f.client().Fetch(t.Context(), Credential{Cookie: canaryCookie})
	if err == nil {
		t.Fatal("Fetch err = nil, want an auth failure")
	}
	if KindOf(err) != KindAuth {
		t.Errorf("kind = %q, want %q", KindOf(err), KindAuth)
	}
	assertNoSecrets(t, "login-page error", err.Error())
}

// TestFetchFallsBackToSettings covers SPEC 3.2: when the key path fails and a
// cookie is present, the settings page is tried.
func TestFetchFallsBackToSettings(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	f.settings(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<div><p>Session usage</p><p>20% used</p></div>`))
	})
	snapshot, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey, Cookie: canaryCookie})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.Source != SourceSettings {
		t.Errorf("source = %q, want %q", snapshot.Source, SourceSettings)
	}
	if len(snapshot.Windows) != 1 {
		t.Fatalf("windows = %+v", snapshot.Windows)
	}
}

// TestFetchMissingCredential pins that an empty credential is an auth failure,
// not a crash and not an upstream call.
func TestFetchMissingCredential(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.client().Fetch(t.Context(), Credential{})
	if err == nil {
		t.Fatal("Fetch err = nil, want a missing-credential failure")
	}
	if !errors.Is(err, ErrAuth) || !errors.Is(err, ErrMissingCredential) {
		t.Errorf("err = %v, want ErrAuth and ErrMissingCredential", err)
	}
	if snapshot.HasData() {
		t.Errorf("snapshot carried data: %+v", snapshot)
	}
	if f.lastBalanceRequest != nil || f.lastSettingsRequest != nil {
		t.Error("an empty credential must not reach the network")
	}
}

// TestTransportPanicRecovered covers SPEC 4.4: a panic in an injected
// transport must not escape into the CPA process.
func TestTransportPanicRecovered(t *testing.T) {
	f := newFixture(t)
	panicking := transportFunc(func(*http.Request) (*http.Response, error) {
		panic("boom")
	})
	_, err := f.client(WithTransport(panicking)).Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err == nil {
		t.Fatal("Fetch err = nil, want the panic recovered as an error")
	}
	if KindOf(err) != KindNetwork {
		t.Errorf("kind = %q, want %q", KindOf(err), KindNetwork)
	}
	assertNoSecrets(t, "panic error", err.Error())
}

// TestBodyLimitEnforced pins that an oversized body is rejected rather than
// read into memory unbounded.
func TestBodyLimitEnforced(t *testing.T) {
	f := newFixture(t)
	f.settings(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("A", 2048)))
	})
	_, err := f.client(WithMaxBodyBytes(1024)).Fetch(t.Context(), Credential{Cookie: canaryCookie})
	if err == nil {
		t.Fatal("Fetch err = nil, want a body-limit failure")
	}
	if KindOf(err) != KindNetwork {
		t.Errorf("kind = %q, want %q", KindOf(err), KindNetwork)
	}
}

// TestWithHTTPClientRefusesRedirects pins that the *http.Client convenience
// option keeps the redirect refusal even when the injected client would follow.
func TestWithHTTPClientRefusesRedirects(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/x", http.StatusFound)
	})
	// A client whose own policy is to follow redirects everywhere.
	followAll := &http.Client{}
	_, err := f.client(WithHTTPClient(followAll)).Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err == nil {
		t.Fatal("Fetch err = nil, want the redirect refused despite the injected client")
	}
	if KindOf(err) != KindNetwork {
		t.Errorf("kind = %q, want %q", KindOf(err), KindNetwork)
	}
}

// TestWithTransportRejectsRawRedirectingHTTPClient is the regression test for a
// real bypass: *http.Client satisfies Transport, so passing one straight to
// WithTransport could let it follow a 302 and carry the Bearer key to another
// host, then hand the followed body back as a successful reading. The injected
// client must be neutralised.
func TestWithTransportRejectsRawRedirectingHTTPClient(t *testing.T) {
	f := newFixture(t)
	var evilHits int
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHits++
		_, _ = w.Write([]byte(`{"included":{"balance_usd":999}}`))
	}))
	t.Cleanup(evil.Close)

	f.balance(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
	})
	// A client with the default (follow-redirects) policy, passed as Transport.
	_, err := f.client(WithTransport(&http.Client{})).Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err == nil {
		t.Fatal("Fetch err = nil, want the redirect refused")
	}
	if KindOf(err) != KindNetwork {
		t.Errorf("kind = %q, want %q", KindOf(err), KindNetwork)
	}
	if evilHits != 0 {
		t.Fatalf("the credential was carried to the off-origin host %d time(s)", evilHits)
	}
	assertNoSecrets(t, "raw-client redirect error", err.Error())
}

// TestCredentialStringRedacted pins that printing a Credential (which a debug
// log might do) reveals only presence, never the value.
func TestCredentialStringRedacted(t *testing.T) {
	credential := Credential{APIKey: canaryAPIKey, Cookie: canaryCookie}
	assertNoSecrets(t, "Credential.String", credential.String())
	assertNoSecrets(t, "Credential %+#v", fmt.Sprintf("%+#v", credential))
	if !strings.Contains(credential.String(), "set") {
		t.Errorf("String() = %q, want it to report presence", credential.String())
	}
}

// TestErrorClassifiers pins the errors.Is/errors.As contract across all four
// classes.
func TestErrorClassifiers(t *testing.T) {
	cases := []struct {
		status int
		kind   Kind
	}{
		{http.StatusUnauthorized, KindAuth},
		{http.StatusForbidden, KindAuth},
		{http.StatusTooManyRequests, KindRateLimit},
		{http.StatusInternalServerError, KindNetwork},
		{http.StatusBadGateway, KindNetwork},
	}
	for _, tc := range cases {
		err := errorForStatus("balance", tc.status)
		if err.Kind != tc.kind {
			t.Errorf("status %d -> kind %q, want %q", tc.status, err.Kind, tc.kind)
		}
		if !errors.Is(err, ErrUpstream) {
			t.Errorf("status %d: not ErrUpstream", tc.status)
		}
		if KindOf(err) != tc.kind {
			t.Errorf("status %d: KindOf = %q", tc.status, KindOf(err))
		}
	}
	if !errors.Is(newError(KindAuth, "balance", 401, "x"), ErrAuth) {
		t.Error("auth error did not match ErrAuth")
	}
	if errors.Is(newError(KindAuth, "balance", 401, "x"), ErrRateLimit) {
		t.Error("auth error wrongly matched ErrRateLimit")
	}
}

// --- small test helpers ---

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }
