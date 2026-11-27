package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// DSH reaches CPA over the Anthropic endpoint, so /v1/messages must be routed
// and streamed exactly like the other two protocols.
func TestClaudeRequestsAreEligible(t *testing.T) {
	metadata := map[string]any{"caller_scope": "owner", "request_path": paths[claude]}
	body := []byte(`{"model":"m","system":"you are helpful","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`)
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    skipReason
	}{
		{"agent session", http.Header{"Agent-Session-Id": {"s-1"}}, compressible},
		{"claude session", http.Header{"X-Claude-Code-Session-Id": {"s-2"}}, compressible},
		{"conversation id", http.Header{"X-Conversation-Id": {"s-3"}}, compressible},
		{"no session", http.Header{}, missingSessionID},
		{"wrong endpoint", http.Header{"Agent-Session-Id": {"s"}}, otherEndpoint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := metadata
			if tc.name == "wrong endpoint" {
				meta = map[string]any{"caller_scope": "owner", "request_path": paths[chat]}
			}
			got, session := classify("model", claude, tc.headers, meta, body)
			if got != tc.want {
				t.Fatalf("classify = %q, want %q", got, tc.want)
			}
			if tc.want == compressible && session == "" {
				t.Fatal("an eligible request must carry its session id")
			}
		})
	}
}

func TestClaudeRejectsAnEmptyConversation(t *testing.T) {
	metadata := map[string]any{"caller_scope": "owner", "request_path": paths[claude]}
	headers := http.Header{"Agent-Session-Id": {"s"}}
	if got, _ := classify("model", claude, headers, metadata, []byte(`{"model":"m","messages":[]}`)); got != emptyConversation {
		t.Fatalf("classify = %q", got)
	}
}

func TestClaudeStreamsFramesWithAnEventLine(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","content":[]}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"smoke-ok"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
	var emitted []string
	var closed *string
	p := newPlugin(func(method string, value any, _ any) *rpcError {
		switch method {
		case "host.stream.emit":
			emitted = append(emitted, string(value.(streamEmit).Payload))
		case "host.stream.close":
			err := value.(streamClose).Error
			closed = &err
		}
		return nil
	})
	t.Cleanup(p.shutdown)
	p.forwardStream(executorRequest{Format: claude, StreamID: "s"}, strings.NewReader(stream))

	if len(emitted) != 3 {
		t.Fatalf("emitted %d frames, want 3: %q", len(emitted), emitted)
	}
	for i, want := range []string{"message_start", "content_block_delta", "message_stop"} {
		if !strings.HasPrefix(emitted[i], "event: "+want+"\n") {
			t.Fatalf("frame %d = %q, want an event line for %q", i, emitted[i], want)
		}
		if !strings.Contains(emitted[i], "\ndata: {") {
			t.Fatalf("frame %d has no data line: %q", i, emitted[i])
		}
	}
	var delta struct {
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	}
	body := emitted[1][strings.Index(emitted[1], "data: ")+len("data: "):]
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &delta) != nil || delta.Delta.Text != "smoke-ok" {
		t.Fatalf("delta body changed: %s", body)
	}
	if closed == nil || *closed != "" {
		t.Fatalf("a completed stream must close cleanly, got %v", closed)
	}
}

func TestClaudeStreamWithoutEventLinesGetsTheTypeFromTheBody(t *testing.T) {
	stream := "data: {\"type\":\"message_stop\"}\n\n"
	var emitted []string
	p := newPlugin(func(method string, value any, _ any) *rpcError {
		if method == "host.stream.emit" {
			emitted = append(emitted, string(value.(streamEmit).Payload))
		}
		return nil
	})
	t.Cleanup(p.shutdown)
	p.forwardStream(executorRequest{Format: claude, StreamID: "s"}, strings.NewReader(stream))
	if len(emitted) != 1 || !strings.HasPrefix(emitted[0], "event: message_stop\n") {
		t.Fatalf("emitted = %q", emitted)
	}
}

func TestClaudeStreamThatEndsEarlyIsReported(t *testing.T) {
	stream := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"half\"}}\n\n"
	var closed *string
	p := newPlugin(func(method string, value any, _ any) *rpcError {
		if method == "host.stream.close" {
			err := value.(streamClose).Error
			closed = &err
		}
		return nil
	})
	t.Cleanup(p.shutdown)
	p.forwardStream(executorRequest{Format: claude, StreamID: "s"}, strings.NewReader(stream))
	if closed == nil || *closed == "" {
		t.Fatal("a stream without message_stop must be reported as incomplete")
	}
}
