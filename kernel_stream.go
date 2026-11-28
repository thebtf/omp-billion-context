package main

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type streamedTool struct {
	name, id  string
	args      strings.Builder
	complete  string
	confirmed bool
}
type kernelStream struct {
	format              protocol
	client              map[string]bool
	emit                func([]byte) error
	round               kernelRound
	pending             [][]byte
	buffered, toolBytes int
	tools               map[int]*streamedTool
	hidden              map[int]bool
	terminal, finished  bool
	incomplete          bool
	stopReason          string
	tailBytes           int
	tail                [][]byte
	base, chatEnd       map[string]any
}

func readKernelStream(ctx context.Context, format protocol, clientTools map[string]bool, call hostCall, streamID string, emit func([]byte) error) (kernelRound, *rpcError) {
	s := kernelStream{format: format, client: clientTools, emit: emit, tools: map[int]*streamedTool{}, hidden: map[int]bool{}}
	ref := hostModelStreamRequest{StreamID: streamID}
	var once sync.Once
	closed := make(chan struct{})
	closeIt := func() { once.Do(func() { _ = call(hostModelStreamClose, ref, nil); close(closed) }) }
	interrupt := context.AfterFunc(ctx, closeIt)
	defer func() {
		if interrupt() {
			closeIt()
		}
		<-closed
	}()
	for ctx.Err() == nil {
		var chunk hostModelStreamChunk
		f := call(hostModelStreamRead, ref, &chunk)
		if len(chunk.Payload) > 0 {
			if err := s.consume(chunk.Payload); err != nil {
				return s.round, err
			}
		}
		if f != nil {
			return s.round, f
		}
		if chunk.Error != "" {
			return s.round, kernelError("upstream_stream", "The provider stream failed", 502)
		}
		if chunk.Done || s.terminal {
			break
		}
	}
	if ctx.Err() != nil {
		return s.round, sidecarFailure(ctx.Err())
	}
	if !s.terminal && !(format == chat && s.finished) {
		return s.round, kernelError("incomplete_stream", "The provider stream ended before its terminal event", 502)
	}
	indices := make([]int, 0, len(s.tools))
	for index := range s.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, i := range indices {
		t := s.tools[i]
		args := t.complete
		if args == "" {
			args = t.args.String()
		}
		if privateTool(t.name, s.client) {
			if s.incomplete || (format == responses && !t.confirmed) || (format == chat && s.stopReason != "tool_calls") || (format == claude && s.stopReason != "tool_use") {
				return s.round, kernelError("incomplete_tool_call", "The provider did not complete the compression call; no fold was saved", 422)
			}
			s.round.Calls = append(s.round.Calls, kernelToolCall{t.name, toolArgs(args), t.id})
		} else if s.format == chat {
			s.round.External = true
			tool := map[string]any{"index": i, "id": t.id, "type": "function", "function": map[string]any{"name": t.name, "arguments": args}}
			frame := s.chatFrame(map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{tool}}, "finish_reason": nil})
			if e := s.publish(frame); e != nil {
				return s.round, e
			}
		}
	}
	if len(s.round.Calls) == 0 || s.round.External {
		if s.format == chat && s.chatEnd != nil {
			choice := mapCopy(s.chatEnd)
			if !s.round.External && len(s.round.Calls) > 0 {
				choice["finish_reason"] = "stop"
			}
			if e := s.publish(s.chatFrame(choice)); e != nil {
				return s.round, e
			}
		} else if e := s.flush(); e != nil {
			return s.round, e
		}
		for _, frame := range s.tail {
			if e := s.publish(frame); e != nil {
				return s.round, e
			}
		}
	}
	return s.round, nil
}

func (s *kernelStream) consume(chunk []byte) *rpcError {
	trim := bytes.TrimSpace(chunk)
	if len(trim) == 0 {
		return nil
	}
	if len(trim) > maxSSEFrameBytes {
		return kernelError("invalid_stream", "Model stream frame too large", 502)
	}
	if bytes.Equal(trim, []byte("[DONE]")) {
		s.terminal = true
		return nil
	}
	if trim[0] == '{' {
		return s.event(trim)
	}
	normalized := bytes.ReplaceAll(trim, []byte("\r\n"), []byte("\n"))
	for _, packet := range bytes.Split(normalized, []byte("\n\n")) {
		var frame sseFrame
		for _, line := range bytes.Split(packet, []byte{'\n'}) {
			frame.add(line)
		}
		if len(frame.data) == 0 {
			continue
		}
		if bytes.Equal(frame.data, []byte("[DONE]")) {
			s.terminal = true
			continue
		}
		if e := s.event(frame.data); e != nil {
			return e
		}
	}
	return nil
}

