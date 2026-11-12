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

// The three fixtures below are what each upstream returns for a healthy account.

// cmcCreditsBody: 12.5 plan credits plus 3 purchased, a 5-hour window at 30% and
// a weekly window at 80%, both with millisecond reset instants.
const cmcCreditsBody = `{
  "credits": {"monthlyCredits": 12.5, "purchasedCredits": 3, "freeCredits": 0},
  "windowLimits": {
    "limited": true,
    "exceeded": null,
    "fiveHour": {"used": 30, "cap": 100, "exceeded": false, "resetAt": 1762000000000},
    "weekly":   {"used": 80, "cap": 100, "exceeded": false, "resetAt": 1762500000000}
  }
}`

const cmcSubscriptionBody = `{"success":true,"data":{"planId":"individual-max","status":"active"}}`

// opcUsageBody: the live shape, with the rolling window rate limited.
const opcUsageBody = `{
  "usage": {
    "rolling": {"status": "rate-limited", "percent": 100, "resetsAt": "2026-10-11T18:00:00Z"},
    "weekly":  {"status": "ok", "percent": 42, "resetsAt": "2026-10-14T00:00:00Z"},
    "monthly": {"status": "ok", "percent": 21, "resetsAt": "2026-11-01T00:00:00Z"}
  }
}`

// upstreamRouter answers each upstream by host and path, so one fixture host can
// serve all three providers at once.
func upstreamRouter(routes map[string]string) func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
	return func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		key := request.URL
		for prefix, body := range routes {
			if strings.HasPrefix(key, prefix) {
				return pluginapi.HTTPResponse{
					StatusCode: http.StatusOK,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       []byte(body),
				}
			}
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusNotFound}
	}
}

// newProviderPlugin registers a handler whose one credential is whatever the
// caller describes, and whose upstream answers come from routes.
func newProviderPlugin(t *testing.T, credential map[string]any, routes map[string]string) (*Handler, *fixtureHost) {
	t.Helper()
	host := newFixtureHost()
	host.setCredential("cred-1", credential)
	host.setRespond(upstreamRouter(routes))
	state := newFetchTestState(host)
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })
	return handler, host
}

// fullFetchPayload is the fetch response with the group name and bucket reset
// times, which the Command Code and OpenCode Go assertions need.
type fullFetchPayload struct {
	Subscription *struct {
		Plan string `json:"plan"`
	} `json:"subscription"`
	Summary []struct {
		Key      string  `json:"key"`
		Label    string  `json:"label"`
		Value    float64 `json:"value"`
		Format   string  `json:"format"`
		Currency string  `json:"currency"`
	} `json:"summary"`
	Groups []struct {
		DisplayName string `json:"displayName"`
		Buckets     []struct {
			Window            string  `json:"window"`
			RemainingFraction float64 `json:"remainingFraction"`
			ResetTime         string  `json:"resetTime"`
			Description       string  `json:"description"`
		} `json:"buckets"`
	} `json:"groups"`
}

func fetchFull(t *testing.T, handler *Handler, provider string) fullFetchPayload {
	t.Helper()
	raw, errHandle := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", provider))
	if errHandle != nil {
		t.Fatalf("fetch: %v", errHandle)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("fetch for %q failed: %+v", provider, envelope.Error)
	}
	var payload fullFetchPayload
	if errUnmarshal := json.Unmarshal(envelope.Result, &payload); errUnmarshal != nil {
		t.Fatalf("decode fetch: %v", errUnmarshal)
	}
	return payload
}

