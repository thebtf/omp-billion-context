package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func lifecycleAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle operation did not finish")
	}
}

func lifecycleRead(l *nativeLifecycle) (managementResponse, error) {
	raw, err := json.Marshal(managementRequest{Method: http.MethodGet, Path: "/v0/management" + webUIRoute, Query: url.Values{"target": {"/__bili/overview"}}})
	if err != nil {
		return managementResponse{}, err
	}
	result, failure := l.handle("management.handle", raw)
	if failure != nil {
		return managementResponse{}, errors.New(failure.Message)
	}
	return result.(managementResponse), nil
}

// lifecycleStreamsDone waits for the admitted stream bodies to leave the
// generation. The close callback fires before that bookkeeping finishes, so a
// quiesce straight after it could still observe the stream as live.
func lifecycleStreamsDone(t *testing.T, p *plugin) {
	t.Helper()
	p.streamMu.Lock()
	idle := p.streamIdle
	p.streamMu.Unlock()
	lifecycleAwait(t, idle)
}

// lifecycleKernel is the answer of a stand-in worker: it sees the operation and
// the whole request frame, and returns either a result or a refusal message.
type lifecycleKernel func(op string, request map[string]json.RawMessage) (any, string)

// lifecycleWorker answers the kernel's newline-delimited JSON on a pair of
// pipes, so a lifecycle test drives the production transport without starting
// the interpreter, a listener or an HTTP client.
func lifecycleWorker(t *testing.T, kernel lifecycleKernel) engine {
	t.Helper()
	requests, stdin := io.Pipe()
	frames, stdout := io.Pipe()
	process := newKernelProcess(stdin, frames, strings.NewReader(""))
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		defer stdout.Close()
		scanner := bufio.NewScanner(requests)
		scanner.Buffer(make([]byte, 0, 32<<10), kernelMaxFrame)
		for scanner.Scan() {
			var request map[string]json.RawMessage
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				return
			}
			var id uint64
			var op string
			_ = json.Unmarshal(request["id"], &id)
			_ = json.Unmarshal(request["op"], &op)
			frame := kernelFrame{ID: id, OK: true}
			result, refusal := kernel(op, request)
			if refusal != "" {
				frame.OK, frame.Error = false, refusal
			} else if encoded, err := json.Marshal(result); err == nil {
				frame.Result = encoded
			}
			line, err := json.Marshal(frame)
			if err != nil {
				return
			}
			if _, err := stdout.Write(append(line, '\n')); err != nil {
				return
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			// A stand-in that is still inside one answer holds the loop; the
			// pipes close anyway and the worker exits once that answer returns.
			_ = stdin.Close()
			_ = stdout.Close()
			select {
			case <-stopped:
			case <-time.After(nativeDrainTimeout):
			}
		})
	}
	t.Cleanup(stop)
	return engine{worker: process, done: process.exited(), stop: stop}
}

// lifecycleEngine builds one worker-backed engine for a plugin factory and
// counts the cleanups the plugin performs on it.
func lifecycleEngine(t *testing.T, kernel lifecycleKernel, stopped *atomic.Int32) engine {
	e := lifecycleWorker(t, kernel)
	inner := e.stop
	e.stop = func() {
		if stopped != nil {
			stopped.Add(1)
		}
		inner()
	}
	return e
}

// lifecycleProjection answers what any executor turn asks before the model
// call: the projection returns the caller's body unchanged.
func lifecycleProjection(op string, request map[string]json.RawMessage) (any, string) {
	if op == "prepare" {
		return map[string]json.RawMessage{"body": request["body"]}, ""
	}
	return nil, ""
}

// lifecycleDashboard answers a dashboard read with one JSON object for the
// overview route, which the plugin decorates with its own counters.
func lifecycleDashboard(status string) lifecycleKernel {
	return func(op string, request map[string]json.RawMessage) (any, string) {
		if op == "dashboard" {
			return map[string]string{"status": status}, ""
		}
		return lifecycleProjection(op, request)
	}
}

