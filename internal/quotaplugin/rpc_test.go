package quotaplugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const enabledConfig = "enabled: true\n"
const brokenConfig = "ignored: 1\ninterval_seconds: not-a-number\n"

// newHandlerForTest returns a handler over a fresh state.
func newHandlerForTest(t *testing.T) *Handler {
	t.Helper()
	return NewHandler(NewState())
}

// newRegisteredHandler returns a handler that has completed plugin.register,
// which is the state every quota call arrives in.
func newRegisteredHandler(t *testing.T) *Handler {
	t.Helper()
	h := newHandlerForTest(t)
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("register: %v", err)
	}
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
	if resp.DisplayName != "Ollama" {
		t.Errorf("display_name = %q, want Ollama", resp.DisplayName)
	}
	if len(resp.SupportedProviders) != 1 || resp.SupportedProviders[0] != "ollama" {
		t.Errorf("supported_providers = %v, want [ollama]", resp.SupportedProviders)
	}
	if resp.SupportsReset {
		t.Error("supports_reset should be false until reset is implemented")
	}
}

func TestQuotaFetchReturnsHalfUsedWindowAndTenDollarBalance(t *testing.T) {
	raw, err := newRegisteredHandler(t).Handle(pluginabi.MethodQuotaFetch, []byte(`{"provider":"ollama"}`))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("fetch failed: %+v", envelope.Error)
	}
	var resp struct {
		Summary []struct {
			Key      string  `json:"key"`
			Value    float64 `json:"value"`
			Format   string  `json:"format"`
			Currency string  `json:"currency"`
		} `json:"summary"`
		Groups []struct {
			Buckets []struct {
				RemainingFraction float64 `json:"remainingFraction"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(envelope.Result, &resp); err != nil {
		t.Fatalf("decode fetch: %v", err)
	}
	if len(resp.Summary) != 1 || resp.Summary[0].Value != 10.00 {
		t.Fatalf("summary = %+v, want one $10.00 balance", resp.Summary)
	}
	if resp.Summary[0].Format != "currency" || resp.Summary[0].Currency != "USD" {
		t.Errorf("balance format = %+v", resp.Summary[0])
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %+v, want one group with one bucket", resp.Groups)
	}
	if got := resp.Groups[0].Buckets[0].RemainingFraction; got != 0.5 {
		t.Errorf("remainingFraction = %v, want 0.5 (50%% used)", got)
	}
}

func TestQuotaFetchRejectsForeignProvider(t *testing.T) {
	raw, err := newRegisteredHandler(t).Handle(pluginabi.MethodQuotaFetch, []byte(`{"provider":"anthropic"}`))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("a foreign provider must not receive sample numbers")
	}
	if envelope.Error.Code != "unsupported_provider" {
		t.Errorf("error code = %q, want unsupported_provider", envelope.Error.Code)
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