// A Command Code credential gets a group named for it, a credit-only balance
// metric, and one bucket per window with the remaining share pointing the other
// way from the used share.
func TestFetchCommandCodeMapsCreditsAndWindows(t *testing.T) {
	handler, host := newProviderPlugin(t, map[string]any{
		"type":     CommandCodeProvider,
		"api_key":  "cmc-fixture-key",
		"base_url": "https://api.commandcode.ai/provider/v1",
	}, map[string]string{
		"https://api.commandcode.ai/alpha/billing/credits":       cmcCreditsBody,
		"https://api.commandcode.ai/alpha/billing/subscriptions": cmcSubscriptionBody,
	})

	payload := fetchFull(t, handler, CommandCodeProvider)

	if payload.Subscription == nil || payload.Subscription.Plan != "Max" {
		t.Errorf("plan = %+v, want Max", payload.Subscription)
	}
	if len(payload.Summary) != 1 {
		t.Fatalf("summary = %+v, want one balance metric", payload.Summary)
	}
	// 12.5 plan + 3 purchased credits.
	if payload.Summary[0].Value != 15.5 {
		t.Errorf("balance = %v, want 15.5 credits", payload.Summary[0].Value)
	}
	if payload.Summary[0].Format != "number" || payload.Summary[0].Currency != "" {
		t.Errorf("credit figure rendered as %q/%q, want a plain number", payload.Summary[0].Format, payload.Summary[0].Currency)
	}
	if len(payload.Groups) != 1 || payload.Groups[0].DisplayName != "Command Code" {
		t.Fatalf("groups = %+v, want one group named Command Code", payload.Groups)
	}
	if len(payload.Groups[0].Buckets) != 2 {
		t.Fatalf("buckets = %+v, want two", payload.Groups[0].Buckets)
	}
	fiveHour, weekly := payload.Groups[0].Buckets[0], payload.Groups[0].Buckets[1]
	// 30% used → 0.7 remaining; 80% used → 0.2 remaining.
	if fiveHour.Window != "5h" || fiveHour.RemainingFraction < 0.699 || fiveHour.RemainingFraction > 0.701 {
		t.Errorf("five-hour bucket = %+v, want 0.7 remaining", fiveHour)
	}
	if weekly.Window != "7d" || weekly.RemainingFraction < 0.199 || weekly.RemainingFraction > 0.201 {
		t.Errorf("weekly bucket = %+v, want 0.2 remaining", weekly)
	}
	// The reset instant is the millisecond value, rendered as RFC3339.
	want := time.UnixMilli(1762000000000).UTC().Format(time.RFC3339)
	if fiveHour.ResetTime != want {
		t.Errorf("five-hour reset = %q, want %q", fiveHour.ResetTime, want)
	}

	// The key went to the account API, with the provider/v1 suffix trimmed off.
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != 2 {
		t.Fatalf("requests = %d, want the credits and subscription calls", len(host.requests))
	}
	for _, request := range host.requests {
		if request.Headers.Get("Authorization") != "Bearer cmc-fixture-key" {
			t.Errorf("%s Authorization = %q", request.URL, request.Headers.Get("Authorization"))
		}
		if strings.Contains(request.URL, "/provider/v1") {
			t.Errorf("request %q still carries the provider suffix", request.URL)
		}
	}
}

// Credits succeeding while subscriptions fails still yields the balance and the
// windows: the plan name is the only thing lost.
func TestFetchCommandCodeSurvivesSubscriptionFailure(t *testing.T) {
	handler, _ := newProviderPlugin(t, map[string]any{
		"type":    CommandCodeProvider,
		"api_key": "cmc-fixture-key",
	}, map[string]string{
		"https://api.commandcode.ai/alpha/billing/credits": cmcCreditsBody,
	})
	payload := fetchFull(t, handler, CommandCodeProvider)
	if payload.Subscription != nil && payload.Subscription.Plan != "" {
		t.Errorf("plan = %q, want none", payload.Subscription.Plan)
	}
	if len(payload.Summary) != 1 || payload.Summary[0].Value != 15.5 {
		t.Errorf("summary = %+v, want the credit figure", payload.Summary)
	}
	if len(payload.Groups) != 1 || len(payload.Groups[0].Buckets) != 2 {
		t.Errorf("groups = %+v, want both windows", payload.Groups)
	}
}

