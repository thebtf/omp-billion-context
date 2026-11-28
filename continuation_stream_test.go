package main

import (
	"context"
	"encoding/json"
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
