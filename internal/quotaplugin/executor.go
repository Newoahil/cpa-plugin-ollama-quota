package quotaplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The executor half of the plugin: the reason this plugin declares provider
// "ollama" at all (scheme B).
//
// Before it, CPA had no Ollama inference provider. The quota monitor could be
// told about an Ollama credential, but the host had nobody to hand its requests
// to, so the only workable configuration was an openai-compatibility credential
// with its own base_url and model list — a *second* credential, not the one the
// quota collector reads, and one the background collector cannot see at all
// (host.auth.list enumerates file auths, and a synthesized one is not there).
//
// With an executor registered for the identifier "ollama", the same auth file
// serves both halves: `{"type":"ollama","api_key":"..."}` is both the credential
// CPA routes inference through and the credential the collector polls. One key,
// one file, one set of numbers.
//
// Everything here goes through the host's HTTP callback (host.http.do) rather
// than a client of its own, because ExecutorRequest.HTTPClient is tagged
// `json:"-"` and cannot cross the C ABI (see transport.go). That also means the
// host's proxy and request-log policy apply, which is what "public SDK only"
// buys us.
const (
	// ollamaOrigin is pinned. Requests are built here, never taken from a
	// request field, so a caller cannot make the plugin attach the credential to
	// a host of its choosing (SPEC 4.2).
	ollamaOrigin = "https://ollama.com"
	// chatCompletionsURL is Ollama Cloud's OpenAI-compatible endpoint.
	chatCompletionsURL = ollamaOrigin + "/v1/chat/completions"
	// tagsURL lists the models the credential can reach. Discovery is per auth:
	// the plugin hardcodes no model catalog, because the catalog is per account
	// and changes without a plugin release.
	tagsURL = ollamaOrigin + "/api/tags"

	// formatChatCompletions is the one protocol this executor speaks. The host
	// translates other entry formats to and from it before and after the call.
	formatChatCompletions = "chat-completions"

	// modelDiscoveryTimeout bounds one discovery call. It is NOT the SPEC 4.4
	// collection budget: discovery happens during config apply, not collection,
	// and the host has no cancellation signal for it beyond the callback scope.
	modelDiscoveryTimeout = 10 * time.Second

	// maxUpstreamResponseBytes caps one upstream body. A non-streaming cloud
	// completion is a few hundred kilobytes at most; the cap exists so a
	// misbehaving upstream cannot buffer without bound.
	maxUpstreamResponseBytes = 32 << 20

	// maxUpstreamMessageLength caps the upstream text quoted into an error.
	maxUpstreamMessageLength = 240
)

// ErrExecutorUnavailable reports an executor call that arrived while the plugin
// is quiescing. The host has already stopped routing to it, so this is the
// honest answer to a call that raced the swap.
var ErrExecutorUnavailable = errors.New("the Ollama executor is not accepting requests")

// executorRequest is the payload of executor.execute and executor.execute_stream.
// The host embeds pluginapi.ExecutorRequest and adds these two fields
// (internal/pluginhost/rpc_schema.go:80). Only HostCallbackID is used: it is the
// scope every host.http.do of this call is bound to, so a cancelled client
// request also cancels the upstream one at the host.
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// authModelRequest is the payload of model.for_auth (rpc_schema.go:75).
type authModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// executorHTTPRequest is the payload of executor.http_request
// (rpc_schema.go:86).
type executorHTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// executorStreamResponse is the wire shape of a streaming result. The host
// decodes exactly these two fields (rpc_schema.go:55); chunks that are present
// are handed back to the client as a closed channel, so this plugin can answer
// a stream synchronously without the older host.stream.emit protocol.
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks"`
}

// upstreamTarget is one generation's bridge, as the executor needs it.
type upstreamTarget struct {
	bridge *hostBridge
}