func (s *kernelStream) event(data []byte) *rpcError {
	var o map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if d.Decode(&o) != nil || o == nil {
		return kernelError("invalid_stream", "Invalid provider stream event", 502)
	}
	kind := wireText(o, "type")
	if kind == "error" || kind == "response.failed" || o["error"] != nil {
		nested := wireObject(o, "error")
		if nested == nil {
			nested = wireObject(wireObject(o, "response"), "error")
		}
		if isOverflow(kernelError(wireText(nested, "code"), wireText(nested, "message"), 400)) {
			return kernelError("context_length_exceeded", "The provider rejected the context length", 400)
		}
		return kernelError("upstream_stream", "The provider reported a stream error", 502)
	}
	if s.format == chat {
		return s.chatEvent(o)
	}
	idx := wireIndex(o, "index")
	if s.format == responses {
		idx = wireIndex(o, "output_index")
	}
	private := false
	visible := false
	terminal := false
	if s.format == claude {
		switch kind {
		case "content_block_start":
			b := wireObject(o, "content_block")
			switch wireText(b, "type") {
			case "tool_use":
				name := wireText(b, "name")
				if privateTool(name, s.client) {
					s.hidden[idx] = true
					t := s.tool(idx)
					t.name = name
					t.id = wireText(b, "id")
					if args, ok := b["input"].(map[string]any); ok && len(args) > 0 {
						t.complete = string(toolArgs(args))
					}
				} else {
					s.round.External = true
					visible = true
				}
			case "text":
				visible = wireText(b, "text") != ""
			}
		case "content_block_delta":
			delta := wireObject(o, "delta")
			if s.hidden[idx] {
				s.tool(idx).args.WriteString(wireText(delta, "partial_json"))
			} else if wireText(delta, "type") == "text_delta" {
				visible = wireText(delta, "text") != ""
			}
		case "message_delta":
			if reason := wireText(wireObject(o, "delta"), "stop_reason"); reason != "" {
				s.stopReason = reason
			}
			if n, ok := wireObject(o, "usage")["output_tokens"].(json.Number); ok {
				if count, e := n.Int64(); e == nil && count >= 0 {
					s.round.OutputTokens = &count
				}
			}
			if !s.round.External && len(s.hidden) > 0 && wireText(wireObject(o, "delta"), "stop_reason") == "tool_use" {
				wireObject(o, "delta")["stop_reason"] = "end_turn"
			}
		case "message_stop":
			terminal = true
		}
		if strings.HasPrefix(kind, "content_block_") {
			private = s.hidden[idx]
			if !private {
				s.reindex(o, "index", idx)
			}
		}
	} else {
		item := wireObject(o, "item")
		switch kind {
		case "response.output_item.added", "response.output_item.done":
			if wireText(item, "type") == "function_call" {
				name := wireText(item, "name")
				if privateTool(name, s.client) {
					s.hidden[idx] = true
					t := s.tool(idx)
					t.name = name
					t.id = wireText(item, "call_id")
					if kind == "response.output_item.done" && wireText(item, "status") == "completed" {
						t.confirmed = true
					}
					if a := wireText(item, "arguments"); a != "" {
						t.complete = a
					}
				} else {
					s.round.External = true
					visible = true
				}
			}
		case "response.function_call_arguments.delta":
			if s.hidden[idx] {
				s.tool(idx).args.WriteString(wireText(o, "delta"))
			}
		case "response.function_call_arguments.done":
			if s.hidden[idx] {
				s.tool(idx).complete = wireText(o, "arguments")
			}
		case "response.output_text.delta", "response.refusal.delta":
			visible = wireText(o, "delta") != ""
		case "response.completed", "response.incomplete":
			s.incomplete = kind == "response.incomplete"
			terminal = true
			res := wireObject(o, "response")
			if a, ok := res["output"].([]any); ok {
				out := make([]any, 0, len(a))
				for index, v := range a {
					m, _ := v.(map[string]any)
					if wireText(m, "type") == "function_call" && privateTool(wireText(m, "name"), s.client) {
						tool := s.tool(index)
						tool.name = wireText(m, "name")
						tool.id = wireText(m, "call_id")
						s.hidden[index] = true
						if args := wireText(m, "arguments"); args != "" {
							tool.complete = args
						}
						if wireText(m, "status") == "completed" {
							tool.confirmed = true
						} else if wireText(m, "status") != "" {
							tool.confirmed = false
						}
						continue
					}
					out = append(out, v)
				}
				res["output"] = out
			}
		}
		if _, ok := o["output_index"]; ok {
			private = s.hidden[idx]
			if !private {
				s.reindex(o, "output_index", idx)
			}
		}
	}
	s.terminal = s.terminal || terminal
	if private {
		s.toolBytes += len(data)
		if s.toolBytes > maxSSEFrameBytes {
			return kernelError("invalid_stream", "Compression tool response too large", 502)
		}
		return nil
	}
	b, _ := json.Marshal(o)
	frame := sseFrame{event: kind, data: b}
	wire := frame.eventPayload(kind)
	if terminal || (s.format == claude && kind == "message_delta") {
		return s.keepTail(wire)
	}
	return s.send(wire, visible)
}

