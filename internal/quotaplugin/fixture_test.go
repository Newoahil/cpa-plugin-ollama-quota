package quotaplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// balanceBody is the upstream payload most tests use: a Pro plan with $12.40
// left and both legacy quota windows (5h at 30% used, weekly at 80% used).
const balanceBody = `{
  "plan": "pro",
  "included": {
    "balance_usd": 12.40,
    "allowance_usd": 20.00,
    "session": {"remaining_percent": 70, "resets_at": "2026-10-30T20:00:00Z"},
    "weekly": {"remaining_percent": 20, "resets_at": "2026-11-04T00:00:00Z"}
  }
}`

// balanceOK answers any upstream request with a 200 and the given body.
func balanceOK(body string) func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
	return func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		return pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(body),
		}
	}
}

// fixtureHost stands in for CPA's host callback table. It answers the callbacks
// this plugin uses, records what was asked, and lets a test choose what the
// upstream "ollama.com" returns.
//
// It lives in the test files on purpose: the production implementation is the
// cgo shim in cmd/plugin, and the point of the seam is that both satisfy the
// same HostCall type.
type fixtureHost struct {
	mu sync.Mutex
	// files is what host.auth.list returns.
	files []pluginapi.HostAuthFileEntry
	// storages maps a credential index to the JSON host.auth.get returns.
	storages map[string]json.RawMessage
	// respond answers one upstream HTTP request.
	respond func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse
	// respondErr, when set, fails the HTTP callback itself (a transport error).
	respondErr error
	// block, when non-nil, holds an HTTP callback until it is closed or the
	// request's operation is cancelled through host.http.cancel.
	block chan struct{}
	// requests records every upstream request, in order.
	requests []pluginapi.HTTPRequest
	// logs records every host.log line.
	logs []string
	// operations counts open/close pairs, which a test uses to prove a timed-out
	// collection was actually cancelled rather than abandoned.
	opened  int
	cancels int
	// authListErr, when set, fails host.auth.list.
	authListErr error
	// panicOnHTTP makes host.http.do panic, which is what a defect in the host
	// bridge or the HTTP stack looks like from here.
	panicOnHTTP bool
	// panicOnListAuths makes host.auth.list panic.
	panicOnListAuths bool
	// panicOnCancel makes host.http.cancel panic (P1-a).
	panicOnCancel bool
	// authListBlock, when non-nil, parks host.auth.list until it is closed. The
	// real host callback is synchronous and has no operation to cancel, so a
	// plain block is the honest model of a wedged host (P2-1).
	authListBlock chan struct{}
	// authListEntered is closed once host.auth.list has been entered and is
	// parked, so a test can act only after the call is provably in progress.
	authListEntered chan struct{}
	// cancelEntered / cancelRelease let a test hold the cancel callback inside
	// the host while it checks the quiesce barrier (P1-b).
	cancelEntered chan struct{}
	cancelRelease chan struct{}
	// cancelExited closes when a held cancel callback returns. A test uses it
	// to wait for the callback to genuinely leave the host, instead of
	// assuming a release implies an exit (P1-b round 3).
	cancelExited chan struct{}
	// authGetBlock, when non-nil, parks host.auth.get until it is closed (P2-1).
	authGetBlock chan struct{}
	// authGetHadDeadline records whether the last host.auth.get arrived with a
	// deadline. P2-1 is exactly the assertion that it does.
	authGetHadDeadline atomic.Bool
}

// settingsHTML is a settings page the #2 parser understands: a session window
// and a weekly window, plus the plan.
const settingsHTML = `<html><body>
<div><h3>Session usage</h3><span>30% used</span><p>Resets in 4 hours</p></div>
<div><h3>Weekly usage</h3><span>60% used</span><p>Resets in 3 days</p></div>
<p>Your plan: Pro</p>
</body></html>`

func newFixtureHost() *fixtureHost {
	return &fixtureHost{
		storages: make(map[string]json.RawMessage),
		respond: func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
			// A test that forgets to set an answer fails loudly instead of
			// silently succeeding with an empty body.
			return pluginapi.HTTPResponse{StatusCode: http.StatusInternalServerError}
		},
	}
}

