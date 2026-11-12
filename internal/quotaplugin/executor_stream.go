package quotaplugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// HTTPClient does not cross the C ABI. These are the wire shapes of the public
// host.http.do_stream / stream_read callbacks, not a second HTTP client.
type hostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id"`
}

type hostHTTPStreamRead struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// hostHTTPStream reads arbitrary transport fragments lazily. Its operation and
// callback scope remain live until Close, including after execute_stream returns.
// The host binds that scope to the original client context, so a disconnect also
// interrupts a blocked stream_read. The cancel guard additionally propagates a
// plugin-side context cancellation across the native ABI.
type hostHTTPStream struct {
	bridge      *hostBridge
	ctx         context.Context
	streamID    string
	callbackID  string
	operationID string
	guard       *cancelGuard
	closeOnce   sync.Once
	pending     []byte
	terminal    error
	total       int
}

func (t upstreamTarget) openStream(ctx context.Context, credential ollama.Credential, body []byte, callbackID string) (*hostHTTPStream, hostHTTPStreamResponse, error) {
	operationID, err := t.bridge.openOperation(ctx, callbackID)
	if err != nil {
		return nil, hostHTTPStreamResponse{}, err
	}
	stream := &hostHTTPStream{bridge: t.bridge, ctx: ctx, callbackID: callbackID, operationID: operationID}
	stream.guard = t.bridge.newCancelGuard(ctx, callbackID, operationID)
	request := hostHTTPRequest{
		Method: http.MethodPost, URL: chatCompletionsURL, Body: body,
		Headers: httpHeaders{
			"Content-Type": {"application/json"}, "Accept": {"text/event-stream"},
			"Authorization": {"Bearer " + credential.APIKey},
		},
		HostCallbackID: callbackID, OperationID: operationID,
	}
	var response hostHTTPStreamResponse
	if err := t.bridge.invoke(ctx, pluginabi.MethodHostHTTPDoStream, request, &response); err != nil {
		stream.Close()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, response, err
	}
	stream.streamID = strings.TrimSpace(response.StreamID)
	if stream.streamID == "" {
		stream.Close()
		return nil, response, ollama.ErrParse
	}
	return stream, response, nil
}

func (s *hostHTTPStream) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	for emptyReads := 0; len(s.pending) == 0 && s.terminal == nil; emptyReads++ {
		if emptyReads == 100 {
			s.terminal = io.ErrNoProgress
			break
		}
		if err := s.ctx.Err(); err != nil {
			s.terminal = err
			break
		}
		var read hostHTTPStreamRead
		if err := s.bridge.invoke(s.ctx, pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": s.streamID}, &read); err != nil {
			s.terminal = err
			if s.ctx.Err() != nil {
				s.terminal = s.ctx.Err()
			}
			break
		}
		s.total += len(read.Payload)
		if s.total > maxUpstreamResponseBytes {
			s.terminal = ollama.ErrParse
			break
		}
		s.pending = read.Payload
		if read.Error != "" {
			// Never echo a host/upstream error string: it can contain headers.
			s.terminal = ollama.ErrNetwork
		} else if read.Done {
			s.terminal = io.EOF
		}
	}
	if len(s.pending) > 0 {
		n := copy(buffer, s.pending)
		s.pending = s.pending[n:]
		return n, nil // deliver bytes before a simultaneous error/EOF
	}
	return 0, s.terminal
}

func (s *hostHTTPStream) Close() error {
	s.closeOnce.Do(func() {
		defer s.guard.release()
		defer s.bridge.closeOperation(context.WithoutCancel(s.ctx), s.callbackID, s.operationID)
		if s.streamID != "" {
			_ = s.bridge.invoke(context.WithoutCancel(s.ctx), pluginabi.MethodHostHTTPStreamClose,
				map[string]string{"stream_id": s.streamID}, nil)
		}
	})
	return nil
}

// eventReader keeps only the current SSE event, never the full answer. A host
// read may contain half a UTF-8 code point or many events; only complete data
// events cross the executor boundary. The old total 32 MiB response cap is also
// retained by hostHTTPStream, including comments and metadata.
type eventReader struct {
	reader      *bufio.Reader
	eventStream bool
	ended       bool
}

func newEventReader(reader io.Reader, eventStream bool) *eventReader {
	return &eventReader{reader: bufio.NewReader(reader), eventStream: eventStream}
}

func compactEvent(data []byte) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return nil, ollama.ErrParse
	}
	return compact.Bytes(), nil
}

