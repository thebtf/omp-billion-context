package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const maxModelRequestBytes = 64 << 20

const (
	hostModelExecute       = "host.model.execute"
	hostModelExecuteStream = "host.model.execute_stream"
	hostModelStreamRead    = "host.model.stream_read"
	hostModelStreamClose   = "host.model.stream_close"
)

type modelRelay struct {
	origin      string
	server      *http.Server
	done        chan struct{}
	mu          sync.Mutex
	turns       map[string]modelTurn
	active      sync.WaitGroup
	activeCount int
	idle        chan struct{}
}

type modelTurn struct {
	ctx     context.Context
	call    hostCall
	request hostModelRequest
}

type hostModelRequest struct {
	EntryProtocol  protocol    `json:"entry_protocol"`
	ExitProtocol   protocol    `json:"exit_protocol"`
	Model          string      `json:"model"`
	Stream         bool        `json:"stream"`
	Body           []byte      `json:"body"`
	Headers        http.Header `json:"headers"`
	Query          url.Values  `json:"query"`
	Alt            string      `json:"alt"`
	Path           string      `json:"path,omitempty"`
	CallerScope    string      `json:"caller_scope,omitempty"`
	HostCallbackID string      `json:"host_callback_id"`
}

type hostModelResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
	StreamID   string      `json:"stream_id"`
}

type hostModelStreamRequest struct {
	StreamID string `json:"stream_id"`
}

type hostModelStreamChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func newModelRelay() (*modelRelay, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	relay := &modelRelay{
		origin: "http://" + listener.Addr().String(),
		done:   make(chan struct{}),
		turns:  make(map[string]modelTurn),
	}
	relay.idle = make(chan struct{})
	close(relay.idle)
	relay.server = &http.Server{
		Handler:           http.HandlerFunc(relay.serveHTTP),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	go func() {
		defer close(relay.done)
		_ = relay.server.Serve(listener)
	}()
	return relay, nil
}

func (r *modelRelay) stop() {
	r.mu.Lock()
	r.turns = nil
	r.mu.Unlock()
	_ = r.server.Close()
	<-r.done
	r.active.Wait()
}

func (r *modelRelay) begin(ctx context.Context, req executorRequest, call hostCall) (string, func(), error) {
	path, supported := paths[req.SourceFormat]
	if !supported || req.SourceFormat != req.Format || req.Model == "" || req.HostCallbackID == "" || call == nil {
		return "", nil, errors.New("invalid model relay request")
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", nil, err
	}
	prefix := "/" + hex.EncodeToString(token[:])
	endpoint := prefix + path
	requestPath, _ := req.Metadata["request_path"].(string)
	callerScope, _ := req.Metadata["caller_scope"].(string)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if r.turns == nil {
		return "", nil, errors.New("model relay is closed")
	}
	turnCtx, cancel := context.WithCancel(ctx)
	r.turns[endpoint] = modelTurn{
		ctx:  turnCtx,
		call: call,
		request: hostModelRequest{
			EntryProtocol:  req.SourceFormat,
			ExitProtocol:   req.Format,
			Model:          req.Model,
			Headers:        req.Headers,
			Query:          req.Query,
			Alt:            req.Alt,
			Path:           requestPath,
			CallerScope:    callerScope,
			HostCallbackID: req.HostCallbackID,
		},
	}
	return r.origin + prefix, func() {
		r.mu.Lock()
		delete(r.turns, endpoint)
		r.mu.Unlock()
		cancel()
	}, nil
}

func (r *modelRelay) serveHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	turn, ok := r.turns[req.URL.Path]
	if !ok || turn.ctx.Err() != nil || req.Method != http.MethodPost || req.URL.EscapedPath() != req.URL.Path {
		r.mu.Unlock()
		http.NotFound(w, req)
		return
	}
	r.active.Add(1)
	if r.activeCount == 0 {
		r.idle = make(chan struct{})
	}
	r.activeCount++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.activeCount--
		if r.activeCount == 0 {
			close(r.idle)
		}
		r.mu.Unlock()
		r.active.Done()
	}()

	ctx, cancel := context.WithCancel(req.Context())
	stopTurn := context.AfterFunc(turn.ctx, cancel)
	defer stopTurn()
	defer cancel()
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxModelRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeModelError(w, &rpcError{Code: "invalid_payload", Message: "model payload is too large", HTTPStatus: http.StatusRequestEntityTooLarge})
		} else {
			writeModelError(w, &rpcError{Code: "invalid_payload", Message: "could not read model payload", HTTPStatus: http.StatusBadRequest})
		}
		return
	}
	request := turn.request
	request.Body, request.Stream, err = bindModelBody(body, request.Model)
	if err != nil {
		writeModelError(w, &rpcError{Code: "invalid_payload", Message: err.Error(), HTTPStatus: http.StatusBadRequest})
		return
	}
	if ctx.Err() != nil || turn.ctx.Err() != nil {
		return
	}
	if request.Stream {
		relayModelStream(ctx, w, request, turn.call)
		return
	}
	var response hostModelResponse
	if failure := turn.call(hostModelExecute, request, &response); failure != nil {
		writeModelError(w, failure)
		return
	}
	copyModelHeaders(w.Header(), response.Headers)
	if response.StatusCode < 200 || response.StatusCode > 299 || !json.Valid(response.Body) {
		writeModelError(w, &rpcError{Code: "host_model_response", Message: "CPA returned an unsuccessful or invalid model response", HTTPStatus: response.StatusCode})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(response.Body)
}

