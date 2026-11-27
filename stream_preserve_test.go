package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A compressed summary carries deliberate line breaks. Losing them turns a
// traceable summary into one glued sentence, so every hop we own must preserve
// them byte for byte.
const summaryWithBreaks = "Summary:\n- first item\n- second item\n\nParagraph after the list."

func TestStreamEmitPreservesSummaryLineBreaks(t *testing.T) {
	elements := []map[string]any{{
		"type": "response.output_item.done",
		"item": map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": summaryWithBreaks}},
		},
	}}
	payload, err := json.Marshal(map[string]any{
		"type":     "response.completed",
		"response": map[string]any{"output": elements},
	})
	if err != nil {
		t.Fatal(err)
	}
	var emitted []byte
	p := newPlugin(func(method string, value any, _ any) *rpcError {
		if method == "host.stream.emit" {
			emitted = append(emitted, value.(streamEmit).Payload...)
		}
		return nil
	})
	t.Cleanup(p.shutdown)
	p.forwardStream(executorRequest{Format: responses, StreamID: "s"}, strings.NewReader("data: "+string(payload)+"\n\n"))
	if len(emitted) == 0 {
		t.Fatal("nothing was emitted")
	}
	body := string(emitted)
	if !strings.Contains(body, `\n`) {
		t.Fatalf("line breaks were lost in the emitted frame: %s", body)
	}
	decoded := decodeText(t, body)
	if decoded != summaryWithBreaks {
		t.Fatalf("summary text changed:\n got %q\nwant %q", decoded, summaryWithBreaks)
	}
}

func TestChatStreamDeltasPreserveLineBreaks(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": summaryWithBreaks}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var emitted []byte
	p := newPlugin(func(method string, value any, _ any) *rpcError {
		if method == "host.stream.emit" {
			emitted = append(emitted, value.(streamEmit).Payload...)
		}
		return nil
	})
	t.Cleanup(p.shutdown)
	p.forwardStream(executorRequest{Format: chat, StreamID: "s"}, strings.NewReader("data: "+string(payload)+"\n\n"))
	decoded := decodeChatDelta(t, string(emitted))
	if decoded != summaryWithBreaks {
		t.Fatalf("delta text changed:\n got %q\nwant %q", decoded, summaryWithBreaks)
	}
}

// decodeText pulls the assistant text out of a Responses frame, whether the
// frame is a bare JSON object or an SSE event with a data line.
func decodeText(t *testing.T, frame string) string {
	t.Helper()
	raw := frame
	if index := strings.Index(raw, "data: "); index >= 0 {
		raw = raw[index+len("data: "):]
	}
	var decoded struct {
		Type     string `json:"type"`
		Item     responseItem
		Output   []json.RawMessage `json:"output"`
		Response struct {
			Output []responseItem `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &decoded); err != nil {
		t.Fatalf("frame is not decodable JSON: %v\n%s", err, frame)
	}
	// bili re-emits the frame with the item at the top level, so accept both
	// the raw Responses shape and the compact one.
	if decoded.Item.Text() != "" {
		return decoded.Item.Text()
	}
	for _, item := range decoded.Response.Output {
		if text := item.Text(); text != "" {
			return text
		}
	}
	t.Fatalf("no text in frame: %s", frame)
	return ""
}

type responseItem struct {
	Item    *responseItem `json:"item"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (r responseItem) Text() string {
	if r.Item != nil {
		if text := r.Item.Text(); text != "" {
			return text
		}
	}
	for _, content := range r.Content {
		if content.Text != "" {
			return content.Text
		}
	}
	return ""
}

func decodeChatDelta(t *testing.T, frame string) string {
	t.Helper()
	raw := strings.TrimSpace(frame)
	if index := strings.Index(raw, "data: "); index >= 0 {
		raw = strings.TrimSpace(raw[index+len("data: "):])
	}
	var decoded struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("frame is not decodable JSON: %v\n%s", err, frame)
	}
	if len(decoded.Choices) == 0 {
		t.Fatalf("no choices in frame: %s", frame)
	}
	return decoded.Choices[0].Delta.Content
}
