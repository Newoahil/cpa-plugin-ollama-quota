package quotaplugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fetchPayload is the part of a quota.fetch response the tests assert on.
type fetchPayload struct {
	Subscription *struct {
		Plan string `json:"plan"`
	} `json:"subscription"`
	Summary []struct {
		Key   string  `json:"key"`
		Label string  `json:"label"`
		Value float64 `json:"value"`
	} `json:"summary"`
	Groups []struct {
		Buckets []struct {
			Window            string  `json:"window"`
			RemainingFraction float64 `json:"remainingFraction"`
			Description       string  `json:"description"`
		} `json:"buckets"`
	} `json:"groups"`
}

// newFixturePlugin builds a registered handler over a fixture host holding one
// credential ("cred-1") with an API key, answering with the given upstream
// responder. The handler is shut down at the end of the test, which also stops
// the collector it started.
func newFixturePlugin(t *testing.T, respond func(pluginapi.HTTPRequest) pluginapi.HTTPResponse) (*Handler, *fixtureHost) {
	t.Helper()
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(respond)
	state := newFetchTestState(host)
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })
	return handler, host
}

// newFetchTestState returns a state whose provider collects through the fixture
// host but which starts no background timer. The fetch tests assert exact
// upstream request counts, and the collector's immediate first sweep would race
// them for that count; the timer has its own tests in collector_test.go.
func newFetchTestState(host *fixtureHost) *State {
	state := NewStateFor(host.call)
	state.SetCollectorStarter(nil)
	return state
}

// fetchOnce performs one quota fetch and returns the decoded payload. It fails
// the test if the fetch itself errored, which is the "never succeeded" case the
// caller wants to distinguish explicitly.
func fetchOnce(t *testing.T, handler *Handler) fetchPayload {
	t.Helper()
	raw, errHandle := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	if errHandle != nil {
		t.Fatalf("fetch: %v", errHandle)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("fetch failed: %+v", envelope.Error)
	}
	var payload fetchPayload
	if errUnmarshal := json.Unmarshal(envelope.Result, &payload); errUnmarshal != nil {
		t.Fatalf("decode fetch: %v", errUnmarshal)
	}
	return payload
}

// A failed attempt after a success serves the last good figures, marks them
// stale, and does not clear them.
func TestFetchFallsBackToStaleReading(t *testing.T) {
	succeeding := true
	handler, _ := newFixturePlugin(t, func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if succeeding {
			return balanceOK(balanceBody)(pluginapi.HTTPRequest{})
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusInternalServerError, Body: []byte("upstream is down")}
	})

	first := fetchOnce(t, handler)
	if len(first.Summary) != 1 || first.Summary[0].Value != 12.40 {
		t.Fatalf("first fetch = %+v, want the $12.40 balance", first)
	}
	if strings.Contains(first.Summary[0].Label, "stale") {
		t.Errorf("a fresh reading must not be labelled stale: %q", first.Summary[0].Label)
	}

	succeeding = false
	stale := fetchOnce(t, handler)

	// The numbers are the ones actually read: not zeroes, not a fresh value.
	if len(stale.Summary) != 1 {
		t.Fatalf("summary = %+v, want the previous balance kept", stale.Summary)
	}
	if stale.Summary[0].Value != 12.40 {
		t.Errorf("stale balance = %v, want the previous 12.4", stale.Summary[0].Value)
	}
	if stale.Subscription == nil || stale.Subscription.Plan != "Pro" {
		t.Errorf("stale plan = %+v, want the previous Pro", stale.Subscription)
	}
	// And it says so: nothing in this response can be mistaken for fresh.
	if !strings.Contains(stale.Summary[0].Label, "stale") {
		t.Errorf("stale balance label = %q, must say stale", stale.Summary[0].Label)
	}
	if len(stale.Groups) != 1 || len(stale.Groups[0].Buckets) == 0 {
		t.Fatalf("stale groups = %+v, want the previous buckets kept", stale.Groups)
	}
	for _, bucket := range stale.Groups[0].Buckets {
		if !strings.Contains(bucket.Description, "stale") {
			t.Errorf("bucket %q description = %q, must say stale", bucket.Window, bucket.Description)
		}
	}
}