// executorTarget returns the bridge to use, or the envelope to refuse with.
//
// The check is the same one the quota methods make: a handler served by a
// quiesced or retired instance must not start new upstream work. In-flight
// executor calls are deliberately NOT tracked by Provider.Track — SPEC 4.1
// requires them to finish across a hot swap, and Track's contexts are cancelled
// by CancelAll — so this is the only gate, and the bridge's in-flight set is
// what quiesce's bounded wait observes.
func (h *Handler) executorTarget() (upstreamTarget, []byte) {
	if !h.state.Active() {
		return upstreamTarget{}, ErrorEnvelope("plugin_quiesced", ErrExecutorUnavailable.Error())
	}
	provider, ok := h.state.Provider()
	if !ok {
		return upstreamTarget{}, ErrorEnvelope("plugin_quiesced", ErrExecutorUnavailable.Error())
	}
	bridge := provider.bridge()
	if bridge == nil || bridge.call == nil {
		return upstreamTarget{}, errorEnvelopeStatus("host_unavailable", ErrHostUnavailable.Error(), http.StatusServiceUnavailable)
	}
	return upstreamTarget{bridge: bridge}, nil
}

// ---------------------------------------------------------------------------
// RPC handlers
// ---------------------------------------------------------------------------

// handleExecutorIdentifier answers executor.identifier. The identifier is the
// provider key CPA routes by, and it is deliberately the same string the quota
// provider uses: one credential, one key, two capabilities.
func (h *Handler) handleExecutorIdentifier([]byte) ([]byte, error) {
	return OKEnvelope(IdentifierResponse{Identifier: QuotaIdentifier})
}

// handleModelStatic contributes no static models.
//
// That is a design decision, not an omission. Ollama Cloud's catalog is per
// account and changes without a plugin release, so a hardcoded list would go
// stale and a probe at load time would need a credential this plugin does not
// have yet. The host registers this executor even with zero static models (the
// second pass of RegisterExecutors only needs a resolvable provider), and the
// models arrive per auth through model.for_auth.
func (h *Handler) handleModelStatic([]byte) ([]byte, error) {
	return OKEnvelope(pluginapi.ModelResponse{Provider: QuotaIdentifier})
}

// handleModelForAuth discovers the models one credential can reach.
//
// A cookie-only credential is not an error here: it can still report quota, it
// just cannot run inference, so it contributes no models and says nothing. A
// discovery failure is reported as an error envelope — the host logs it against
// this auth and registers no models, which is the honest outcome.
func (h *Handler) handleModelForAuth(raw []byte) ([]byte, error) {
	target, refusal := h.executorTarget()
	if refusal != nil {
		return refusal, nil
	}
	var req authModelRequest
	if errUnmarshal := decodeOptional(raw, &req); errUnmarshal != nil {
		return ErrorEnvelope("invalid_request", "decode model discovery request: "+errUnmarshal.Error()), nil
	}
	if provider := ProviderKey(req.AuthProvider); provider != "" && provider != QuotaIdentifier {
		// Not ours. Naming the provider back keeps the host from attributing an
		// empty result to ollama.
		return OKEnvelope(pluginapi.ModelResponse{Provider: provider})
	}

	credential, errCredential := credentialFromStorage(req.StorageJSON, req.Metadata)
	if errCredential != nil || credential.APIKey == "" {
		// Incomplete or cookie-only: this credential reports quota only.
		return OKEnvelope(pluginapi.ModelResponse{Provider: QuotaIdentifier})
	}

	ctx, cancel := context.WithTimeout(h.ctx, modelDiscoveryTimeout)
	defer cancel()
	models, errDiscover := target.models(ctx, credential, req.HostCallbackID)
	if errDiscover != nil {
		return executorFailure(errDiscover, credential), nil
	}
	return OKEnvelope(pluginapi.ModelResponse{Provider: QuotaIdentifier, Models: models})
}

