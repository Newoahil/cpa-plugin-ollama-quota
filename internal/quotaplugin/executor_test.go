package quotaplugin

import (
	"context"
	"encoding/base64"
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

// The executor tests use the same fixture host as the quota tests: no network,
// no real credential. Every credential value here is a fixture string, and the
// assertions check that none of them reaches a response — that is the unit-level
// half of SPEC 4.2. The end-to-end half is a live CPA and is not reproducible in
// a unit test.

const fixtureAPIKey = "fixture-executor-key"

// requestRecorder captures the upstream requests a test caused, so assertions
// do not have to look at the fixture's shared slice (the collector's own sweep
// writes there too).
type requestRecorder struct {
	mu       sync.Mutex
	requests []pluginapi.HTTPRequest
}

func (r *requestRecorder) record(request pluginapi.HTTPRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
}

func (r *requestRecorder) snapshot() []pluginapi.HTTPRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]pluginapi.HTTPRequest(nil), r.requests...)
}

// find returns the first recorded request whose URL contains the fragment.
func (r *requestRecorder) find(fragment string) (pluginapi.HTTPRequest, bool) {
	for _, request := range r.snapshot() {
		if strings.Contains(request.URL, fragment) {
			return request, true
		}
	}
	return pluginapi.HTTPRequest{}, false
}

func newExecutorHandler(t *testing.T, storage map[string]any) (*Handler, *fixtureHost) {
	t.Helper()
	host := newFixtureHost()
	host.setCredential("cred-1", storage)
	h := newHandlerWithHost(t, host)
	if _, errRegister := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { h.state.Shutdown() })
	return h, host
}

// executorRequestPayload builds what the host sends to executor.execute: the SDK
// request plus the host's callback scope.
func executorRequestPayload(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(fields)
	if errMarshal != nil {
		t.Fatalf("marshal executor request: %v", errMarshal)
	}
	return raw
}

