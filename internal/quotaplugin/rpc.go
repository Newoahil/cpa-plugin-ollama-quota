// Package quotaplugin defines the JSON-RPC surface CPA drives for the
// ollama-quota plugin, independent of the C ABI shim in cmd/plugin.
package quotaplugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama"
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

// quotaFetchRequest is the payload of quota.fetch.
//
// It is pluginapi.QuotaFetchRequest plus the field the host adds to every
// callback-bearing RPC: rpcQuotaFetchRequest embeds the SDK struct and adds
// HostCallbackID (internal/pluginhost/rpc_schema.go:136). HostCallbackID is the
// only handle this plugin gets on the request's transport scope — the SDK's own
// HTTPClient field is `json:"-"` and cannot cross the C ABI — so it is what
// every host.http.do issued for this call is bound to.
type quotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
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
	// hostCall is the host callback bridge this generation was initialised
	// with. It is per generation because the host's callback scope belongs to
	// one loaded client (see Runtime).
	hostCall HostCall
}

// NewHandler returns a handler bound to state, with no host bridge. It exists
// for the lifecycle tests, which never reach the network; a quota call on such
// a handler fails with host_unavailable rather than pretending to collect.
func NewHandler(state *State) *Handler {
	return &Handler{state: state, ctx: context.Background()}
}

// NewHandlerWithHost returns a handler bound to state and to the host callback
// bridge of the generation the host just initialised.
func NewHandlerWithHost(state *State, hostCall HostCall) *Handler {
	return &Handler{state: state, ctx: context.Background(), hostCall: hostCall}
}

// Shutdown retires the instance behind this handler. Later calls that reach
// this same handler are refused, which is what stops a call still in flight
// during an unload from resurrecting a retired generation.
func (h *Handler) Shutdown() {
	h.state.Shutdown()
}

// State exposes the instance this handler drives, for diagnostics and tests.
func (h *Handler) State() *State {
	return h.state
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
	// The bridge belongs to this handler's generation, so it is installed
	// before register can start a collector that will need it.
	h.state.provider.setHostBridge(h.hostCall)
	cfg, errConfig := h.state.Register(req.ConfigYAML)
	if errors.Is(errConfig, ErrStopped) {
		// The host retired this instance; reporting a capability set would let
		// it start routing quota calls to a dead library.
		return ErrorEnvelope("plugin_stopped", ErrStopped.Error()), nil
	}

	reg := Registration{
		SchemaVersion: SchemaVersion,
		Metadata:      metadata(cfg),
		Capabilities:  RegistrationCapable{QuotaProvider: true},
	}
	if errConfig != nil {
		// Keep the plugin loadable: the host drops a registration that
		// errors, and a YAML typo should not take the quota page down. The
		// message shows up as a config field in the management UI, so it must
		// not carry the offending value (SPEC 4.2) — ParseConfig only reports
		// the field name, line and expected type.
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
	var req quotaFetchRequest
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

	response, errFetch := provider.fetchQuota(ctx, req.QuotaFetchRequest, req.HostCallbackID)
	if errFetch != nil {
		return ErrorEnvelope(fetchErrorCode(errFetch), fetchErrorMessage(errFetch)), nil
	}
	return OKEnvelope(response)
}

// fetchErrorCode maps a collection failure onto the host's error taxonomy.
// The codes are the ones the host and the ticket name; only unsupported_provider
// and invalid_request are acted on by the host today, the rest reach the page as
// the message text.
func fetchErrorCode(err error) string {
	switch {
	case IsUnsupportedProvider(err):
		return "unsupported_provider"
	case errors.Is(err, ErrIncompleteCredential):
		return "credential_incomplete"
	case errors.Is(err, ErrHostUnavailable):
		return "host_unavailable"
	case errors.Is(err, ollama.ErrRateLimit):
		return "rate_limited"
	case errors.Is(err, ollama.ErrAuth):
		return "auth_failed"
	case errors.Is(err, ollama.ErrParse):
		return "parse_failed"
	case errors.Is(err, ollama.ErrNetwork):
		return "upstream_unreachable"
	default:
		return "quota_fetch_failed"
	}
}

// fetchErrorMessage is the text the host puts in its 502 body, so it must be
// useful and must never carry a credential. Every error this package and the
// collector produce is already scrubbed (SPEC 4.2); the fallthrough keeps an
// unrecognised error from being rendered as an empty string.
func fetchErrorMessage(err error) string {
	if err == nil {
		return "the Ollama quota fetch failed"
	}
	if message := strings.TrimSpace(err.Error()); message != "" {
		return message
	}
	return "the Ollama quota fetch failed"
}

func (h *Handler) handleReset(raw []byte) ([]byte, error) {
	if !h.state.Active() {
		return ErrorEnvelope("plugin_quiesced", "plugin is quiescing"), nil
	}
	var req pluginapi.QuotaResetRequest
	if errUnmarshal := decodeOptional(raw, &req); errUnmarshal != nil {
		return ErrorEnvelope("invalid_request", "decode quota reset request: "+errUnmarshal.Error()), nil
	}
	// Ollama has no endpoint that resets a quota window, so this is a definite
	// refusal rather than a silent success. The host turns a Success=false
	// answer into a 502 with this message, which is exactly the behaviour a
	// button that cannot work should have.
	return OKEnvelope(pluginapi.QuotaResetResponse{
		Success: false,
		Message: ErrQuotaResetUnsupported.Error(),
	})
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
				Description: "Quota collection interval in seconds (default 900).",
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