// handleExecutorExecute forwards one non-streaming chat completion.
func (h *Handler) handleExecutorExecute(raw []byte) ([]byte, error) {
	target, refusal := h.executorTarget()
	if refusal != nil {
		return refusal, nil
	}
	var req executorRequest
	if errUnmarshal := decodeOptional(raw, &req); errUnmarshal != nil {
		return ErrorEnvelope("invalid_request", "decode executor request: "+errUnmarshal.Error()), nil
	}
	credential, errCredential := inferenceCredential(req.ExecutorRequest)
	if errCredential != nil {
		return executorFailure(errCredential, credential), nil
	}
	body, errPayload := chatPayload(req.Model, req.Payload, req.OriginalRequest, false)
	if errPayload != nil {
		return ErrorEnvelope("invalid_request", errPayload.Error()), nil
	}

	response, errCall := target.post(h.ctx, chatCompletionsURL, credential, body, false, req.HostCallbackID)
	if errCall != nil {
		return executorFailure(errCall, credential), nil
	}
	if response.status != http.StatusOK {
		return upstreamFailure(response, credential), nil
	}
	return OKEnvelope(pluginapi.ExecutorResponse{
		Payload: response.body,
		Headers: http.Header{"Content-Type": []string{response.contentType("application/json")}},
	})
}

// handleExecutorExecuteStream forwards one streaming chat completion.
//
// The upstream body is read in full and split into event data here, then handed
// back as ready-made chunks. That is the synchronous half of the plugin stream
// protocol (see rpc_client_stream.go:29: a non-empty chunk list is returned as a
// closed channel). The trade-off is described in the README's 推理 section: no
// incremental delivery, so the client sees the first frame after the whole
// generation finishes. It is the version of streaming this SDK version can do
// without the older host.stream.emit side channel, and it keeps the credential
// handling in one place.
func (h *Handler) handleExecutorExecuteStream(raw []byte) ([]byte, error) {
	target, refusal := h.executorTarget()
	if refusal != nil {
		return refusal, nil
	}
	var req executorRequest
	if errUnmarshal := decodeOptional(raw, &req); errUnmarshal != nil {
		return ErrorEnvelope("invalid_request", "decode executor stream request: "+errUnmarshal.Error()), nil
	}
	credential, errCredential := inferenceCredential(req.ExecutorRequest)
	if errCredential != nil {
		return executorFailure(errCredential, credential), nil
	}
	body, errPayload := chatPayload(req.Model, req.Payload, req.OriginalRequest, true)
	if errPayload != nil {
		return ErrorEnvelope("invalid_request", errPayload.Error()), nil
	}

	response, errCall := target.post(h.ctx, chatCompletionsURL, credential, body, true, req.HostCallbackID)
	if errCall != nil {
		return executorFailure(errCall, credential), nil
	}
	if response.status != http.StatusOK {
		return upstreamFailure(response, credential), nil
	}

	return OKEnvelope(executorStreamResponse{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  sseFrames(response.body, response.isEventStream()),
	})
}

// handleExecutorCountTokens refuses.
//
// Ollama Cloud exposes no token-counting endpoint, and guessing would be a wrong
// number rather than a missing one (SPEC 4.3). The host falls back to its own
// estimator when an executor reports 501.
func (h *Handler) handleExecutorCountTokens([]byte) ([]byte, error) {
	if _, refusal := h.executorTarget(); refusal != nil {
		return refusal, nil
	}
	return errorEnvelopeStatus("unsupported_method",
		"the ollama executor does not count tokens; the host should use its own estimator",
		http.StatusNotImplemented), nil
}