func (r *eventReader) next() ([]byte, error) {
	if r.ended {
		return nil, io.EOF
	}
	if !r.eventStream {
		r.ended = true
		raw, err := io.ReadAll(io.LimitReader(r.reader, maxUpstreamResponseBytes+1))
		if err != nil {
			return nil, err
		}
		if len(raw) > maxUpstreamResponseBytes {
			return nil, ollama.ErrParse
		}
		return compactEvent(raw)
	}

	var data []byte
	for {
		line, err := r.reader.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err // an interrupted partial frame is not an event
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if field, value, _ := bytes.Cut(line, []byte(":")); bytes.Equal(field, []byte("data")) {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(value, []byte(" "))...)
			if len(data) > maxUpstreamResponseBytes {
				return nil, ollama.ErrParse
			}
		}
		if errors.Is(err, io.EOF) {
			r.ended = true
		}
		if len(bytes.TrimSpace(line)) == 0 || r.ended {
			trimmed := bytes.TrimSpace(data)
			if bytes.Equal(trimmed, []byte("[DONE]")) {
				r.ended = true
				return nil, io.EOF // only the host writes the downstream sentinel
			}
			if len(trimmed) > 0 {
				return compactEvent(data)
			}
			data = data[:0]
		}
		if r.ended {
			return nil, io.EOF
		}
	}
}

type executorStreamEmit struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload"`
}

type executorStreamClose struct {
	StreamID    string           `json:"stream_id"`
	ErrorDetail *pluginabi.Error `json:"error_detail,omitempty"`
}

func (h *Handler) startExecutorStream(target upstreamTarget, req executorRequest, credential ollama.Credential, body []byte) ([]byte, error) {
	// Reserve before opening HTTP, not after the first event: otherwise quiesce
	// could freeze an idle bridge between preflight and starting the pump.
	if !target.bridge.inflight.begin() {
		return executorFailure(ErrExecutorUnavailable, credential), nil
	}
	handedOff := false
	defer func() {
		if !handedOff {
			target.bridge.inflight.end()
		}
	}()
	stream, response, err := target.openStream(h.ctx, credential, body, req.HostCallbackID)
	if err != nil {
		return executorFailure(err, credential), nil
	}
	defer func() {
		if !handedOff {
			_ = stream.Close()
		}
	}()
	if response.StatusCode != http.StatusOK {
		raw, err := io.ReadAll(stream)
		if err != nil {
			return executorFailure(err, credential), nil
		}
		return upstreamFailure(upstreamResponse{status: response.StatusCode, body: raw}, credential), nil
	}
	reader := newEventReader(stream, strings.Contains(strings.ToLower(response.Headers.Get("Content-Type")), "text/event-stream"))
	first, err := reader.next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = ollama.ErrParse // keep T13's empty-stream HTTP 502, not a hanging async response
		}
		return executorFailure(err, credential), nil
	}
	raw, err := OKEnvelope(executorStreamResponse{Headers: http.Header{"Content-Type": {"text/event-stream"}}})
	if err != nil {
		return nil, err
	}

	// Keep the WHOLE pump in the unload barrier, including gaps between host
	// callbacks. Do not use Provider.Track: quiesce must not cancel inference.
	handedOff = true
	go func() {
		var streamErr error
		defer target.bridge.inflight.end()
		defer func() {
			if recover() != nil {
				streamErr = errHostCallbackPanicked // fixed safe text, never the panic value
			}
			_ = stream.Close()
			closeRequest := executorStreamClose{StreamID: req.StreamID}
			if streamErr != nil {
				code, message, status := classifyExecutorFailure(streamErr)
				closeRequest.ErrorDetail = &pluginabi.Error{Code: code, Message: scrubSecret(message, credential), HTTPStatus: status}
			}
			_ = target.bridge.invoke(context.WithoutCancel(h.ctx), pluginabi.MethodHostStreamClose, closeRequest, nil)
		}()
		for payload := first; ; {
			if err := target.bridge.invoke(h.ctx, pluginabi.MethodHostStreamEmit, executorStreamEmit{StreamID: req.StreamID, Payload: payload}, nil); err != nil {
				streamErr = err
				return
			}
			payload, streamErr = reader.next()
			if errors.Is(streamErr, io.EOF) {
				streamErr = nil
				return
			}
			if streamErr != nil {
				return
			}
		}
	}()
	return raw, nil
}