// chatRequestBody returns the payload a chat-completions call would carry.
func chatRequestBody() []byte {
	return []byte(`{"model":"stale-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
}

// storageJSON encodes a credential the way the host does: StorageJSON is a
// []byte on the wire, so it travels as a base64 string, not a JSON object.
func storageJSON(t *testing.T, storage map[string]any) string {
	t.Helper()
	raw, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		t.Fatalf("marshal storage: %v", errMarshal)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestRegisterDeclaresExecutorAndModelProvider(t *testing.T) {
	raw, err := newHandlerForTest(t).Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("register failed: %+v", envelope.Error)
	}
	var reg Registration
	if err := json.Unmarshal(envelope.Result, &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if !reg.Capabilities.QuotaProvider {
		t.Error("quota_provider capability not declared")
	}
	if !reg.Capabilities.Executor {
		t.Error("executor capability not declared")
	}
	if !reg.Capabilities.ModelProvider {
		t.Error("model_provider capability not declared")
	}
	if reg.Capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeBoth {
		t.Errorf("executor_model_scope = %q, want both", reg.Capabilities.ExecutorModelScope)
	}
	// The host rejects an executor that declares no format, so both lists must
	// be present and name the protocol this executor actually speaks.
	for _, formats := range [][]string{reg.Capabilities.ExecutorInputFormats, reg.Capabilities.ExecutorOutputFormats} {
		if len(formats) != 1 || formats[0] != formatChatCompletions {
			t.Errorf("executor formats = %v, want [%s]", formats, formatChatCompletions)
		}
	}
}

func TestExecutorIdentifierIsTheOllamaProviderKey(t *testing.T) {
	raw, err := newHandlerForTest(t).Handle(pluginabi.MethodExecutorIdentifier, nil)
	if err != nil {
		t.Fatalf("executor.identifier: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("executor.identifier failed: %+v", envelope.Error)
	}
	var identifier IdentifierResponse
	if err := json.Unmarshal(envelope.Result, &identifier); err != nil {
		t.Fatalf("decode identifier: %v", err)
	}
	if identifier.Identifier != QuotaIdentifier {
		t.Errorf("identifier = %q, want %q", identifier.Identifier, QuotaIdentifier)
	}
}

func TestModelStaticContributesNoModels(t *testing.T) {
	h, host := newExecutorHandler(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey})
	recorder := &requestRecorder{}
	host.setRespond(func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		recorder.record(request)
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK}
	})

	raw, err := h.Handle(pluginabi.MethodModelStatic, nil)
	if err != nil {
		t.Fatalf("model.static: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("model.static failed: %+v", envelope.Error)
	}
	var response pluginapi.ModelResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode model response: %v", err)
	}
	if response.Provider != QuotaIdentifier {
		t.Errorf("provider = %q, want %q", response.Provider, QuotaIdentifier)
	}
	// A static catalog is exactly what this design refuses to hardcode: the
	// catalog is per account and must come from model.for_auth.
	if len(response.Models) != 0 {
		t.Errorf("model.static returned %d models, want none", len(response.Models))
	}
	if _, found := recorder.find("/api/tags"); found {
		t.Error("model.static reached the discovery endpoint, want no upstream call")
	}
	if _, found := recorder.find("/v1/"); found {
		t.Error("model.static reached the chat endpoint, want no upstream call")
	}
}

func TestExecuteForwardsToOllamaCloudWithTheStoredKey(t *testing.T) {
	h, host := newExecutorHandler(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey})
	recorder := &requestRecorder{}
	host.setRespond(func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		recorder.record(request)
		return pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`),
		}
	})

	raw, err := h.Handle(pluginabi.MethodExecutorExecute, executorRequestPayload(t, map[string]any{
		"model":          "gpt-oss:120b",
		"format":         formatChatCompletions,
		"payload":        chatRequestBody(),
		"StorageJSON":    storageJSON(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey}),
		"HostCallbackID": "cb-exec-1",
	}))
	if err != nil {
		t.Fatalf("executor.execute: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("executor.execute failed: %+v", envelope.Error)
	}
	var response pluginapi.ExecutorResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode executor response: %v", err)
	}
	if !strings.Contains(string(response.Payload), `"assistant"`) {
		t.Errorf("payload = %s", response.Payload)
	}

	request, found := recorder.find("/v1/chat/completions")
	if !found {
		t.Fatalf("no request reached the chat endpoint: %+v", recorder.snapshot())
	}
	if request.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", request.Method)
	}
	if !strings.HasPrefix(request.URL, ollamaOrigin) {
		t.Errorf("url = %q, want the pinned origin", request.URL)
	}
	if got := request.Headers.Get("Authorization"); got != "Bearer "+fixtureAPIKey {
		// The value itself is a fixture; what matters is that it is the stored
		// key and that it is only in the outbound header.
		t.Errorf("authorization header = %q, want the stored key", got)
	}
	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	if body["model"] != "gpt-oss:120b" {
		t.Errorf("forwarded model = %v, want the routed model", body["model"])
	}
	// The request arrived with stream:true but this is the buffered method.
	if body["stream"] != false {
		t.Errorf("forwarded stream = %v, want false", body["stream"])
	}
	if body["messages"] == nil {
		t.Error("forwarded body dropped the messages")
	}

	if strings.Contains(string(raw), fixtureAPIKey) {
		t.Error("the response envelope contains the credential")
	}
}