// call is the HostCall the plugin under test is built with.
func (f *fixtureHost) call(ctxOfCall context.Context, method string, request []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch method {
	case pluginabi.MethodHostAuthList:
		if f.panicOnListAuths {
			panic("fixture: host.auth.list panicked")
		}
		if f.authListBlock != nil {
			entered := f.authListEntered
			block := f.authListBlock
			f.mu.Unlock()
			if entered != nil {
				select {
				case <-entered:
				default:
					close(entered)
				}
			}
			<-block
			f.mu.Lock()
			return nil, fmt.Errorf("host.auth.list: released")
		}
		if f.authListErr != nil {
			return nil, f.authListErr
		}
		return okResult(struct {
			Files []pluginapi.HostAuthFileEntry `json:"files"`
		}{Files: f.files})

	case pluginabi.MethodHostAuthGet:
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		if ctxOfCall != nil {
			if _, hasDeadline := ctxOfCall.Deadline(); hasDeadline {
				f.authGetHadDeadline.Store(true)
			}
		}
		if f.authGetBlock != nil {
			block := f.authGetBlock
			f.mu.Unlock()
			<-block
			f.mu.Lock()
			return nil, fmt.Errorf("host.auth.get: released")
		}
		stored, ok := f.storages[req.AuthIndex]
		if !ok {
			return nil, fmt.Errorf("host.auth.get: no credential %q", req.AuthIndex)
		}
		return okResult(struct {
			AuthIndex string          `json:"auth_index"`
			JSON      json.RawMessage `json:"json"`
		}{AuthIndex: req.AuthIndex, JSON: stored})

	case pluginabi.MethodHostHTTPOperationOpen:
		f.opened++
		return okResult(map[string]string{"operation_id": fmt.Sprintf("op-%d", f.opened)})

	case pluginabi.MethodHostHTTPCancel:
		f.cancels++
		// Abort the blocked exchange first, the way a real host cancel does.
		// The panic then models a defect after the abort, which is the case the
		// plugin has to survive (P1-a): the request itself is already finished,
		// so nothing should be waiting on this callback.
		if f.block != nil {
			select {
			case <-f.block:
			default:
				close(f.block)
			}
		}
		if f.panicOnCancel {
			// Deliberately panics with a value that looks like a secret: the
			// plugin must not put it anywhere (P1-a / SPEC 4.2).
			panic("fixture-cancel-panic-canary-CANCELSECRET-77")
		}
		if f.cancelEntered != nil {
			entered := f.cancelEntered
			release := f.cancelRelease
			f.mu.Unlock()
			select {
			case <-entered:
			default:
				close(entered)
			}
			// The exchange was already released above; park the callback inside
			// the host to model the P1-b race.
			if release != nil {
				<-release
			}
			f.mu.Lock()
			if f.cancelExited != nil {
				exited := f.cancelExited
				select {
				case <-exited:
				default:
					close(exited)
				}
			}
			return okResult(struct{}{})
		}
		// Releasing the blocker is what a real cancel does: the in-flight
		// exchange returns immediately.
		if f.block != nil {
			select {
			case <-f.block:
			default:
				close(f.block)
			}
		}
		return okResult(struct{}{})

	case pluginabi.MethodHostHTTPDo:
		if f.panicOnHTTP {
			panic("fixture: host.http.do panicked")
		}
		var req hostHTTPRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		f.requests = append(f.requests, pluginapi.HTTPRequest{
			Method:  req.Method,
			URL:     req.URL,
			Headers: http.Header(req.Headers),
			Body:    req.Body,
		})
		if f.respondErr != nil {
			return nil, f.respondErr
		}
		if f.block != nil {
			// Simulates an upstream that never answers: the callback parks until
			// the operation is cancelled. (The real host returns an error then;
			// here a plain wait is enough because the caller has already stopped
			// waiting for this goroutine's result.)
			block := f.block
			f.mu.Unlock()
			<-block
			f.mu.Lock()
			return nil, fmt.Errorf("host.http.do: operation cancelled")
		}
		return okResult(f.respond(pluginapi.HTTPRequest{
			Method:  req.Method,
			URL:     req.URL,
			Headers: http.Header(req.Headers),
			Body:    req.Body,
		}))

	case pluginabi.MethodHostLog:
		var req struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(request, &req)
		f.logs = append(f.logs, req.Message)
		return okResult(struct{}{})

	default:
		return nil, fmt.Errorf("fixture does not implement %s", method)
	}
}

