package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
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
		{"no session", http.Header{}, compressible},
		{"wrong endpoint", http.Header{"Agent-Session-Id": {"s"}}, otherEndpoint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := metadata
			if tc.name == "wrong endpoint" {
				meta = map[string]any{"caller_scope": "owner", "request_path": paths[chat]}
			}
			got, session, _ := classify("model", claude, tc.headers, meta, body)
			if got != tc.want {
				t.Fatalf("classify = %q, want %q", got, tc.want)
			}
			if tc.want == compressible && session == "" {
				t.Fatal("an eligible request must carry its session id")
			}
		})
	}
}

func TestClaudeWithoutHeadersUsesTheOpeningTurn(t *testing.T) {
	// The Anthropic handler strips custom session headers, so the opening turn is
	// the only identity left. It must stay stable while the conversation grows.
	metadata := map[string]any{"caller_scope": "owner", "request_path": paths[claude]}
	first := []byte(`{"model":"m","messages":[{"role":"user","content":"start here"}],"max_tokens":1000}`)
	later := []byte(`{"model":"m","messages":[{"role":"user","content":"start here"},{"role":"assistant","content":"ok"},{"role":"user","content":"more"}],"max_tokens":1000}`)
	reason, session, _ := classify("model", claude, http.Header{}, metadata, first)
	if reason != compressible || session == "" {
		t.Fatalf("classify = %q, %q", reason, session)
	}
	if again, laterSession, _ := classify("model", claude, http.Header{}, metadata, later); again != compressible || laterSession != session {
		t.Fatalf("the identity must not change as the conversation grows: %q %q vs %q", again, laterSession, session)
	}
}

func TestResponsesWithoutHeadersUsesTheOpeningTurn(t *testing.T) {
	// Responses carries the opening under `input` with `input_text` parts. Reading
	// only `messages` refused every unidentified Responses request.
	metadata := map[string]any{"caller_scope": "owner", "request_path": paths[responses]}
	first := []byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"start here"}]}]}`)
	later := []byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"start here"}]},{"role":"assistant","content":[{"type":"output_text","text":"ok"}]},{"role":"user","content":[{"type":"input_text","text":"more"}]}]}`)
	reason, session, _ := classify("model", responses, http.Header{}, metadata, first)
	if reason != compressible || session == "" {
		t.Fatalf("classify = %q, %q", reason, session)
	}
	if again, laterSession, _ := classify("model", responses, http.Header{}, metadata, later); again != compressible || laterSession != session {
		t.Fatalf("the identity must not change as the conversation grows: %q %q vs %q", again, laterSession, session)
	}
}

func TestClaudeRejectsAnEmptyConversation(t *testing.T) {
	metadata := map[string]any{"caller_scope": "owner", "request_path": paths[claude]}
	headers := http.Header{"Agent-Session-Id": {"s"}}
	if got, _, _ := classify("model", claude, headers, metadata, []byte(`{"model":"m","messages":[]}`)); got != emptyConversation {
		t.Fatalf("classify = %q", got)
	}
}

func TestChatSentinelEndsReadingWithoutWaitingForEOF(t *testing.T) {
	// The upstream keeps the body open after [DONE]; reading must still stop,
	// otherwise CPA waits for the 15-minute stream timeout.
	reader := io.MultiReader(
		strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"),
		blockingReader{},
	)
	emitted := 0
	var closed *string
	p := newPlugin(func(method string, value any, _ any) *rpcError {
		switch method {
		case "host.stream.emit":
			emitted++
		case "host.stream.close":
			err := value.(streamClose).Error
			closed = &err
		}
		return nil
	})
	t.Cleanup(p.shutdown)
	done := make(chan struct{})
	go func() {
		p.forwardStream(executorRequest{Format: chat, StreamID: "s"}, reader)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("forwardStream kept reading after the terminal sentinel")
	}
	if emitted != 1 {
		t.Fatalf("emitted %d frames, want the single delta", emitted)
	}
	if closed == nil || *closed != "" {
		t.Fatalf("a sentinel-terminated stream must close cleanly, got %v", closed)
	}
}

// blockingReader never returns, standing in for an upstream that keeps the
// response body open after its terminal event.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) {
	select {}
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
