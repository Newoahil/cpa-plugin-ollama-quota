package quotaplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// streamTestHost models the two independent streams in the native ABI. HTTP
// reads are arbitrary byte fragments, NOT SSE events; executor emits are event
// data, with bounded backpressure just like the host's stream bridge.
// Nothing here opens a socket or uses a real credential.
type streamTestRead struct {
	Payload    []byte `json:"payload,omitempty"`
	Error      string `json:"error,omitempty"`
	Done       bool   `json:"done,omitempty"`
	panicValue any
}

type streamTestHost struct {
	*fixtureHost
	clientCtx context.Context
	cancel    context.CancelFunc
	reads     chan streamTestRead
	started   chan struct{}
	closed    chan struct{}
	emitted   chan pluginapi.ExecutorStreamChunk
	mu        sync.Mutex
	body      []byte
	readBody  bool
	closeErr  *pluginabi.Error
	upClosed  int
}

func newStreamExecutorHandler(t *testing.T) (*Handler, *streamTestHost) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	host := &streamTestHost{
		fixtureHost: newFixtureHost(), clientCtx: ctx, cancel: cancel,
		started: make(chan struct{}), closed: make(chan struct{}),
		emitted: make(chan pluginapi.ExecutorStreamChunk, 16),
	}
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": fixtureAPIKey})
	h := NewHandlerWithHost(NewStateFor(host.call), host.call)
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { cancel(); h.state.Shutdown() })
	return h, host
}

func (f *streamTestHost) call(ctx context.Context, method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostHTTPDoStream:
		raw, err := f.fixtureHost.call(ctx, pluginabi.MethodHostHTTPDo, request)
		if err != nil {
			return nil, err
		}
		var envelope hostEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, err
		}
		var response pluginapi.HTTPResponse
		if err := json.Unmarshal(envelope.Result, &response); err != nil {
			return nil, err
		}
		f.body = response.Body
		close(f.started)
		return okResult(struct {
			StatusCode int         `json:"status_code"`
			Headers    http.Header `json:"headers"`
			StreamID   string      `json:"stream_id"`
		}{response.StatusCode, response.Headers, "http-fixture"})
	case pluginabi.MethodHostHTTPStreamRead:
		if f.reads != nil {
			select {
			case read := <-f.reads:
				if read.panicValue != nil {
					panic(read.panicValue)
				}
				return okResult(read)
			case <-f.clientCtx.Done():
				return okResult(streamTestRead{Error: "client cancelled " + fixtureAPIKey, Done: true})
			}
		}
		if !f.readBody {
			f.readBody = true
			return okResult(streamTestRead{Payload: f.body})
		}
		return okResult(streamTestRead{Done: true})
	case pluginabi.MethodHostHTTPStreamClose:
		f.mu.Lock()
		f.upClosed++
		f.mu.Unlock()
		return okResult(struct{}{})
	case pluginabi.MethodHostStreamEmit:
		var req struct {
			StreamID string `json:"stream_id"`
			Payload  []byte `json:"payload"`
		}
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		if req.StreamID != "executor-fixture" {
			return nil, errors.New("wrong executor stream id")
		}
		select {
		case f.emitted <- pluginapi.ExecutorStreamChunk{Payload: req.Payload}:
			return okResult(struct{}{})
		case <-f.clientCtx.Done():
			return nil, errors.New("executor stream cancelled")
		}
	case pluginabi.MethodHostStreamClose:
		var req struct {
			StreamID    string           `json:"stream_id"`
			Error       string           `json:"error"`
			ErrorDetail *pluginabi.Error `json:"error_detail"`
		}
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.closeErr = req.ErrorDetail
		f.mu.Unlock()
		close(f.closed)
		return okResult(struct{}{})
	default:
		return f.fixtureHost.call(ctx, method, request)
	}
}

func streamPayload(t *testing.T) []byte {
	t.Helper()
	return executorRequestPayload(t, map[string]any{
		"model": "gpt-oss:120b", "payload": chatRequestBody(),
		"StorageJSON": storageJSON(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey}),
		"stream_id":   "executor-fixture", "host_callback_id": "callback-fixture",
	})
}