func TestExecuteRefusesACookieOnlyCredential(t *testing.T) {
	h, host := newExecutorHandler(t, map[string]any{"type": "ollama", "cookie": "fixture-cookie"})
	recorder := &requestRecorder{}
	host.setRespond(func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		recorder.record(request)
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK}
	})

	raw, err := h.Handle(pluginabi.MethodExecutorExecute, executorRequestPayload(t, map[string]any{
		"model":       "gpt-oss:120b",
		"payload":     chatRequestBody(),
		"StorageJSON": storageJSON(t, map[string]any{"type": "ollama", "cookie": "fixture-cookie"}),
	}))
	if err != nil {
		t.Fatalf("executor.execute: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("a cookie-only credential must not be used for inference")
	}
	if envelope.Error.Code != "credential_incomplete" {
		t.Errorf("code = %q, want credential_incomplete", envelope.Error.Code)
	}
	if envelope.Error.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("http_status = %d, want 401", envelope.Error.HTTPStatus)
	}
	if _, found := recorder.find("/v1/chat/completions"); found {
		t.Error("a refused credential still reached upstream")
	}
	if strings.Contains(string(raw), "fixture-cookie") {
		t.Error("the response envelope contains the cookie")
	}
}

func TestExecuteMapsAnUpstreamRejection(t *testing.T) {
	h, host := newExecutorHandler(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey})
	host.setRespond(func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		return pluginapi.HTTPResponse{
			StatusCode: http.StatusUnauthorized,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"error":"invalid api key"}`),
		}
	})

	raw, err := h.Handle(pluginabi.MethodExecutorExecute, executorRequestPayload(t, map[string]any{
		"model":       "gpt-oss:120b",
		"payload":     chatRequestBody(),
		"StorageJSON": storageJSON(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey}),
	}))
	if err != nil {
		t.Fatalf("executor.execute: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("a 401 from upstream must not look like a successful call")
	}
	if envelope.Error.Code != "auth_failed" {
		t.Errorf("code = %q, want auth_failed", envelope.Error.Code)
	}
	if envelope.Error.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("http_status = %d, want 401 (the client must see a credential problem)", envelope.Error.HTTPStatus)
	}
	if strings.Contains(envelope.Error.Message, fixtureAPIKey) {
		t.Error("the error message contains the credential")
	}
}

func TestExecuteStreamReturnsDataForHostFraming(t *testing.T) {
	h, host := newStreamExecutorHandler(t)
	recorder := &requestRecorder{}
	host.setRespond(func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		recorder.record(request)
		return pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       []byte("data: {\"n\":1}\n\ndata: {\"n\":2}\n\ndata: [DONE]\n\n"),
		}
	})

	raw, err := h.Handle(pluginabi.MethodExecutorExecuteStream, streamPayload(t))
	if err != nil {
		t.Fatalf("executor.execute_stream: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("executor.execute_stream failed: %+v", envelope.Error)
	}
	var response executorStreamResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode stream response: %v", err)
	}
	if response.Headers.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content type = %q", response.Headers.Get("Content-Type"))
	}
	response.Chunks = host.collect(t, response)
	want := []string{`{"n":1}`, `{"n":2}`}
	if len(response.Chunks) != len(want) {
		t.Fatalf("got %d chunks, want %d: %q", len(response.Chunks), len(want), response.Chunks)
	}
	for index, expected := range want {
		if string(response.Chunks[index].Payload) != expected {
			t.Errorf("chunk %d = %q, want %q", index, response.Chunks[index].Payload, expected)
		}
	}

	request, found := recorder.find("/v1/chat/completions")
	if !found {
		t.Fatal("no request reached the chat endpoint")
	}
	if request.Headers.Get("Accept") != "text/event-stream" {
		t.Errorf("accept = %q, want text/event-stream", request.Headers.Get("Accept"))
	}
	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	if body["stream"] != true {
		t.Errorf("forwarded stream = %v, want true", body["stream"])
	}
}

func TestExecuteStreamWrapsANonStreamingAnswer(t *testing.T) {
	h, host := newStreamExecutorHandler(t)
	host.setRespond(func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		return pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"choices":[]}`),
		}
	})

	raw, err := h.Handle(pluginabi.MethodExecutorExecuteStream, streamPayload(t))
	if err != nil {
		t.Fatalf("executor.execute_stream: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("executor.execute_stream failed: %+v", envelope.Error)
	}
	var response executorStreamResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode stream response: %v", err)
	}
	response.Chunks = host.collect(t, response)
	// The host frames this JSON and writes DONE when the channel closes; the
	// plugin must supply neither the data: prefix nor its own sentinel.
	if len(response.Chunks) != 1 {
		t.Fatalf("got %d chunks, want 1: %q", len(response.Chunks), response.Chunks)
	}
	if string(response.Chunks[0].Payload) != `{"choices":[]}` {
		t.Errorf("chunk 0 = %q", response.Chunks[0].Payload)
	}
	hostFramedEvents(t, response.Chunks)
}

