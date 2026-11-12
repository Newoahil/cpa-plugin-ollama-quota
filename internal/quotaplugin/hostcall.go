package quotaplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

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

// errHostCallbackPanicked is the fixed, safe message a host callback panic is
// reduced to.
//
// The recovered value is never formatted into it: a panicking host callback can
// carry anything at all, including the request body it was handed, and the
// request body of an Ollama collection carries the credential. A constant keeps
// the panic from becoming a way out for the secret (SPEC 4.2).
var errHostCallbackPanicked = fmt.Errorf("%w: the host callback panicked", ErrHostUnavailable)

// hostBridge adapts the host callbacks this plugin uses onto plain Go types.
// It also implements ollama.Transport (see Do) so the #2 collector can issue
// its requests through the host's transport policy.
type hostBridge struct {
	// call is the raw host callback.
	call HostCall
	// inflight tracks host callbacks and whole asynchronous executor pumps. The
	// latter stay counted between callbacks too, so quiesce cannot mistake a
	// live inference stream for an idle library and unload it.
	inflight inflightSet
	// drainChecks counts waitInflight calls that actually examined the
	// in-flight set. It is a diagnostic: it lets a test tell a real drain
	// judgement apart from one that answered from cached state.
	drainChecks atomic.Uint64
}

// inflightSet counts outstanding host calls and offers a channel that is closed
// while none are outstanding.
//
// It exists instead of a sync.WaitGroup because the wait has to be selectable
// against a deadline, and a WaitGroup's Wait is not. The alternative — waiting
// on the WaitGroup from a goroutine and selecting on that goroutine — would add
// a goroutine per quiesce, which is exactly what this plugin is required not to
// do (the collector's goroutine count must stay flat, SPEC 4.4).
type inflightSet struct {
	mu sync.Mutex
	// count is the number of outstanding calls.
	count int
	// open reports whether idle is currently an unfolded channel that end() must
	// close. It is separate from count == 0 because a set that was frozen while
	// idle keeps a closed channel that must never be closed again.
	open bool
	// frozen stops new calls from reopening the channel once quiesce has taken
	// the barrier: a call that starts after that point will be cancelled by the
	// host anyway and must not extend the wait indefinitely.
	frozen bool
	// idle is closed while count == 0.
	idle chan struct{}
}

func newInflightSet() inflightSet {
	idle := make(chan struct{})
	close(idle)
	return inflightSet{idle: idle}
}

// begin registers one outstanding call and reports whether it was counted. A
// call begun after freeze is not counted, so it must not retire another call's
// entry with end. Long-lived executor streams also reserve one entry here.
func (s *inflightSet) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen {
		return false
	}
	if !s.open {
		s.idle = make(chan struct{})
		s.open = true
	}
	s.count++
	return true
}

// end retires one outstanding call.
func (s *inflightSet) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count <= 0 {
		return
	}
	s.count--
	if s.count == 0 && s.open {
		close(s.idle)
		s.open = false
	}
}

// pending reports how many calls are outstanding right now.
func (s *inflightSet) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// freeze stops counting new calls and returns the channel that is closed once
// every call already registered has returned.
func (s *inflightSet) freeze() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = true
	return s.idle
}

// thaw re-enables counting after a drain attempt gave up. A host that ignores
// the cancel cannot be abandoned from inside this plugin (the callback is a
// synchronous C call), so the barrier has to be reusable: a later quiesce
// must be able to observe the same still-running call and fail again rather
// than report an empty set it never actually drained.
//
// Thawing must NOT discard the idle channel's identity: a call that was
// already registered keeps counting against the same channel, so the next
// attempt observes it going to zero instead of starting from a fresh, empty
// set that would look drained.
func (s *inflightSet) thaw() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = false
}

