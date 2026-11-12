package quotaplugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const enabledConfig = "enabled: true\n"
const brokenConfig = "ignored: 1\ninterval_seconds: not-a-number\n"

// newHandlerForTest returns a handler over a fresh state with no host bridge.
// It is for tests that never reach the network.
func newHandlerForTest(t *testing.T) *Handler {
	t.Helper()
	return NewHandler(NewState())
}

// newHandlerWithHost returns a handler whose collections and credential reads
// go through the fixture host.
func newHandlerWithHost(t *testing.T, host *fixtureHost) *Handler {
	t.Helper()
	return NewHandlerWithHost(NewStateFor(host.call), host.call)
}

// newRegisteredHandler returns a registered handler backed by a fixture host
// with one credential and a successful balance answer.
func newRegisteredHandler(t *testing.T) *Handler {
	t.Helper()
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(balanceBody))
	h := newHandlerWithHost(t, host)
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { h.state.Shutdown() })
	return h
}

func decodeEnvelope(t *testing.T, raw []byte) Envelope {
	t.Helper()
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v (body %s)", err, raw)
	}
	return envelope
}

// fetchRequest builds the quota.fetch payload the host sends: the SDK request
// plus the host_callback_id the host adds.
func fetchRequest(t *testing.T, authIndex, provider string) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(map[string]any{
		"auth_index":       authIndex,
		"provider":         provider,
		"host_callback_id": "cb-1",
	})
	if errMarshal != nil {
		t.Fatalf("marshal fetch request: %v", errMarshal)
	}
	return raw
}

func TestRegisterReportsQuotaCapability(t *testing.T) {
	raw, err := newHandlerForTest(t).Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("register failed: %+v", envelope.Error)
	}
	var reg Registration
	if err := json.Unmarshal(envelope.Result, &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if !reg.Capabilities.QuotaProvider {
		t.Error("quota_provider capability not declared")
	}
	if reg.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", reg.SchemaVersion, SchemaVersion)
	}
	if reg.Metadata.Name != Name || reg.Metadata.Version != Version {
		t.Errorf("metadata = %+v", reg.Metadata)
	}
	// The host drops a plugin whose metadata is missing either of these.
	if reg.Metadata.Author == "" || reg.Metadata.GitHubRepository == "" {
		t.Errorf("metadata missing author/repository: %+v", reg.Metadata)
	}
}

func TestRegisterSurvivesInvalidConfig(t *testing.T) {
	raw, err := newHandlerForTest(t).Handle(pluginabi.MethodPluginRegister, registerPayload(t, brokenConfig))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("bad config must not fail registration: %+v", envelope.Error)
	}
	var reg Registration
	if err := json.Unmarshal(envelope.Result, &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	found := false
	for _, field := range reg.Metadata.ConfigFields {
		if field.Name == "config_error" {
			found = true
		}
	}
	if !found {
		t.Error("invalid config should surface a config_error field")
	}
}