// hostFramedEvents mirrors the public OpenAI handler's transport boundary:
// WriteChunk adds data: and WriteDone adds the sole sentinel. Parse only one
// layer of SSE fields, exactly as a standard client does, not a second prefix
// to work around the T10 defect.
func hostFramedEvents(t *testing.T, chunks []pluginapi.ExecutorStreamChunk) []json.RawMessage {
	t.Helper()
	var wire strings.Builder
	for _, chunk := range chunks {
		_, _ = fmt.Fprintf(&wire, "data: %s\n\n", chunk.Payload)
	}
	wire.WriteString("data: [DONE]\n\n")

	var events []json.RawMessage
	done := 0
	for _, frame := range strings.Split(wire.String(), "\n\n") {
		var data []string
		for _, line := range strings.Split(frame, "\n") {
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(value, " "))
			}
		}
		if len(data) == 0 {
			continue
		}
		payload := strings.Join(data, "\n")
		if payload == "[DONE]" {
			done++
			continue
		}
		if done > 0 {
			t.Fatal("host-framed stream contains data after DONE")
		}
		if !json.Valid([]byte(payload)) {
			t.Fatalf("host-framed event is not JSON after removing one SSE prefix: %q", payload)
		}
		events = append(events, json.RawMessage(payload))
	}
	if done != 1 {
		t.Errorf("host-framed stream has %d DONE events, want exactly 1", done)
	}
	return events
}

func TestSSEFramesReturnsOnlyEventData(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		eventStream bool
		want        []string
	}{
		{"upstream DONE", "data: {\"n\":1}\n\ndata: {\"n\":2}\n\ndata: [DONE]\n\n", true, []string{`{"n":1}`, `{"n":2}`}},
		{"no upstream DONE", "data: {\"n\":1}\n\n", true, []string{`{"n":1}`}},
		{"duplicate upstream DONE", "data: {\"n\":1}\n\ndata: [DONE]\n\ndata:[DONE]\n\n", true, []string{`{"n":1}`}},
		{"CRLF and metadata", ": heartbeat\r\n\r\nevent: message\r\nid: 1\r\nretry: 1000\r\ndata:{\"n\":1}\r\n\r\ndata: [DONE]\r\n\r\n", true, []string{`{"n":1}`}},
		{"multiple data lines", "data: {\"n\":\ndata: 1}\n\n", true, []string{`{"n":1}`}},
		{"last frame without separator", "data: {\"n\":1}", true, []string{`{"n":1}`}},
		{"literal prefix and sentinel in content", "data: {\"content\":\"data: [DONE] 雪\"}\n\n", true, []string{`{"content":"data: [DONE] 雪"}`}},
		{"empty and non-data events", ": heartbeat\n\nevent: ping\nid: 1\n\ndata:\n\ndata\n\n", true, nil},
		{"only DONE", "data: [DONE]\n\n", true, nil},
		{"JSON fallback", `{"choices":[]}`, false, []string{`{"choices":[]}`}},
		{"multiline JSON fallback", " {\n  \"choices\": []\n}\n", false, []string{`{"choices":[]}`}},
		{"invalid JSON fallback", "not JSON", false, nil},
		{"empty JSON fallback", "", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := sseFrames([]byte(tc.body), tc.eventStream)
			// Check the client-facing boundary as well as the raw SDK payload;
			// complete SSE frames used to pass the old unit tests but fail here.
			events := hostFramedEvents(t, chunks)
			if len(chunks) != len(tc.want) || len(events) != len(tc.want) {
				t.Fatalf("got %d chunks / %d events, want %d", len(chunks), len(events), len(tc.want))
			}
			for i, want := range tc.want {
				if string(chunks[i].Payload) != want || string(events[i]) != want {
					t.Errorf("event %d: chunk=%q, client=%q, want %q", i, chunks[i].Payload, events[i], want)
				}
			}
		})
	}
}

