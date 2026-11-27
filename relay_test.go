package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelRelayPinsCallerAndModelAcrossRounds(t *testing.T) {
	relay, err := newModelRelay()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.stop)

	original := executorRequest{
		Model: "original-model", SourceFormat: chat, Format: chat, Stream: true, Alt: "original-alt",
		Headers: http.Header{"X-Cpa-Session": {"original-session"}},
		Query:   url.Values{"mode": {"original-mode"}},
		Metadata: map[string]any{
			"request_path": "/v1/chat/completions", "caller_scope": "original-scope",
		},
		HostCallbackID: "original-callback",
	}
	const unchanged = `{ "model" : "original-model", "stream": false }`
	const data = `{"choices":[{"finish_reason":"stop"}]}`
	requests := make(chan hostModelRequest, 8)
	origin, end, err := relay.begin(context.Background(), original, nil, func(method string, payload any, into any) *rpcError {
		switch method {
		case hostModelExecute, hostModelExecuteStream:
			request := payload.(hostModelRequest)
			requests <- request
			if request.Stream != (method == hostModelExecuteStream) {
				t.Errorf("model callback %q with stream=%t", method, request.Stream)
			}
			*into.(*hostModelResponse) = hostModelResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`), StreamID: "provider-stream"}
		case hostModelStreamRead:
			*into.(*hostModelStreamChunk) = hostModelStreamChunk{Payload: []byte(data), Done: true}
		case hostModelStreamClose:
		default:
			t.Errorf("unexpected callback %q", method)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(end)

	client := &http.Client{Timeout: 5 * time.Second}
	post := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, origin+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sidecar-only")
		req.Header.Set("X-Cpa-Session", "sidecar-only")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(raw)
	}
	if status, _ := post(http.MethodGet, "/v1/chat/completions", ""); status != http.StatusNotFound {
		t.Fatalf("GET relay status = %d", status)
	}
	if status, _ := post(http.MethodPost, "/v1/responses", "{}"); status != http.StatusNotFound {
		t.Fatalf("mismatched protocol status = %d", status)
	}
	if status, _ := post(http.MethodPost, "/v1/chat/%63ompletions", "{}"); status != http.StatusNotFound {
		t.Fatalf("escaped relay path status = %d", status)
	}
	if status, body := post(http.MethodPost, "/v1/chat/completions?mode=sidecar-only", unchanged); status != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("nonstream response = %d %q", status, body)
	}
	if status, body := post(http.MethodPost, "/v1/chat/completions", `{"model":"wrong","stream":true,"messages":[{"role":"tool"}]}`); status != http.StatusOK || body != "data: "+data+"\n\ndata: [DONE]\n\n" {
		t.Fatalf("stream response = %d %q", status, body)
	}
	end()
	if status, _ := post(http.MethodPost, "/v1/chat/completions", "{}"); status != http.StatusNotFound {
		t.Fatalf("expired relay status = %d", status)
	}

	if len(requests) != 2 {
		t.Fatalf("host model callback count = %d", len(requests))
	}
	first, second := <-requests, <-requests
	for _, request := range []hostModelRequest{first, second} {
		if request.EntryProtocol != chat || request.ExitProtocol != chat || request.Model != original.Model || request.HostCallbackID != original.HostCallbackID || request.Path != "/v1/chat/completions" || request.Alt != original.Alt || request.CallerScope != "original-scope" {
			t.Errorf("host model routing fields = %+v", request)
		}
		if request.Headers.Get("Authorization") != "" || request.Headers.Get("X-Cpa-Session") != "original-session" || request.Query.Get("mode") != "original-mode" {
			t.Errorf("host model caller fields = %v %v", request.Headers, request.Query)
		}
	}
	var nextRound struct {
		Model    string
		Stream   bool
		Messages []struct{ Role string }
	}
	if first.Stream || string(first.Body) != unchanged || !second.Stream || json.Unmarshal(second.Body, &nextRound) != nil ||
		nextRound.Model != original.Model || !nextRound.Stream || len(nextRound.Messages) != 1 || nextRound.Messages[0].Role != "tool" {
		t.Fatalf("host model round bodies = %q, %q", first.Body, second.Body)
	}
}

func TestModelRelayPreservesResponsesTerminalEvent(t *testing.T) {
	const frame = "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	relay, err := newModelRelay()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.stop)
	origin, end, err := relay.begin(context.Background(), executorRequest{Model: "model", SourceFormat: responses, Format: responses, HostCallbackID: "caller"}, nil, func(method string, _ any, into any) *rpcError {
		switch method {
		case hostModelExecute:
			return &rpcError{Code: "rate_limit", Message: "quoted \"retry\"\nmessage", HTTPStatus: http.StatusTooManyRequests}
		case hostModelExecuteStream:
			*into.(*hostModelResponse) = hostModelResponse{StatusCode: http.StatusOK, StreamID: "provider-stream"}
		case hostModelStreamRead:
			*into.(*hostModelStreamChunk) = hostModelStreamChunk{Payload: []byte(strings.TrimSuffix(frame, "\n\n")), Done: true}
		case hostModelStreamClose:
		default:
			t.Errorf("unexpected callback %q", method)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(end)

	client := &http.Client{Timeout: 5 * time.Second}
	post := func(payload string) (int, string) {
		t.Helper()
		response, err := client.Post(origin+"/v1/responses", "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body)
	}
	if status, got := post(`{"stream":true}`); status != http.StatusOK || got != frame {
		t.Fatalf("terminal response = %d %q", status, got)
	}
	status, body := post(`{"stream":false}`)
	var failure struct{ Error rpcError }
	if status != http.StatusTooManyRequests || json.Unmarshal([]byte(body), &failure) != nil || failure.Error.Code != "rate_limit" || failure.Error.Message != "quoted \"retry\"\nmessage" {
		t.Fatalf("typed error response = %d %q", status, body)
	}
}

func TestModelRelayDisconnectClosesBlockedHostStream(t *testing.T) {
	relay, err := newModelRelay()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	reading, closed := make(chan struct{}), make(chan struct{})
	var onceRead, onceClose sync.Once
	origin, end, err := relay.begin(context.Background(), executorRequest{Model: "model", SourceFormat: chat, Format: chat, HostCallbackID: "caller"}, nil, func(method string, _ any, into any) *rpcError {
		switch method {
		case hostModelExecuteStream:
			*into.(*hostModelResponse) = hostModelResponse{StatusCode: http.StatusOK, StreamID: "provider-stream"}
		case hostModelStreamRead:
			onceRead.Do(func() { close(reading) })
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
			}
			*into.(*hostModelStreamChunk) = hostModelStreamChunk{Done: true}
		case hostModelStreamClose:
			onceClose.Do(func() { close(closed) })
		default:
			t.Errorf("unexpected callback %q", method)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(end)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case <-reading:
	case <-ctx.Done():
		t.Fatal("provider stream read did not start")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("provider stream did not close after the HTTP client disconnected")
	}
}

func TestProtectedRelayBeforeNonStreamingHostCall(t *testing.T) {
	const model = "model"
	original := []byte(protectedFixture)
	meta := map[string]any{"caller_scope": "one", "request_path": paths[responses]}
	_, groups := requestSession(model, responses, http.Header{"X-Session-Id": {"s"}}, meta, original)
	if len(groups) == 0 {
		t.Fatal("protected input was not admitted")
	}
	relay, err := newModelRelay()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.stop)
	var fallbacks atomic.Uint64
	guard := &replayGuard{original: original, groups: groups, fallbacks: &fallbacks}
	calls := make(chan hostModelRequest, 3)
	origin, end, err := relay.begin(context.Background(), executorRequest{Model: model, Format: responses, SourceFormat: responses, HostCallbackID: "cb", Metadata: meta}, guard, func(method string, payload any, into any) *rpcError {
		if method != hostModelExecute {
			t.Errorf("unexpected host method %s", method)
			return &rpcError{HTTPStatus: 500}
		}
		calls <- payload.(hostModelRequest)
		return &rpcError{Code: "provider_limit", Message: "test", HTTPStatus: 429}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(end)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"model":"model","input":[],"stream":true}`, 409},
		{`{"model":"model","input":[]}`, 429},
		{`{"model":"model","input":[]}`, 409},
	} {
		r, err := http.Post(origin+paths[responses], "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != tc.status {
			t.Fatalf("status = %d, want %d", r.StatusCode, tc.status)
		}
	}
	if len(calls) != 1 || fallbacks.Load() != 1 {
		t.Fatalf("host calls=%d, fallbacks=%d", len(calls), fallbacks.Load())
	}
	got := <-calls
	want, _, err := bindModelBody(original, model)
	if err != nil || string(got.Body) != string(want) || got.CallerScope != "one" || got.HostCallbackID != "cb" {
		t.Fatal("full replay or callback binding changed")
	}
}