// handleExecutorHTTPRequest forwards one provider-owned request.
//
// The URL is validated rather than trusted. An executor is handed a URL the host
// built, but this handler attaches the stored credential to whatever it is
// given, so the origin has to be pinned here — the same rule transport.go
// enforces for the collector, for the same reason (SPEC 4.2).
func (h *Handler) handleExecutorHTTPRequest(raw []byte) ([]byte, error) {
	target, refusal := h.executorTarget()
	if refusal != nil {
		return refusal, nil
	}
	var req executorHTTPRequest
	if errUnmarshal := decodeOptional(raw, &req); errUnmarshal != nil {
		return ErrorEnvelope("invalid_request", "decode executor http request: "+errUnmarshal.Error()), nil
	}
	credential, errCredential := inferenceCredential(pluginapi.ExecutorRequest{
		StorageJSON:  req.StorageJSON,
		AuthMetadata: req.Metadata,
	})
	if errCredential != nil {
		return executorFailure(errCredential, credential), nil
	}
	parsed, errURL := url.Parse(strings.TrimSpace(req.URL))
	if errURL != nil || parsed.Host == "" {
		return errorEnvelopeStatus("invalid_request", "the ollama executor was given no usable URL", http.StatusBadRequest), nil
	}
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "ollama.com") {
		return errorEnvelopeStatus("invalid_request",
			"the ollama executor forwards only to https://ollama.com", http.StatusBadRequest), nil
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodPost
	}
	headers := credentialsFreeHeaders(req.Headers)

	response, errCall := target.send(h.ctx, method, parsed.String(), headers, credential, req.Body, req.HostCallbackID)
	if errCall != nil {
		return executorFailure(errCall, credential), nil
	}
	return OKEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: response.status,
		Headers:    responseHeaders(response.headers),
		Body:       response.body,
	})
}

// ---------------------------------------------------------------------------
// Upstream calls
// ---------------------------------------------------------------------------

// upstreamResponse is one exchange, already read into memory.
type upstreamResponse struct {
	status  int
	headers http.Header
	body    []byte
}

// contentType reports the upstream content type, or fallback when it sent none.
func (r upstreamResponse) contentType(fallback string) string {
	value := strings.TrimSpace(r.headers.Get("Content-Type"))
	if value == "" {
		return fallback
	}
	return value
}

// isEventStream reports whether upstream answered with an SSE body.
func (r upstreamResponse) isEventStream() bool {
	return strings.Contains(strings.ToLower(r.contentType("")), "text/event-stream")
}

// post issues one JSON request to Ollama Cloud with this credential.
func (t upstreamTarget) post(ctx context.Context, endpoint string, credential ollama.Credential, body []byte, stream bool, callbackID string) (upstreamResponse, error) {
	headers := make(http.Header, 3)
	headers.Set("Content-Type", "application/json")
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	return t.send(ctx, http.MethodPost, endpoint, headers, credential, body, callbackID)
}

// send performs one exchange through the host's transport policy.
//
// ctx is the plugin's own and carries no deadline for an execution: an inference
// call may legitimately take minutes, and the host's callback scope already ties
// the upstream request to the inbound call, so a client disconnect cancels it
// without a budget invented here. A caller that does want a bound (model
// discovery) passes a context that has one.
//
// The Authorization header is written here and only here, after the caller's
// headers, so no caller-supplied value can override it. Nothing in this path
// formats the credential into an error: the request itself is the only place it
// appears (SPEC 4.2).
func (t upstreamTarget) send(ctx context.Context, method, endpoint string, headers http.Header, credential ollama.Credential, body []byte, callbackID string) (upstreamResponse, error) {
	request, errNew := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if errNew != nil {
		return upstreamResponse{}, fmt.Errorf("build the Ollama request: %w", errNew)
	}
	request.Header = headers.Clone()
	request.Header.Set("Authorization", "Bearer "+credential.APIKey)

	response, errExchange := t.bridge.exchange(ctx, request, callbackID)
	if errExchange != nil {
		return upstreamResponse{}, errExchange
	}
	defer func() { _ = response.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(response.Body, maxUpstreamResponseBytes+1))
	if errRead != nil {
		return upstreamResponse{}, fmt.Errorf("read the Ollama response: %w", errRead)
	}
	if len(raw) > maxUpstreamResponseBytes {
		return upstreamResponse{}, fmt.Errorf("the Ollama response exceeded %d bytes", maxUpstreamResponseBytes)
	}
	return upstreamResponse{status: response.StatusCode, headers: response.Header.Clone(), body: raw}, nil
}

