package quotaplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// HostCall performs one host callback (pluginabi.MethodHostHTTPDo,
// MethodHostAuthList, MethodHostAuthGet, MethodHostLog) and returns the raw
// JSON envelope result.
//
// It exists because the native plugin ABI carries no Go values: the host hands
// the plugin a C function pointer at cliproxy_plugin_init and everything else
// crosses as JSON. In particular QuotaFetchRequest.HTTPClient is tagged
// `json:"-"`, so the host transport the interface advertises never reaches a
// .so plugin — the only way to reach it is a host.http.do callback.
//
// The cgo shim in cmd/plugin supplies the real implementation. Tests supply a
// fake, which is why this is a function type and not an interface: the
// implementation needs no state of its own.
type HostCall func(ctx context.Context, method string, request []byte) ([]byte, error)

// ErrHostUnavailable reports a plugin running without a host bridge. It can
// only happen in tests or in a host that never called cliproxy_plugin_init.
var ErrHostUnavailable = errors.New("the host callback bridge is not available")

// hostBridge adapts the host callbacks this plugin uses onto plain Go types.
// It also implements ollama.Transport (see Do) so the #2 collector can issue
// its requests through the host's transport policy.
type hostBridge struct {
	call HostCall
}

func newHostBridge(call HostCall) *hostBridge {
	return &hostBridge{call: call}
}

// hostEnvelope mirrors pluginabi.Envelope. A callback that fails returns an
// error envelope rather than a Go error, so both shapes have to be handled.
type hostEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// invoke calls one host method and unwraps the envelope.
//
// The error it returns is deliberately built from the envelope's own message
// and never from the request or the credential: a host bridge failure must not
// become a way for the API key to reach a log line (SPEC 4.2).
func (b *hostBridge) invoke(ctx context.Context, method string, payload any, out any) error {
	if b == nil || b.call == nil {
		return ErrHostUnavailable
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return fmt.Errorf("encode %s request: %w", method, errMarshal)
	}
	response, errCall := b.call(ctx, method, raw)
	if errCall != nil {
		// The host's own callback error may quote the request it rejected, and
		// the request can carry an Authorization or Cookie header. Only the
		// fact and the method name are kept.
		return fmt.Errorf("the host refused %s", method)
	}
	var envelope hostEnvelope
	if errUnmarshal := json.Unmarshal(response, &envelope); errUnmarshal != nil {
		return fmt.Errorf("decode %s response: %w", method, errUnmarshal)
	}
	if !envelope.OK {
		if envelope.Error != nil && envelope.Error.Message != "" {
			// The host's message here is its own diagnostic text (for example
			// "host callback ID is not open"); it is not an echoed request.
			return fmt.Errorf("%s failed: %s", method, envelope.Error.Message)
		}
		return fmt.Errorf("%s failed", method)
	}
	if out == nil {
		return nil
	}
	if errUnmarshal := json.Unmarshal(envelope.Result, out); errUnmarshal != nil {
		return fmt.Errorf("decode %s result: %w", method, errUnmarshal)
	}
	return nil
}

// hostHTTPRequest is the wire shape of pluginabi.HTTPRequest plus the callback
// scope. pluginapi.HTTPRequest's fields have no json tags of their own, so the
// host decodes them case-insensitively from these names. HostCallbackID and
// OperationID are the two scopes the host uses to cancel an in-flight call
// (internal/pluginhost/host_callbacks.go rpcHostHTTPRequest).
type hostHTTPRequest struct {
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        httpHeaders `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	OperationID    string      `json:"operation_id,omitempty"`
}

// httpHeaders pins the JSON shape to map[string][]string, which is what the
// host's decoder expects for a header map.
type httpHeaders map[string][]string

// hostHTTPResponse is the wire shape of pluginapi.HTTPResponse.
type hostHTTPResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    httpHeaders `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

// hostAuthListResponse is the wire shape of the host's auth.list result.
type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

// hostAuthGetResponse is the wire shape of the host's auth.get result.
//
// JSON is the credential file's contents, so it is a JSON *object* on the wire.
// It must stay json.RawMessage: a []byte would expect a base64 string and fail
// to decode the object the host actually sends.
type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

// do performs one request through the host's transport policy.
//
// callbackID binds the request to the inbound quota call, so a quiesce
// cancels it; operationID binds it to the cancellable operation openOperation
// opened. Both are empty on the collector's own goroutine, where there is no
// inbound call and the collection timeout is the only bound.
func (b *hostBridge) do(ctx context.Context, request pluginapi.HTTPRequest, callbackID, operationID string) (pluginapi.HTTPResponse, error) {
	wire := hostHTTPRequest{
		Method:         request.Method,
		URL:            request.URL,
		Headers:        httpHeaders(request.Headers),
		Body:           request.Body,
		HostCallbackID: strings.TrimSpace(callbackID),
		OperationID:    strings.TrimSpace(operationID),
	}
	var response hostHTTPResponse
	if errInvoke := b.invoke(ctx, pluginabi.MethodHostHTTPDo, wire, &response); errInvoke != nil {
		return pluginapi.HTTPResponse{}, errInvoke
	}
	return pluginapi.HTTPResponse{
		StatusCode: response.StatusCode,
		Headers:    map[string][]string(response.Headers),
		Body:       response.Body,
	}, nil
}

// listAuths enumerates the credentials the host currently holds.
func (b *hostBridge) listAuths(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	var response hostAuthListResponse
	if errInvoke := b.invoke(ctx, pluginabi.MethodHostAuthList, map[string]any{}, &response); errInvoke != nil {
		return nil, errInvoke
	}
	return response.Files, nil
}

// getAuth reads one credential's raw stored JSON.
func (b *hostBridge) getAuth(ctx context.Context, authIndex string) (hostAuthGetResponse, error) {
	var response hostAuthGetResponse
	payload := map[string]string{"auth_index": authIndex}
	if errInvoke := b.invoke(ctx, pluginabi.MethodHostAuthGet, payload, &response); errInvoke != nil {
		return hostAuthGetResponse{}, errInvoke
	}
	return response, nil
}

// logNote records one line through the host logger. Messages are built by this
// package and never interpolate a credential.
func (b *hostBridge) logNote(ctx context.Context, level, message string) {
	payload := map[string]any{"level": level, "message": message}
	var ignored json.RawMessage
	_ = b.invoke(ctx, pluginabi.MethodHostLog, payload, &ignored)
}
