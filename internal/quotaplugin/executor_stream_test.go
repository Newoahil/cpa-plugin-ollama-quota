package quotaplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// sseFrames keeps the original T13 table on the production incremental parser.
// Only tests collect a whole answer; the executor itself never uses this helper.
func sseFrames(body []byte, eventStream bool) []pluginapi.ExecutorStreamChunk {
	reader := newEventReader(strings.NewReader(string(body)), eventStream)
	var chunks []pluginapi.ExecutorStreamChunk
	for {
		payload, err := reader.next()
		if err != nil {
			return chunks
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: payload})
	}
}

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

func (f *streamTestHost) assertClosed(t *testing.T, wantStatus int) {
	t.Helper()
	select {
	case <-f.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("executor stream never closed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if (f.closeErr != nil) != (wantStatus != 0) {
		t.Errorf("stream close error = %+v, wantStatus=%d", f.closeErr, wantStatus)
	}
	if f.closeErr != nil {
		wantCode := "upstream_unreachable"
		if wantStatus == http.StatusServiceUnavailable {
			wantCode = "host_unavailable"
		}
		if strings.Contains(f.closeErr.Message, fixtureAPIKey) || f.closeErr.HTTPStatus != wantStatus || f.closeErr.Code != wantCode {
			t.Errorf("stream error leaked credential or lost classification: %+v", f.closeErr)
		}
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
	host.assertClosed(t, 0)
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
	host.assertClosed(t, 0)
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
			wantStatus := http.StatusBadGateway
			if mode == "host panic" {
				wantStatus = http.StatusServiceUnavailable
			}
			host.assertClosed(t, wantStatus)
			provider, _ := h.state.Provider()
			waitUntil(t, "stream goroutine and host callbacks to drain", func() bool { return provider.bridge().inflight.pending() == 0 })
		})
	}
}

type byteFragmentReader struct {
	data []byte
	err  error
}

func (r *byteFragmentReader) Read(out []byte) (int, error) {
	if len(r.data) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	out[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestEventReaderByteFragmentsAndReadErrors(t *testing.T) {
	body := "event: message\r\ndata: {\"content\":\r\ndata: \"雪 data: [DONE]\"}\r\n\r\ndata: [DONE]\r\n\r\n"
	reader := newEventReader(&byteFragmentReader{data: []byte(body)}, true)
	payload, err := reader.next()
	if err != nil || string(payload) != `{"content":"雪 data: [DONE]"}` {
		t.Fatalf("split UTF-8/CRLF/multiline JSON changed: %q, %v", payload, err)
	}
	if _, err := reader.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("DONE = %v", err)
	}

	for _, body := range []string{"data: {\"n\":", "data: {\"n\":1}\n\n"} {
		upstreamErr := errors.New("fixture read failed")
		reader := newEventReader(&byteFragmentReader{data: []byte(body), err: upstreamErr}, true)
		payload, err := reader.next()
		if strings.HasSuffix(body, "\n\n") {
			if err != nil || string(payload) != `{"n":1}` {
				t.Fatalf("data before error lost: %q %v", payload, err)
			}
			_, err = reader.next()
		}
		if !errors.Is(err, upstreamErr) {
			t.Fatalf("read error lost: %v", err)
		}
	}
}

func TestExecuteStreamRejectsOversizeAndMalformedEvents(t *testing.T) {
	for _, body := range []string{"data: not JSON\n\n", "data: " + strings.Repeat(" ", maxUpstreamResponseBytes) + "{}\n\n"} {
		h, host := newStreamExecutorHandler(t)
		host.respondSSE(body)
		raw, _ := h.Handle(pluginabi.MethodExecutorExecuteStream, streamPayload(t))
		envelope := decodeEnvelope(t, raw)
		if envelope.OK || envelope.Error == nil || envelope.Error.Code != "upstream_invalid_response" || envelope.Error.HTTPStatus != http.StatusBadGateway {
			t.Fatalf("invalid upstream accepted: %s", raw)
		}
		host.mu.Lock()
		closed := host.upClosed
		host.mu.Unlock()
		if closed != 1 {
			t.Errorf("upstream not released: %d", closed)
		}
	}
}

func TestExecutorStreamFrozenCallbacksDoNotSpendThePumpEntry(t *testing.T) {
	bridge := newHostBridge(func(context.Context, string, []byte) ([]byte, error) { return okResult(struct{}{}) })
	if !bridge.inflight.begin() {
		t.Fatal("pump reservation refused")
	}
	idle := bridge.inflight.freeze()
	if err := bridge.invoke(context.Background(), pluginabi.MethodHostLog, struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	if pending := bridge.inflight.pending(); pending != 1 {
		t.Fatalf("frozen callback spent pump entry: %d", pending)
	}
	select {
	case <-idle:
		t.Fatal("idle signalled while pump is live")
	default:
	}
	bridge.inflight.end()
	select {
	case <-idle:
	default:
		t.Fatal("pump completion did not release barrier")
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
	host.assertClosed(t, 0)
	waitUntil(t, "the full stream lifetime to drain", func() bool { return provider.bridge().inflight.pending() == 0 })
}
