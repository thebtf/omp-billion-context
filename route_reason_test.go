package main

import (
	"net/http"
	"testing"
)

func TestClassifyReportsActionableBypassReasons(t *testing.T) {
	headers := func(pairs ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(pairs); i += 2 {
			h[pairs[i]] = append(h[pairs[i]], pairs[i+1])
		}
		return h
	}
	scope := map[string]any{"caller_scope": "owner", "request_path": paths[chat]}
	responsesScope := map[string]any{"caller_scope": "owner", "request_path": paths[responses]}
	for _, tc := range []struct {
		name     string
		format   protocol
		metadata map[string]any
		headers  http.Header
		payload  string
		want     skipReason
	}{
		{"eligible chat", chat, scope, headers("X-Acp-Session", "s"), `{"messages":[{"role":"user","content":"hi"}]}`, compressible},
		{"no caller scope", chat, map[string]any{"request_path": paths[chat]}, headers("X-Acp-Session", "s"), `{"messages":[{"role":"user","content":"hi"}]}`, missingCallerScope},
		{"no session", chat, scope, headers(), `{"messages":[{"role":"user","content":"hi"}]}`, missingSessionID},
		{"conflicting session", chat, scope, headers("X-Acp-Session", "a", "X-Session-Id", "b"), `{"messages":[{"role":"user","content":"hi"}]}`, ambiguousSessionID},
		{"unusable session", chat, scope, headers("X-Acp-Session", "has space"), `{"messages":[{"role":"user","content":"hi"}]}`, missingSessionID},
		{"other endpoint", responses, scope, headers("X-Acp-Session", "s"), `{"input":"hi"}`, otherEndpoint},
		{"websocket", responses, responsesScope, headers("Upgrade", "websocket"), `{"input":"hi","prompt_cache_key":"c"}`, websocketUpgrade},
		{"nested callback", chat, map[string]any{"caller_scope": "owner", "request_path": paths[chat], "source": "plugin_host_model_callback"}, headers("X-Acp-Session", "s"), `{"messages":[{"role":"user","content":"hi"}]}`, nestedCallback},
		{"opaque", responses, responsesScope, headers(), `{"input":"hi","previous_response_id":"x","prompt_cache_key":"c"}`, opaqueContinuation},
		{"encrypted", responses, responsesScope, headers(), `{"input":[{"type":"reasoning","encrypted_content":"x"}],"prompt_cache_key":"c"}`, encryptedReasoning},
		{"empty chat", chat, scope, headers("X-Acp-Session", "s"), `{"messages":[]}`, emptyConversation},
		{"invalid payload", chat, scope, headers("X-Acp-Session", "s"), `not json`, invalidPayload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := classify("model", tc.format, tc.headers, tc.metadata, []byte(tc.payload)); got != tc.want {
				t.Fatalf("classify = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyReturnsSessionForTheSameEligibleRequest(t *testing.T) {
	metadata := map[string]any{"caller_scope": "owner", "request_path": paths[chat]}
	payload := []byte(`{"messages":[{"role":"user","content":"hi"}],"session_id":"body-session"}`)
	reason, session := classify("model", chat, http.Header{}, metadata, payload)
	if reason != compressible || session != "body-session" {
		t.Fatalf("classify = (%q, %q)", reason, session)
	}
	if requestSession("model", chat, http.Header{}, metadata, payload) == "" {
		t.Fatal("requestSession rejected an eligible request")
	}
}

func TestOnlyActionableReasonsAreLogged(t *testing.T) {
	for reason := range silentReasons {
		if !silentReasons[reason] {
			t.Fatalf("%q is not actionable", reason)
		}
	}
	for _, reason := range []skipReason{otherEndpoint, unsupportedFormat, alternateMode, nestedCallback, websocketUpgrade, invalidPayload, emptyConversation, opaqueContinuation, encryptedReasoning, compressible} {
		if silentReasons[reason] {
			t.Fatalf("%q must stay quiet: it is normal traffic", reason)
		}
	}
}
