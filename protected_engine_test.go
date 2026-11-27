package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const protectedAnswer = "fixture-accepted"

func TestProtectedBundledFold(t *testing.T) {
	if os.Getenv("BILI_TEST_PROTECTED_ENGINE") != "1" {
		t.Skip("requires the packaged platform runtime")
	}
	root := t.TempDir()
	data, err := bundledRuntime.ReadFile("bundle/runtime.json")
	var manifest runtimeManifest
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	// The fold test exercises a warm installation; slow mounted volumes must
	// not turn it into another cold-start deadline test.
	prep, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err = ensureRuntime(prep, root, manifest, func() ([]byte, error) { return bundledRuntime.ReadFile("bundle/runtime.zip") }); err != nil {
		t.Fatal(err)
	}
	const summary = "FOLDPROOF The first turns agreed on UTF-8 configuration and local tests. Preserve that result for later work."
	for _, tc := range []struct {
		name     string
		insert   int
		last     string
		fallback bool
	}{{"outside", 18, "m00004", false}, {"crossing", 3, "m00010", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var mutex sync.Mutex
			var frames [][]byte
			var calls [][]byte
			closed := make(chan streamClose, 2)
			var output bytes.Buffer
			plug := newPlugin(func(method string, value any, into any) *rpcError {
				mutex.Lock()
				defer mutex.Unlock()
				switch method {
				case hostModelExecuteStream:
					calls = append(calls, bytes.Clone(value.(hostModelRequest).Body))
					if len(calls) == 1 {
						frames = compressEvents(map[string]string{"startId": "m00001", "endId": tc.last, "summary": summary})
					} else {
						frames = answerEvents()
					}
					*into.(*hostModelResponse) = hostModelResponse{StreamID: "provider", StatusCode: 200}
				case hostModelStreamRead:
					*into.(*hostModelStreamChunk) = hostModelStreamChunk{Payload: frames[0], Done: len(frames) == 1}
					frames = frames[1:]
				case hostModelStreamClose, "host.log":
				case "host.stream.emit":
					output.Write(value.(streamEmit).Payload)
				case "host.stream.close":
					closed <- value.(streamClose)
				default:
					return &rpcError{Code: "unexpected", Message: method}
				}
				return nil
			})
			plug.startEngine = func(start, life context.Context) (engine, error) { return startEngineAt(start, life, root) }
			t.Cleanup(plug.shutdown)
			var orig struct{ Input []json.RawMessage }
			if err := json.Unmarshal([]byte(protectedFixture), &orig); err != nil {
				t.Fatal(err)
			}
			trio := orig.Input[1:4]
			history := []json.RawMessage{}
			for i := 0; i < 41; i++ {
				if i == tc.insert {
					history = append(history, trio...)
				}
				role := "user"
				if i%2 != 0 {
					role = "assistant"
				}
				msg, _ := json.Marshal(map[string]string{"type": "message", "role": role, "content": strings.Repeat(fmt.Sprintf("Turn_%02d ", i), 460)})
				history = append(history, msg)
			}
			var prefix []json.RawMessage
			for turn := 0; turn < 2; turn++ {
				if turn == 1 {
					history = append(history, json.RawMessage(`{"role":"assistant","content":"fixture-accepted"}`), json.RawMessage(`{"role":"user","content":"follow-up"}`))
				}
				body, _ := json.Marshal(map[string]any{"model": "gpt-5.4", "stream": true, "reasoning": map[string]string{"context": "current_turn"}, "max_output_tokens": 1200, "input": history, "prompt_cache_key": "cache-test"})
				_, failure := plug.execute(executorRequest{Model: "gpt-5.4", SourceFormat: responses, Format: responses, Stream: true, StreamID: "stream", HostCallbackID: "cb", Payload: body, OriginalRequest: body, Headers: http.Header{"X-Acp-Session": {tc.name}}, Metadata: map[string]any{"caller_scope": "local-test", "request_path": paths[responses]}})
				if failure != nil {
					t.Fatal(failure)
				}
				select {
				case c := <-closed:
					if c.Error != "" {
						t.Fatal(c.Error)
					}
				case <-time.After(15 * time.Second):
					t.Fatal("no terminal event")
				}
				mutex.Lock()
				snapshot := append([][]byte(nil), calls...)
				clientText := output.String()
				output.Reset()
				mutex.Unlock()
				if !strings.Contains(clientText, protectedAnswer) || strings.Contains(clientText, "call_compress") {
					t.Fatal("lost final reply or leaked internal tool call")
				}
				if len(snapshot) != 2+turn {
					t.Fatalf("provider calls=%d, want %d", len(snapshot), 2+turn)
				}
				wire := snapshot[len(snapshot)-1]
				if turn == 0 && bytes.Equal(snapshot[0], body) {
					t.Fatal("initial request bypassed engine")
				}
				if tc.fallback {
					if !bytes.Equal(wire, body) {
						t.Fatal("crossing fold did not dispatch original complete turn")
					}
					if plug.protectedFallbacks.Load() != uint64(turn+1) {
						t.Fatal("fallback count differs from provider evidence")
					}
					continue
				}
				var b struct {
					Input          []json.RawMessage
					PromptCacheKey string `json:"prompt_cache_key"`
				}
				if err := json.Unmarshal(wire, &b); err != nil {
					t.Fatal(err)
				}
				if b.PromptCacheKey != "cache-test" || bytes.Contains(wire, []byte(strings.Repeat("Turn_02 ", 10))) || !bytes.Contains(wire, []byte("Turn_39")) {
					t.Fatal("safe fold lost recent text, cache key, or retained the folded range")
				}
				anchor, reason := -1, -1
				for i, item := range b.Input {
					var obj map[string]any
					json.Unmarshal(item, &obj)
					if obj["role"] == "user" && bytes.Contains(item, []byte(summary)) {
						anchor = i
					}
					if obj["type"] == "reasoning" {
						reason = i
					}
				}
				if anchor < 0 || reason <= anchor || reason+2 >= len(b.Input) {
					t.Fatal("missing summary anchor or protected group")
				}
				for i, raw := range trio {
					var x, y any
					json.Unmarshal(raw, &x)
					json.Unmarshal(b.Input[reason+i], &y)
					if !reflect.DeepEqual(x, y) {
						t.Fatalf("protected item %d changed", i)
					}
				}
				if turn == 0 {
					prefix = b.Input[:anchor+1]
				} else if !reflect.DeepEqual(prefix, b.Input[:anchor+1]) {
					t.Fatal("folded prefix changed on next turn")
				}
				if plug.protectedFallbacks.Load() != 0 {
					t.Fatal("safe range used original replay")
				}
			}
		})
	}
}