// A credential that has never succeeded returns an error and no figures at all.
func TestFetchNeverSucceededReturnsErrorWithoutNumbers(t *testing.T) {
	handler, _ := newFixturePlugin(t, func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		return pluginapi.HTTPResponse{StatusCode: http.StatusBadGateway, Body: []byte("nope")}
	})

	raw, errHandle := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	if errHandle != nil {
		t.Fatalf("fetch: %v", errHandle)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatalf("a credential that never succeeded must not return figures: %s", envelope.Result)
	}
	if envelope.Error == nil || envelope.Error.Message == "" {
		t.Fatal("the failure must carry a reason")
	}
	// Nothing displayable may have leaked into the error frame: no metric, no
	// bucket, no figure. The message may name the endpoint ("the balance
	// endpoint"), which is why the check is on the response keys.
	for _, key := range []string{`"summary"`, `"groups"`, `"remainingFraction"`, `"subscription"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("the error frame carries quota data (%s): %s", key, raw)
		}
	}
}

// A 429 whose Retry-After is in the future is served from the cache and is not
// retried until the window passes.
func TestFetchHonoursRetryAfterBackoff(t *testing.T) {
	rateLimited := false
	handler, host := newFixturePlugin(t, func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if rateLimited {
			return pluginapi.HTTPResponse{
				StatusCode: http.StatusTooManyRequests,
				Headers:    http.Header{"Retry-After": []string{"120"}},
			}
		}
		return balanceOK(balanceBody)(pluginapi.HTTPRequest{})
	})

	_ = fetchOnce(t, handler)
	before := host.requestCount()

	rateLimited = true
	stale := fetchOnce(t, handler)
	if len(stale.Summary) != 1 || stale.Summary[0].Value != 12.40 {
		t.Fatalf("a 429 after a success must serve the previous figures: %+v", stale.Summary)
	}
	if !strings.Contains(stale.Summary[0].Label, "stale") {
		t.Errorf("the 429 response must be marked stale: %q", stale.Summary[0].Label)
	}
	afterFirstLimited := host.requestCount()
	if afterFirstLimited != before+1 {
		t.Fatalf("upstream requests = %d, want %d (one retry that hit the 429)", afterFirstLimited, before+1)
	}

	// Inside the 120s window there must be no further upstream request, and the
	// response must say why it is not retrying.
	for i := 0; i < 3; i++ {
		again := fetchOnce(t, handler)
		if len(again.Summary) != 1 || again.Summary[0].Value != 12.40 {
			t.Fatalf("fetch %d inside the backoff = %+v", i, again.Summary)
		}
		if len(again.Groups) == 0 || len(again.Groups[0].Buckets) == 0 {
			t.Fatalf("fetch %d inside the backoff lost its buckets", i)
		}
		if description := again.Groups[0].Buckets[0].Description; !strings.Contains(description, "wait") {
			t.Errorf("fetch %d bucket description = %q, want the backoff explained", i, description)
		}
	}
	if got := host.requestCount(); got != afterFirstLimited {
		t.Errorf("upstream requests = %d, want %d: the backoff window was ignored", got, afterFirstLimited)
	}
}

// A 429 on a credential that has never succeeded produces an error rather than
// a zero, and still installs the backoff window.
func TestFetchRateLimitBeforeAnySuccessReturnsError(t *testing.T) {
	handler, host := newFixturePlugin(t, func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		return pluginapi.HTTPResponse{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": []string{"60"}},
		}
	})

	raw, _ := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("a rate-limited credential with no history must not return figures")
	}
	if envelope.Error.Code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", envelope.Error.Code)
	}

	issued := host.requestCount()
	raw, _ = handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	if envelope := decodeEnvelope(t, raw); envelope.OK {
		t.Fatal("a retry inside the backoff window must not succeed")
	}
	if got := host.requestCount(); got != issued {
		t.Errorf("upstream requests = %d, want %d: the backoff window was ignored", got, issued)
	}
}

// A 429 with no Retry-After still backs off, using the collector's default.
func TestFetchRateLimitWithoutRetryAfterStillBacksOff(t *testing.T) {
	handler, host := newFixturePlugin(t, func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		return pluginapi.HTTPResponse{StatusCode: http.StatusTooManyRequests}
	})
	_, _ = handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	issued := host.requestCount()
	_, _ = handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	if got := host.requestCount(); got != issued {
		t.Errorf("upstream requests = %d, want %d: a 429 without Retry-After must still back off", got, issued)
	}
}

// A credential with no API key and no cookie fails with an explicit error and
// issues no request.
func TestFetchMissingCredentialFailsExplicitly(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama"})
	host.setRespond(balanceOK(balanceBody))
	state := newFetchTestState(host)
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })

	raw, _ := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("an incomplete credential must not produce a figure")
	}
	if envelope.Error.Code != "credential_incomplete" {
		t.Errorf("error code = %q, want credential_incomplete", envelope.Error.Code)
	}
	if got := host.requestCount(); got != 0 {
		t.Errorf("upstream requests = %d, want 0", got)
	}
}

// The cookie path is used when only a cookie is stored (SPEC 3.2 path two).
func TestFetchUsesCookieWhenNoAPIKey(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "cookie": "session-token"})
	host.setRespond(func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if !strings.HasSuffix(request.URL, "/settings") {
			return pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized}
		}
		return pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html"}},
			Body:       []byte(settingsHTML),
		}
	})
	state := newFetchTestState(host)
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })

	payload := fetchOnce(t, handler)
	if payload.Subscription == nil || payload.Subscription.Plan == "" {
		t.Errorf("cookie path returned no plan: %+v", payload.Subscription)
	}
	if len(payload.Groups) == 0 {
		t.Fatal("cookie path returned no quota window")
	}
	// The request carried the cookie header and went to the fixed host.
	request := host.lastRequest()
	if !strings.Contains(request.URL, "ollama.com") {
		t.Errorf("request URL = %q, want the fixed ollama.com origin", request.URL)
	}
	if got := request.Headers.Get("Cookie"); got == "" {
		t.Error("the settings request carried no Cookie header")
	}
}

// An upstream that never answers is abandoned at the collection deadline rather
// than hanging the call, and the abandoned exchange is cancelled through the
// host so nothing is left parked.
func TestFetchTimeoutIsEnforcedAndCancelled(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setBlock()

	provider := NewProvider(ProviderOptions{HostCall: host.call, Timeout: 150 * time.Millisecond})
	state := newFetchTestState(host)
	state.provider = provider
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "interval_seconds: 3600\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })

	start := time.Now()
	raw, _ := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	elapsed := time.Since(start)

	if envelope := decodeEnvelope(t, raw); envelope.OK {
		t.Fatal("a hung upstream must produce a failure, not a figure")
	}
	if elapsed > 5*time.Second {
		t.Errorf("the fetch took %s; the 150ms deadline was not enforced", elapsed)
	}
	if host.cancelCount() == 0 {
		t.Error("the timed-out exchange was never cancelled through the host")
	}
}

// A panic anywhere in the collection path becomes a recorded failure; the
// process keeps running and the plugin serves again afterwards.
func TestPanickingCollectionDoesNotCrash(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(balanceBody))
	host.setPanicOnHTTP(true)

	state := newFetchTestState(host)
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })

	raw, errHandle := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	if errHandle != nil {
		t.Fatalf("handler returned a Go error instead of an envelope: %v", errHandle)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("a panicking collection must not be reported as a success")
	}

	host.setPanicOnHTTP(false)
	payload := fetchOnce(t, handler)
	if len(payload.Summary) != 1 || payload.Summary[0].Value != 12.40 {
		t.Errorf("the plugin must recover after a panic: %+v", payload)
	}
}
