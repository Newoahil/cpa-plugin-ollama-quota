// Package quotaplugin defines the JSON-RPC surface CPA drives for the
// ollama-quota plugin, independent of the C ABI shim in cmd/plugin.
package quotaplugin

import (
	"context"
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Envelope is the RPC frame the host decodes for every plugin call.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError reports a failed call. Message must never contain credential
// material.
type EnvelopeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

// IdentifierResponse is the shape the host decodes for *.identifier methods.
type IdentifierResponse struct {
	Identifier string `json:"identifier"`
}

// LifecycleRequest is the payload of plugin.register and plugin.reconfigure.
type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// Registration mirrors the host's rpcRegistration schema. Only quota_provider
// is declared: the plugin does not touch auth, routing, or request paths.
type Registration struct {
	SchemaVersion uint32              `json:"schema_version"`
	Metadata      pluginapi.Metadata  `json:"metadata"`
	Capabilities  RegistrationCapable `json:"capabilities"`
}

// RegistrationCapable lists the capability flags the host understands. Every
// other flag is left at its zero value.
type RegistrationCapable struct {
	QuotaProvider bool `json:"quota_provider"`
}

// Handler dispatches RPC methods against one plugin instance.
type Handler struct {
	state *State
	// ctx is the parent for RPC work. The C ABI carries no context, so
	// cancellation flows through quiesce instead of per-call deadlines.
	ctx context.Context
}

// NewHandler returns a handler bound to state.
func NewHandler(state *State) *Handler {
	return &Handler{state: state, ctx: context.Background()}
}

// Handle dispatches one RPC call. It never returns a Go error: a failure of
// the plugin itself belongs in the envelope so the host can classify it (and
// treat unknown_method as "not supported").
func (h *Handler) Handle(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return h.handleRegister(request)
	case pluginabi.MethodPluginQuiesce:
		h.state.Quiesce()
		return OKEnvelope(struct{}{})
	case pluginabi.MethodPluginShutdown:
		h.state.Shutdown()
		return OKEnvelope(struct{}{})
	case pluginabi.MethodQuotaIdentifier:
		return OKEnvelope(IdentifierResponse{Identifier: QuotaIdentifier})
	case pluginabi.MethodQuotaDescribe:
		return h.handleDescribe(request)
	case pluginabi.MethodQuotaFetch:
		return h.handleFetch(request)
	case pluginabi.MethodQuotaReset:
		return h.handleReset(request)
	default:
		// The host probes optional methods and treats unknown_method as
		// "not supported", so anything outside this surface reports that way.
		return ErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func (h *Handler) handleRegister(raw []byte) ([]byte, error) {
	var req LifecycleRequest
	if errUnmarshal := decodeOptional(raw, &req); errUnmarshal != nil {
		return ErrorEnvelope("invalid_request", "decode lifecycle request: "+errUnmarshal.Error()), nil
	}
	cfg, errConfig := h.state.Register(req.ConfigYAML)

	reg := Registration{
		SchemaVersion: SchemaVersion,
		Metadata:      metadata(cfg),
		Capabilities:  RegistrationCapable{QuotaProvider: true},
	}
	if errConfig != nil {
		// Keep the plugin loadable: the host drops a registration that
		// errors, and a YAML typo should not take the quota page down. The
		// message shows up as a config field in the management UI.
		reg.Metadata.ConfigFields = append(reg.Metadata.ConfigFields, pluginapi.ConfigField{
			Name:        "config_error",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "config not applied: " + errConfig.Error(),
		})
	}
	return OKEnvelope(reg)
}

func (h *Handler) handleDescribe(raw []byte) ([]byte, error) {
	provider, ok := h.state.Provider()
	if !ok {
		return ErrorEnvelope("plugin_quiesced", "plugin is quiescing"), nil
	}
	var req pluginapi.QuotaDescribeRequest
	if errUnmarshal := decodeOptional(raw, &req); errUnmarshal != nil {
		return ErrorEnvelope("invalid_request", "decode quota describe request: "+errUnmarshal.Error()), nil
	}
	displayName, supported, supportsReset := provider.Describe()
	return OKEnvelope(pluginapi.QuotaDescribeResponse{
		DisplayName:        displayName,
		SupportedProviders: supported,
		SupportsReset:      supportsReset,
	})
}

func (h *Handler) handleFetch(raw []byte) ([]byte, error) {
	provider, ok := h.state.Provider()
	if !ok {
		return ErrorEnvelope("plugin_quiesced", "plugin is quiescing"), nil
	}
	var req pluginapi.QuotaFetchRequest
	if errUnmarshal := decodeOptional(raw, &req); errUnmarshal != nil {
		return ErrorEnvelope("invalid_request", "decode quota fetch request: "+errUnmarshal.Error()), nil
	}

	ctx, release, tracked := provider.Track(h.ctx)
	if !tracked {
		return ErrorEnvelope("plugin_quiesced", ErrQuotaUnavailable.Error()), nil
	}
	defer release()

	select {
	case <-ctx.Done():
		return ErrorEnvelope("plugin_quiesced", ErrQuotaUnavailable.Error()), nil
	default:
	}

	sample, errSample := provider.Sample(ProviderKey(req.Provider))
	if errSample != nil {
		code := "quota_fetch_failed"
		if IsUnsupportedProvider(errSample) {
			code = "unsupported_provider"
		}
		return ErrorEnvelope(code, errSample.Error()), nil
	}
	return OKEnvelope(sampleResponse(sample))
}

func (h *Handler) handleReset(raw []byte) ([]byte, error) {
	if !h.state.Active() {
		return ErrorEnvelope("plugin_quiesced", "plugin is quiescing"), nil
	}
	var req pluginapi.QuotaResetRequest
	if errUnmarshal := decodeOptional(raw, &req); errUnmarshal != nil {
		return ErrorEnvelope("invalid_request", "decode quota reset request: "+errUnmarshal.Error()), nil
	}
	// Reset stays unsupported until a real provider can honour it.
	return OKEnvelope(pluginapi.QuotaResetResponse{
		Success: false,
		Message: "reset is not supported for Ollama quota",
	})
}

// sampleResponse maps the placeholder payload onto the host's quota model.
func sampleResponse(s Sample) pluginapi.QuotaFetchResponse {
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     s.Plan,
			TierName: s.TierName,
			TierID:   s.TierID,
		},
		Summary: []pluginapi.QuotaMetric{{
			Key:      "balance",
			Label:    "Balance",
			Value:    s.BalanceUSD,
			Format:   "currency",
			Currency: "USD",
		}},
		Groups: []pluginapi.QuotaGroup{{
			DisplayName: "Ollama",
			Buckets: []pluginapi.QuotaBucket{{
				Window:            s.Window,
				RemainingFraction: s.RemainingFraction,
				Description:       s.WindowNote,
			}},
		}},
	}
}

func metadata(cfg Config) pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             Name,
		Version:          Version,
		Author:           "LambdaTheory",
		GitHubRepository: "https://github.com/LambdaTheory/cpa-plugin-ollama-quota",
		ConfigFields: []pluginapi.ConfigField{
			{
				Name:        "interval_seconds",
				Type:        pluginapi.ConfigFieldTypeInteger,
				Description: "Quota collection interval in seconds (default 900). Read by the collector added in #2.",
			},
		},
	}
}

// OKEnvelope wraps a result as a successful envelope.
func OKEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(Envelope{OK: true, Result: raw})
}

// ErrorEnvelope wraps a failure. It cannot fail: the frame is all strings.
func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{Code: code, Message: message}})
	return raw
}

// decodeOptional tolerates an empty body, which the host sends for methods
// that take no arguments.
func decodeOptional(raw []byte, target any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, target)
}