// collect checks the asynchronous wire contract before collecting output. It
// does NOT manufacture an async stream from a synchronous chunk list.
func (f *streamTestHost) collect(t *testing.T, response executorStreamResponse) []pluginapi.ExecutorStreamChunk {
	t.Helper()
	if len(response.Chunks) != 0 {
		t.Fatalf("executor returned %d synchronous chunks, want incremental host.stream.emit", len(response.Chunks))
	}
	var chunks []pluginapi.ExecutorStreamChunk
	for {
		select {
		case chunk := <-f.emitted:
			chunks = append(chunks, chunk)
		case <-f.closed:
			for {
				select {
				case chunk := <-f.emitted:
					chunks = append(chunks, chunk)
				default:
					return chunks
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatal("executor stream never closed")
		}
	}
}

func (f *streamTestHost) assertClosed(t *testing.T, wantError bool) {
	t.Helper()
	select {
	case <-f.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("executor stream never closed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if (f.closeErr != nil) != wantError {
		t.Errorf("stream close error = %+v, wantError=%v", f.closeErr, wantError)
	}
	if f.closeErr != nil && (strings.Contains(f.closeErr.Message, fixtureAPIKey) || f.closeErr.HTTPStatus != http.StatusBadGateway) {
		t.Errorf("stream error leaked credential or lost upstream status: %+v", f.closeErr)
	}
	if f.upClosed != 1 {
		t.Errorf("upstream stream closed %d times, want exactly once", f.upClosed)
	}
}

func (f *streamTestHost) respondSSE(body string) {
	f.setRespond(func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK,
			Headers: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte(body)}
	})
}

func streamResult(t *testing.T, raw []byte) executorStreamResponse {
	t.Helper()
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("executor.execute_stream failed: %+v", envelope.Error)
	}
	var response executorStreamResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode stream response: %v", err)
	}
	return response
}

