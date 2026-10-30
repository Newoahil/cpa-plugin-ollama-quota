package ollama

// Regression tests for the nine required fixes in the #2 review round
// (.orchestration/run_b5727398c1ca/reports/review-2.md). Each test is named for
// its finding and uses that finding's minimal reproduction.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --- R1: the error chain must not carry the raw credential ---

// TestReviewRedactedErrorChain pins R1. A transport error whose text is the
// cookie itself must not be reachable by walking Unwrap on the returned error:
// err.Error() being safe is not enough when Unwrap() []error is public.
func TestReviewRedactedErrorChain(t *testing.T) {
	raw := errors.New(canaryCookie)
	transport := transportFunc(func(*http.Request) (*http.Response, error) {
		return nil, raw
	})
	_, err := NewClient(WithTransport(transport)).Fetch(t.Context(), Credential{Cookie: canaryCookie})
	if err == nil {
		t.Fatal("Fetch err = nil, want a network failure")
	}
	assertNoUnwrappedSecret(t, err)
}

// TestReviewRedactedErrorChainWrapped pins R1 against a transport that wraps the
// credential in another error type, which an errors.Is-based sanitisation pass
// could have missed.
func TestReviewRedactedErrorChainWrapped(t *testing.T) {
	raw := fmt.Errorf("dial failed: %w", errors.New("credential "+canaryAPIKey))
	transport := transportFunc(func(*http.Request) (*http.Response, error) {
		return nil, &urlErrorShim{inner: raw}
	})
	_, err := NewClient(WithTransport(transport)).Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err == nil {
		t.Fatal("Fetch err = nil, want a network failure")
	}
	assertNoUnwrappedSecret(t, err)
}

// TestNetworkErrorKeepsDeadlineSentinel pins the other half of R1: dropping the
// upstream error must not cost the caller the errors.Is(err,
// context.DeadlineExceeded) that TestFetchTimeout depends on.
func TestNetworkErrorKeepsDeadlineSentinel(t *testing.T) {
	failure := networkError("balance", context.DeadlineExceeded)
	if !errors.Is(failure, context.DeadlineExceeded) {
		t.Error("a deadline must still match context.DeadlineExceeded")
	}
	canceled := networkError("balance", context.Canceled)
	if !errors.Is(canceled, context.Canceled) {
		t.Error("a cancellation must still match context.Canceled")
	}
	if errors.Is(canceled, context.DeadlineExceeded) {
		t.Error("a cancellation wrongly matched DeadlineExceeded")
	}
}

// urlErrorShim wraps an error the way a *url.Error does, so the test exercises
// a cause that is not the original value.
type urlErrorShim struct{ inner error }

func (e *urlErrorShim) Error() string { return "Get \"https://ollama.com\": " + e.inner.Error() }
func (e *urlErrorShim) Unwrap() error { return e.inner }

// assertNoUnwrappedSecret walks the whole error chain and fails if the canary is
// reachable anywhere in it.
func assertNoUnwrappedSecret(t *testing.T, err error) {
	t.Helper()
	var leaked string
	seen := map[error]bool{}
	var walk func(error)
	walk = func(current error) {
		for current != nil && !seen[current] {
			seen[current] = true
			if strings.Contains(current.Error(), canaryCookie) || strings.Contains(current.Error(), canaryAPIKey) {
				leaked = current.Error()
				return
			}
			switch unwrapper := current.(type) {
			case interface{ Unwrap() []error }:
				for _, child := range unwrapper.Unwrap() {
					walk(child)
				}
				current = nil
			case interface{ Unwrap() error }:
				current = unwrapper.Unwrap()
			default:
				current = nil
			}
		}
	}
	walk(err)
	if leaked != "" {
		t.Fatalf("raw credential reachable in the error chain: %q", leaked)
	}
}

// --- R2: an echoed credential must not reach a successful Snapshot ---