// An OpenCode Go credential maps the three windows; the rate-limited one is
// fully used, so its remaining share is 0.
func TestFetchOpenCodeMapsWindows(t *testing.T) {
	handler, host := newProviderPlugin(t, map[string]any{
		"type":     OpenCodeProvider,
		"api_key":  "opc-fixture-key",
		"base_url": "https://opencode.ai/zen/go",
	}, map[string]string{
		"https://opencode.ai/zen/go/v1/usage": opcUsageBody,
	})

	payload := fetchFull(t, handler, OpenCodeProvider)
	if len(payload.Summary) != 0 {
		t.Errorf("summary = %+v, want none: the usage endpoint reports no balance", payload.Summary)
	}
	if len(payload.Groups) != 1 || payload.Groups[0].DisplayName != "OpenCode Go" {
		t.Fatalf("groups = %+v, want one group named OpenCode Go", payload.Groups)
	}
	buckets := payload.Groups[0].Buckets
	if len(buckets) != 3 {
		t.Fatalf("buckets = %+v, want three", buckets)
	}
	if buckets[0].Window != "rolling" || buckets[0].RemainingFraction != 0 {
		t.Errorf("rolling bucket = %+v, want 0 remaining for a rate-limited window", buckets[0])
	}
	if !strings.Contains(buckets[0].Description, "rate limited") {
		t.Errorf("rate-limited bucket description = %q, must say so", buckets[0].Description)
	}
	if buckets[1].RemainingFraction < 0.579 || buckets[1].RemainingFraction > 0.581 {
		t.Errorf("weekly remaining = %v, want 0.58", buckets[1].RemainingFraction)
	}
	if buckets[2].RemainingFraction < 0.789 || buckets[2].RemainingFraction > 0.791 {
		t.Errorf("monthly remaining = %v, want 0.79", buckets[2].RemainingFraction)
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != 1 {
		t.Fatalf("requests = %d, want one", len(host.requests))
	}
	if got := host.requests[0].URL; got != "https://opencode.ai/zen/go/v1/usage" {
		t.Errorf("request = %q, want the usage path appended once", got)
	}
}

// The base URL a credential carries is used when it is not the vendor default,
// so a self-hosted or staging deployment is read correctly.
func TestFetchOpenCodeUsesCredentialBaseURL(t *testing.T) {
	handler, host := newProviderPlugin(t, map[string]any{
		"type":     OpenCodeProvider,
		"api_key":  "opc-fixture-key",
		"base_url": "https://staging.example.test/zen/go/",
	}, map[string]string{
		"https://staging.example.test/zen/go/v1/usage": opcUsageBody,
	})
	fetchFull(t, handler, OpenCodeProvider)
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != 1 || host.requests[0].URL != "https://staging.example.test/zen/go/v1/usage" {
		t.Fatalf("request = %+v, want the credential's own base", host.requests)
	}
}

// A window the provider package skips is skipped, not failed: an unknown status
// leaves the other windows on the card.
func TestFetchOpenCodeSkipsUnknownStatus(t *testing.T) {
	const body = `{"usage":{"rolling":{"status":"paused","percent":90},"weekly":{"status":"ok","percent":10}}}`
	handler, _ := newProviderPlugin(t, map[string]any{"type": OpenCodeProvider, "api_key": "k"},
		map[string]string{"https://opencode.ai/zen/go/v1/usage": body})
	payload := fetchFull(t, handler, OpenCodeProvider)
	buckets := payload.Groups[0].Buckets
	if len(buckets) != 1 || buckets[0].Window != "weekly" {
		t.Fatalf("buckets = %+v, want only the weekly window", buckets)
	}
}

// When no window is readable the fetch fails with no figures: the page must not
// show an empty card as a success.
func TestFetchOpenCodeFailsWhenNothingReadable(t *testing.T) {
	handler, _ := newProviderPlugin(t, map[string]any{"type": OpenCodeProvider, "api_key": "k"},
		map[string]string{"https://opencode.ai/zen/go/v1/usage": `{"usage":{"rolling":{"status":"paused"}}}`})
	raw, errHandle := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", OpenCodeProvider))
	if errHandle != nil {
		t.Fatalf("fetch: %v", errHandle)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatalf("an unreadable payload must not succeed: %s", envelope.Result)
	}
	if !strings.Contains(envelope.Error.Code, "parse") {
		t.Errorf("error code = %q, want parse_failed", envelope.Error.Code)
	}
}

// A credential with no key is a credential problem, reported as such and with no
// upstream request spent.
func TestFetchAPICompatibleWithoutKeyFailsExplicitly(t *testing.T) {
	handler, host := newProviderPlugin(t, map[string]any{"type": CommandCodeProvider}, nil)
	raw, _ := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", CommandCodeProvider))
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatalf("a keyless credential must not succeed: %s", envelope.Result)
	}
	if envelope.Error.Code != "credential_incomplete" {
		t.Errorf("error code = %q, want credential_incomplete", envelope.Error.Code)
	}
	if host.requestCount() != 0 {
		t.Errorf("requests = %d, want none", host.requestCount())
	}
}

// A provider key the plugin does not serve is refused, so the page never shows a
// number collected for the wrong provider.
func TestFetchRefusesForeignProvider(t *testing.T) {
	handler, _ := newProviderPlugin(t, map[string]any{"type": QuotaIdentifier, "api_key": "k"}, nil)
	raw, _ := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "some-other-provider"))
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("a foreign provider must be refused")
	}
	if envelope.Error.Code != "unsupported_provider" {
		t.Errorf("error code = %q, want unsupported_provider", envelope.Error.Code)
	}
}

