package main

import (
	"net/http"
	"testing"
)

func TestSummaryFallbackRequiresACompletedEmptyReply(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		empty      bool
	}{
		{"reasoning only", `{"type":"message","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"synthetic"}]}`, 200, true},
		{"text", `{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"visible summary"}]}`, 200, false},
		{"tool", `{"type":"message","stop_reason":"tool_use","content":[{"type":"tool_use"}]}`, 200, false},
		{"incomplete", `{"type":"message","stop_reason":"max_tokens","content":[]}`, 200, false},
		{"rate limit", `{"error":"rate limit"}`, 429, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := emptyClaudeCompletion(hostModelResponse{StatusCode: tc.status, Body: []byte(tc.body)}, nil); got != tc.empty {
				t.Fatalf("switch transport = %v, want %v", got, tc.empty)
			}
		})
	}
	if emptyClaudeCompletion(hostModelResponse{}, &rpcError{HTTPStatus: http.StatusServiceUnavailable, Message: "model returned an empty response"}) {
		t.Fatal("an unavailable model must keep its status")
	}
}

func TestPartialSummaryStreamDoesNotYieldASummary(t *testing.T) {
	for _, terminal := range []string{`{"type":"error","error":{"message":"fixture"}}`, `{"type":"message_stop"}`} {
		s := claudeSummary{}
		if !s.chunk([]byte("event: message_start\r\ndata: {\"type\":\"message_start\"}\r\n\r\nevent: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\r\n\r\n")) {
			t.Fatal("valid stream prefix rejected")
		}
		if s.chunk([]byte(terminal)) || s.stopped {
			t.Fatal("partial text accepted without a clean end_turn")
		}
	}
}