// TestReviewEchoCookieInResetText pins R2 with the review's input: the settings
// page renders the cookie as the reset wording, which used to land verbatim in
// windows[0].reset_text.
func TestReviewEchoCookieInResetText(t *testing.T) {
	f := newFixture(t)
	f.settings(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<div>Session usage</div><div>20%% used</div><div>Resets at %s</div>`, canaryCookie)
	})
	snapshot, err := f.client().Fetch(t.Context(), Credential{Cookie: canaryCookie})
	if err == nil {
		t.Fatalf("Fetch err = nil, want the echo refused; snapshot = %s", snapshotDump(t, snapshot))
	}
	if KindOf(err) != KindParse {
		t.Errorf("kind = %q, want %q", KindOf(err), KindParse)
	}
	if snapshot.HasData() {
		t.Errorf("a refused reading must carry no figures: %s", snapshotDump(t, snapshot))
	}
	assertNoSecrets(t, "echoed reset text", err.Error(), snapshotDump(t, snapshot))
}

// TestReviewEchoAPIKeyInPage pins R2 at the balance boundary: the upstream
// echoes the key the request carried.
func TestReviewEchoAPIKeyInPage(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"included":{"balance_usd":12.4},"plan":"Pro ` + canaryAPIKey + `"}`))
	})
	snapshot, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err == nil {
		t.Fatalf("Fetch err = nil, want the echoed key refused; snapshot = %s", snapshotDump(t, snapshot))
	}
	if KindOf(err) != KindParse {
		t.Errorf("kind = %q, want %q", KindOf(err), KindParse)
	}
	if snapshot.HasData() {
		t.Errorf("a refused reading must carry no figures: %s", snapshotDump(t, snapshot))
	}
	assertNoSecrets(t, "echoed key", err.Error(), snapshotDump(t, snapshot))
}

// TestReviewEchoCookieValueOnly pins R2 for a cookie sent as "name=value": a
// page that echoes only the value must still be caught.
func TestReviewEchoCookieValueOnly(t *testing.T) {
	const value = "CANARY-cookie-DO-NOT-LEAK-7c1d4e"
	f := newFixture(t)
	f.settings(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<div><p>Session usage</p><p>20%% used</p><p>Resets at %s</p></div>`, value)
	})
	_, err := f.client().Fetch(t.Context(), Credential{Cookie: "__Secure-session=" + value})
	if err == nil {
		t.Fatal("Fetch err = nil, want the echoed cookie value refused")
	}
	if KindOf(err) != KindParse {
		t.Errorf("kind = %q, want %q", KindOf(err), KindParse)
	}
}

// TestRejectEchoRefusesEchoedPlan pins R2 for the plan field, which also carries
// upstream text unchanged.
func TestRejectEchoRefusesEchoedPlan(t *testing.T) {
	snapshot := Snapshot{
		Plan:    "Pro " + canaryCookie,
		Windows: []Window{{Name: WindowFiveHour, UsedPercent: 10}},
		Status:  StatusOK,
	}
	refused, err := rejectEcho(snapshot, nil, Credential{Cookie: canaryCookie})
	if err == nil {
		t.Fatalf("rejectEcho accepted an echoed plan; snapshot = %+v", refused)
	}
	if refused.HasData() {
		t.Errorf("refused snapshot carried data: %+v", refused)
	}
	assertNoSecrets(t, "echoed plan error", err.Error())
}

// --- R3: production must not be able to change the fixed origin ---

// TestReviewFixedHost pins R3. The credential may only ever be sent to the
// fixed ollama.com origin; there is no exported option that can move it.
func TestReviewFixedHost(t *testing.T) {
	var leaked bool
	transport := transportFunc(func(request *http.Request) (*http.Response, error) {
		if !strings.EqualFold(request.URL.Host, "ollama.com") && request.Header.Get("Authorization") != "" {
			leaked = true
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"included":{"balance_usd":1}}`)),
		}, nil
	})
	client := NewClient(WithTransport(transport))
	if client.balanceURL != BalanceURL || client.baseURL != "https://ollama.com" {
		t.Fatalf("default endpoints = %q / %q, want the fixed ollama.com origin", client.balanceURL, client.baseURL)
	}
	_, _ = client.Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if leaked {
		t.Fatal("the credential was sent to a host other than ollama.com")
	}
}

// TestCheckHostRefusesAnotherOrigin pins the last line of defence behind R3: a
// request whose origin is not the pinned one is refused before it is sent.
func TestCheckHostRefusesAnotherOrigin(t *testing.T) {
	client := NewClient()
	request, err := http.NewRequest(http.MethodGet, "https://evil-ollama.com/api/balance", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := client.checkHost(request.URL); err == nil {
		t.Fatal("checkHost accepted an off-origin request")
	}
}

// --- R4: a panic after do returns must not escape Fetch ---

// reviewPanicBody panics on Read, like a transport returning a body that blows
// up after the response was already handed back.
type reviewPanicBody struct{}

func (reviewPanicBody) Read([]byte) (int, error) { panic("body read panic") }
func (reviewPanicBody) Close() error             { return nil }

// TestReviewBodyReadPanicRecovered pins R4 for Body.Read.
func TestReviewBodyReadPanicRecovered(t *testing.T) {
	transport := transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: reviewPanicBody{}}, nil
	})
	assertPanicContained(t, NewClient(WithTransport(transport)), Credential{APIKey: canaryAPIKey})
}