func (s *kernelStream) reindex(o map[string]any, field string, index int) {
	n := index
	for i := range s.hidden {
		if i < index {
			n--
		}
	}
	o[field] = n
}

func (s *kernelStream) tool(i int) *streamedTool {
	if s.tools[i] == nil {
		s.tools[i] = &streamedTool{}
	}
	return s.tools[i]
}

func (s *kernelStream) chatEvent(o map[string]any) *rpcError {
	choices, _ := o["choices"].([]any)
	if len(choices) == 0 {
		data, _ := json.Marshal(o)
		if o["usage"] != nil {
			return s.keepTail(data)
		}
		return s.send(data, false)
	}
	choice, _ := choices[0].(map[string]any)
	delta := wireObject(choice, "delta")
	s.chatBase(o)
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, v := range calls {
			c, _ := v.(map[string]any)
			t := s.tool(wireIndex(c, "index"))
			if id := wireText(c, "id"); id != "" {
				t.id = id
			}
			fn := wireObject(c, "function")
			name, args := wireText(fn, "name"), wireText(fn, "arguments")
			s.toolBytes += len(name) + len(args)
			if s.toolBytes > maxSSEFrameBytes {
				return kernelError("invalid_stream", "Compression tool response too large", 502)
			}
			t.name += name
			t.args.WriteString(args)
		}
		delete(delta, "tool_calls")
	}
	finish := wireText(choice, "finish_reason")
	if finish != "" {
		s.finished = true
		s.stopReason = finish
		s.chatEnd = mapCopy(choice)
		s.chatEnd["delta"] = map[string]any{}
		choice["finish_reason"] = nil
	}
	if len(delta) == 0 {
		return nil
	}
	visible := wireText(delta, "content") != "" || wireText(delta, "refusal") != ""
	b, _ := json.Marshal(o)
	return s.send(b, visible)
}

func (s *kernelStream) send(data []byte, visible bool) *rpcError {
	if visible {
		s.round.Visible = true
	}
	if s.round.Visible || s.round.External {
		return s.publish(data)
	}
	s.buffered += len(data)
	if s.buffered+s.tailBytes > maxSSEFrameBytes {
		return kernelError("invalid_stream", "Uncommitted stream prefix too large", 502)
	}
	s.pending = append(s.pending, data)
	return nil
}

func (s *kernelStream) publish(b []byte) *rpcError {
	if e := s.flush(); e != nil {
		return e
	}
	if e := s.emit(b); e != nil {
		return kernelError("stream_delivery", "CPA could not deliver the model stream", 502)
	}
	return nil
}

func (s *kernelStream) flush() *rpcError {
	for _, b := range s.pending {
		if err := s.emit(b); err != nil {
			return kernelError("stream_delivery", "CPA could not deliver the model stream", 502)
		}
	}
	s.pending = nil
	s.buffered = 0
	return nil
}
func wireText(m map[string]any, key string) string { v, _ := m[key].(string); return v }
func wireObject(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func wireIndex(m map[string]any, key string) int {
	n, _ := m[key].(json.Number)
	i, _ := strconv.Atoi(string(n))
	return i
}

func mapCopy(m map[string]any) map[string]any {
	r := make(map[string]any, len(m))
	for k, v := range m {
		r[k] = v
	}
	return r
}

func (s *kernelStream) chatBase(o map[string]any) {
	if s.base != nil {
		return
	}
	s.base = mapCopy(o)
	delete(s.base, "choices")
	delete(s.base, "usage")
}

func (s *kernelStream) chatFrame(choice any) []byte {
	o := mapCopy(s.base)
	o["choices"] = []any{choice}
	b, _ := json.Marshal(o)
	return b
}

func (s *kernelStream) keepTail(frame []byte) *rpcError {
	s.tailBytes += len(frame)
	if s.buffered+s.tailBytes > maxSSEFrameBytes {
		return kernelError("invalid_stream", "Model stream trailer too large", 502)
	}
	s.tail = append(s.tail, frame)
	return nil
}