func TestExecuteStreamRejectsEmptyData(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
	}{
		{"empty event stream", "text/event-stream", ""},
		{"empty JSON response", "application/json", ""},
		{"comments only", "text/event-stream", ": heartbeat\n\n"},
		{"DONE only", "text/event-stream", "data: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, host := newStreamExecutorHandler(t)
			host.setRespond(func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
				return pluginapi.HTTPResponse{
					StatusCode: http.StatusOK,
					Headers:    http.Header{"Content-Type": []string{tc.contentType}},
					Body:       []byte(tc.body),
				}
			})
			raw, err := h.Handle(pluginabi.MethodExecutorExecuteStream, streamPayload(t))
			if err != nil {
				t.Fatalf("executor.execute_stream: %v", err)
			}
			envelope := decodeEnvelope(t, raw)
			if envelope.OK || envelope.Error == nil {
				t.Fatalf("empty upstream data returned a successful chunk list; want upstream_invalid_response / 502: %s", raw)
			}
			if envelope.Error.Code != "upstream_invalid_response" || envelope.Error.HTTPStatus != http.StatusBadGateway {
				t.Errorf("error = %+v, want upstream_invalid_response / 502", envelope.Error)
			}
			if envelope.Error.Message != "the Ollama upstream sent a response this plugin could not read" {
				t.Errorf("message = %q, want the existing parse-failure message", envelope.Error.Message)
			}
			if strings.Contains(string(raw), fixtureAPIKey) {
				t.Error("the error response contains the credential")
			}
		})
	}
}

