package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEngine stands in for the bundled billion-context process: it answers every
// forwarded turn with the scripted response, so the plugin's own handling of a
// refusal is observable without a provider.
type fakeEngine struct {
	server  *httptest.Server
	mutex   sync.Mutex
	calls   int
	respond func(http.ResponseWriter, *http.Request)
}

func newFakeEngine(t *testing.T, respond func(http.ResponseWriter, *http.Request)) *fakeEngine {
	t.Helper()
	engine := &fakeEngine{respond: respond}
	engine.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engine.mutex.Lock()
		engine.calls++
		engine.mutex.Unlock()
		engine.respond(w, r)
	}))
	t.Cleanup(engine.server.Close)
	return engine
}

func (f *fakeEngine) start(plug *plugin) {
	address, err := url.Parse(f.server.URL)
	if err != nil {
		panic(err)
	}
	done := make(chan struct{})
	plug.startEngine = func(context.Context, context.Context) (engine, error) {
		return engine{url: *address, client: &http.Client{}, done: done, token: "test-token"}, nil
	}
}

func (f *fakeEngine) count() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.calls
}

// synopsis stands for the text bili derives from the provider exchange. Nothing
// from it may reach the client.
const synopsis = "upstream said: prompt span SECRET-PROMPT and provider body SECRET-BODY"

func refusalEnvelope(retryable *bool, message string) map[string]any {
	nested := map[string]any{"type": "server_error", "code": preflightCode, "message": message}
	if retryable != nil {
		nested["retryable"] = *retryable
	}
	return map[string]any{"error": nested}
}

func refusalBody(retryable *bool) string {
	encoded, _ := json.Marshal(refusalEnvelope(retryable, synopsis))
	return string(encoded)
}

// chatTurn is the smallest request the plugin admits for compression. The
// callback id is part of admission: without it the relay rejects the turn and
// the scripted engine is never reached.
func chatTurn() executorRequest {
	const payload = `{"model":"m","messages":[{"role":"user","content":"summarize this"}]}`
	return executorRequest{
		Model: "m", SourceFormat: chat, Format: chat, HostCallbackID: "callback",
		Headers:  http.Header{"X-Acp-Session": {"session"}},
		Metadata: map[string]any{"caller_scope": "owner", "request_path": paths[chat]},
		Payload:  []byte(payload),
	}
}

func streamTurn() executorRequest {
	turn := chatTurn()
	turn.Stream, turn.StreamID = true, "stream"
	return turn
}

// streamSink records what the plugin delivers to the host while forwarding, so
// frames can be inspected after the detachable goroutine finished.
type streamSink struct {
	mutex  sync.Mutex
	frames [][]byte
	closed chan streamClose
}

func newStreamSink() *streamSink {
	return &streamSink{closed: make(chan streamClose, 4)}
}

func (s *streamSink) call(method string, value any, _ any) *rpcError {
	switch method {
	case "host.stream.emit":
		s.mutex.Lock()
		s.frames = append(s.frames, append([]byte(nil), value.(streamEmit).Payload...))
		s.mutex.Unlock()
	case "host.stream.close":
		s.closed <- value.(streamClose)
	}
	return nil
}

func (s *streamSink) emitted() [][]byte {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return append([][]byte(nil), s.frames...)
}

func (s *streamSink) await(t *testing.T) streamClose {
	t.Helper()
	select {
	case close := <-s.closed:
		return close
	case <-time.After(10 * time.Second):
		t.Fatal("stream never closed")
		return streamClose{}
	}
}

// heldStream answers the way bili does after its hold grace: 200 with the final
// content type, the hold marker, one comment keep-alive, then the outcome.
func heldStream(frames ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(preflightHoldHeader, preflightHolding)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ": bili-preflight\n\n")
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
		}
	}
}

func TestPermanentRefusalIsNotReportedAsAGatewayFault(t *testing.T) {
	permanent := false
	engine := newFakeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, refusalBody(&permanent))
	})
	plug := newPlugin(func(string, any, any) *rpcError { return nil })
	t.Cleanup(plug.shutdown)
	engine.start(plug)

	_, failure := plug.execute(chatTurn())
	if failure == nil || failure.Code != preflightFailedCode || failure.HTTPStatus != http.StatusUnprocessableEntity {
		t.Fatalf("permanent refusal = %+v", failure)
	}
	for _, leak := range []string{"SECRET-PROMPT", "SECRET-BODY", "upstream said"} {
		if strings.Contains(failure.Message, leak) {
			t.Fatalf("refusal message echoed %q: %q", leak, failure.Message)
		}
	}
	if got := engine.count(); got != 1 {
		t.Fatalf("plugin issued %d engine calls, want exactly one", got)
	}
}

func TestRetryableRefusalStaysRetryable(t *testing.T) {
	retryable := true
	engine := newFakeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, refusalBody(&retryable))
	})
	plug := newPlugin(func(string, any, any) *rpcError { return nil })
	t.Cleanup(plug.shutdown)
	engine.start(plug)

	if _, failure := plug.execute(chatTurn()); failure == nil || failure.Code != preflightRetryCode || failure.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("retryable refusal = %+v", failure)
	}
	if got := engine.count(); got != 1 {
		t.Fatalf("plugin issued %d engine calls, want exactly one", got)
	}
}

