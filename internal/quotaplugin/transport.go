package quotaplugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// transport adapts one collected request onto the host's HTTP policy. It is the
// bridge between the #2 collector's plain Go seam (ollama.Transport, which takes
// a *http.Request and returns an *http.Response) and the host's JSON callback.
//
// Two things make this more than a field copy.
//
// First, QuotaFetchRequest.HTTPClient never reaches a native plugin: the field
// is tagged `json:"-"` because an interface cannot cross the C ABI, and the
// host's rpcQuotaProvider replaces it with a host_callback_id
// (internal/pluginhost/rpc_client.go:654). So the transport is rebuilt on this
// side from host.http.do callbacks, and callbackID is the scope that ties the
// request to the inbound quota call.
//
// Second, a context deadline does not survive the C boundary: cliproxyHostCall
// rebuilds its context from context.Background() on the host side
// (internal/pluginhost/host_callbacks_unix.go:39), and the host's HTTP client is
// created with no timeout (internal/runtime/executor/helps/proxy_helpers.go:32).
// A plain synchronous host.http.do against a hung upstream would therefore block
// until the process exits. exchange binds the call to a host HTTP operation and
// cancels that operation when the caller's deadline expires, so the 20-second
// collection budget (SPEC 4.4) actually holds.
type transport struct {
	bridge     *hostBridge
	callbackID string
}

// compile-time proof the adapter satisfies the seam #2 exposes.
var _ ollama.Transport = (*transport)(nil)

// Do implements ollama.Transport.
func (t *transport) Do(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("the Ollama transport was given no request")
	}
	if t.bridge == nil {
		return nil, ErrHostUnavailable
	}
	return t.bridge.exchange(request.Context(), request, t.callbackID)
}

// exchange performs one host HTTP exchange under a deadline the host honours.
func (b *hostBridge) exchange(ctx context.Context, request *http.Request, callbackID string) (*http.Response, error) {
	hostRequest := pluginapi.HTTPRequest{
		Method:  request.Method,
		URL:     request.URL.String(),
		Headers: request.Header.Clone(),
	}
	if request.Body != nil && request.Body != http.NoBody {
		body, errRead := io.ReadAll(request.Body)
		if errRead != nil {
			return nil, fmt.Errorf("read the Ollama request body: %w", errRead)
		}
		hostRequest.Body = body
	}

	response, errExchange := b.exchangeWithDeadline(ctx, hostRequest, callbackID)
	if errExchange != nil {
		return nil, errExchange
	}
	// The host does not report the final URL, so the response names the request
	// that was issued. The collector's pinned-origin check has already refused
	// any request not aimed at ollama.com; a redirect the host chose to follow is
	// the residual risk documented in docs/agents/quota.md.
	return &http.Response{
		StatusCode: response.StatusCode,
		Status:     fmt.Sprintf("%d %s", response.StatusCode, http.StatusText(response.StatusCode)),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     response.Headers,
		Body:       io.NopCloser(bytes.NewReader(response.Body)),
		Request:    request,
	}, nil
}

// exchangeWithDeadline runs one host.http.do so that the caller's context can
// interrupt it.
//
// The host call must be made synchronously: its only cancellation path is a
// separate host.http.cancel on the same operation. context.AfterFunc supplies
// exactly that — a runtime-managed callback that fires on cancellation, sends
// the cancel, and is stopped on the normal path. No goroutine of this package's
// own is left behind either way, so the collector's goroutine count stays flat
// (SPEC 4.4).
func (b *hostBridge) exchangeWithDeadline(ctx context.Context, request pluginapi.HTTPRequest, callbackID string) (pluginapi.HTTPResponse, error) {
	operationID, errOpen := b.openOperation(ctx, callbackID)
	if errOpen != nil {
		return pluginapi.HTTPResponse{}, errOpen
	}
	// Release the operation once the call is over. The cancellation below may
	// run concurrently with this; host.http.cancel and host.http.do are both
	// keyed by operationID and the host treats a cancel of a finished operation
	// as a no-op, so ordering does not matter.
	stopCancel := context.AfterFunc(ctx, func() {
		b.closeOperation(context.WithoutCancel(ctx), callbackID, operationID)
	})
	defer func() {
		stopCancel()
		b.closeOperation(context.WithoutCancel(ctx), callbackID, operationID)
	}()

	response, errDo := b.do(ctx, request, callbackID, operationID)
	if errDo != nil {
		// A cancelled operation surfaces as a generic host error. Reporting the
		// context's own error keeps the failure classified as a network timeout
		// rather than an unclassified host refusal (ollama.networkError only
		// recognises the context sentinels).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return pluginapi.HTTPResponse{}, fmt.Errorf("the Ollama request ended with %w", ctxErr)
		}
		return pluginapi.HTTPResponse{}, errDo
	}
	return response, nil
}

// openOperation opens one host HTTP operation so the request can be cancelled.
// A host that does not offer the bridge still gets a usable client: an empty
// operation id means "no cancel available", and the deadline then only bounds
// how long this plugin waits.
func (b *hostBridge) openOperation(ctx context.Context, callbackID string) (string, error) {
	var response struct {
		OperationID string `json:"operation_id"`
	}
	payload := map[string]string{}
	if callbackID != "" {
		payload["host_callback_id"] = callbackID
	}
	if errInvoke := b.invoke(ctx, pluginabi.MethodHostHTTPOperationOpen, payload, &response); errInvoke != nil {
		return "", errInvoke
	}
	return response.OperationID, nil
}

// closeOperation retires an operation, aborting whatever is still running on it.
// Failures are dropped: this runs on the way out of a request that has already
// produced its result, and a broken bridge here must not replace it.
func (b *hostBridge) closeOperation(ctx context.Context, callbackID, operationID string) {
	if operationID == "" {
		return
	}
	payload := map[string]string{"operation_id": operationID}
	if callbackID != "" {
		payload["host_callback_id"] = callbackID
	}
	var ignored any
	_ = b.invoke(ctx, pluginabi.MethodHostHTTPCancel, payload, &ignored)
}