// models lists the models this credential can reach.
func (t upstreamTarget) models(ctx context.Context, credential ollama.Credential, callbackID string) ([]pluginapi.ModelInfo, error) {
	headers := make(http.Header, 2)
	headers.Set("Accept", "application/json")
	response, errCall := t.send(ctx, http.MethodGet, tagsURL, headers, credential, nil, callbackID)
	if errCall != nil {
		return nil, errCall
	}
	if response.status != http.StatusOK {
		return nil, upstreamError{status: response.status, message: upstreamMessage(response.body)}
	}

	var payload struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(response.body, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("%w: the Ollama model list is not valid JSON", ollama.ErrParse)
	}

	seen := make(map[string]struct{}, len(payload.Models))
	models := make([]pluginapi.ModelInfo, 0, len(payload.Models))
	for _, entry := range payload.Models {
		id := strings.TrimSpace(entry.Name)
		if id == "" {
			id = strings.TrimSpace(entry.Model)
		}
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, pluginapi.ModelInfo{
			ID:      id,
			Object:  "model",
			OwnedBy: QuotaIdentifier,
			Type:    "model",
			Name:    id,
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

// ---------------------------------------------------------------------------
// Credentials and payloads
// ---------------------------------------------------------------------------

// inferenceCredential reads the API key an execution needs.
//
// The same auth file serves quota and inference, but the two halves need
// different fields: the collector accepts an API key *or* a session cookie
// (SPEC 3.2), while inference can only use the key. A cookie-only credential is
// therefore a complete quota credential and an unusable inference credential,
// and the two failure classes must not be conflated.
func inferenceCredential(req pluginapi.ExecutorRequest) (ollama.Credential, error) {
	credential, errCredential := credentialFromStorage(req.StorageJSON, req.AuthMetadata)
	if errCredential != nil {
		return credential, errCredential
	}
	if credential.APIKey == "" {
		return credential, fmt.Errorf("%w: Ollama inference needs an %q field, this credential has only a session cookie",
			ErrIncompleteCredential, credentialKeyAPIKey)
	}
	return credential, nil
}

// chatPayload returns the request body to send upstream.
//
// Model and stream are rewritten rather than trusted: the host fills Payload
// from the translated request, but the model has to be the one CPA routed on, and
// the stream flag has to match the RPC method that arrived — a buffered answer
// requested as a stream (or the reverse) is a client-visible breakage. Values
// keep their exact bytes; only these two keys are replaced.
//
// A body that is not a JSON object is refused rather than forwarded: a non-JSON
// body here means the host and the plugin disagree about the format, and sending
// it would produce an upstream error this plugin would then have to explain.
func chatPayload(model string, payload, original []byte, stream bool) ([]byte, error) {
	body := payload
	if len(bytes.TrimSpace(body)) == 0 {
		body = original
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("the executor request carried no payload")
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &fields); errUnmarshal != nil || fields == nil {
		return nil, errors.New("the executor payload is not a JSON object")
	}
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		encoded, errMarshal := json.Marshal(trimmed)
		if errMarshal != nil {
			return nil, fmt.Errorf("encode the model name: %w", errMarshal)
		}
		fields["model"] = encoded
	}
	if stream {
		fields["stream"] = json.RawMessage("true")
	} else {
		fields["stream"] = json.RawMessage("false")
	}
	encoded, errMarshal := json.Marshal(fields)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode the executor payload: %w", errMarshal)
	}
	return encoded, nil
}

// credentialsFreeHeaders copies request headers without anything that could
// carry a credential or confuse the upstream connection.
func credentialsFreeHeaders(source http.Header) http.Header {
	headers := make(http.Header, len(source))
	for key, values := range source {
		switch strings.ToLower(key) {
		case "authorization", "cookie", "host", "content-length", "connection", "accept-encoding", "proxy-authorization":
			continue
		}
		headers[key] = append([]string(nil), values...)
	}
	return headers
}

// responseHeaders keeps the upstream headers a client may see. Cookies are
// stripped so a future upstream change cannot turn them into a client-visible
// leak; the transport-level headers are dropped because the host sets its own.
func responseHeaders(source http.Header) http.Header {
	headers := make(http.Header, len(source))
	for key, values := range source {
		switch strings.ToLower(key) {
		case "set-cookie", "content-length", "connection", "transfer-encoding", "content-encoding":
			continue
		}
		headers[key] = append([]string(nil), values...)
	}
	return headers
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// sseFrames extracts event data for the declared chat-completions output format.
// The host adds the SSE data: wrapper and writes DONE when the chunk channel
// closes. Supplying either here would double-frame the data or the sentinel.
// Non-data fields (including comments) do not belong in a JSON payload.
//
// A non-event-stream JSON answer is supplied as one payload, also without an
// SSE wrapper. The synchronous, whole-response buffering is unchanged.
func sseFrames(body []byte, eventStream bool) []pluginapi.ExecutorStreamChunk {
	payloads := make([][]byte, 0, 8)
	if eventStream {
		for _, frame := range splitSSE(body) {
			payload := sseData(frame)
			trimmed := bytes.TrimSpace(payload)
			if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
				continue
			}
			payloads = append(payloads, payload)
		}
	} else if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && json.Valid(trimmed) {
		payloads = append(payloads, trimmed)
	}

	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(payloads))
	for _, payload := range payloads {
		// Multiple SSE data fields are joined by newlines. Compact valid JSON
		// so the host's single data: field remains one physical line; JSON
		// string contents (including escaped newlines) retain their meaning.
		var compact bytes.Buffer
		if errCompact := json.Compact(&compact, payload); errCompact == nil {
			payload = compact.Bytes()
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: payload})
	}
	return chunks
}