func newHostBridge(call HostCall) *hostBridge {
	return &hostBridge{call: call, inflight: newInflightSet()}
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
// It is the one place a raw host callback is made, so it is where three
// properties are established for every caller:
//
//  1. a panic raised anywhere in the callback becomes errHostCallbackPanicked
//     rather than propagating. That covers a callback reached through
//     context.AfterFunc on its own goroutine, where no caller's recover can
//     help (the P1-a finding: a panicking cancel callback took the process
//     down with exit 1);
//  2. the call is registered as in-flight for its whole duration, including
//     the panic path, so waitInflight cannot return while it is running;
//  3. the returned error is built from the envelope's own message and never
//     from the request or the credential (SPEC 4.2).
func (b *hostBridge) invoke(ctx context.Context, method string, payload any, out any) (err error) {
	if b == nil || b.call == nil {
		return ErrHostUnavailable
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return fmt.Errorf("encode %s request: %w", method, errMarshal)
	}

	// begin must run before the call, not as a defer: it is what marks this
	// call as in flight for the whole duration. Deferring it would register the
	// call only when it was already over, and the quiesce barrier would see an
	// always-empty set.
	if b.inflight.begin() {
		defer b.inflight.end()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errHostCallbackPanicked
		}
	}()

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

// waitInflight blocks until every host call this bridge has started has
// returned, or until ctx expires. It reports whether the bridge drained.
//
// This is the P1-b barrier. The collector's stop function waits for the
// collector goroutine, but a context.AfterFunc callback runs on a runtime
// goroutine of its own and can still be inside host.http.cancel after the
// collector has exited. Because every such call is registered by invoke,
// waiting here drains exactly those calls.
//
// It adds no goroutine of its own: the wait is a plain select on a channel
// that the last caller closes, so the collector's goroutine count stays flat
// (SPEC 4.4).
//
// ctx bounds the wait. A host call that never returns cannot be abandoned by
// this plugin -- the callback is a synchronous C function -- so an unbounded
// wait would make quiesce hang forever on a broken host. Returning false is
// therefore a real answer, not a formality: the caller must surface it
// instead of reporting a successful shutdown (see drainOutcome).
//
// A failed wait thaws the set, so a later quiesce can observe the same
// still-running call and fail again rather than seeing a set it never drained.
func (b *hostBridge) waitInflight(ctx context.Context) drainOutcome {
	if b == nil {
		return drainOutcome{drained: true}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	b.drainChecks.Add(1)
	idle := b.inflight.freeze()
	// An already idle set has drained even when the deadline has expired.
	// Prefer that evidence over a random choice between two ready cases.
	select {
	case <-idle:
		return drainOutcome{drained: true}
	default:
	}
	select {
	case <-idle:
		return drainOutcome{drained: true}
	case <-ctx.Done():
		pending := b.inflight.pending()
		b.inflight.thaw()
		return drainOutcome{pending: pending}
	}
}

// DrainChecks reports how many times this bridge actually examined its
// in-flight set. Nothing in the plugin depends on it; tests use it to prove
// a drain judgement really happened.
func (b *hostBridge) DrainChecks() uint64 {
	if b == nil {
		return 0
	}
	return b.drainChecks.Load()
}

// drainOutcome is what a collector stop reports about the host calls it could
// not finish draining.
//
// The plugin cannot wait forever (a host that ignores the cancel would wedge
// hot replacement), so a bounded wait is the right shape. What is NOT
// acceptable is reporting success when the bound expired: the host would go on
// to swap or unload a library with a live call in it. This type is how that
// turns into an observable failure at the RPC boundary instead of a log line.
type drainOutcome struct {
	// drained is true when every host call the plugin started has returned.
	drained bool
	// pending is the number of calls still running when the budget expired.
	pending int
	// unsettled reports that the stop's result is not known because another
	// caller is inside it right now. It is a failure, not an empty set: the
	// collector the stop stands for may still have a callback inside the host,
	// and inferring success from work whose result nobody has seen is exactly
	// the false success that makes a host unload a live library.
	unsettled bool
}

// errDrainTimeout reports a collection that could not be drained.
//
// The message is a constant plus a count: the calls still running are host
// callbacks holding a credential, and none of that may reach a log or a
// response (SPEC 4.2).
func (o drainOutcome) errDrainTimeout() error {
	if o.unsettled {
		// No count is quoted because none was observed: another lifecycle call
		// is inside this collector's stop, and its answer is what decides.
		return fmt.Errorf("%w: a collection stop is still in progress", ErrQuiesceIncomplete)
	}
	return fmt.Errorf("%w: %d collection host call(s) still running", ErrQuiesceIncomplete, o.pending)
}

// callBounded performs one host callback under a deadline the plugin enforces
// for itself (P2-1).
//
// The problem it solves: a deadline set by this package does not reach the work
// behind a host callback. The native host entry point rebuilds its context from
// context.Background() (internal/pluginhost/host_callbacks_unix.go:45), so a
// single context.WithTimeout cannot interrupt a synchronous C call. The host's
// HTTP operation bridge can interrupt host.http.do, but the credential reads
// (host.auth.list / host.auth.get) have no operation at all — and those run
// before any HTTP request, so an enumeration that never returns would hold the
// collector, and therefore quiesce, forever.
//
// The budget is therefore enforced from this side: the callback runs on a
// goroutine and the caller stops waiting when the deadline passes. Two things
// keep that from becoming a leak:
//
//   - invoke registers the call as in-flight before it starts and unregisters
//     it when it returns, so the quiesce barrier (waitInflight) still waits for
//     an abandoned call rather than letting the library unload underneath it;
//   - the goroutine is per call, and exits as soon as the host call returns, so
//     it cannot accumulate across refreshes.
//
// A callback on a context with no deadline takes the synchronous path, so the
// usual inbound quota fetch adds no goroutine.
func (b *hostBridge) callBounded(ctx context.Context, method string, payload any, out any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if b == nil || b.call == nil {
		return b.invoke(ctx, method, payload, out)
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return b.invoke(ctx, method, payload, out)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s was not attempted: %w", method, ctx.Err())
	}

	// Buffered so an abandoned result never strands the sender.
	done := make(chan error, 1)
	go func() {
		// invoke already recovers a panicking host callback; this covers the
		// bookkeeping around it so nothing can escape on this goroutine.
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- errHostCallbackPanicked
			}
		}()
		done <- b.invoke(ctx, method, payload, out)
	}()

	select {
	case errCall := <-done:
		return errCall
	case <-ctx.Done():
		return fmt.Errorf("%s did not finish before the deadline: %w", method, ctx.Err())
	}
}

// hostHTTPRequest is the wire shape of pluginapi.HTTPRequest plus the callback
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
//
// It goes through callBounded so a host that never answers this callback cannot
// hold a sweep — and therefore quiesce — forever (P2-1).
func (b *hostBridge) listAuths(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	var response hostAuthListResponse
	if errInvoke := b.callBounded(ctx, pluginabi.MethodHostAuthList, map[string]any{}, &response); errInvoke != nil {
		return nil, errInvoke
	}
	return response.Files, nil
}

// getAuth reads one credential's raw stored JSON, under the caller's budget
// (P2-1).
func (b *hostBridge) getAuth(ctx context.Context, authIndex string) (hostAuthGetResponse, error) {
	var response hostAuthGetResponse
	payload := map[string]string{"auth_index": authIndex}
	if errInvoke := b.callBounded(ctx, pluginabi.MethodHostAuthGet, payload, &response); errInvoke != nil {
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