func TestExecuteStreamHostFramingPreservesContent(t *testing.T) {
	for _, tokens := range []int{3, 40} {
		t.Run(fmt.Sprintf("%d tokens", tokens), func(t *testing.T) {
			h, host := newStreamExecutorHandler(t)
			var upstream, want strings.Builder
			for i := 1; i <= tokens; i++ {
				token := fmt.Sprintf("T%02d ", i)
				want.WriteString(token)
				_, _ = fmt.Fprintf(&upstream, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", token)
			}
			upstream.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = fmt.Fprintf(&upstream, "data: {\"choices\":[],\"usage\":{\"completion_tokens\":%d}}\n\ndata: [DONE]\n\n", tokens)
			host.setRespond(func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
				return pluginapi.HTTPResponse{
					StatusCode: http.StatusOK,
					Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       []byte(upstream.String()),
				}
			})
			raw, err := h.Handle(pluginabi.MethodExecutorExecuteStream, streamPayload(t))
			if err != nil {
				t.Fatalf("executor.execute_stream: %v", err)
			}
			envelope := decodeEnvelope(t, raw)
			if !envelope.OK {
				t.Fatalf("executor.execute_stream failed: %+v", envelope.Error)
			}
			var response executorStreamResponse
			if err := json.Unmarshal(envelope.Result, &response); err != nil {
				t.Fatalf("decode stream response: %v", err)
			}
			response.Chunks = host.collect(t, response)
			events := hostFramedEvents(t, response.Chunks)
			if len(events) != tokens+2 {
				t.Fatalf("got %d data events, want %d tokens + stop + usage", len(events), tokens)
			}
			var content strings.Builder
			for i, event := range events {
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
						FinishReason *string `json:"finish_reason"`
					} `json:"choices"`
					Usage *struct {
						CompletionTokens int `json:"completion_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(event, &chunk); err != nil {
					t.Fatalf("decode event %d: %v", i, err)
				}
				if i <= tokens {
					if len(chunk.Choices) != 1 {
						t.Fatalf("event %d has %d choices, want 1", i, len(chunk.Choices))
					}
					content.WriteString(chunk.Choices[0].Delta.Content)
					if i < tokens && (chunk.Choices[0].Delta.Content != fmt.Sprintf("T%02d ", i+1) || chunk.Choices[0].FinishReason != nil) {
						t.Errorf("token event %d is out of order or has a premature finish", i)
					}
					if i == tokens && (chunk.Choices[0].FinishReason == nil || *chunk.Choices[0].FinishReason != "stop") {
						t.Error("stop event missing or out of order")
					}
				} else if len(chunk.Choices) != 0 || chunk.Usage == nil || chunk.Usage.CompletionTokens != tokens {
					t.Error("final usage missing, wrong or out of order")
				}
			}
			if content.String() != want.String() {
				t.Errorf("client content = %q, want every token once in order: %q", content.String(), want.String())
			}
			if strings.Contains(string(raw), fixtureAPIKey) {
				t.Error("the stream response envelope contains the credential")
			}
		})
	}
}

func TestCountTokensIsRefusedWithNotImplemented(t *testing.T) {
	h, _ := newExecutorHandler(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey})
	raw, err := h.Handle(pluginabi.MethodExecutorCountTokens, nil)
	if err != nil {
		t.Fatalf("executor.count_tokens: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("token counting must be refused rather than guessed (SPEC 4.3)")
	}
	if envelope.Error.HTTPStatus != http.StatusNotImplemented {
		t.Errorf("http_status = %d, want 501", envelope.Error.HTTPStatus)
	}
}

func TestHTTPRequestRefusesAForeignOrigin(t *testing.T) {
	h, host := newExecutorHandler(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey})
	recorder := &requestRecorder{}
	host.setRespond(func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		recorder.record(request)
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK}
	})

	for _, target := range []string{
		"https://evil.example.com/v1/chat/completions",
		"http://ollama.com/v1/chat/completions",
		"https://ollama.com.evil.example.com/v1/chat/completions",
	} {
		raw, err := h.Handle(pluginabi.MethodExecutorHTTPRequest, executorRequestPayload(t, map[string]any{
			"method":      "POST",
			"url":         target,
			"StorageJSON": storageJSON(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey}),
		}))
		if err != nil {
			t.Fatalf("executor.http_request: %v", err)
		}
		envelope := decodeEnvelope(t, raw)
		if envelope.OK {
			t.Fatalf("%q was forwarded; the credential must never leave ollama.com", target)
		}
		if envelope.Error.HTTPStatus != http.StatusBadRequest {
			t.Errorf("%q http_status = %d, want 400", target, envelope.Error.HTTPStatus)
		}
	}
	for _, request := range recorder.snapshot() {
		if !strings.HasPrefix(request.URL, ollamaOrigin+"/") {
			t.Errorf("a refused URL still reached upstream: %q", request.URL)
		}
	}
}

func TestHTTPRequestForwardsToOllamaCloud(t *testing.T) {
	h, host := newExecutorHandler(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey})
	recorder := &requestRecorder{}
	host.setRespond(func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		recorder.record(request)
		return pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"ok":true}`),
		}
	})

	raw, err := h.Handle(pluginabi.MethodExecutorHTTPRequest, executorRequestPayload(t, map[string]any{
		"method":      "GET",
		"url":         ollamaOrigin + "/api/tags",
		"headers":     map[string][]string{"Authorization": {"Bearer caller-supplied"}, "Cookie": {"session=1"}},
		"StorageJSON": storageJSON(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey}),
	}))
	if err != nil {
		t.Fatalf("executor.http_request: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("executor.http_request failed: %+v", envelope.Error)
	}
	request, found := recorder.find("/api/tags")
	if !found {
		t.Fatal("no request reached the tags endpoint")
	}
	if got := request.Headers.Get("Authorization"); got != "Bearer "+fixtureAPIKey {
		t.Error("the stored key must replace any caller-supplied authorization")
	}
	if got := request.Headers.Get("Cookie"); got != "" {
		t.Errorf("a caller cookie was forwarded: %q", got)
	}
}

