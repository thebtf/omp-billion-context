package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func streamSequence(t *testing.T, format protocol, frames ...string) (kernelRound, []string, *rpcError) {
	t.Helper()
	index := 0
	var emitted []string
	round, failure := readKernelStream(context.Background(), format, nil, func(method string, _ any, into any) *rpcError {
		if method == hostModelStreamClose {
			return nil
		}
		if method != hostModelStreamRead || index >= len(frames) {
			t.Fatalf("unexpected host read %s after %d frames", method, index)
		}
		*into.(*hostModelStreamChunk) = hostModelStreamChunk{Payload: []byte(frames[index]), Done: index+1 == len(frames)}
		index++
		return nil
	}, "fixture", func(frame []byte) error { emitted = append(emitted, string(frame)); return nil })
	return round, emitted, failure
}

// claudeReplayData decodes one client-visible Anthropic event, with or without an event line.
func claudeReplayData(t *testing.T, frame string) map[string]any {
	t.Helper()
	data := frame
	if strings.HasPrefix(data, "event: ") {
		_, rest, _ := strings.Cut(data, "\n")
		data = rest
	}
	data = strings.TrimSpace(data)
	data = strings.TrimPrefix(data, "data: ")
	var body map[string]any
	d := json.NewDecoder(strings.NewReader(strings.TrimSpace(data)))
	d.UseNumber()
	if d.Decode(&body) != nil {
		t.Fatalf("emitted frame is not a JSON body: %q", frame)
	}
	return body
}

func tokens(n int64) *int64 { return &n }

func TestChatFinalUsageFollowsFinishReason(t *testing.T) {
	_, frames, fail := streamSequence(t, chat, `{"choices":[{"index":0,"delta":{"content":"ok"}}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, `{"choices":[],"usage":{"prompt_tokens":64}}`, `[DONE]`)
	if fail != nil || len(frames) != 3 || !strings.Contains(frames[1], `"finish_reason":"stop"`) || !strings.Contains(frames[2], `"usage"`) {
		t.Fatal(frames, fail)
	}
}

func TestUnfinishedResponsesCallCannotCreateFold(t *testing.T) {
	item := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "c1", "name": "compress", "arguments": "{}", "status": "in_progress"}
	encode := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	for _, status := range []string{"incomplete", "completed"} {
		packet := map[string]any{"status": status, "output": []any{item}}
		if r, _, err := kernelJSON(responses, []byte(encode(packet)), nil, ""); err == nil || len(r.Calls) != 0 {
			t.Fatal(r, err)
		}
		_, _, f := streamSequence(t, responses, encode(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item}), encode(map[string]any{"type": "response." + status, "response": packet}))
		if f == nil || f.Code != "incomplete_tool_call" {
			t.Fatal(status, f)
		}
	}
}

func TestAnthropicPromptTooLongIsRecoverable(t *testing.T) {
	if !isOverflow(kernelError("invalid_request_error", "prompt is too long: 212000 tokens > 200000 maximum", 400)) {
		t.Fatal("provider overflow was mistaken for an unrelated 400")
	}
}

func TestPrivateStreamToolRequiresCompletedReason(t *testing.T) {
	const chatTool = `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"compress","arguments":"{}"}}]}}]}`
	const claudeTool = `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"compress","input":{}}}`
	for _, tc := range []struct {
		format                   protocol
		tool, finish, end, valid string
		invalid                  []string
	}{
		{chat, chatTool, `{"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, `[DONE]`, "tool_calls", []string{"", "stop", "length"}},
		{claude, claudeTool, `{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":8}}`, `{"type":"message_stop"}`, "tool_use", []string{"", "max_tokens", "model_context_window_exceeded"}},
	} {
		for _, reason := range append(tc.invalid, tc.valid) {
			frames := []string{tc.tool}
			if reason != "" {
				frames = append(frames, fmt.Sprintf(tc.finish, reason))
			}
			frames = append(frames, tc.end)
			round, _, f := streamSequence(t, tc.format, frames...)
			if reason == tc.valid {
				if f != nil || len(round.Calls) != 1 {
					t.Fatal(tc.format, reason, round, f)
				}
			} else if f == nil || f.Code != "incomplete_tool_call" || len(round.Calls) != 0 {
				t.Fatal(tc.format, reason, round, f)
			}
		}
	}
}

func TestClaudeClientUsageIncludesHiddenRound(t *testing.T) {
	for _, test := range []struct {
		prior *int64
		want  string
	}{{tokens(12), "19"}, {tokens(0), "7"}, {nil, ""}} {
		var got string
		c := continuationStream{format: claude, emit: func(frame []byte) error {
			body := claudeReplayData(t, string(frame))
			if n, ok := wireObject(body, "usage")["output_tokens"].(json.Number); ok {
				got = string(n)
			}
			return nil
		}}
		c.addClaudeUsage(test.prior)
		emit := c.begin()
		data := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`
		frame := sseFrame{event: "message_delta", data: []byte(data)}
		if err := emit(frame.eventPayload("message_delta")); err != nil || got != test.want {
			t.Fatal(got, test.want, err)
		}
	}
}

func TestPendingTrailerRemainsBoundedAcrossFlush(t *testing.T) {
	s := kernelStream{emit: func([]byte) error { return nil }}
	chunk := make([]byte, maxSSEFrameBytes/2)
	if f := s.keepTail(chunk); f != nil {
		t.Fatal(f)
	}
	if f := s.send([]byte("pending"), false); f != nil {
		t.Fatal(f)
	}
	if f := s.flush(); f != nil {
		t.Fatal(f)
	}
	if f := s.keepTail(chunk); f != nil {
		t.Fatal(f)
	}
	if f := s.keepTail([]byte("x")); f == nil || f.Code != "invalid_stream" {
		t.Fatal("exceeded combined trailer limit", f)
	}
}
