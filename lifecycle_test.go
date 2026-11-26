package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func lifecycleRead(t *testing.T, l *nativeLifecycle) managementResponse {
	t.Helper()
	raw, err := json.Marshal(managementRequest{Method: http.MethodGet, Path: "/v0/management" + webUIRoute, Query: url.Values{"target": {"/__bili/status"}}})
	if err != nil {
		t.Fatal(err)
	}
	result, failure := l.handle("management.handle", raw)
	if failure != nil {
		t.Fatalf("management read failed: %+v", failure)
	}
	return result.(managementResponse)
}

func TestNativeInitializationReplacesOnlyRetiredOwners(t *testing.T) {
	for _, retire := range []string{"plugin.quiesce", "plugin.shutdown"} {
		t.Run(retire, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"text\":\"fresh-host\"}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(server.Close)
			var registry nativeRegistry
			var oldCalls, newCalls, stops atomic.Int32
			if !registry.initialize(func(string, any, any) *rpcError { oldCalls.Add(1); return nil }) {
				t.Fatal("initial host initialization failed")
			}
			old := registry.current.Load()
			t.Cleanup(old.shutdown)
			t.Cleanup(func() { registry.current.Load().shutdown() })
			endpoint, _ := url.Parse(server.URL)
			factory := func(context.Context, context.Context) (engine, error) {
				return engine{url: *endpoint, client: server.Client(), done: make(chan struct{}), stop: func() { stops.Add(1) }}, nil
			}
			old.generation.startEngine = factory
			if _, _, err := old.generation.runningEngine(context.Background()); err != nil {
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
			if !registry.initialize(func(method string, _ any, _ any) *rpcError {
				newCalls.Add(1)
				if method == "host.stream.close" {
					close(finished)
				}
				return nil
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
			old.shutdown()
			if _, failure := current.handle("executor.identifier", nil); failure != nil {
				t.Fatalf("old owner shutdown stopped the new host: %+v", failure)
			}
			current.shutdown()
			if oldCalls.Load() != 0 || newCalls.Load() != 2 || stops.Load() != 2 {
				t.Fatalf("old callbacks = %d, new callbacks = %d, cleanup calls = %d", oldCalls.Load(), newCalls.Load(), stops.Load())
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
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			old.generation.startEngine = func(context.Context, context.Context) (engine, error) {
				return engine{done: make(chan struct{}), stop: func() { close(entered); <-release }}, nil
			}
			if _, _, err := old.generation.runningEngine(context.Background()); err != nil {
				t.Fatal(err)
			}
			go func() {
				defer close(retired)
				if _, failure := old.handle(retire, nil); failure != nil {
					t.Error(failure)
				}
			}()
			lifecycleAwait(t, entered)
			started, initialized := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(initialized)
				close(started)
				if !registry.initialize(nil) {
					t.Error("initialization rejected completed retirement")
				}
			}()
			lifecycleAwait(t, started)
			select {
			case <-initialized:
				t.Fatal("initialization overlapped old cleanup")
			case <-time.After(20 * time.Millisecond):
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
	first := lifecycleRead(t, l)
	for range 3 {
		if _, failure := l.handle("plugin.reconfigure", nil); failure != nil {
			t.Fatal(failure)
		}
		next := lifecycleRead(t, l)
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	}))
	t.Cleanup(server.Close)
	l := newNativeLifecycle(nil)
	t.Cleanup(l.shutdown)
	t.Cleanup(unblock)
	endpoint, _ := url.Parse(server.URL)
	var stops atomic.Int32
	l.generation.startEngine = func(context.Context, context.Context) (engine, error) {
		return engine{url: *endpoint, client: server.Client(), done: make(chan struct{}), stop: func() { stops.Add(1) }}, nil
	}
	go func() {
		defer close(finished)
		response := lifecycleRead(t, l)
		if response.StatusCode != http.StatusOK || string(response.Body) != `{"status":"ready"}` {
			t.Errorf("admitted read did not finish normally: %d %s", response.StatusCode, response.Body)
		}
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"fresh"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"text\":\"before-replacement\"}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	l := newNativeLifecycle(func(method string, _ any, _ any) *rpcError {
		switch method {
		case "host.stream.emit":
			close(entered)
			<-release
		case "host.stream.close":
			close(finished)
		default:
			t.Errorf("unexpected callback %q", method)
		}
		return nil
	})
	t.Cleanup(l.shutdown)
	t.Cleanup(unblock)
	var stops atomic.Int32
	endpoint, _ := url.Parse(server.URL)
	l.generation.startEngine = func(context.Context, context.Context) (engine, error) {
		return engine{url: *endpoint, client: server.Client(), done: make(chan struct{}), stop: func() { stops.Add(1) }}, nil
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
		return engine{url: *endpoint, client: server.Client(), done: make(chan struct{}), stop: func() { stops.Add(1) }}, nil
	}
	response := lifecycleRead(t, l)
	if response.StatusCode != http.StatusOK || string(response.Body) != `{"status":"fresh"}` {
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

func TestNativeLifecycleBusyRelayCallbackOutlivesCanceledHTTP(t *testing.T) {
	entered, release, callbackDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	l := newNativeLifecycle(nil)
	t.Cleanup(l.shutdown)
	t.Cleanup(unblock)
	var stops atomic.Int32
	l.generation.startEngine = func(context.Context, context.Context) (engine, error) {
		return engine{done: make(chan struct{}), stop: func() { stops.Add(1) }}, nil
	}
	_, relay, err := l.generation.runningEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	origin, end, err := relay.begin(context.Background(), executorRequest{Model: "model", SourceFormat: chat, Format: chat, HostCallbackID: "caller"}, func(method string, _ any, into any) *rpcError {
		if method != hostModelExecute {
			t.Errorf("unexpected callback %q", method)
		}
		close(entered)
		<-release
		*into.(*hostModelResponse) = hostModelResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}
		close(callbackDone)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(end)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+paths[chat], strings.NewReader(`{"model":"model"}`))
	if err != nil {
		t.Fatal(err)
	}
	httpDone := make(chan struct{})
	go func() {
		defer close(httpDone)
		response, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	lifecycleAwait(t, entered)
	cancel()
	end()
	lifecycleAwait(t, httpDone)
	started := time.Now()
	if _, failure := l.handle("plugin.quiesce", nil); failure == nil || failure.Code != "plugin_busy" {
		t.Fatalf("blocked relay quiesce = %+v", failure)
	}
	if time.Since(started) > 3*time.Second || stops.Load() != 0 {
		t.Fatal("busy relay callback blocked or stopped its generation")
	}
	if _, failure := l.handle("plugin.reconfigure", nil); failure != nil {
		t.Fatal(failure)
	}
	if _, failure := l.handle("executor.identifier", nil); failure != nil {
		t.Fatalf("busy relay generation did not resume: %+v", failure)
	}
	unblock()
	lifecycleAwait(t, callbackDone)
	if _, failure := l.handle("plugin.quiesce", nil); failure != nil {
		t.Fatal(failure)
	}
	if stops.Load() != 1 {
		t.Fatalf("drained relay cleanup calls = %d", stops.Load())
	}
}

func TestNativeLifecycleSerializesCleanupResumeAndPermanentShutdown(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	l := newNativeLifecycle(nil)
	t.Cleanup(l.shutdown)
	t.Cleanup(unblock)
	l.generation.startEngine = func(context.Context, context.Context) (engine, error) {
		return engine{done: make(chan struct{}), stop: func() { close(entered); <-release }}, nil
	}
	if _, _, err := l.generation.runningEngine(context.Background()); err != nil {
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
	go func() {
		defer close(resumed)
		_, failure := l.handle("plugin.reconfigure", nil)
		if failure != nil && failure.Code != "plugin_closed" {
			t.Error(failure)
		}
	}()
	go func() { defer close(closed); l.shutdown() }()
	select {
	case <-resumed:
		t.Fatal("reconfigure returned before old cleanup finished")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	lifecycleAwait(t, quiesced)
	lifecycleAwait(t, resumed)
	lifecycleAwait(t, closed)
	for _, method := range []string{"plugin.reconfigure", "executor.identifier", "plugin.quiesce"} {
		if _, failure := l.handle(method, nil); failure == nil || failure.Code != "plugin_closed" {
			t.Fatalf("%s after permanent shutdown = %+v", method, failure)
		}
	}
	l.shutdown()
}