func TestModelForAuthDiscoversPerCredentialModels(t *testing.T) {
	h, host := newExecutorHandler(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey})
	recorder := &requestRecorder{}
	host.setRespond(func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		recorder.record(request)
		return pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"models":[{"name":"qwen3:480b"},{"name":"gpt-oss:120b"},{"name":"qwen3:480b"}]}`),
		}
	})

	raw, err := h.Handle(pluginabi.MethodModelForAuth, executorRequestPayload(t, map[string]any{
		"AuthID":       "cred-1",
		"AuthProvider": QuotaIdentifier,
		"StorageJSON":  storageJSON(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey}),
		"metadata":     map[string]any{"type": "ollama"},
	}))
	if err != nil {
		t.Fatalf("model.for_auth: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("model.for_auth failed: %+v", envelope.Error)
	}
	var response pluginapi.ModelResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode model response: %v", err)
	}
	if response.Provider != QuotaIdentifier {
		t.Errorf("provider = %q", response.Provider)
	}
	got := make([]string, 0, len(response.Models))
	for _, model := range response.Models {
		got = append(got, model.ID)
	}
	want := []string{"gpt-oss:120b", "qwen3:480b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("models = %v, want %v", got, want)
	}

	request, found := recorder.find("/api/tags")
	if !found {
		t.Fatal("discovery did not reach /api/tags")
	}
	if request.Headers.Get("Authorization") != "Bearer "+fixtureAPIKey {
		t.Error("discovery did not use the stored key")
	}
	if strings.Contains(string(raw), fixtureAPIKey) {
		t.Error("the model response contains the credential")
	}
}

func TestModelForAuthSkipsACookieOnlyCredential(t *testing.T) {
	h, host := newExecutorHandler(t, map[string]any{"type": "ollama", "cookie": "fixture-cookie"})
	recorder := &requestRecorder{}
	host.setRespond(func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		recorder.record(request)
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK}
	})

	raw, err := h.Handle(pluginabi.MethodModelForAuth, executorRequestPayload(t, map[string]any{
		"AuthID":       "cred-1",
		"AuthProvider": QuotaIdentifier,
		"StorageJSON":  storageJSON(t, map[string]any{"type": "ollama", "cookie": "fixture-cookie"}),
	}))
	if err != nil {
		t.Fatalf("model.for_auth: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		// A cookie-only credential is a complete quota credential; failing here
		// would report a config problem the operator does not have.
		t.Fatalf("a cookie-only credential must not fail model discovery: %+v", envelope.Error)
	}
	var response pluginapi.ModelResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode model response: %v", err)
	}
	if len(response.Models) != 0 {
		t.Errorf("models = %v, want none", response.Models)
	}
	if _, found := recorder.find("/api/tags"); found {
		t.Error("a cookie-only credential still reached upstream")
	}
}

// The executor must refuse NEW work once the plugin is quiescing: the host has
// already stopped routing to it, and a new upstream call would be work nobody
// is waiting for. In-flight calls are the opposite case and are covered by the
// drain tests: they must finish (SPEC 4.1).
func TestExecutorRefusesNewWorkWhileQuiescing(t *testing.T) {
	h, host := newExecutorHandler(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey})
	recorder := &requestRecorder{}
	host.setRespond(func(request pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		recorder.record(request)
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK}
	})
	if _, errQuiesce := h.Handle(pluginabi.MethodPluginQuiesce, nil); errQuiesce != nil {
		t.Fatalf("quiesce: %v", errQuiesce)
	}

	raw, err := h.Handle(pluginabi.MethodExecutorExecute, executorRequestPayload(t, map[string]any{
		"model":       "gpt-oss:120b",
		"payload":     chatRequestBody(),
		"StorageJSON": storageJSON(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey}),
	}))
	if err != nil {
		t.Fatalf("executor.execute: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("a quiescing plugin must not start new inference")
	}
	if envelope.Error.Code != "plugin_quiesced" {
		t.Errorf("code = %q, want plugin_quiesced", envelope.Error.Code)
	}
	if _, found := recorder.find("/v1/chat/completions"); found {
		t.Error("a quiescing plugin still reached upstream")
	}
}

func TestChatPayloadKeepsUnrelatedFields(t *testing.T) {
	payload, err := chatPayload("gpt-oss:120b", chatRequestBody(), nil, true)
	if err != nil {
		t.Fatalf("chatPayload: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fields["model"] != "gpt-oss:120b" || fields["stream"] != true {
		t.Errorf("fields = %v", fields)
	}
	if _, ok := fields["messages"]; !ok {
		t.Error("chatPayload dropped the messages")
	}
	if _, errRefuse := chatPayload("m", []byte("not json"), nil, false); errRefuse == nil {
		t.Error("a non-JSON payload must be refused")
	}
}

// An execution is not tracked by Provider.Track (SPEC 4.1: it must survive a
// hot swap), so the only thing that makes it visible to quiesce is the host
// bridge's in-flight set. This pins that: with one execution parked inside
// host.http.do, the barrier must report work outstanding rather than an empty
// set it never waited for — and it must drain once the call returns.
func TestInFlightExecutionHoldsTheQuiesceBarrier(t *testing.T) {
	// This handler's collector fails at host.auth.list, so the only host calls in
	// this test are the executor's own. A blocked collector would otherwise make
	// the bridge's in-flight count ambiguous.
	host := newFixtureHost()
	host.authListErr = errors.New("fixture: this test does not run a collector")
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": fixtureAPIKey})
	h := newHandlerWithHost(t, host)
	if _, errRegister := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { h.state.Shutdown() })
	host.setBlock()
	t.Cleanup(host.setBlockCleared)

	provider, ok := h.State().Provider()
	if !ok {
		t.Fatal("the registered handler must have a provider")
	}
	bridge := provider.bridge()

	done := make(chan []byte, 1)
	go func() {
		raw, errCall := h.Handle(pluginabi.MethodExecutorExecute, executorRequestPayload(t, map[string]any{
			"Model":          "gpt-oss:120b",
			"Payload":        chatRequestBody(),
			"StorageJSON":    storageJSON(t, map[string]any{"type": "ollama", "api_key": fixtureAPIKey}),
			"HostCallbackID": "cb-inflight-1",
		}))
		if errCall != nil {
			done <- nil
			return
		}
		done <- raw
	}()

	// Wait for the executor's own host.http.do, not just any in-flight callback.
	// The collector also makes one host call (its deliberately failing
	// auth.list), and treating that short-lived call as the execution would let
	// the barrier run during the gap before the executor reaches the host.
	waitUntil(t, "the execution to reach the host", func() bool {
		return host.requestCount() > 0 && bridge.inflight.pending() > 0
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if outcome := bridge.waitInflight(ctx); outcome.drained {
		t.Fatal("the barrier reported a live execution as drained")
	} else if outcome.pending == 0 {
		t.Error("the barrier reported no outstanding work")
	}

	host.setBlockCleared()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the execution never returned after the host released it")
	}
	waitUntil(t, "the in-flight set to drain", func() bool { return bridge.inflight.pending() == 0 })

	ctxDrain, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	if outcome := bridge.waitInflight(ctxDrain); !outcome.drained {
		t.Error("the barrier must drain once the execution has returned")
	}
}

// waitUntil polls condition until it holds, and fails the test at the deadline.
// Polling rather than sleeping a fixed time keeps the test fast on a quiet
// machine and still reliable on a loaded one.
func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