// The Anthropic and Responses error events carry no retryable field. Absence is
// not proof of permanence, so an ordinary verdict-less refusal stays temporary.
func TestRefusalWithoutAVerdictIsNotTreatedAsPermanent(t *testing.T) {
	engine := newFakeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, refusalBody(nil))
	})
	plug := newPlugin(func(string, any, any) *rpcError { return nil })
	t.Cleanup(plug.shutdown)
	engine.start(plug)

	failure := func() *rpcError {
		_, failure := plug.execute(chatTurn())
		return failure
	}()
	if failure == nil || failure.Code != preflightRetryCode || failure.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("verdict-less refusal = %+v", failure)
	}
}

// A settled diagnosis is the one verdict-less refusal that is evidence: bili
// states the summary model answers without visible text, so repeating the same
// conversation cannot help.
func TestSettledDiagnosisWithoutAVerdictIsPermanent(t *testing.T) {
	const settled = "the summary probe path is dead for host|model: 3 consecutive completions ended normally (end_turn/stop/completed) with zero answer text"
	engine := newFakeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		encoded, _ := json.Marshal(refusalEnvelope(nil, settled))
		_, _ = w.Write(encoded)
	})
	plug := newPlugin(func(string, any, any) *rpcError { return nil })
	t.Cleanup(plug.shutdown)
	engine.start(plug)

	failure := func() *rpcError {
		_, failure := plug.execute(chatTurn())
		return failure
	}()
	if failure == nil || failure.Code != preflightFailedCode || failure.HTTPStatus != http.StatusUnprocessableEntity {
		t.Fatalf("settled refusal = %+v", failure)
	}
	if strings.Contains(failure.Message, "end_turn") || strings.Contains(failure.Message, "host|model") {
		t.Fatalf("settled message quotes bili's diagnosis: %q", failure.Message)
	}
}

// A rate limit is the upstream's own temporary verdict. Reporting it as 503
// would hide that the client is being throttled rather than served.
func TestTemporaryUpstreamStatusIsNotFlattened(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int
	}{
		{"rate limit", http.StatusTooManyRequests},
		{"unavailable", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.state)
				_, _ = io.WriteString(w, refusalBody(nil))
			})
			plug := newPlugin(func(string, any, any) *rpcError { return nil })
			t.Cleanup(plug.shutdown)
			engine.start(plug)

			_, failure := plug.execute(chatTurn())
			if failure == nil || failure.Code != preflightRetryCode || failure.HTTPStatus != tc.state {
				t.Fatalf("%s refusal = %+v", tc.name, failure)
			}
		})
	}
}

func TestCommittedOKBodyWithRefusalIsNotASuccess(t *testing.T) {
	permanent := false
	engine := newFakeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, refusalBody(&permanent))
	})
	plug := newPlugin(func(string, any, any) *rpcError { return nil })
	t.Cleanup(plug.shutdown)
	engine.start(plug)

	result, failure := plug.execute(chatTurn())
	if failure == nil {
		t.Fatalf("a 200 carrying a refusal returned a result: %+v", result)
	}
	if failure.HTTPStatus != http.StatusUnprocessableEntity {
		t.Fatalf("committed refusal status = %d", failure.HTTPStatus)
	}
}