func compressEvents(fold map[string]string) [][]byte {
	arguments, _ := json.Marshal(map[string]any{"content": []map[string]string{fold}})
	call := map[string]any{
		"type": "function_call", "id": "fc_compress", "call_id": "call_compress",
		"name": "compress", "arguments": "", "status": "in_progress",
	}
	done := map[string]any{
		"type": "function_call", "id": "fc_compress", "call_id": "call_compress",
		"name": "compress", "arguments": string(arguments), "status": "completed",
	}
	return [][]byte{
		protectedSSE("response.created", map[string]any{
			"type": "response.created", "response": map[string]any{"id": "resp_compress", "status": "in_progress"},
		}),
		protectedSSE("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0, "item": call,
		}),
		protectedSSE("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "output_index": 0,
			"item_id": "fc_compress", "call_id": "call_compress", "arguments": string(arguments),
		}),
		protectedSSE("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0, "item": done,
		}),
		protectedSSE("response.completed", map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_compress", "status": "completed", "output": []any{done}},
		}),
	}
}

func answerEvents() [][]byte {
	item := map[string]any{
		"type": "message", "id": "msg_protected", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": protectedAnswer}},
	}
	return [][]byte{
		protectedSSE("response.created", map[string]any{
			"type": "response.created", "response": map[string]any{"id": "resp_protected", "status": "in_progress"},
		}),
		protectedSSE("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0, "item": item,
		}),
		protectedSSE("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": "msg_protected",
			"output_index": 0, "content_index": 0, "delta": protectedAnswer,
		}),
		protectedSSE("response.completed", map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_protected", "status": "completed", "output": []any{item}},
		}),
	}
}

func protectedSSE(kind string, payload map[string]any) []byte {
	encoded, _ := json.Marshal(payload)
	frame := make([]byte, 0, len(kind)+len(encoded)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, kind...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, encoded...)
	return append(frame, "\n\n"...)
}