// splitSSE cuts an upstream body into complete event frames, normalising CRLF.
func splitSSE(body []byte) [][]byte {
	var frames [][]byte
	pending := make([]byte, 0, 512)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		frame := make([]byte, 0, len(pending)+2)
		frame = append(frame, pending...)
		frame = append(frame, '\n', '\n')
		frames = append(frames, frame)
		pending = pending[:0]
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(bytes.TrimSpace(line)) == 0 {
			flush()
			continue
		}
		if len(pending) > 0 {
			pending = append(pending, '\n')
		}
		pending = append(pending, line...)
	}
	flush()
	return frames
}

// sseData joins only data fields, removing the one optional space after ':'.
// Comments and event/id/retry fields are SSE metadata, not response JSON.
func sseData(frame []byte) []byte {
	var data [][]byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		field, value, _ := bytes.Cut(line, []byte(":"))
		if bytes.Equal(field, []byte("data")) {
			data = append(data, bytes.TrimPrefix(value, []byte(" ")))
		}
	}
	return bytes.Join(data, []byte("\n"))
}

// ---------------------------------------------------------------------------
// Failures
// ---------------------------------------------------------------------------

// upstreamError reports a non-200 answer from Ollama Cloud.
type upstreamError struct {
	status  int
	message string
}

func (e upstreamError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("the Ollama upstream answered HTTP %d: %s", e.status, e.message)
	}
	return fmt.Sprintf("the Ollama upstream answered HTTP %d", e.status)
}

// executorFailure renders any execution failure as the envelope the host turns
// into a client error. The HTTP status travels with it so a rate limit or an
// expired key reaches the caller as itself instead of a generic 502.
//
// The message is built here and never interpolated from a request; the only
// upstream text that can appear is the parsed error field of a response, and it
// is scrubbed of the credential before it is used (SPEC 4.2).
func executorFailure(err error, credential ollama.Credential) []byte {
	code, message, status := classifyExecutorFailure(err)
	return errorEnvelopeStatus(code, scrubSecret(message, credential), status)
}

