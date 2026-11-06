// Package quotaplugin defines the JSON-RPC surface CPA drives for the
// ollama-quota plugin, independent of the C ABI shim in cmd/plugin.
package quotaplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const DrainTimeoutCode = "quiesce_drain_timeout"

type LifecycleResponse struct{}

// Envelope is the RPC frame the host decodes for every plugin call.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError reports a failed call. Message must never contain credential
// material.
//
// HTTPStatus is what the host turns into an rpcError's StatusCode
// (internal/pluginhost/rpc_client.go:371), which is how a client sees Ollama's
// 429 or a rejected key as itself instead of a generic 502. It is omitted when
// zero, so the quota envelopes keep their existing shape.
type EnvelopeError struct {
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	Retryable  bool           `json:"retryable,omitempty"`
	HTTPStatus int            `json:"http_status,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
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

// Registration mirrors the host's rpcRegistration schema. The plugin exposes
// quota collection, its configuration page, and Ollama inference.
type Registration struct {
	SchemaVersion uint32              `json:"schema_version"`
	Metadata      pluginapi.Metadata  `json:"metadata"`
	Capabilities  RegistrationCapable `json:"capabilities"`
}

// RegistrationCapable lists the capability flags the host understands. Every
// other flag is left at its zero value.
//
// Three capabilities are declared together and they only make sense as a set:
//
//   - Executor registers this plugin as the inference provider for "ollama",
//     which is what turns a file auth into a usable one;
//   - ModelProvider supplies the per-credential model list through
//     model.for_auth, since there is no static catalog to declare;
//   - QuotaProvider is the original capability, unchanged.
//
// ExecutorModelScope is "both": the host registers static models from this
// plugin when it declares any (it does not) and per-auth models for OAuth-style
// credentials, which is the path a file auth with an API key takes.
type RegistrationCapable struct {
	QuotaProvider         bool                         `json:"quota_provider"`
	ManagementAPI         bool                         `json:"management_api"`
	ModelProvider         bool                         `json:"model_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
}

// registeredCapabilities is the single declaration the register handler uses, so
// a test can assert the exact set the host is told about.
func registeredCapabilities() RegistrationCapable {
	return RegistrationCapable{
		QuotaProvider:         true,
		ManagementAPI:         true,
		ModelProvider:         true,
		Executor:              true,
		ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
		ExecutorInputFormats:  []string{formatChatCompletions},
		ExecutorOutputFormats: []string{formatChatCompletions},
	}
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
		_, errDrain := h.state.Quiesce()
		return h.lifecycleEnvelope(errDrain)
	case pluginabi.MethodPluginShutdown:
		return h.lifecycleEnvelope(h.state.Shutdown())
	case pluginabi.MethodQuotaIdentifier:
		return OKEnvelope(IdentifierResponse{Identifier: QuotaIdentifier})
	case pluginabi.MethodQuotaDescribe:
		return h.handleDescribe(request)
	case pluginabi.MethodQuotaFetch:
		return h.handleFetch(request)
	case pluginabi.MethodQuotaReset:
		return h.handleReset(request)
	case pluginabi.MethodExecutorIdentifier:
		return h.handleExecutorIdentifier(request)
	case pluginabi.MethodExecutorExecute:
		return h.handleExecutorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return h.handleExecutorExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return h.handleExecutorCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return h.handleExecutorHTTPRequest(request)
	case pluginabi.MethodModelStatic:
		return h.handleModelStatic(request)
	case pluginabi.MethodModelForAuth:
		return h.handleModelForAuth(request)
	case pluginabi.MethodManagementRegister:
		return OKEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return h.handleManagement(request)
	default:
		// The host probes optional methods and treats unknown_method as
		// "not supported", so anything outside this surface reports that way.
		return ErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleEnvelope reports one combined drain result. CPA v8.0.47 callQuiesce
// discards the bool: this honestly reports failure, but cannot prevent unload.
// Use this call's error, since another lifecycle call can overwrite LastDrain.
func (h *Handler) lifecycleEnvelope(err error) ([]byte, error) {
	if err == nil {
		return OKEnvelope(LifecycleResponse{})
	}
	var report DrainReport
	if !errors.As(err, &report) {
		report.TimedOut = true
	}
	return json.Marshal(Envelope{Error: &EnvelopeError{
		Code:    DrainTimeoutCode,
		Message: fmt.Sprintf("%s; tracked wait budget %s", report.Error(), report.Timeout),
		Detail: map[string]any{
			"in_flight":    report.InFlight,
			"timeout_s":    report.Timeout.Seconds(),
			"host_pending": report.HostPending,
			"unsettled":    report.Unsettled,
		},
	}})
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
		Capabilities:  registeredCapabilities(),
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
			{Name: "state_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Writable persistent directory for settings.json; defaults to the user config directory/cpa-plugin-ollama-quota."},
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