// The provider key is matched the way the host spells it: the Command Code key
// keeps its internal space and a differently-cased key still resolves.
func TestFetchMatchesProviderKeysVerbatim(t *testing.T) {
	for _, provider := range []string{CommandCodeProvider, strings.ToUpper(CommandCodeProvider)} {
		handler, _ := newProviderPlugin(t, map[string]any{"type": CommandCodeProvider, "api_key": "cmc-fixture-key"},
			map[string]string{"https://api.commandcode.ai/alpha/billing/credits": cmcCreditsBody})
		payload := fetchFull(t, handler, provider)
		if len(payload.Groups) != 1 || payload.Groups[0].DisplayName != "Command Code" {
			t.Errorf("provider %q produced %+v", provider, payload.Groups)
		}
	}
}

// The collector dispatches by the credential's own provider: a Command Code
// credential must not be collected through the Ollama client, and an Ollama
// credential must not be sent to the Command Code account API.
func TestCollectorDispatchesByCredentialProvider(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{
		"type":     CommandCodeProvider,
		"api_key":  "cmc-fixture-key",
		"base_url": "https://api.commandcode.ai/provider/v1",
	})
	host.setRespond(upstreamRouter(map[string]string{
		"https://api.commandcode.ai/alpha/billing/credits":       cmcCreditsBody,
		"https://api.commandcode.ai/alpha/billing/subscriptions": cmcSubscriptionBody,
	}))
	host.files[0].Provider = CommandCodeProvider

	state := NewStateFor(host.call)
	if _, errRegister := state.Register([]byte("enabled: true\ninterval_seconds: 3600\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		if entry, ok := state.provider.store.load("openai-compatible-command code:cred-1"); ok && entry.hasData() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the collector never cached a Command Code reading")
		}
		time.Sleep(10 * time.Millisecond)
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	for _, request := range host.requests {
		if strings.Contains(request.URL, "ollama.com") {
			t.Fatalf("a Command Code credential was collected through the Ollama client: %s", request.URL)
		}
	}
}