func bindModelBody(body []byte, model string) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, false, errors.New("model payload must be a JSON object")
	}
	var stream bool
	if raw, ok := fields["stream"]; ok && json.Unmarshal(raw, &stream) != nil {
		return nil, false, errors.New("model payload stream must be a boolean")
	}
	var bodyModel string
	if json.Unmarshal(fields["model"], &bodyModel) == nil && bodyModel == model {
		return body, stream, nil
	}
	fields["model"], _ = json.Marshal(model)
	bound, err := json.Marshal(fields)
	return bound, stream, err
}

func relayModelStream(ctx context.Context, w http.ResponseWriter, request hostModelRequest, call hostCall) {
	var response hostModelResponse
	if failure := call(hostModelExecuteStream, request, &response); failure != nil {
		writeModelError(w, failure)
		return
	}
	if response.StreamID == "" {
		writeModelError(w, &rpcError{Code: "host_model_response", Message: "CPA returned no model stream ID", HTTPStatus: response.StatusCode})
		return
	}
	target := hostModelStreamRequest{StreamID: response.StreamID}
	closed := make(chan struct{})
	closeStream := func() {
		defer close(closed)
		_ = call(hostModelStreamClose, target, nil)
	}
	// A native stream_read blocks in CPA. Close from a separate goroutine on disconnect.
	stopClose := context.AfterFunc(ctx, closeStream)
	defer func() {
		if stopClose() {
			closeStream()
		} else {
			<-closed
		}
	}()
	if ctx.Err() != nil {
		return
	}
	copyModelHeaders(w.Header(), response.Headers)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		writeModelError(w, &rpcError{Code: "host_model_response", Message: "CPA returned an unsuccessful model stream response", HTTPStatus: response.StatusCode})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(response.StatusCode)
	controller := http.NewResponseController(w)
	if controller.Flush() != nil {
		return
	}

	var chatDone bool
	for ctx.Err() == nil {
		var chunk hostModelStreamChunk
		failure := call(hostModelStreamRead, target, &chunk)
		if ctx.Err() != nil {
			return
		}
		if len(chunk.Payload) != 0 {
			terminal, err := writeModelSSE(w, chunk.Payload)
			if err != nil {
				return
			}
			chatDone = chatDone || terminal
			if controller.Flush() != nil {
				return
			}
		}
		if failure == nil && chunk.Error != "" {
			failure = &rpcError{Code: "host_model_stream", Message: chunk.Error}
		}
		if failure != nil {
			writeModelStreamError(w, request.ExitProtocol, failure)
		}
		if chunk.Done || failure != nil {
			if request.ExitProtocol == chat && !chatDone {
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}
			return
		}
	}
}

func copyModelHeaders(dst, src http.Header) {
	for _, key := range []string{"Retry-After", "X-Request-Id"} {
		if value := src.Get(key); value != "" {
			dst.Set(key, value)
		}
	}
}

func modelStatus(status int) int {
	if status >= 400 && status <= 599 {
		return status
	}
	return http.StatusBadGateway
}

func writeModelError(w http.ResponseWriter, failure *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(modelStatus(failure.HTTPStatus))
	_ = json.NewEncoder(w).Encode(map[string]*rpcError{"error": failure})
}

func writeModelStreamError(w http.ResponseWriter, format protocol, failure *rpcError) {
	if format == responses {
		_, _ = io.WriteString(w, "event: error\n")
	}
	payload := map[string]any{"error": failure}
	if format == responses {
		payload = map[string]any{"type": "error", "code": failure.Code, "message": failure.Message}
	}
	encoded, _ := json.Marshal(payload)
	_, _ = writeModelSSE(w, encoded)
}

func writeModelSSE(w io.Writer, chunk []byte) (bool, error) {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 {
		return false, nil
	}
	terminal := bytes.Equal(trimmed, []byte("[DONE]"))
	if terminal || json.Valid(trimmed) {
		if bytes.ContainsAny(trimmed, "\r\n") {
			var compact bytes.Buffer
			_ = json.Compact(&compact, trimmed)
			trimmed = compact.Bytes()
		}
		if _, err := io.WriteString(w, "data: "); err != nil {
			return false, err
		}
		if _, err := w.Write(trimmed); err != nil {
			return false, err
		}
		_, err := io.WriteString(w, "\n\n")
		return terminal, err
	}
	for lines := trimmed; len(lines) != 0; {
		line, rest, _ := bytes.Cut(lines, []byte("\n"))
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok && bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			terminal = true
			break
		}
		lines = rest
	}
	if _, err := w.Write(chunk); err != nil {
		return false, err
	}
	var suffix string
	switch {
	case bytes.HasSuffix(chunk, []byte("\n\n")), bytes.HasSuffix(chunk, []byte("\r\n\r\n")):
	case bytes.HasSuffix(chunk, []byte("\r\n")):
		suffix = "\r\n"
	case bytes.HasSuffix(chunk, []byte("\n")):
		suffix = "\n"
	default:
		suffix = "\n\n"
	}
	if suffix != "" {
		_, err := io.WriteString(w, suffix)
		return terminal, err
	}
	return terminal, nil
}