// lifecycleChatStream answers the model callbacks of one chat stream: a single
// visible text delta followed by the terminal sentinel.
func lifecycleChatStream(text string, reads *atomic.Int32) hostCall {
	return func(method string, _ any, into any) *rpcError {
		switch method {
		case hostModelExecuteStream:
			*into.(*hostModelResponse) = hostModelResponse{StatusCode: http.StatusOK, StreamID: "provider"}
		case hostModelStreamRead:
			if reads.Add(1) == 1 {
				chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}}
				encoded, _ := json.Marshal(chunk)
				*into.(*hostModelStreamChunk) = hostModelStreamChunk{Payload: encoded}
			} else {
				*into.(*hostModelStreamChunk) = hostModelStreamChunk{Payload: []byte("[DONE]"), Done: true}
			}
		case hostModelStreamClose:
		default:
			return &rpcError{Code: "unexpected_callback", Message: method}
		}
		return nil
	}
}

func TestNativeInitializationReplacesOnlyRetiredOwners(t *testing.T) {
	for _, retire := range []string{"plugin.quiesce", "plugin.shutdown"} {
		t.Run(retire, func(t *testing.T) {
			var registry nativeRegistry
			var oldCalls, emits, streamCloses, reads atomic.Int32
			if !registry.initialize(func(string, any, any) *rpcError { oldCalls.Add(1); return nil }) {
				t.Fatal("initial host initialization failed")
			}
			old := registry.current.Load()
			t.Cleanup(old.shutdown)
			t.Cleanup(func() { registry.current.Load().shutdown() })
			var stops atomic.Int32
			factory := func(context.Context, context.Context) (engine, error) {
				return lifecycleEngine(t, lifecycleProjection, &stops), nil
			}
			old.generation.startEngine = factory
			if _, err := old.generation.runningEngine(context.Background()); err != nil {
				t.Fatal(err)
			}
			if registry.initialize(nil) {
				t.Fatal("initialization replaced an active owner")
			}
			if _, failure := old.handle("executor.identifier", nil); failure != nil {
				t.Fatalf("rejected initialization damaged the active owner: %+v", failure)
			}
			if _, failure := old.handle(retire, nil); failure != nil {
				t.Fatal(failure)
			}
			finished := make(chan struct{})
			stream := lifecycleChatStream("hello", &reads)
			if !registry.initialize(func(method string, payload any, into any) *rpcError {
				switch method {
				case "host.stream.emit":
					emits.Add(1)
					return nil
				case "host.stream.close":
					streamCloses.Add(1)
					close(finished)
					return nil
				}
				return stream(method, payload, into)
			}) {
				t.Fatal("new host initialization rejected a retired owner")
			}
			current := registry.current.Load()
			if current == old || stops.Load() != 1 {
				t.Fatal("initialization did not replace the fully cleaned owner")
			}
			if _, failure := old.handle("plugin.reconfigure", nil); failure == nil || failure.Code != "plugin_closed" {
				t.Fatalf("old host owner resumed after initialization: %+v", failure)
			}
			current.generation.startEngine = factory
			raw, err := json.Marshal(executorRequest{
				Model: "model", SourceFormat: chat, Format: chat, Stream: true,
				Headers:  http.Header{"X-Acp-Session": {"session"}},
				Metadata: map[string]any{"caller_scope": "owner", "request_path": paths[chat]},
				Payload:  []byte(`{"messages":[{"role":"user","content":"hello"}],"stream":true}`),
				StreamID: "output", HostCallbackID: "new-host",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, failure := current.handle("executor.execute_stream", raw); failure != nil {
				t.Fatal(failure)
			}
			lifecycleAwait(t, finished)
			lifecycleStreamsDone(t, current.generation)
			old.shutdown()
			if _, failure := current.handle("executor.identifier", nil); failure != nil {
				t.Fatalf("old owner shutdown stopped the new host: %+v", failure)
			}
			current.shutdown()
			if oldCalls.Load() != 0 || emits.Load() != 1 || streamCloses.Load() != 1 || stops.Load() != 2 {
				t.Fatalf("old callbacks = %d, emits = %d, stream closes = %d, cleanups = %d", oldCalls.Load(), emits.Load(), streamCloses.Load(), stops.Load())
			}
		})
	}
}

func TestNativeInitializationWaitsForRetiredCleanup(t *testing.T) {
	for _, retire := range []string{"plugin.quiesce", "plugin.shutdown"} {
		t.Run(retire, func(t *testing.T) {
			var registry nativeRegistry
			if !registry.initialize(nil) {
				t.Fatal("initialization failed")
			}
			old := registry.current.Load()
			t.Cleanup(old.shutdown)
			t.Cleanup(func() { registry.current.Load().shutdown() })
			entered, release, retired := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var cleanupFinished atomic.Bool
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			old.generation.startEngine = func(context.Context, context.Context) (engine, error) {
				return engine{done: make(chan struct{}), stop: func() {
					close(entered)
					<-release
					cleanupFinished.Store(true)
				}}, nil
			}
			if _, err := old.generation.runningEngine(context.Background()); err != nil {
				t.Fatal(err)
			}
			go func() {
				defer close(retired)
				if _, failure := old.handle(retire, nil); failure != nil {
					t.Error(failure)
				}
			}()
			lifecycleAwait(t, entered)
			initialized := make(chan struct{})
			go func() {
				defer close(initialized)
				ok := registry.initialize(nil)
				if !cleanupFinished.Load() {
					t.Error("initialization returned before old cleanup finished")
				}
				if !ok {
					t.Error("initialization rejected completed retirement")
				}
			}()
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for registry.initialization.TryLock() {
				registry.initialization.Unlock()
				select {
				case <-initialized:
					t.Fatal("initialization returned before entering cleanup wait")
				case <-deadline.C:
					t.Fatal("initialization did not enter cleanup wait")
				default:
					runtime.Gosched()
				}
			}
			unblock()
			lifecycleAwait(t, retired)
			lifecycleAwait(t, initialized)
			if registry.current.Load() == old {
				t.Fatal("retirement did not permit a fresh owner")
			}
			if _, failure := old.handle("plugin.reconfigure", nil); failure == nil || failure.Code != "plugin_closed" {
				t.Fatalf("retired owner resumed: %+v", failure)
			}
			if _, failure := registry.current.Load().handle("executor.identifier", nil); failure != nil {
				t.Fatalf("fresh owner unavailable: %+v", failure)
			}
		})
	}
}

func TestNativeLifecycleReconfigurePreservesTerminalStartupFailure(t *testing.T) {
	l := newNativeLifecycle(nil)
	t.Cleanup(l.shutdown)
	var starts atomic.Int32
	l.generation.startEngine = func(context.Context, context.Context) (engine, error) {
		starts.Add(1)
		return engine{}, &engineStartupError{stage: "start", cause: errors.New("fixture failure")}
	}
	first, err := lifecycleRead(l)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, failure := l.handle("plugin.reconfigure", nil); failure != nil {
			t.Fatal(failure)
		}
		next, err := lifecycleRead(l)
		if err != nil {
			t.Fatal(err)
		}
		if next.StatusCode != http.StatusServiceUnavailable || string(next.Body) != string(first.Body) {
			t.Fatalf("reconfigure changed cached startup failure: %d %s", next.StatusCode, next.Body)
		}
	}
	if first.StatusCode != http.StatusServiceUnavailable || starts.Load() != 1 {
		t.Fatalf("startup status = %d, attempts = %d", first.StatusCode, starts.Load())
	}
}

func TestNativeLifecycleBusyAdmittedReadFinishesNormally(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	l := newNativeLifecycle(nil)
	t.Cleanup(l.shutdown)
	t.Cleanup(unblock)
	var stops atomic.Int32
	l.generation.startEngine = func(context.Context, context.Context) (engine, error) {
		return lifecycleEngine(t, func(op string, request map[string]json.RawMessage) (any, string) {
			if op == "dashboard" {
				close(entered)
				<-release
				return map[string]string{"status": "ready"}, ""
			}
			return lifecycleProjection(op, request)
		}, &stops), nil
	}
	type readResult struct {
		response managementResponse
		err      error
	}
	read := make(chan readResult, 1)
	go func() {
		defer close(finished)
		response, err := lifecycleRead(l)
		read <- readResult{response: response, err: err}
	}()
	lifecycleAwait(t, entered)
	if _, failure := l.handle("plugin.quiesce", nil); failure == nil || failure.Code != "plugin_busy" {
		t.Fatalf("active read quiesce = %+v", failure)
	}
	if stops.Load() != 0 {
		t.Fatal("busy quiesce stopped the admitted read")
	}
	unblock()
	lifecycleAwait(t, finished)
	result := <-read
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.response.StatusCode != http.StatusOK || !strings.Contains(string(result.response.Body), `"status":"ready"`) {
		t.Fatalf("admitted read did not finish normally: %d %s", result.response.StatusCode, result.response.Body)
	}
	if _, failure := l.handle("plugin.quiesce", nil); failure != nil {
		t.Fatal(failure)
	}
	if stops.Load() != 1 {
		t.Fatalf("drained read cleanup calls = %d", stops.Load())
	}
}

func TestNativeLifecycleBusyOutputCallbackKeepsStreamAlive(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var reads atomic.Int32
	stream := lifecycleChatStream("before-replacement", &reads)
	l := newNativeLifecycle(func(method string, payload any, into any) *rpcError {
		switch method {
		case "host.stream.emit":
			close(entered)
			<-release
			return nil
		case "host.stream.close":
			close(finished)
			return nil
		}
		return stream(method, payload, into)
	})
	t.Cleanup(l.shutdown)
	t.Cleanup(unblock)
	var stops atomic.Int32
	l.generation.startEngine = func(context.Context, context.Context) (engine, error) {
		return lifecycleEngine(t, lifecycleDashboard("fresh"), &stops), nil
	}
	raw, err := json.Marshal(executorRequest{
		Model: "model", SourceFormat: chat, Format: chat, Stream: true,
		Headers:  http.Header{"X-Acp-Session": {"session"}},
		Metadata: map[string]any{"caller_scope": "owner", "request_path": paths[chat]},
		Payload:  []byte(`{"messages":[{"role":"user","content":"hello"}],"stream":true}`),
		StreamID: "output", HostCallbackID: "caller",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, failure := l.handle("executor.execute_stream", raw); failure != nil {
		t.Fatal(failure)
	}
	lifecycleAwait(t, entered)
	old := l.generation
	started := time.Now()
	if _, failure := l.handle("plugin.quiesce", nil); failure == nil || failure.Code != "plugin_busy" || failure.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("blocked output quiesce = %+v", failure)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("busy output callback exceeded the bounded drain")
	}
	if old.ctx.Err() != nil || stops.Load() != 0 {
		t.Fatal("busy quiesce canceled the live generation")
	}
	if _, failure := l.handle("plugin.reconfigure", nil); failure != nil {
		t.Fatal(failure)
	}
	if _, failure := l.handle("executor.identifier", nil); failure != nil {
		t.Fatalf("busy generation did not resume admission: %+v", failure)
	}
	unblock()
	lifecycleAwait(t, finished)
	lifecycleStreamsDone(t, l.generation)
	if _, failure := l.handle("plugin.quiesce", nil); failure != nil {
		t.Fatal(failure)
	}
	if stops.Load() != 1 {
		t.Fatalf("idle quiesce cleanup calls = %d", stops.Load())
	}
	if _, failure := l.handle("executor.identifier", nil); failure == nil || failure.Code != "plugin_quiesced" {
		t.Fatalf("quiesced generation admitted work: %+v", failure)
	}
	if _, failure := l.handle("plugin.reconfigure", nil); failure != nil {
		t.Fatal(failure)
	}
	l.generation.startEngine = func(context.Context, context.Context) (engine, error) {
		return lifecycleEngine(t, lifecycleDashboard("fresh"), &stops), nil
	}
	response, err := lifecycleRead(l)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"status":"fresh"`) {
		t.Fatalf("fresh generation did not reach the fixture engine: %d %s", response.StatusCode, response.Body)
	}
	l.shutdown()
	if stops.Load() != 2 {
		t.Fatalf("resumed generation cleanup calls = %d", stops.Load())
	}
	if _, failure := l.handle("plugin.reconfigure", nil); failure == nil || failure.Code != "plugin_closed" {
		t.Fatalf("shutdown generation resumed: %+v", failure)
	}
}

// A direct host callback runs inside the request CPA already admitted, so the
// drain reports busy and leaves the generation running until the model answers.
// There is no HTTP client here that could outlive the native call.
func TestNativeLifecycleBusyDirectCallbackDrainsAfterAnswer(t *testing.T) {
	entered, release, answered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	l := newNativeLifecycle(func(method string, _ any, into any) *rpcError {
		switch method {
		case hostModelExecute:
			close(entered)
			<-release
			*into.(*hostModelResponse) = hostModelResponse{StatusCode: http.StatusOK, Body: []byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)}
			close(answered)
		default:
			t.Errorf("unexpected callback %q", method)
		}
		return nil
	})
	t.Cleanup(l.shutdown)
	t.Cleanup(unblock)
	var stops atomic.Int32
	l.generation.startEngine = func(context.Context, context.Context) (engine, error) {
		return lifecycleEngine(t, lifecycleProjection, &stops), nil
	}
	raw, err := json.Marshal(executorRequest{
		Model: "model", SourceFormat: chat, Format: chat,
		Headers:        http.Header{"X-Acp-Session": {"session"}},
		Metadata:       map[string]any{"caller_scope": "owner", "request_path": paths[chat]},
		Payload:        []byte(`{"model":"model","messages":[{"role":"user","content":"hello"}]}`),
		HostCallbackID: "caller",
	})
	if err != nil {
		t.Fatal(err)
	}
	executed, finished := make(chan any, 1), make(chan struct{})
	go func() {
		defer close(finished)
		result, failure := l.handle("executor.execute", raw)
		if failure != nil {
			executed <- failure
			return
		}
		executed <- result
	}()
	lifecycleAwait(t, entered)
	started := time.Now()
	if _, failure := l.handle("plugin.quiesce", nil); failure == nil || failure.Code != "plugin_busy" || failure.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("blocked callback quiesce = %+v", failure)
	}
	if time.Since(started) > 3*time.Second || stops.Load() != 0 {
		t.Fatal("blocked callback outran the bounded drain or stopped its generation")
	}
	if _, failure := l.handle("plugin.reconfigure", nil); failure != nil {
		t.Fatal(failure)
	}
	if _, failure := l.handle("executor.identifier", nil); failure != nil {
		t.Fatalf("busy generation did not resume admission: %+v", failure)
	}
	unblock()
	lifecycleAwait(t, answered)
	lifecycleAwait(t, finished)
	result := <-executed
	response, ok := result.(executorResponse)
	if !ok || !strings.Contains(string(response.Payload), `"content":"ok"`) {
		t.Fatalf("blocked callback result = %#v", result)
	}
	if _, failure := l.handle("plugin.quiesce", nil); failure != nil {
		t.Fatal(failure)
	}
	if stops.Load() != 1 {
		t.Fatalf("drained callback cleanup calls = %d", stops.Load())
	}
	if _, failure := l.handle("executor.identifier", nil); failure == nil || failure.Code != "plugin_quiesced" {
		t.Fatalf("quiesced generation admitted work: %+v", failure)
	}
}

func TestNativeLifecycleSerializesCleanupResumeAndPermanentShutdown(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	l := newNativeLifecycle(nil)
	t.Cleanup(l.shutdown)
	t.Cleanup(unblock)
	order := make(chan string, 3)
	l.generation.startEngine = func(context.Context, context.Context) (engine, error) {
		return engine{done: make(chan struct{}), stop: func() {
			close(entered)
			<-release
			order <- "cleanup"
		}}, nil
	}
	if _, err := l.generation.runningEngine(context.Background()); err != nil {
		t.Fatal(err)
	}
	quiesced := make(chan struct{})
	go func() {
		defer close(quiesced)
		if _, failure := l.handle("plugin.quiesce", nil); failure != nil {
			t.Error(failure)
		}
	}()
	lifecycleAwait(t, entered)
	if _, failure := l.handle("executor.identifier", nil); failure == nil || failure.Code != "plugin_quiesced" {
		t.Fatalf("cleanup admitted work: %+v", failure)
	}
	resumed, closed := make(chan struct{}), make(chan struct{})
	resumeEntered, shutdownEntered := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(resumed)
		close(resumeEntered)
		_, failure := l.handle("plugin.reconfigure", nil)
		order <- "reconfigure"
		if failure != nil && failure.Code != "plugin_closed" {
			t.Error(failure)
		}
	}()
	go func() {
		defer close(closed)
		close(shutdownEntered)
		l.shutdown()
		order <- "shutdown"
	}()
	lifecycleAwait(t, resumeEntered)
	lifecycleAwait(t, shutdownEntered)
	unblock()
	lifecycleAwait(t, quiesced)
	lifecycleAwait(t, resumed)
	lifecycleAwait(t, closed)
	if first := <-order; first != "cleanup" {
		t.Fatalf("%s returned before old cleanup finished", first)
	}
	for _, method := range []string{"plugin.reconfigure", "executor.identifier", "plugin.quiesce"} {
		if _, failure := l.handle(method, nil); failure == nil || failure.Code != "plugin_closed" {
			t.Fatalf("%s after permanent shutdown = %+v", method, failure)
		}
	}
	l.shutdown()
}