// isQuotaCredential must recognise all three keys and reject a foreign one, with
// the Command Code key's internal space intact.
func TestIsQuotaCredential(t *testing.T) {
	cases := []struct {
		name  string
		entry pluginapi.HostAuthFileEntry
		want  bool
	}{
		{"ollama provider", pluginapi.HostAuthFileEntry{Provider: "ollama"}, true},
		{"ollama type only", pluginapi.HostAuthFileEntry{Type: "ollama"}, true},
		{"command code with space", pluginapi.HostAuthFileEntry{Provider: "openai-compatible-command code"}, true},
		{"command code uppercase", pluginapi.HostAuthFileEntry{Provider: "OpenAI-Compatible-Command Code"}, true},
		{"opencode", pluginapi.HostAuthFileEntry{Provider: "openai-compatible-opencode"}, true},
		{"foreign provider", pluginapi.HostAuthFileEntry{Provider: "openai-compatible-something"}, false},
		{"no provider", pluginapi.HostAuthFileEntry{}, true},
	}
	for _, tc := range cases {
		if got := isQuotaCredential(tc.entry); got != tc.want {
			t.Errorf("%s: isQuotaCredential = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// A 429 after a successful read still serves the last figures, and the card says
// upstream asked it to wait. A silent stale number would look like a live one.
func TestFetchRateLimitedAfterSuccessSaysWait(t *testing.T) {
	handler, host := newProviderPlugin(t, map[string]any{
		"type":    CommandCodeProvider,
		"api_key": "cmc-fixture-key",
	}, map[string]string{
		"https://api.commandcode.ai/alpha/billing/credits": cmcCreditsBody,
	})
	first := fetchFull(t, handler, CommandCodeProvider)
	if len(first.Groups) != 1 {
		t.Fatalf("the first read must produce a card: %+v", first.Groups)
	}

	// Now upstream rate limits this credential.
	host.setRespond(func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		return pluginapi.HTTPResponse{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": []string{"120"}},
		}
	})

	payload := fetchFull(t, handler, CommandCodeProvider)
	if len(payload.Groups) != 1 {
		t.Fatalf("a rate-limited read must serve the last figures: %+v", payload.Groups)
	}
	description := payload.Groups[0].Buckets[0].Description
	if !strings.Contains(description, "stale") {
		t.Errorf("description = %q, must say the figures are stale", description)
	}
	if !strings.Contains(description, "wait") {
		t.Errorf("description = %q, must say upstream asked to wait", description)
	}
}

// A saved page-configured Ollama key must not stop the other providers from
// being collected. The configured key is Ollama's alone, so a sweep that
// returned at it would leave every Command Code and OpenCode Go card empty
// forever once someone had entered an Ollama key.
func TestCollectorStillSweepsOtherProvidersWhenAKeyIsConfigured(t *testing.T) {
	stateDir := t.TempDir()
	store := &settingsStore{}
	store.setDirectory(stateDir)
	if err := store.save("ollama-configured-key"); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	host := newFixtureHost()
	host.setCredential("cmc-1", map[string]any{
		"type":    CommandCodeProvider,
		"api_key": "cmc-fixture-key",
	})
	host.setRespond(upstreamRouter(map[string]string{
		"https://api.commandcode.ai/alpha/billing/credits":       cmcCreditsBody,
		"https://api.commandcode.ai/alpha/billing/subscriptions": cmcSubscriptionBody,
	}))
	host.files[0].Provider = CommandCodeProvider

	state := NewStateFor(host.call)
	if _, errRegister := state.Register([]byte("enabled: true\ninterval_seconds: 3600\nstate_dir: " + stateDir + "\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		if entry, ok := state.provider.store.load(CommandCodeProvider + ":cmc-1"); ok && entry.hasData() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a configured Ollama key stopped the Command Code credential from being collected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The credential layers are read in the mandated order: Metadata, then the
// stored JSON, then Attributes — which is last because a verified capture from
// CPA v8.0.47 shows a disk credential arriving with only auth_kind/path/source/
// source_backend in its attributes and the key in metadata.
func TestAPICompatibleCredentialPrecedence(t *testing.T) {
	const (
		fromMetadata  = "key-from-metadata"
		fromStorage   = "key-from-storage"
		fromAttribute = "key-from-attributes"
	)
	stored := func(key string) []byte {
		raw, _ := json.Marshal(map[string]any{"api_key": key})
		return raw
	}

	cases := []struct {
		name       string
		attributes map[string]string
		metadata   map[string]any
		storage    []byte
		want       string
	}{
		{
			name:       "all three present: metadata wins",
			attributes: map[string]string{"api_key": fromAttribute},
			metadata:   map[string]any{"api_key": fromMetadata},
			storage:    stored(fromStorage),
			want:       fromMetadata,
		},
		{
			name:       "no metadata: the stored JSON is next",
			attributes: map[string]string{"api_key": fromAttribute},
			storage:    stored(fromStorage),
			want:       fromStorage,
		},
		{
			name:       "config-synthesized: attributes are the only source",
			attributes: map[string]string{"api_key": fromAttribute},
			want:       fromAttribute,
		},
	}
	for _, tc := range cases {
		key, _, err := apiCompatibleCredential(tc.attributes, tc.metadata, tc.storage)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if key != tc.want {
			t.Errorf("%s: key came from the wrong layer, want the %q one", tc.name, tc.want)
		}
	}
}

// A disk credential as CPA v8.0.47 actually delivers it: attributes carry only
// auth_kind/path/source/source_backend, and the key is in metadata.
func TestAPICompatibleCredentialReadsDiskShape(t *testing.T) {
	key, baseURL, err := apiCompatibleCredential(
		map[string]string{
			"auth_kind":      "api_key",
			"path":           "/data/auths/command-code.json",
			"source":         "file",
			"source_backend": "file",
		},
		map[string]any{"api_key": "disk-key", "base_url": "https://api.commandcode.ai/provider/v1", "type": CommandCodeProvider},
		nil,
	)
	if err != nil {
		t.Fatalf("a disk-shaped credential must be readable: %v", err)
	}
	if key != "disk-key" {
		t.Error("the key was not read from metadata")
	}
	if baseURL != "https://api.commandcode.ai/provider/v1" {
		t.Errorf("baseURL = %q, want the metadata value", baseURL)
	}
}

// The key and base URL never appear in the error a credential failure returns.
func TestAPICompatibleCredentialErrorCarriesNoSecret(t *testing.T) {
	const canary = "cmc-canary-key-7731"
	_, _, err := apiCompatibleCredential(map[string]string{"base_url": canary}, nil, nil)
	if err == nil {
		t.Fatal("a keyless credential must fail")
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "auths/") {
		t.Errorf("error leaks credential material: %s", err)
	}
}

// The store keys are per provider, so the same credential index read for one
// provider never serves another's figure.
func TestStoreKeysAreProviderScoped(t *testing.T) {
	request := pluginapi.QuotaFetchRequest{AuthIndex: "cred-1"}
	first := providerReadingKey(CommandCodeProvider, request, "key-a")
	second := providerReadingKey(OpenCodeProvider, request, "key-a")
	if first == second {
		t.Fatalf("both providers key the same entry %q", first)
	}
	if again := providerReadingKey(CommandCodeProvider, request, "key-b"); again != first {
		t.Error("the key must be stable for one credential regardless of which secret arrived")
	}
}