// reviewPanicCloseBody panics only on Close.
type reviewPanicCloseBody struct{}

func (reviewPanicCloseBody) Read(buffer []byte) (int, error) { return 0, io.EOF }
func (reviewPanicCloseBody) Close() error                    { panic("body close panic") }

// TestReviewBodyClosePanicRecovered pins R4 for Body.Close, which runs in a
// deferred drainClose after a successful read.
func TestReviewBodyClosePanicRecovered(t *testing.T) {
	transport := transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: reviewPanicCloseBody{}}, nil
	})
	assertPanicContained(t, NewClient(WithTransport(transport)), Credential{APIKey: canaryAPIKey})
}

// TestReviewClockPanicRecovered pins R4 for the earliest thing Fetch does.
func TestReviewClockPanicRecovered(t *testing.T) {
	client := NewClient(WithClock(func() time.Time { panic("clock panic") }))
	assertPanicContained(t, client, Credential{APIKey: canaryAPIKey})
}

// assertPanicContained fails if Fetch lets a panic through, and checks the
// failure result carries no figures and a usable status.
func assertPanicContained(t *testing.T, client *Client, credential Credential) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("panic escaped Fetch: %v", recovered)
		}
	}()
	snapshot, err := client.Fetch(context.Background(), credential)
	if err == nil {
		t.Fatalf("Fetch err = nil after a panic; snapshot = %+v", snapshot)
	}
	if snapshot.HasData() {
		t.Errorf("panic failure carried figures: %+v", snapshot)
	}
	if snapshot.Status != StatusUnavailable {
		t.Errorf("status = %q, want %q", snapshot.Status, StatusUnavailable)
	}
	if snapshot.ObservedAt.IsZero() {
		t.Error("ObservedAt is zero, want the attempt's timestamp")
	}
	assertNoSecrets(t, "panic error", err.Error())
}

// --- R5: structure and attribution must survive ---

// TestReviewInlineWindows pins R5 case 1: two labelled windows inline in one
// div must keep their own percentages, not give both windows the first one.
func TestReviewInlineWindows(t *testing.T) {
	parsed, err := ParseSettings(`<div><span>Session usage 20% used</span><span>Weekly usage 80% used</span></div>`, time.Now())
	if err != nil {
		t.Fatalf("ParseSettings: %v", err)
	}
	fiveHour, ok := findWindow(parsed, WindowFiveHour)
	if !ok || fiveHour.UsedPercent != 20 {
		t.Fatalf("5h window = %+v, want 20", fiveHour)
	}
	weekly, ok := findWindow(parsed, WindowWeekly)
	if !ok || weekly.UsedPercent != 80 {
		t.Fatalf("weekly window = %+v, want 80 (not the 5h figure)", weekly)
	}
}

// TestReviewAttributeData pins R5 case 2: quota-looking text inside an
// attribute value is not page text, so the page has no readable window.
func TestReviewAttributeData(t *testing.T) {
	parsed, err := ParseSettings(`<div title="fake > Session usage 40% used">No quota available</div>`, time.Now())
	if err == nil {
		t.Fatalf("ParseSettings accepted fake quota text from an attribute: %+v", parsed)
	}
	if KindOf(err) != KindParse {
		t.Errorf("kind = %q, want %q", KindOf(err), KindParse)
	}
	if len(parsed.Windows) != 0 {
		t.Errorf("a refused parse exposed windows: %+v", parsed.Windows)
	}
}

