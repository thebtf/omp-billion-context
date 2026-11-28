package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A native host callback cannot be preempted, so the client deadline expires
// while the callback still holds the conversation. The caller gets its timeout,
// no second model call is sent for that conversation, and the slot stays held
// until the callback physically returns.
func TestSessionDeadlineKeepsSlotUntilCallbackReturns(t *testing.T) {
	entered, release, answered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32
	plug := newPlugin(func(method string, _ any, into any) *rpcError {
		if method != hostModelExecute {
			t.Errorf("unexpected callback %q", method)
			return &rpcError{Code: "unexpected_callback", Message: method}
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			close(answered)
		}
		*into.(*hostModelResponse) = hostModelResponse{StatusCode: http.StatusOK, Body: []byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)}
		return nil
	})
	t.Cleanup(plug.shutdown)
	t.Cleanup(unblock)
	plug.startEngine = func(context.Context, context.Context) (engine, error) {
		return lifecycleEngine(t, lifecycleProjection, nil), nil
	}
	request := func(session string) executorRequest {
		return executorRequest{
			Model: "model", SourceFormat: chat, Format: chat, HostCallbackID: "caller",
			Headers:  http.Header{"X-Acp-Session": {session}},
			Metadata: map[string]any{"caller_scope": "owner", "request_path": paths[chat]},
			Payload:  []byte(`{"model":"model","messages":[{"role":"user","content":"hello"}]}`),
		}
	}
	type outcome struct {
		result  any
		failure *rpcError
	}
	first := make(chan outcome, 1)
	go func() {
		result, failure := plug.executeWithTimeout(request("session"), time.Second)
		first <- outcome{result, failure}
	}()
	lifecycleAwait(t, entered)
	select {
	case got := <-first:
		t.Fatalf("callback was not blocked at the client deadline: %+v", got.failure)
	default:
	}
	var expired outcome
	select {
	case expired = <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not get its own deadline while the callback was blocked")
	}
	if expired.failure == nil || expired.failure.Code != "sidecar_timeout" || expired.failure.HTTPStatus != http.StatusGatewayTimeout {
		t.Fatalf("expired callback deadline = %+v", expired.failure)
	}
	started := time.Now()
	_, busy := plug.executeWithTimeout(request("session"), 5*time.Second)
	if busy == nil || busy.Code != "session_busy" || busy.HTTPStatus != http.StatusConflict || !strings.Contains(strings.ToLower(busy.Message), "unknown") {
		t.Fatalf("pending callback refusal = %+v", busy)
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("refused turn waited %s for the unknown outcome", waited)
	}
	if calls.Load() != 1 {
		t.Fatalf("refused turn sent %d model calls, want 1", calls.Load())
	}
	other, otherFailure := plug.executeWithTimeout(request("other"), 5*time.Second)
	if otherFailure != nil {
		t.Fatalf("independent session was blocked: %+v", otherFailure)
	}
	if response, ok := other.(executorResponse); !ok || !strings.Contains(string(response.Payload), `"content":"ok"`) {
		t.Fatalf("independent session result = %#v", other)
	}
	unblock()
	lifecycleAwait(t, answered)
	lifecycleStreamsDone(t, plug)
	result, failure := plug.executeWithTimeout(request("session"), 5*time.Second)
	if failure != nil {
		t.Fatalf("turn after the callback returned = %+v", failure)
	}
	if response, ok := result.(executorResponse); !ok || !strings.Contains(string(response.Payload), `"content":"ok"`) {
		t.Fatalf("turn after the callback returned = %#v", result)
	}
	if calls.Load() != 3 {
		t.Fatalf("model calls = %d, want 3", calls.Load())
	}
}
