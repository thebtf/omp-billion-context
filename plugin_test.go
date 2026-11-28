package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompressionOnlyForSupportedAuthenticatedConversations(t *testing.T) {
	p := newPlugin(nil)
	defer p.shutdown()
	for _, tc := range []struct {
		name     string
		format   protocol
		metadata map[string]any
		headers  http.Header
		payload  string
		want     bool
	}{
		{"chat", chat, map[string]any{"caller_scope": "owner", "request_path": paths[chat]}, http.Header{"X-Acp-Session": {"s"}}, `{"messages":[{"role":"user","content":"hello"}]}`, true},
		{"responses cache key", responses, map[string]any{"caller_scope": "owner", "request_path": paths[responses]}, nil, `{"input":"hello","prompt_cache_key":"conversation"}`, true},
		{"unscoped", chat, map[string]any{"request_path": paths[chat]}, http.Header{"X-Acp-Session": {"s"}}, `{"messages":[{}]}`, false},
		{"websocket", responses, map[string]any{"caller_scope": "owner", "request_path": paths[responses]}, http.Header{"Upgrade": {"websocket"}}, `{"input":"hello","prompt_cache_key":"conversation"}`, false},
		{"compact", responses, map[string]any{"caller_scope": "owner", "request_path": "/v1/responses/compact"}, nil, `{"input":"hello","prompt_cache_key":"conversation"}`, false},
		{"opaque continuation", responses, map[string]any{"caller_scope": "owner", "request_path": paths[responses]}, nil, `{"input":"hello","prompt_cache_key":"conversation","previous_response_id":"opaque"}`, false},
		{"native compaction", responses, map[string]any{"caller_scope": "owner", "request_path": paths[responses]}, nil, `{"input":[{"type":"compaction","encrypted_content":"opaque"}],"prompt_cache_key":"conversation"}`, false},
		{"encrypted reasoning", responses, map[string]any{"caller_scope": "owner", "request_path": paths[responses]}, nil, `{"input":[{"type":"reasoning","encrypted_content":"opaque"}],"prompt_cache_key":"conversation"}`, false},
		{"nested callback", chat, map[string]any{"caller_scope": "owner", "request_path": paths[chat], "source": "plugin_host_model_callback"}, http.Header{"X-Acp-Session": {"s"}}, `{"messages":[{}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(routeRequest{RequestedModel: "configured-cpa-model", SourceFormat: tc.format, Headers: tc.headers, Metadata: tc.metadata, Body: []byte(tc.payload)})
			if err != nil {
				t.Fatal(err)
			}
			value, failure := p.handle("model.route", data)
			if failure != nil {
				t.Fatal(failure.Message)
			}
			decision := value.(routeResponse)
			if decision.Handled != tc.want || tc.want && decision.TargetKind != "self" {
				t.Fatalf("route = %+v", decision)
			}
		})
	}
}

func TestCompressionIdentityIsScopedAndCacheKeyIsNotRewritten(t *testing.T) {
	payload := []byte(`{"messages":[{}],"prompt_cache_key":"original-key"}`)
	headers := http.Header{"X-Acp-Session": {"same-id"}}
	metadata := map[string]any{"request_path": paths[chat], "caller_scope": "first"}
	first, _ := requestSession("model", chat, headers, metadata, payload)
	repeat, _ := requestSession("model", chat, headers, metadata, payload)
	if first == "" || first == "same-id" || first != repeat {
		t.Fatal("unstable or exposed session identity")
	}
	metadata["caller_scope"] = "second"
	if other, _ := requestSession("model", chat, headers, metadata, payload); other == "" || other == first {
		t.Fatal("two authenticated callers share a session")
	}
	changed, _ := requestSession("different-model", chat, headers, metadata, payload)
	same, _ := requestSession("model", chat, headers, metadata, payload)
	if changed == same {
		t.Fatal("models share a session")
	}
	headers.Set("X-Session-Id", "conflicting-id")
	if id, _ := requestSession("model", chat, headers, metadata, payload); id != "" {
		t.Fatal("claimed ambiguous session")
	}
}

func TestChatAndResponsesStreamFraming(t *testing.T) {
	for _, tc := range []struct {
		format     protocol
		body, want string
	}{
		{chat, "data: {\"delta\":1}\n\ndata: [DONE]\n\n", `{"delta":1}`},
		{responses, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n", "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"},
	} {
		p := newPlugin(func(method string, payload any, _ any) *rpcError {
			switch method {
			case "host.stream.emit":
				if got := string(payload.(streamEmit).Payload); got != tc.want {
					t.Errorf("payload = %q, want %q", got, tc.want)
				}
			case "host.stream.close":
				if payload.(streamClose).Error != "" {
					t.Error(payload)
				}
			default:
				t.Errorf("unexpected callback %q", method)
			}
			return nil
		})
		p.forwardStream(executorRequest{Format: tc.format, StreamID: "fixture"}, strings.NewReader(tc.body))
		p.shutdown()
	}
}

func TestEngineStartupFailureIsSharedAndTerminal(t *testing.T) {
	p := newPlugin(nil)
	t.Cleanup(p.shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var starts, stops atomic.Int32
	failure := &engineStartupError{stage: "start", cause: &os.PathError{Op: "fork/exec", Path: "secret-runtime-path", Err: os.ErrPermission}}
	p.startEngine = func(startupCtx, _ context.Context) (engine, error) {
		if starts.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
		case <-startupCtx.Done():
		}
		return engine{stop: func() { stops.Add(1) }}, failure
	}
	const callers = 24
	results := make(chan error, callers)
	for range callers {
		go func() {
			_, err := p.runningEngine(ctx)
			results <- err
		}()
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("startup did not begin")
	}
	close(release)
	for range callers {
		select {
		case err := <-results:
			if !errors.Is(err, failure) {
				t.Fatalf("startup error = %v, want shared failure", err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent startup waiter did not return")
		}
	}
	for range 3 {
		if _, err := p.runningEngine(ctx); !errors.Is(err, failure) {
			t.Fatalf("later startup error = %v, want cached failure", err)
		}
	}
	response := p.readWebUI(&url.URL{Path: biliWebUIRoot + "status"})
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(response.Body), "Compression storage is unavailable") {
		t.Fatalf("management startup failure = %d %s", response.StatusCode, response.Body)
	}
	_, rpcFailure := p.execute(executorRequest{
		Model: "model", SourceFormat: chat, Format: chat,
		Headers:  http.Header{"X-Acp-Session": {"session"}},
		Metadata: map[string]any{"caller_scope": "owner", "request_path": paths[chat]},
		Payload:  []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
	})
	if rpcFailure == nil || rpcFailure.Code != "engine_unavailable" || rpcFailure.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("executor startup failure = %+v", rpcFailure)
	}
	if strings.Contains(string(response.Body), "secret-runtime-path") || strings.Contains(rpcFailure.Message, "secret-runtime-path") {
		t.Fatal("startup diagnostics exposed a runtime path")
	}
	if starts.Load() != 1 || stops.Load() != 1 {
		t.Fatalf("startup attempts = %d, cleanup calls = %d, want 1 each", starts.Load(), stops.Load())
	}
}

func TestCanceledEngineWaiterDoesNotCancelOtherRequests(t *testing.T) {
	p := newPlugin(nil)
	t.Cleanup(p.shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waiterCtx, cancelWaiter := context.WithCancel(ctx)
	defer cancelWaiter()
	type contexts struct{ startup, lifetime context.Context }
	entered := make(chan contexts, 1)
	release := make(chan struct{})
	var starts atomic.Int32
	p.startEngine = func(startupCtx, lifetimeCtx context.Context) (engine, error) {
		starts.Add(1)
		entered <- contexts{startupCtx, lifetimeCtx}
		select {
		case <-release:
			return engine{done: make(chan struct{})}, nil
		case <-startupCtx.Done():
			return engine{}, startupCtx.Err()
		}
	}
	first := make(chan error, 1)
	go func() {
		_, err := p.runningEngine(waiterCtx)
		first <- err
	}()
	var owned contexts
	select {
	case owned = <-entered:
	case <-ctx.Done():
		t.Fatal("startup did not begin")
	}
	deadline, ok := owned.startup.Deadline()
	if !ok || time.Until(deadline) > engineStartupTimeout {
		t.Fatal("startup work has no bounded deadline")
	}
	second := make(chan error, 1)
	go func() {
		_, err := p.runningEngine(ctx)
		second <- err
	}()
	cancelWaiter()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("canceled waiter did not return")
	}
	if owned.startup.Err() != nil || owned.lifetime.Err() != nil {
		t.Fatal("one canceled request canceled shared startup")
	}
	close(release)
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("remaining waiter failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("remaining waiter did not receive the ready engine")
	}
	select {
	case <-owned.startup.Done():
	case <-ctx.Done():
		t.Fatal("completed startup retained its deadline context")
	}
	if owned.lifetime.Err() != nil {
		t.Fatal("completing startup canceled the child lifetime")
	}
	if _, ok := owned.lifetime.Deadline(); ok {
		t.Fatal("child lifetime inherited the startup deadline")
	}
	if _, err := p.runningEngine(ctx); err != nil {
		t.Fatalf("ready engine unavailable after starter returned: %v", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("startup attempts = %d, want 1", got)
	}
}

func TestEngineWaitDeadlineDoesNotCancelStartup(t *testing.T) {
	p := newPlugin(nil)
	t.Cleanup(p.shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var starts atomic.Int32
	p.startEngine = func(startupCtx, _ context.Context) (engine, error) {
		starts.Add(1)
		entered <- startupCtx
		select {
		case <-release:
			return engine{done: make(chan struct{})}, nil
		case <-startupCtx.Done():
			return engine{}, startupCtx.Err()
		}
	}
	ready := make(chan error, 1)
	go func() {
		_, err := p.runningEngine(ctx)
		ready <- err
	}()
	var startupCtx context.Context
	select {
	case startupCtx = <-entered:
	case <-ctx.Done():
		t.Fatal("startup did not begin")
	}
	waiterCtx, cancelWaiter := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelWaiter()
	_, err := p.runningEngine(waiterCtx)
	if !errors.Is(err, context.DeadlineExceeded) || engineFailure(err).HTTPStatus != http.StatusGatewayTimeout {
		t.Fatalf("waiter deadline error = %v", err)
	}
	if startupCtx.Err() != nil {
		t.Fatal("waiter deadline canceled shared startup")
	}
	close(release)
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("remaining request failed after waiter deadline: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("remaining request did not receive the ready engine")
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("startup attempts = %d, want 1", got)
	}
}

func TestShutdownCancelsStartupAndWaitsForCleanup(t *testing.T) {
	p := newPlugin(nil)
	t.Cleanup(p.shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finishCleanup := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(finishCleanup)
	var starts, stops atomic.Int32
	p.startEngine = func(startupCtx, _ context.Context) (engine, error) {
		starts.Add(1)
		close(entered)
		<-startupCtx.Done()
		close(canceled)
		<-release
		return engine{stop: func() { stops.Add(1) }}, nil
	}
	waiter := make(chan error, 1)
	go func() {
		_, err := p.runningEngine(ctx)
		waiter <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("startup did not begin")
	}
	shutdown := make(chan struct{}, 2)
	for range 2 {
		go func() { p.shutdown(); shutdown <- struct{}{} }()
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("shutdown did not cancel startup")
	}
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown waiter error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("shutdown left a request waiting for startup cleanup")
	}
	select {
	case <-shutdown:
		t.Fatal("shutdown returned before startup cleanup finished")
	default:
	}
	finishCleanup()
	for range 2 {
		select {
		case <-shutdown:
		case <-ctx.Done():
			t.Fatal("shutdown did not finish after startup cleanup")
		}
	}
	if _, err := p.runningEngine(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("request after shutdown error = %v", err)
	}
	if starts.Load() != 1 || stops.Load() != 1 {
		t.Fatalf("startup attempts = %d, cleanup calls = %d, want 1 each", starts.Load(), stops.Load())
	}
}

func TestExitedEngineIsTerminalAndShutdownIsIdempotent(t *testing.T) {
	p := newPlugin(nil)
	t.Cleanup(p.shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	var starts, stops atomic.Int32
	p.startEngine = func(context.Context, context.Context) (engine, error) {
		starts.Add(1)
		return engine{done: done, stop: func() { stops.Add(1) }}, nil
	}
	if _, err := p.runningEngine(ctx); err != nil {
		t.Fatalf("initial startup failed: %v", err)
	}
	close(done)
	for range 3 {
		if _, err := p.runningEngine(ctx); !errors.Is(err, errEngineExited) {
			t.Fatalf("request after child exit error = %v", err)
		}
	}
	p.shutdown()
	p.shutdown()
	if starts.Load() != 1 || stops.Load() != 1 {
		t.Fatalf("startup attempts = %d, cleanup calls = %d, want 1 each", starts.Load(), stops.Load())
	}
}

func TestRuntimeStartupDeadlineIncludesExtraction(t *testing.T) {
	root := t.TempDir()
	data := runtimeTestArchive(t, []byte("runtime fixture"))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := unpackRuntime(ctx, root, data); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired unpack error = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expired unpack created files: %v, %v", entries, err)
	}
	_, err = startEngine(ctx, context.Background())
	var startup *engineStartupError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &startup) || startup.stage != "extract" {
		t.Fatalf("expired startup error = %v, want extraction deadline", err)
	}
	if err := unpackRuntime(context.Background(), root, data); err != nil {
		t.Fatalf("uncanceled unpack failed: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "node"))
	if err != nil || string(content) != "runtime fixture" {
		t.Fatalf("unpacked runtime = %q, %v", content, err)
	}
}

func TestRuntimeExtractionCancellationStopsLargeCopyAndRemovesPartialFile(t *testing.T) {
	payload := bytes.Repeat([]byte("runtime fixture\n"), 1<<16)
	data := runtimeTestArchive(t, payload)
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var source *cancelingArchiveReader
	archive.RegisterDecompressor(zip.Store, func(reader io.Reader) io.ReadCloser {
		source = &cancelingArchiveReader{reader: reader, cancel: cancel}
		return io.NopCloser(source)
	})
	destination := filepath.Join(t.TempDir(), "node")
	if err := extractRuntimeFile(ctx, destination, archive.File[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled extraction error = %v", err)
	}
	if source == nil || source.read == 0 || source.read >= len(payload) {
		t.Fatal("cancellation did not interrupt a partially copied runtime file")
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial runtime file remains after cancellation: %v", err)
	}
}

type cancelingArchiveReader struct {
	reader io.Reader
	cancel context.CancelFunc
	read   int
}

func (r *cancelingArchiveReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.read += n
	r.cancel()
	return n, err
}

func runtimeTestArchive(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	header := &zip.FileHeader{Name: "node", Method: zip.Store}
	header.SetMode(0o700)
	file, err := archive.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestStartupFailureDiagnosticsAreSafeAndKeepTimeoutStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		status int
		text   string
	}{
		{"permission", &os.PathError{Op: "exec", Path: "hidden-token", Err: os.ErrPermission}, http.StatusServiceUnavailable, "permission denied"},
		{"deadline", context.DeadlineExceeded, http.StatusGatewayTimeout, "startup timed out"},
		{"unexpected", errors.New("hidden-token\nOMP_ENGINE_TOKEN=hidden-token"), http.StatusServiceUnavailable, "runtime unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &engineStartupError{stage: "extract", cause: tc.cause}
			failure := engineFailure(err)
			if failure.HTTPStatus != tc.status || !strings.Contains(failure.Message, tc.text) || !strings.Contains(failure.Message, "extract") {
				t.Fatalf("startup failure = %+v", failure)
			}
			if strings.Contains(failure.Message, "hidden-token") || strings.ContainsAny(failure.Message, "\r\n") {
				t.Fatal("startup diagnostics exposed untrusted error text")
			}
			if !errors.Is(err, tc.cause) {
				t.Fatal("startup failure lost its original cause")
			}
		})
	}
}