func TestUnrelatedFailureKeepsItsGenericStatus(t *testing.T) {
	engine := newFakeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"code":"something_else","message":"gateway"}}`)
	})
	plug := newPlugin(func(string, any, any) *rpcError { return nil })
	t.Cleanup(plug.shutdown)
	engine.start(plug)

	failure := func() *rpcError {
		_, failure := plug.execute(chatTurn())
		return failure
	}()
	if failure == nil || failure.Code != "sidecar_response" || failure.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("unrelated 502 = %+v", failure)
	}
}

// A held stream was committed before its outcome was known, so the verdict has
// to travel as an HTTP status: a client that met a settled refusal must not be
// handed a successful empty stream.
func TestHeldStreamRefusalBecomesAnHTTPError(t *testing.T) {
	permanent := false
	engine := newFakeEngine(t, heldStream(
		"data: "+refusalBody(&permanent)+"\n\n",
		"data: [DONE]\n\n",
	))
	sink := newStreamSink()
	plug := newPlugin(sink.call)
	t.Cleanup(plug.shutdown)
	engine.start(plug)

	result, failure := plug.execute(streamTurn())
	if failure == nil {
		t.Fatalf("held refusal was reported as a stream: %+v", result)
	}
	if failure.Code != preflightFailedCode || failure.HTTPStatus != http.StatusUnprocessableEntity {
		t.Fatalf("held refusal = %+v", failure)
	}
	for _, leak := range []string{"SECRET-PROMPT", "SECRET-BODY"} {
		if strings.Contains(failure.Message, leak) {
			t.Fatalf("held refusal echoed %q: %q", leak, failure.Message)
		}
	}
	if seen := sink.emitted(); len(seen) != 0 {
		t.Fatalf("refusal frame reached the client: %q", seen)
	}
	if got := engine.count(); got != 1 {
		t.Fatalf("plugin issued %d engine calls, want exactly one", got)
	}
}

func TestHeldStreamRetryableRefusalStaysRetryable(t *testing.T) {
	retryable := true
	engine := newFakeEngine(t, heldStream("data: "+refusalBody(&retryable)+"\n\n"))
	sink := newStreamSink()
	plug := newPlugin(sink.call)
	t.Cleanup(plug.shutdown)
	engine.start(plug)

	_, failure := plug.execute(streamTurn())
	if failure == nil || failure.Code != preflightRetryCode || failure.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("held retryable refusal = %+v", failure)
	}
}

// A held stream that turns out to be an answer is still forwarded as a stream,
// and the caller receives the answer frame bili produced.
func TestHeldStreamAnswerIsForwarded(t *testing.T) {
	const answer = `{"choices":[{"delta":{"content":"kept"}}]}`
	engine := newFakeEngine(t, heldStream(
		"data: "+answer+"\n\n",
		"data: [DONE]\n\n",
	))
	sink := newStreamSink()
	plug := newPlugin(sink.call)
	t.Cleanup(plug.shutdown)
	engine.start(plug)

	result, failure := plug.execute(streamTurn())
	if failure != nil {
		t.Fatal(failure)
	}
	if _, ok := result.(streamResponse); !ok {
		t.Fatalf("held answer = %T, want a stream", result)
	}
	if close := sink.await(t); close.Error != "" {
		t.Fatalf("stream closed with %q", close.Error)
	}
	frames := sink.emitted()
	if len(frames) != 1 || string(frames[0]) != answer {
		t.Fatalf("held answer frames = %q", frames)
	}
}

// Without the hold marker the stream was never committed early, so the plugin
// hands it to the caller as it is: the refusal stays a frame, not a status.
func TestUnheldStreamIsNotTurnedIntoAnHTTPError(t *testing.T) {
	permanent := false
	engine := newFakeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+refusalBody(&permanent)+"\n\ndata: [DONE]\n\n")
	})
	sink := newStreamSink()
	plug := newPlugin(sink.call)
	t.Cleanup(plug.shutdown)
	engine.start(plug)

	result, failure := plug.execute(streamTurn())
	if failure != nil {
		t.Fatalf("unheld stream became an HTTP error: %+v", failure)
	}
	if _, ok := result.(streamResponse); !ok {
		t.Fatalf("unheld stream = %T, want a stream", result)
	}
	if close := sink.await(t); close.Error != "" {
		t.Fatalf("stream closed with %q", close.Error)
	}
}

// A refusal delivered in-band keeps the frame shape it arrived in but loses
// everything the plugin did not put there itself: bili's provider-derived text,
// a field a later version may add, and any verdict the frame did not carry.
func TestInBandRefusalIsScrubbedToItsOwnShape(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type": "server_error", "code": preflightCode, "message": synopsis,
			"debug": map[string]string{"prompt": "SYNTHETIC-HIDDEN"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var emitted []byte
	plug := newPlugin(func(method string, value any, _ any) *rpcError {
		if method == "host.stream.emit" {
			emitted = append(emitted, value.(streamEmit).Payload...)
		}
		return nil
	})
	t.Cleanup(plug.shutdown)
	plug.forwardStream(executorRequest{Format: claude, StreamID: "s"}, strings.NewReader("event: error\ndata: "+string(payload)+"\n\n"))

	if len(emitted) == 0 {
		t.Fatal("nothing was emitted")
	}
	body := string(emitted)
	for _, leak := range []string{"SECRET-PROMPT", "SECRET-BODY", "SYNTHETIC-HIDDEN", "upstream said"} {
		if strings.Contains(body, leak) {
			t.Fatalf("emitted frame kept %q: %s", leak, body)
		}
	}
	var frame struct {
		Type  string `json:"type"`
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable"`
		} `json:"error"`
	}
	data := body[strings.Index(body, "data: ")+len("data: "):]
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &frame); err != nil {
		t.Fatalf("emitted frame is not decodable: %v\n%s", err, body)
	}
	if frame.Type != "error" || frame.Error.Code != preflightCode || frame.Error.Message == "" {
		t.Fatalf("emitted frame lost the stage or the type: %s", body)
	}
	if frame.Error.Retryable != nil {
		t.Fatalf("emitted frame invented a verdict: %s", body)
	}
}

func TestSuccessfulTurnIsForwardedUnchanged(t *testing.T) {
	const answer = `{"choices":[{"message":{"role":"assistant","content":"kept"}}]}`
	engine := newFakeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
	})
	plug := newPlugin(func(string, any, any) *rpcError { return nil })
	t.Cleanup(plug.shutdown)
	engine.start(plug)

	result, failure := plug.execute(chatTurn())
	if failure != nil {
		t.Fatal(failure)
	}
	if got := string(result.(executorResponse).Payload); got != answer {
		t.Fatalf("answer = %q", got)
	}
}