// okResult wraps a value the way the host does: an ok envelope whose result is
// the JSON of v.
func okResult(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(hostEnvelope{OK: true, Result: raw})
}

// setCredential registers one credential the collector can find.
func (f *fixtureHost) setCredential(authIndex string, storage map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		panic(errMarshal)
	}
	f.storages[authIndex] = raw
	f.files = append(f.files, pluginapi.HostAuthFileEntry{
		ID:        authIndex,
		AuthIndex: authIndex,
		Name:      authIndex + ".json",
		Provider:  QuotaIdentifier,
	})
}

// addListedEntry appends an entry to what host.auth.list returns without
// registering a readable credential, which is how a host that has not wired its
// auth manager yet lists the files on disk.
func (f *fixtureHost) addListedEntry(entry pluginapi.HostAuthFileEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files = append(f.files, entry)
}

// setRespond replaces the upstream answer.
func (f *fixtureHost) setRespond(respond func(pluginapi.HTTPRequest) pluginapi.HTTPResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = respond
}

// setPanicOnHTTP makes every HTTP callback panic.
func (f *fixtureHost) setPanicOnHTTP(value bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panicOnHTTP = value
}

// setPanicOnListAuths makes host.auth.list panic.
func (f *fixtureHost) setPanicOnListAuths(value bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panicOnListAuths = value
}

// setPanicOnCancel makes host.http.cancel panic.
func (f *fixtureHost) setPanicOnCancel(value bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panicOnCancel = value
}

// setAuthListBlock parks host.auth.list until the returned function is called.
// It also returns a channel that closes once the call is inside the block.
func (f *fixtureHost) setAuthListBlock() (release func(), entered <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authListBlock = make(chan struct{})
	f.authListEntered = make(chan struct{})
	block := f.authListBlock
	return func() {
		select {
		case <-block:
		default:
			close(block)
		}
	}, f.authListEntered
}

// setAuthGetBlock parks host.auth.get until the returned function is called.
func (f *fixtureHost) setAuthGetBlock() (release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authGetBlock = make(chan struct{})
	block := f.authGetBlock
	return func() {
		select {
		case <-block:
		default:
			close(block)
		}
	}
}

// setBlockCleared releases a blocker installed by setBlock and removes it, so a
// later callback takes the normal path.
func (f *fixtureHost) setBlockCleared() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		default:
			close(f.block)
		}
	}
	f.block = nil
}

// setCancelHold parks the host.http.cancel callback inside the host. It returns
// a channel that closes once the callback is inside, and the release function
// the test calls to let it finish.
func (f *fixtureHost) setCancelHold() (entered <-chan struct{}, release func(), exited <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelEntered = make(chan struct{})
	f.cancelRelease = make(chan struct{})
	f.cancelExited = make(chan struct{})
	releaseCh := f.cancelRelease
	exitedCh := f.cancelExited
	return f.cancelEntered, func() {
		select {
		case <-releaseCh:
		default:
			close(releaseCh)
		}
	}, exitedCh
}

// setBlock installs a blocker that parks every HTTP callback until it is closed
// by a cancel.
func (f *fixtureHost) setBlock() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = make(chan struct{})
}

// requestCount reports how many upstream requests were issued.
func (f *fixtureHost) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// lastRequest returns the most recent upstream request.
func (f *fixtureHost) lastRequest() pluginapi.HTTPRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return pluginapi.HTTPRequest{}
	}
	return f.requests[len(f.requests)-1]
}

// logCount reports how many log lines were recorded.
func (f *fixtureHost) logCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.logs)
}

// cancelCount reports how many host.http.cancel calls were made.
func (f *fixtureHost) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels
}
