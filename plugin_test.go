package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
	first := requestSession("model", chat, headers, metadata, payload)
	if first == "" || first == "same-id" || first != requestSession("model", chat, headers, metadata, payload) {
		t.Fatal("unstable or exposed session identity")
	}
	metadata["caller_scope"] = "second"
	if other := requestSession("model", chat, headers, metadata, payload); other == "" || other == first {
		t.Fatal("two authenticated callers share a session")
	}
	if changed := requestSession("different-model", chat, headers, metadata, payload); changed == requestSession("model", chat, headers, metadata, payload) {
		t.Fatal("models share a session")
	}
	headers.Set("X-Session-Id", "conflicting-id")
	if requestSession("model", chat, headers, metadata, payload) != "" {
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