func TestExecuteStreamIncrementalFragments(t *testing.T) {
	h, host := newStreamExecutorHandler(t)
	host.respondSSE("")
	host.reads = make(chan streamTestRead, 8)
	result := make(chan []byte, 1)
	payload := streamPayload(t)
	go func() { raw, _ := h.Handle(pluginabi.MethodExecutorExecuteStream, payload); result <- raw }()
	select {
	case <-host.started:
	case raw := <-result:
		t.Fatalf("stream returned before opening streaming HTTP: %s", raw)
	case <-time.After(3 * time.Second):
		t.Fatal("streaming HTTP not opened")
	}
	host.reads <- streamTestRead{Payload: []byte(": heartbeat\r\n\r\ndata: {\"n\":")}
	select {
	case raw := <-result:
		t.Fatalf("partial event returned as success: %s", raw)
	case <-time.After(20 * time.Millisecond):
	}
	host.reads <- streamTestRead{Payload: []byte("1}\r")}
	host.reads <- streamTestRead{Payload: []byte("\n\r\n")}
	select {
	case raw := <-result:
		response := streamResult(t, raw)
		if len(response.Chunks) != 0 {
			t.Fatal("stream did not select the asynchronous contract")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first complete event waited for upstream EOF")
	}
	select {
	case chunk := <-host.emitted:
		if string(chunk.Payload) != `{"n":1}` {
			t.Fatalf("first payload = %q", chunk.Payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first event not forwarded while upstream is still open")
	}
	// The second event spans byte fragments and uses multiple data fields.
	host.reads <- streamTestRead{Payload: []byte("data:{\"content\":\ndata: \"雪\"}\n\ndata: [DO")}
	host.reads <- streamTestRead{Payload: []byte("NE]\n\n")}
	chunks := host.collect(t, executorStreamResponse{})
	if len(chunks) != 1 || string(chunks[0].Payload) != `{"content":"雪"}` {
		t.Fatalf("remaining chunks = %q", chunks)
	}
	hostFramedEvents(t, append([]pluginapi.ExecutorStreamChunk{{Payload: []byte(`{"n":1}`)}}, chunks...))
	host.assertClosed(t, false)
}

func TestExecuteStreamLongBackpressure(t *testing.T) {
	h, host := newStreamExecutorHandler(t)
	var body strings.Builder
	for i := 0; i < 4096; i++ {
		fmt.Fprintf(&body, "data: {\"n\":%d}\n\n", i)
	}
	body.WriteString("data: [DONE]\n\n")
	host.respondSSE(body.String())
	raw, _ := h.Handle(pluginabi.MethodExecutorExecuteStream, streamPayload(t))
	chunks := host.collect(t, streamResult(t, raw))
	if len(chunks) != 4096 {
		t.Fatalf("got %d chunks, want 4096", len(chunks))
	}
	for i, chunk := range chunks {
		if string(chunk.Payload) != fmt.Sprintf(`{"n":%d}`, i) {
			t.Fatalf("chunk %d = %q", i, chunk.Payload)
		}
	}
	hostFramedEvents(t, chunks)
	host.assertClosed(t, false)
}

func TestExecuteStreamInterruptedAndCancelled(t *testing.T) {
	for _, mode := range []string{"upstream error", "client cancelled", "host panic"} {
		t.Run(mode, func(t *testing.T) {
			h, host := newStreamExecutorHandler(t)
			host.respondSSE("")
			host.reads = make(chan streamTestRead, 4)
			host.reads <- streamTestRead{Payload: []byte("data: {\"n\":1}\n\n")}
			raw, _ := h.Handle(pluginabi.MethodExecutorExecuteStream, streamPayload(t))
			response := streamResult(t, raw)
			if len(response.Chunks) != 0 {
				t.Fatal("synchronous chunk list")
			}
			select {
			case chunk := <-host.emitted:
				if string(chunk.Payload) != `{"n":1}` {
					t.Fatalf("first chunk = %q", chunk.Payload)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("first chunk never arrived")
			}
			if mode == "client cancelled" {
				host.cancel()
			} else if mode == "host panic" {
				host.reads <- streamTestRead{panicValue: fixtureAPIKey}
			} else {
				// Even a fragment accompanied by an error must never become a
				// successful partial event or a clean DONE.
				host.reads <- streamTestRead{Payload: []byte("data: {\"n\":"), Error: "broken upstream " + fixtureAPIKey, Done: true}
			}
			chunks := host.collect(t, response)
			if len(chunks) != 0 {
				t.Fatalf("truncated event was emitted: %q", chunks)
			}
			host.assertClosed(t, true)
			provider, _ := h.state.Provider()
			waitUntil(t, "stream goroutine and host callbacks to drain", func() bool { return provider.bridge().inflight.pending() == 0 })
		})
	}
}

func TestExecuteStreamRetainsTheDrainBarrier(t *testing.T) {
	h, host := newStreamExecutorHandler(t)
	host.respondSSE("")
	host.reads = make(chan streamTestRead, 4)
	host.reads <- streamTestRead{Payload: []byte("data: {\"n\":1}\n\n")}
	raw, _ := h.Handle(pluginabi.MethodExecutorExecuteStream, streamPayload(t))
	response := streamResult(t, raw)
	provider, _ := h.state.Provider()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if outcome := provider.bridge().waitInflight(ctx); outcome.drained || outcome.pending == 0 {
		t.Fatal("an unfinished async execution was not counted by the drain barrier")
	}
	host.reads <- streamTestRead{Payload: []byte("data: {\"n\":2}\n\ndata: [DONE]\n\n")}
	chunks := host.collect(t, response)
	if len(chunks) != 2 {
		t.Fatalf("drain attempt interrupted the execution: %q", chunks)
	}
	host.assertClosed(t, false)
	waitUntil(t, "the full stream lifetime to drain", func() bool { return provider.bridge().inflight.pending() == 0 })
}
