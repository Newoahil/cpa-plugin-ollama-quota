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
// Three things make this more than a field copy.
//
// First, QuotaFetchRequest.HTTPClient never reaches a native plugin: the field
// is tagged `json:"-"` because an interface cannot cross the C ABI, and the
// host's rpcQuotaProvider replaces it with a host_callback_id
// (internal/pluginhost/rpc_client.go:654). So the transport is rebuilt on this
// side from host.http.do callbacks, and callbackID is the scope that ties the
// request to the inbound quota call.
//
// Second, a collection deadline the plugin creates cannot travel through the
// callback. The native host entry point rebuilds its context from
// context.Background() (internal/pluginhost/host_callbacks_unix.go:45), so a
// deadline this package sets is invisible to the work it is meant to bound.
// (The inbound quota call's own context is different: the host restores it from
// the callback id — internal/pluginhost/http_operation_bridge.go:364-370 — so a
// quiesce does reach the request. It is only the collection budget this package
// adds for itself that has to be enforced explicitly.) exchangeWithDeadline
// therefore opens a host HTTP operation and cancels that operation itself when
// the deadline expires.
//
// Third, the cancellation is asynchronous, and the callback that performs it
// runs on a goroutine nobody else owns. Both P1 findings live here: the callback
// must survive a panicking host (P1-a), and exchangeWithDeadline must not return
// while the callback is still calling into the host (P1-b). See cancelGuard.
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
	// any request that was not aimed at ollama.com. A redirect the host chose to
	// follow is invisible here and is the residual risk documented in
	// docs/agents/quota.md and the PR description — it is an unmet SPEC 4.2
	// constraint that depends on a host change, not something this plugin can
	// detect or undo.
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
// interrupt it, and returns only once every host call this exchange started has
// finished.
//
// The host call must be made synchronously: the only way to interrupt it is a
// separate host.http.cancel on the same operation. A cancelGuard owns that
// relationship — it arms a context.AfterFunc for the deadline, and its release
// waits for an already-running callback before returning, so the exchange does
// not leave a host call of its own behind. That is the per-exchange half of the
// P1-b barrier; the whole-plugin half is hostBridge.waitInflight, which the
// collector's stop waits on and which therefore also covers a callback that
// outlives the exchange.
func (b *hostBridge) exchangeWithDeadline(ctx context.Context, request pluginapi.HTTPRequest, callbackID string) (pluginapi.HTTPResponse, error) {
	operationID, errOpen := b.openOperation(ctx, callbackID)
	if errOpen != nil {
		return pluginapi.HTTPResponse{}, errOpen
	}
	guard := b.newCancelGuard(ctx, callbackID, operationID)
	defer guard.release()

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

// cancelGuard owns the asynchronous host.http.cancel for one exchange.
//
// Two properties it has to provide, both from the review:
//
//   - a panic in the callback must not escape onto the runtime goroutine that
//     runs it, because nothing above it can recover: that was P1-a, reproduced
//     as exit 1. invoke already turns a callback panic into a fixed error, so
//     nothing here has to format the recovered value; the callback additionally
//     refuses to let a panic past it in case a future host call is added that
//     reaches the host without going through invoke.
//   - release must not return while an already-started callback is still
//     calling into the host, which was P1-b. The completion signal is a channel
//     the callback closes; release waits on it when, and only when, the callback
//     has already been started.
//
// A channel rather than sync.WaitGroup on purpose: WaitGroup forbids Add racing
// with Wait, and here the Add would be on the callback's goroutine while release
// is already waiting — the race detector flags exactly that. AfterFunc's
// contract gives the equivalent guarantee without it: stop() returning false
// means the callback has been started and will therefore close done exactly
// once, so release can simply wait for it.
//
// The callback is fired by the runtime only when ctx is cancelled, and it is
// stopped on the normal path, so this adds no goroutine that can accumulate
// across refreshes.
type cancelGuard struct {
	// done is closed by the deadline callback when it returns. release only
	// waits on it after AfterFunc has reported the callback already started.
	done chan struct{}
	// stopAfterFunc disarms the deadline callback once the exchange is over.
	stopAfterFunc func() bool
}

// newCancelGuard arms the deadline callback for one exchange.
func (b *hostBridge) newCancelGuard(ctx context.Context, callbackID, operationID string) *cancelGuard {
	guard := &cancelGuard{done: make(chan struct{})}
	guard.stopAfterFunc = context.AfterFunc(ctx, func() {
		defer close(guard.done)
		// A panic here would run on the runtime's goroutine with no recover
		// above it (P1-a). invoke already converts a host panic to a fixed
		// error; this belt keeps a future non-invoke host call from escaping too.
		defer func() {
			if recovered := recover(); recovered != nil {
				b.logNoteCancelledSafely()
			}
		}()
		// context.WithoutCancel: the cancel has to go out even though ctx is
		// exactly the thing that already expired.
		b.closeOperation(context.WithoutCancel(ctx), callbackID, operationID)
	})
	return guard
}

// release stops the deadline callback and waits for one that already started.
//
// It runs on the way out of exchangeWithDeadline whether or not the exchange
// failed, so that by the time it returns no host call this exchange started is
// still in progress.
//
// This is defence in depth rather than the only barrier: a callback that
// outlives release is still counted in hostBridge.inflight, so quiesce's
// waitInflight would wait for it too. Keeping both means the exchange cleans up
// after itself, and the plugin-wide barrier covers the paths this one cannot see.
func (g *cancelGuard) release() {
	if g == nil {
		return
	}
	if g.stopAfterFunc != nil && g.stopAfterFunc() {
		// The callback had not started and now never will: nothing to wait for.
		return
	}
	// stop() returning false means the callback has already been started, so it
	// will close done. Waiting needs no goroutine and cannot deadlock: the
	// callback only calls into the host, which does not call back here.
	<-g.done
}

// logNoteCancelledSafely records that a cancel callback panicked, without
// letting the logger become a second failure path.
//
// The message is a constant: the callback may have panicked with whatever the
// host handed it, and that value must never reach a log line (SPEC 4.2).
func (b *hostBridge) logNoteCancelledSafely() {
	defer func() { _ = recover() }()
	b.logNote(context.Background(), "error", "the Ollama quota collector's host cancel callback panicked; the operation may still be running")
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