// TestReviewCrossReset pins R5 case 3: a reset stated in one section must not
// also be handed to the window in the neighbouring section.
func TestReviewCrossReset(t *testing.T) {
	page := `<section><p>Session usage</p><p>20% used</p></section>` +
		`<section><p>Weekly usage</p><p>80% used</p><p>Resets in 6 days</p></section>`
	parsed, err := ParseSettings(page, time.Now())
	if err != nil {
		t.Fatalf("ParseSettings: %v", err)
	}
	fiveHour, ok := findWindow(parsed, WindowFiveHour)
	if !ok {
		t.Fatalf("no 5h window: %+v", parsed.Windows)
	}
	if fiveHour.ResetAt != nil || fiveHour.ResetText != "" {
		t.Fatalf("the 5h window took the weekly reset: %+v", fiveHour)
	}
	weekly, ok := findWindow(parsed, WindowWeekly)
	if !ok {
		t.Fatalf("no weekly window: %+v", parsed.Windows)
	}
	if weekly.ResetAt == nil || !weekly.ResetDerived {
		t.Fatalf("the weekly window did not keep its own reset: %+v", weekly)
	}
}

// TestResetConsumedOnce pins R5 at the narrowest shape: one reset line sitting
// between two labels inside a single container is refused rather than given to
// both windows, and is not given to the window on the far side of the other
// label.
func TestResetConsumedOnce(t *testing.T) {
	page := `<div><span>Session usage</span><span>20% used</span><span>Resets in 3 hours</span><span>Weekly usage</span><span>60% used</span></div>`
	parsed, err := ParseSettings(page, time.Now())
	if err != nil {
		t.Fatalf("ParseSettings: %v", err)
	}
	fiveHour, _ := findWindow(parsed, WindowFiveHour)
	weekly, _ := findWindow(parsed, WindowWeekly)
	if fiveHour.ResetAt != nil && weekly.ResetAt != nil {
		t.Fatalf("both windows claimed the single reset: %+v", parsed.Windows)
	}
	if weekly.ResetAt != nil {
		t.Fatalf("the weekly window took the 5h reset: %+v", parsed.Windows)
	}
}

// --- R6: a declared-but-unreadable window must fail the whole read ---

// TestReviewPartialHTML pins R6: the weekly window is named and its figure is
// out of range, so the page is refused instead of reporting only the 5h window.
func TestReviewPartialHTML(t *testing.T) {
	page := `<section><p>Session usage</p><p>20% used</p></section>` +
		`<section><p>Weekly usage</p><p>140% used</p></section>`
	parsed, err := ParseSettings(page, time.Now())
	if err == nil {
		t.Fatalf("ParseSettings accepted a partially unreadable page: %+v", parsed)
	}
	if KindOf(err) != KindParse {
		t.Errorf("kind = %q, want %q", KindOf(err), KindParse)
	}
	if len(parsed.Windows) != 0 {
		t.Errorf("a refused parse exposed windows: %+v", parsed.Windows)
	}
}

// TestDeclaredWindowWithoutFigure pins R6 for a known label with no percentage
// beside it: the window was declared, so its absence is an error, not a skip.
func TestDeclaredWindowWithoutFigure(t *testing.T) {
	page := `<section><p>Session usage</p><p>20% used</p></section><section><p>Weekly usage</p></section>`
	if _, err := ParseSettings(page, time.Now()); err == nil {
		t.Fatal("ParseSettings reported success while the weekly window had no figure")
	}
}

// TestLegacyWindowUnreadableIsFailure pins R6 for the balance payload's legacy
// shape: a named window with an impossible remaining_percent fails the read.
func TestLegacyWindowUnreadableIsFailure(t *testing.T) {
	_, err := parseBalance([]byte(`{"included":{"session":{"remaining_percent":70},"weekly":{"remaining_percent":140}}}`))
	if err == nil {
		t.Fatal("parseBalance dropped an unreadable declared window")
	}
	if KindOf(err) != KindParse {
		t.Errorf("kind = %q, want %q", KindOf(err), KindParse)
	}
}

// --- R7: purchased credit is spendable balance ---

// TestReviewPurchasedBalance pins R7 with the review's input: $0 included and
// $12.40 purchased is $12.40 available, not an exhausted account.
func TestReviewPurchasedBalance(t *testing.T) {
	parsed, err := parseBalance([]byte(`{"included":{"balance_usd":0},"purchased":{"balance_usd":12.4}}`))
	if err != nil {
		t.Fatalf("parseBalance: %v", err)
	}
	if parsed.Balance == nil || parsed.Balance.Amount != 12.4 {
		t.Fatalf("balance = %+v, want 12.4 available", parsed.Balance)
	}
	if parsed.Balance.Included == nil || *parsed.Balance.Included != 0 {
		t.Errorf("included = %v, want a present 0", parsed.Balance.Included)
	}
	if parsed.Balance.Purchased == nil || *parsed.Balance.Purchased != 12.4 {
		t.Errorf("purchased = %v, want 12.4", parsed.Balance.Purchased)
	}

	snapshot := Snapshot{Balance: parsed.Balance, ObservedAt: time.Now()}
	if status := snapshot.deriveStatus(DefaultWarningPercent); status == StatusExhausted {
		t.Error("an account with $12.40 purchased credit was reported exhausted")
	}
}