func classifyExecutorFailure(err error) (code, message string, status int) {
	var upstream upstreamError
	switch {
	case err == nil:
		return "executor_failed", "the Ollama execution failed", http.StatusBadGateway
	case errors.Is(err, ErrHostUnavailable):
		return "host_unavailable", ErrHostUnavailable.Error(), http.StatusServiceUnavailable
	case errors.Is(err, ErrIncompleteCredential):
		return "credential_incomplete", err.Error(), http.StatusUnauthorized
	case errors.Is(err, ErrExecutorUnavailable):
		return "plugin_quiesced", ErrExecutorUnavailable.Error(), http.StatusServiceUnavailable
	case errors.As(err, &upstream):
		return classifyUpstreamStatus(upstream)
	case errors.Is(err, context.DeadlineExceeded):
		return "upstream_timeout", "the Ollama upstream did not answer in time", http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		return "request_cancelled", "the Ollama request was cancelled", http.StatusRequestTimeout
	case errors.Is(err, ollama.ErrAuth):
		return "auth_failed", "the Ollama credential was rejected (authentication failed)", http.StatusUnauthorized
	case errors.Is(err, ollama.ErrRateLimit):
		return "rate_limited", "the Ollama upstream is rate limiting this account", http.StatusTooManyRequests
	case errors.Is(err, ollama.ErrNetwork):
		return "upstream_unreachable", "the Ollama upstream could not be reached", http.StatusBadGateway
	case errors.Is(err, ollama.ErrParse):
		return "upstream_invalid_response", "the Ollama upstream sent a response this plugin could not read", http.StatusBadGateway
	default:
		return "executor_failed", "the Ollama execution failed", http.StatusBadGateway
	}
}

// classifyUpstreamStatus maps Ollama's own answer onto a client-visible one.
//
// 401 and 403 both become 401: to the caller they are the same instruction,
// "fix the credential". A 429 keeps its status, which is the only thing a client
// can act on. Any other definite client error is passed through; everything else
// is folded into 502 because upstream is at fault.
func classifyUpstreamStatus(upstream upstreamError) (code, message string, status int) {
	text := upstream.Error()
	switch {
	case upstream.status == http.StatusUnauthorized || upstream.status == http.StatusForbidden:
		return "auth_failed", "the Ollama credential was rejected (authentication failed)", http.StatusUnauthorized
	case upstream.status == http.StatusTooManyRequests:
		return "rate_limited", text, http.StatusTooManyRequests
	case upstream.status >= 400 && upstream.status < 500:
		return "upstream_rejected", text, upstream.status
	default:
		return "upstream_error", text, http.StatusBadGateway
	}
}

// upstreamFailure renders a non-200 execution answer.
func upstreamFailure(response upstreamResponse, credential ollama.Credential) []byte {
	code, message, status := classifyUpstreamStatus(upstreamError{
		status:  response.status,
		message: upstreamMessage(response.body),
	})
	return errorEnvelopeStatus(code, scrubSecret(message, credential), status)
}

// upstreamMessage extracts the human-readable part of an Ollama error body.
//
// Only the error field is read, and only as text: quoting the whole body would
// put an arbitrary upstream document into an error message and a log line.
func upstreamMessage(body []byte) string {
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil || len(payload.Error) == 0 {
		return ""
	}
	var text string
	if errUnmarshal := json.Unmarshal(payload.Error, &text); errUnmarshal == nil {
		return truncateMessage(text)
	}
	var detailed struct {
		Message string `json:"message"`
	}
	if errUnmarshal := json.Unmarshal(payload.Error, &detailed); errUnmarshal == nil {
		return truncateMessage(detailed.Message)
	}
	return ""
}

// truncateMessage keeps one line of bounded length.
func truncateMessage(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	if len(collapsed) <= maxUpstreamMessageLength {
		return collapsed
	}
	return collapsed[:maxUpstreamMessageLength] + "..."
}

// scrubSecret removes the credential from text that is about to leave the
// plugin. Defence in depth: an upstream that echoed the Authorization header
// back in its error body would otherwise publish the key into a client response
// and a host log (SPEC 4.2).
func scrubSecret(text string, credential ollama.Credential) string {
	if text == "" || credential.APIKey == "" {
		return text
	}
	return strings.ReplaceAll(text, credential.APIKey, "[redacted]")
}

// errorEnvelopeStatus is ErrorEnvelope with an HTTP status attached, which the
// host turns into rpcError.StatusCode() (internal/pluginhost/rpc_client.go:370).
func errorEnvelopeStatus(code, message string, status int) []byte {
	raw, _ := json.Marshal(Envelope{Error: &EnvelopeError{Code: code, Message: message, HTTPStatus: status}})
	return raw
}