// The registration response is what the management UI renders, so the whole
// encoded envelope is checked for the canary — not just the description field.
func TestRegisterConfigErrorDoesNotLeakInput(t *testing.T) {
	const canary = "fixture-secret-canary"
	raw, err := newHandlerForTest(t).Handle(pluginabi.MethodPluginRegister,
		registerPayload(t, "interval_seconds: "+canary+"\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if strings.Contains(string(raw), canary) {
		t.Fatalf("registration response leaks the config value: %s", raw)
	}
	if !strings.Contains(string(raw), "config_error") {
		t.Fatalf("registration response should still report the config error: %s", raw)
	}
}

func TestQuotaIdentifier(t *testing.T) {
	raw, err := newRegisteredHandler(t).Handle(pluginabi.MethodQuotaIdentifier, nil)
	if err != nil {
		t.Fatalf("identifier: %v", err)
	}
	var resp IdentifierResponse
	if err := json.Unmarshal(decodeEnvelope(t, raw).Result, &resp); err != nil {
		t.Fatalf("decode identifier: %v", err)
	}
	if resp.Identifier != "ollama" {
		t.Errorf("identifier = %q, want ollama", resp.Identifier)
	}
}

func TestQuotaDescribe(t *testing.T) {
	raw, err := newRegisteredHandler(t).Handle(pluginabi.MethodQuotaDescribe, nil)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	var resp struct {
		DisplayName        string   `json:"display_name"`
		SupportedProviders []string `json:"supported_providers"`
		SupportsReset      bool     `json:"supports_reset"`
	}
	if err := json.Unmarshal(decodeEnvelope(t, raw).Result, &resp); err != nil {
		t.Fatalf("decode describe: %v", err)
	}
	if resp.DisplayName != "Quota" {
		t.Errorf("display_name = %q, want Quota", resp.DisplayName)
	}
	// The three provider keys are matched verbatim by the host, including the
	// space inside the Command Code key.
	want := []string{"ollama", "openai-compatible-command code", "openai-compatible-opencode"}
	if len(resp.SupportedProviders) != len(want) {
		t.Fatalf("supported_providers = %v, want %v", resp.SupportedProviders, want)
	}
	for i, key := range want {
		if resp.SupportedProviders[i] != key {
			t.Errorf("supported_providers[%d] = %q, want %q", i, resp.SupportedProviders[i], key)
		}
	}
	if resp.SupportsReset {
		t.Error("supports_reset must be false: none of the three has a reset endpoint")
	}
}

// The mapping test: a real balance payload becomes the plan, one currency
// metric, and one bucket per window, with RemainingFraction pointing the right
// way. balanceBody says the 5h window is 30% used and the weekly window 80%
// used, so the fractions are 0.7 and 0.2 — the reverse of the used shares.
func TestQuotaFetchMapsBalanceAndWindows(t *testing.T) {
	raw, err := newRegisteredHandler(t).Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("fetch failed: %+v", envelope.Error)
	}

	var resp struct {
		Subscription struct {
			Plan string `json:"plan"`
		} `json:"subscription"`
		Summary []struct {
			Key      string  `json:"key"`
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
	if err := json.Unmarshal(envelope.Result, &resp); err != nil {
		t.Fatalf("decode fetch: %v", err)
	}

	if resp.Subscription.Plan != "Pro" {
		t.Errorf("plan = %q, want Pro", resp.Subscription.Plan)
	}
	if len(resp.Summary) != 1 {
		t.Fatalf("summary = %+v, want one balance metric", resp.Summary)
	}
	if resp.Summary[0].Key != "balance" {
		t.Errorf("summary key = %q, want balance", resp.Summary[0].Key)
	}
	if resp.Summary[0].Value != 12.40 {
		t.Errorf("balance = %v, want 12.4", resp.Summary[0].Value)
	}
	if resp.Summary[0].Format != "currency" || resp.Summary[0].Currency != "USD" {
		t.Errorf("balance format = %+v", resp.Summary[0])
	}

	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 2 {
		t.Fatalf("groups = %+v, want one group with two buckets", resp.Groups)
	}
	if resp.Groups[0].DisplayName != "Ollama" {
		t.Errorf("group displayName = %q", resp.Groups[0].DisplayName)
	}

	byWindow := map[string]float64{}
	for _, bucket := range resp.Groups[0].Buckets {
		byWindow[bucket.Window] = bucket.RemainingFraction
		if bucket.Description == "" {
			t.Errorf("bucket %q has no description", bucket.Window)
		}
	}
	// 30% used means 70% remaining.
	if got := byWindow["5h"]; got != 0.7 {
		t.Errorf("5h remainingFraction = %v, want 0.7 (30%% used, remaining not used)", got)
	}
	// 80% used means 20% remaining.
	if got := byWindow["7d"]; got != 0.2 {
		t.Errorf("7d remainingFraction = %v, want 0.2 (80%% used, remaining not used)", got)
	}
}

// A credential that has been used up reports zero remaining, not a negative
// fraction, and the balance metric still appears.
func TestQuotaFetchClampsExhaustedWindow(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(`{"plan":"free","included":{"session":{"remaining_percent":0,"resets_at":"2026-10-30T20:00:00Z"}}}`))
	h := newHandlerWithHost(t, host)
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { h.state.Shutdown() })

	raw, err := h.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("fetch failed: %+v", envelope.Error)
	}
	var resp struct {
		Groups []struct {
			Buckets []struct {
				RemainingFraction float64 `json:"remainingFraction"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(envelope.Result, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %+v", resp.Groups)
	}
	if got := resp.Groups[0].Buckets[0].RemainingFraction; got != 0 {
		t.Errorf("remainingFraction = %v, want 0", got)
	}
}

func TestQuotaFetchRejectsForeignProvider(t *testing.T) {
	raw, err := newRegisteredHandler(t).Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "anthropic"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("a foreign provider must not receive Ollama figures")
	}
	if envelope.Error.Code != "unsupported_provider" {
		t.Errorf("error code = %q, want unsupported_provider", envelope.Error.Code)
	}
}

// A credential with neither an API key nor a cookie must fail before any
// request is issued.
func TestQuotaFetchIncompleteCredentialSendsNothing(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "email": "a@example.com"})
	h := newHandlerWithHost(t, host)
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { h.state.Shutdown() })

	raw, err := h.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("an incomplete credential must not produce a figure")
	}
	if envelope.Error.Code != "credential_incomplete" {
		t.Errorf("error code = %q, want credential_incomplete", envelope.Error.Code)
	}
	if count := host.requestCount(); count != 0 {
		t.Errorf("issued %d upstream request(s) for an incomplete credential, want 0", count)
	}
}

func TestQuotaResetIsUnsupported(t *testing.T) {
	raw, err := newRegisteredHandler(t).Handle(pluginabi.MethodQuotaReset, []byte(`{"provider":"ollama"}`))
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("reset should return a normal response, got %+v", envelope.Error)
	}
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(envelope.Result, &resp); err != nil {
		t.Fatalf("decode reset: %v", err)
	}
	if resp.Success {
		t.Error("reset must report failure for an unsupported provider")
	}
	if !strings.Contains(resp.Message, "not supported") {
		t.Errorf("message = %q", resp.Message)
	}
}

// The credential never reaches the response, in any field, on any path.
func TestQuotaFetchDoesNotEchoCredential(t *testing.T) {
	const canary = "fixture-api-key-canary-value"
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": canary})
	// An upstream that happily repeats whatever it is given back in the plan
	// field — the case #2's rejectEcho guards against.
	host.setRespond(balanceOK(`{"plan":"` + canary + `","included":{"balance_usd":1}}`))
	h := newHandlerWithHost(t, host)
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { h.state.Shutdown() })

	raw, err := h.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if strings.Contains(string(raw), canary) {
		t.Fatalf("the fetch response echoes the credential: %s", raw)
	}
}