// TestFetchPurchasedBalanceIsNotExhausted pins R7 through Fetch end to end.
func TestFetchPurchasedBalanceIsNotExhausted(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"included":{"balance_usd":0},"purchased":{"balance_usd":12.4}}`))
	})
	snapshot, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != 12.4 {
		t.Fatalf("balance = %+v, want $12.40", snapshot.Balance)
	}
	if snapshot.Status != StatusOK {
		t.Errorf("status = %q, want %q with $12.40 available", snapshot.Status, StatusOK)
	}
}

// TestPurchasedOnlyBalanceIsData pins R7 for a response that carries only the
// purchased bucket: it used to be treated as no data at all.
func TestPurchasedOnlyBalanceIsData(t *testing.T) {
	parsed, err := parseBalance([]byte(`{"purchased":{"balance_usd":12.4}}`))
	if err != nil {
		t.Fatalf("parseBalance: %v", err)
	}
	if parsed.Balance == nil || parsed.Balance.Amount != 12.4 {
		t.Fatalf("balance = %+v, want 12.4 from the purchased bucket alone", parsed.Balance)
	}
	if parsed.Balance.Included != nil {
		t.Errorf("included = %v, want absent", *parsed.Balance.Included)
	}
}

// TestIncludedPlusPurchasedSum confirms the two buckets add up.
func TestIncludedPlusPurchasedSum(t *testing.T) {
	parsed, err := parseBalance([]byte(`{"included":{"balance_usd":2.5},"purchased":{"balance_usd":10}}`))
	if err != nil {
		t.Fatalf("parseBalance: %v", err)
	}
	if parsed.Balance == nil || parsed.Balance.Amount != 12.5 {
		t.Fatalf("balance = %+v, want 12.5", parsed.Balance)
	}
}

// testUnusablePurchased pins that an unusable purchased figure fails the read
// instead of being dropped back to the included figure.
func TestUnusablePurchasedIsRefused(t *testing.T) {
	_, err := parseBalance([]byte(`{"included":{"balance_usd":5},"purchased":{"balance_usd":-1}}`))
	if err == nil {
		t.Fatal("parseBalance accepted a negative purchased figure")
	}
}

// --- R8: a huge Retry-After must not become an immediate retry ---

// TestReviewHugeRetryAfter pins R8 with the review's input:
// Retry-After: 9223372036854775807 used to overflow to a negative duration and
// clamp to zero, i.e. retry immediately.
func TestReviewHugeRetryAfter(t *testing.T) {
	wait, ok := parseRetryAfter(http.Header{"Retry-After": []string{"9223372036854775807"}})
	if !ok {
		t.Fatal("parseRetryAfter reported no value for a huge delay-seconds")
	}
	if wait != 24*time.Hour {
		t.Fatalf("huge Retry-After = %v, want the 24h cap", wait)
	}
}

// TestRetryAfterBounds pins the edges around R8.
func TestRetryAfterBounds(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"0", 0},
		{"45", 45 * time.Second},
		{"86400", 24 * time.Hour},
		{"86401", 24 * time.Hour},
		{"9223372036854775808", 0}, // beyond int64: not a delay-seconds value at all
	}
	for _, tc := range cases {
		wait, ok := parseRetryAfter(http.Header{"Retry-After": []string{tc.header}})
		if tc.header == "9223372036854775808" {
			if ok {
				t.Errorf("Retry-After %q was accepted as %v", tc.header, wait)
			}
			continue
		}
		if !ok || wait != tc.want {
			t.Errorf("Retry-After %q = %v, %v; want %v, true", tc.header, wait, ok, tc.want)
		}
	}
}

// TestFetchHugeRetryAfterNeverRetriesImmediately pins R8 through Fetch: the
// reported backoff must be positive.
func TestFetchHugeRetryAfterNeverRetriesImmediately(t *testing.T) {
	f := newFixture(t)
	f.balance(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "9223372036854775807")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := f.client().Fetch(t.Context(), Credential{APIKey: canaryAPIKey})
	if err == nil {
		t.Fatal("Fetch err = nil, want a rate limit")
	}
	wait, ok := RetryAfterOf(err)
	if !ok || wait <= 0 {
		t.Fatalf("backoff = %v, %v; a huge Retry-After must clamp positive", wait, ok)
	}
}

// --- R9: partial numeric matches must not produce data ---

// TestReviewNegativeDuration pins R9 with the review's input: "-3h" used to be
// read as a positive three hours.
func TestReviewNegativeDuration(t *testing.T) {
	if duration, ok := parseDurationText("-3h"); ok {
		t.Fatalf("parseDurationText(\"-3h\") = %v, true; want a refusal", duration)
	}
}

// TestDurationRejectsDecimals pins R9 for "1.5 hours", which the digit scan used
// to read as "5 hours".
func TestDurationDecimalRefused(t *testing.T) {
	if duration, ok := parseDurationText("1.5 hours"); ok {
		t.Fatalf("parseDurationText(\"1.5 hours\") = %v, true; want a refusal", duration)
	}
}

// TestDurationRejectsTrailingGarbage pins R9's "do not skip a fragment and still
// succeed": text after the last recognised token makes the whole parse fail.
func TestDurationRejectsTrailingGarbage(t *testing.T) {
	for _, value := range []string{"3h approx", "about 3 hours", "3 furlongs", "3h and then some"} {
		if duration, ok := parseDurationText(value); ok {
			t.Errorf("parseDurationText(%q) = %v, true; want a refusal", value, duration)
		}
	}
}

// TestDurationRejectsOverflow pins R9's overflow half: a huge count must not
// wrap into a short or negative duration.
func TestDurationRejectsOverflow(t *testing.T) {
	for _, value := range []string{"999999999 weeks", "100000 weeks", "999999999 months"} {
		if duration, ok := parseDurationText(value); ok {
			t.Errorf("parseDurationText(%q) = %v, true; want a refusal", value, duration)
		}
	}
}

// TestDurationAcceptsOrdinaryText keeps R9 honest: the forms the page actually
// uses must still parse.
func TestDurationAcceptsOrdinaryText(t *testing.T) {
	cases := map[string]time.Duration{
		"3h":                  3 * time.Hour,
		"6 days":              6 * 24 * time.Hour,
		"2 hours 30 minutes":  2*time.Hour + 30*time.Minute,
		"2 hours, 30 minutes": 2*time.Hour + 30*time.Minute,
		"1 day and 6 hours":   30 * time.Hour,
		"90 minutes":          90 * time.Minute,
	}
	for text, want := range cases {
		got, ok := parseDurationText(text)
		if !ok || got != want {
			t.Errorf("parseDurationText(%q) = %v, %v; want %v, true", text, got, ok, want)
		}
	}
}

// TestReviewNegativeBalance pins R9's money half: "-$12.40" must not be read as
// a positive balance.
func TestReviewNegativeBalance(t *testing.T) {
	if balance := extractBalance([]string{"Balance: -$12.40"}); balance != nil {
		t.Fatalf("extractBalance read a negative balance as %s", balance.String())
	}
	if _, ok := parseMoney("-$12.40"); ok {
		t.Fatal("parseMoney accepted a negative amount")
	}
	if _, ok := parseMoney("−$12.40"); ok {
		t.Fatal("parseMoney accepted a Unicode-minus amount")
	}
}

// TestNegativeBalanceThroughParseSettings pins R9 for the whole page: a negative
// figure must not be reported as the balance.
func TestNegativeBalanceThroughParseSettings(t *testing.T) {
	parsed, err := ParseSettings(`<div><p>Session usage</p><p>20% used</p><p>Balance -$12.40</p></div>`, time.Now())
	if err != nil {
		t.Fatalf("ParseSettings: %v", err)
	}
	if parsed.Balance != nil {
		t.Fatalf("balance = %s, want no balance for a negative figure", parsed.Balance.String())
	}
}

// findWindow is the test-side lookup for a window by name.
func findWindow(parsed ParsedSettings, name string) (Window, bool) {
	for _, window := range parsed.Windows {
		if window.Name == name {
			return window, true
		}
	}
	return Window{}, false
}
