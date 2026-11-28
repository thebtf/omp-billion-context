package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
)

// Only bili's tier-one preflight probe requests a JSON summary with this task.
// Ordinary turns (including tool calls) keep their original transport.
const preflightTask = "TASK: The conversation segment below (messages "

func streamPreflightSummary(body []byte) []byte {
	if !bytes.Contains(body, []byte(preflightTask)) {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || len(fields) != 5 || fields["model"] == nil || fields["max_tokens"] == nil || string(fields["stream"]) != "false" {
		return nil
	}
	var system string
	if json.Unmarshal(fields["system"], &system) != nil {
		var blocks []struct{ Type, Text string }
		if json.Unmarshal(fields["system"], &blocks) != nil || len(blocks) != 2 || blocks[1].Type != "text" {
			return nil
		}
		system = blocks[1].Text
	}
	if !strings.Contains(system, "\n\n"+preflightTask) || !strings.Contains(system, "Output ONLY the summary text") {
		return nil
	}
	var msgs []struct{ Role, Content string }
	if json.Unmarshal(fields["messages"], &msgs) != nil || len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content == "" {
		return nil
	}
	fields["stream"] = json.RawMessage("true")
	out, _ := json.Marshal(fields)
	return out
}

type emptySummaryCache struct {
	preferStream atomic.Bool
	last         atomic.Pointer[[sha256.Size]byte]
}

type claudeSummary struct {
	cache            *emptySummaryCache
	key              [sha256.Size]byte
	text             strings.Builder
	started, stopped bool
	reason           string
}

func emptyClaudeCompletion(response hostModelResponse, failure *rpcError) bool {
	if failure != nil {
		return failure.HTTPStatus == http.StatusBadGateway && strings.Contains(failure.Message, "model returned an empty response")
	}
	if response.StatusCode != http.StatusOK {
		return false
	}
	var msg struct {
		Type       string
		StopReason string `json:"stop_reason"`
		Content    []struct{ Type, Text string }
	}
	if json.Unmarshal(response.Body, &msg) != nil || msg.Type != "message" || msg.StopReason != "end_turn" || msg.Content == nil {
		return false
	}
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				return false
			}
		case "thinking", "redacted_thinking":
		default:
			return false
		}
	}
	return true
}

func (s *claudeSummary) event(data []byte) bool {
	var e struct {
		Type  string `json:"type"`
		Delta struct {
			Type, Text string
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Block struct{ Type, Text string } `json:"content_block"`
		Error json.RawMessage             `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil || e.Type == "error" || e.Error != nil || s.stopped {
		return false
	}
	switch e.Type {
	case "ping":
		return true
	case "message_start":
		if s.started {
			return false
		}
		s.started = true
	case "content_block_start", "content_block_delta":
		if !s.started {
			return false
		}
		var text string
		if e.Type == "content_block_start" && e.Block.Type == "text" {
			text = e.Block.Text
		}
		if e.Type == "content_block_delta" && e.Delta.Type == "text_delta" {
			text = e.Delta.Text
		}
		if len(text) > maxSSEFrameBytes-s.text.Len() {
			return false
		}
		s.text.WriteString(text)
	case "message_delta":
		if !s.started {
			return false
		}
		if e.Delta.StopReason != "" {
			s.reason = e.Delta.StopReason
		}
	case "message_stop":
		s.stopped = s.started && s.reason == "end_turn"
		return s.stopped
	}
	return true
}

func (s *claudeSummary) chunk(payload []byte) bool {
	data := bytes.TrimSpace(payload)
	if len(data) == 0 {
		return true
	}
	if data[0] == '{' {
		return s.event(data)
	}
	var frame sseFrame
	for len(data) != 0 {
		line, rest, _ := bytes.Cut(data, []byte("\n"))
		data = rest
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			if len(frame.data) != 0 && !s.event(frame.data) {
				return false
			}
			frame.reset()
		} else {
			frame.add(line)
		}
	}
	return len(frame.data) == 0 || s.event(frame.data)
}

func (s *claudeSummary) finish(ctx context.Context, w http.ResponseWriter, target hostModelStreamRequest, call hostCall) {
	for ctx.Err() == nil {
		var chunk hostModelStreamChunk
		failure := call(hostModelStreamRead, target, &chunk)
		if ctx.Err() != nil {
			return
		}
		if failure != nil {
			writeModelError(w, failure)
			return
		}
		if chunk.Error != "" || !s.chunk(chunk.Payload) || (chunk.Done && !s.stopped) {
			writeModelError(w, &rpcError{Code: "summary_stream_incomplete", Message: "summary stream did not complete normally", HTTPStatus: http.StatusBadGateway})
			return
		}
		if s.stopped {
			if strings.TrimSpace(s.text.String()) == "" {
				key := s.key
				s.cache.last.Store(&key)
			}
			s.reply(w)
			return
		}
	}
}

func (s *claudeSummary) reply(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "role": "assistant", "stop_reason": "end_turn", "content": []map[string]string{{"type": "text", "text": s.text.String()}}})
}
