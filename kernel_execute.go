package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
)

type kernelToolCall struct {
	Name string
	Args json.RawMessage
	ID   string
}
type kernelRound struct {
	Calls    []kernelToolCall
	External bool
	Visible  bool
}

func privateTool(name string, client map[string]bool) bool {
	return (name == "compress" || name == "acp_status") && !client[name]
}

func toolNames(body []byte) map[string]bool {
	var b struct {
		Tools []struct {
			Name     string
			Function struct{ Name string }
		}
	}
	_ = json.Unmarshal(body, &b)
	names := make(map[string]bool, len(b.Tools))
	for _, t := range b.Tools {
		if t.Function.Name != "" {
			names[t.Function.Name] = true
		} else {
			names[t.Name] = true
		}
	}
	return names
}

func toolArgs(raw any) json.RawMessage {
	if s, ok := raw.(string); ok {
		if json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
		return json.RawMessage(`{}`)
	}
	b, _ := json.Marshal(raw)
	return b
}

var errUnfinishedPrivate = errors.New("provider did not complete the private tool call")

func kernelJSON(format protocol, data []byte, client map[string]bool, marker string) (kernelRound, []byte, error) {
	var msg map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&msg) != nil || msg == nil {
		return kernelRound{}, nil, errors.New("invalid provider response")
	}
	r := kernelRound{}
	changed := false
	unfinished := false
	str := func(o map[string]any, key string) string { s, _ := o[key].(string); return s }
	prefix := func(o map[string]any, key string) {
		if text, ok := o[key].(string); ok && text != "" {
			r.Visible = true
			if marker != "" {
				o[key] = marker + "\n" + text
				marker = ""
				changed = true
			}
		}
	}
	filter := func(items []any, kind protocol) []any {
		out := make([]any, 0, len(items))
		for _, item := range items {
			o, _ := item.(map[string]any)
			name, id, args := "", "", any(nil)
			switch kind {
			case chat:
				fn, _ := o["function"].(map[string]any)
				name, id, args = str(fn, "name"), str(o, "id"), fn["arguments"]
			case claude:
				if str(o, "type") == "tool_use" {
					name, id, args = str(o, "name"), str(o, "id"), o["input"]
				} else if str(o, "type") == "text" {
					prefix(o, "text")
				}
			case responses:
				if str(o, "type") == "function_call" {
					name, id, args = str(o, "name"), str(o, "call_id"), o["arguments"]
				} else if str(o, "type") == "message" {
					parts, _ := o["content"].([]any)
					for _, part := range parts {
						p, _ := part.(map[string]any)
						if str(p, "type") == "output_text" {
							prefix(p, "text")
						}
					}
				}
			}
			if privateTool(name, client) {
				if kind == responses && (str(msg, "status") != "completed" || (str(o, "status") != "" && str(o, "status") != "completed")) {
					unfinished = true
				}
				r.Calls = append(r.Calls, kernelToolCall{name, toolArgs(args), id})
				changed = true
				continue
			}
			if name != "" {
				r.External = true
			}
			out = append(out, item)
		}
		return out
	}
	switch format {
	case chat:
		choices, _ := msg["choices"].([]any)
		for _, choice := range choices {
			o, _ := choice.(map[string]any)
			m, _ := o["message"].(map[string]any)
			prefix(m, "content")
			if calls, ok := m["tool_calls"].([]any); ok {
				before := len(r.Calls)
				m["tool_calls"] = filter(calls, chat)
				if len(r.Calls) > before && (str(o, "finish_reason") == "length" || str(o, "finish_reason") == "content_filter") {
					unfinished = true
				}
			}
		}
	case claude:
		if content, ok := msg["content"].([]any); ok {
			msg["content"] = filter(content, claude)
			if len(r.Calls) > 0 && str(msg, "stop_reason") == "max_tokens" {
				unfinished = true
			}
		}
	case responses:
		if output, ok := msg["output"].([]any); ok {
			msg["output"] = filter(output, responses)
		}
	}
	if unfinished {
		return kernelRound{}, nil, errUnfinishedPrivate
	}
	if !changed {
		return r, data, nil
	}
	b, err := json.Marshal(msg)
	return r, b, err
}

func kernelError(code, message string, status int) *rpcError {
	return &rpcError{Code: code, Message: message, HTTPStatus: status}
}

func isOverflow(f *rpcError) bool {
	if f == nil {
		return false
	}
	s := strings.ToLower(f.Code + " " + f.Message)
	return (f.HTTPStatus == 400 || f.HTTPStatus == 413 || f.HTTPStatus == 422) && (strings.Contains(s, "context_length") || strings.Contains(s, "context window") || strings.Contains(s, "context length") || strings.Contains(s, "context_window") || strings.Contains(s, "prompt is too long"))
}

type kernelTurn struct {
	p        *plugin
	req      executorRequest
	session  string
	groups   protectedGroups
	client   map[string]bool
	original json.RawMessage
}

func (t *kernelTurn) request(body []byte, stream bool) hostModelRequest {
	path, _ := t.req.Metadata["request_path"].(string)
	scope, _ := t.req.Metadata["caller_scope"].(string)
	return hostModelRequest{EntryProtocol: t.req.SourceFormat, ExitProtocol: t.req.Format, Model: t.req.Model, Headers: t.req.Headers, Query: t.req.Query, Alt: t.req.Alt, Path: path, CallerScope: scope, HostCallbackID: t.req.HostCallbackID, Body: body, Stream: stream}
}

func (t *kernelTurn) run(ctx context.Context, emit func([]byte) error) ([]byte, *rpcError) {
	output := continuationStream{format: t.req.Format, emit: emit}
	feedback, marker, phase := "", "", "turn"
	failures := map[string]bool{}
	preflight := false
	for loop := 0; loop < 8; loop++ {
		if ctx.Err() != nil {
			return nil, sidecarFailure(ctx.Err())
		}
		fields := map[string]any{"session": t.session, "format": t.req.Format, "body": t.original, "feedback": feedback}
		var prepared struct{ Body json.RawMessage }
		if err := t.p.kernelCall(ctx, "prepare", fields, &prepared); err != nil {
			return nil, engineFailure(err)
		}
		fallback := len(t.groups) > 0 && !t.groups.intact(prepared.Body)
		if fallback {
			prepared.Body = t.original
			t.p.protectedFallbacks.Add(1)
		}
		bound, stream, err := bindModelBody(prepared.Body, t.req.Model)
		if err != nil || stream != t.req.Stream {
			return nil, kernelError("invalid_projection", "Compression changed the request transport", 502)
		}
		if err := ctx.Err(); err != nil {
			return nil, sidecarFailure(err)
		}
		call := t.p.cacheObserved(ctx, t.session, t.req.Format, phase, t.p.call)
		req := t.request(bound, stream)
		var res hostModelResponse
		var round kernelRound
		var payload []byte
		var failure *rpcError
		if stream {
			failure = call(hostModelExecuteStream, req, &res)
			if failure == nil && res.StreamID != "" && res.StatusCode >= 200 && res.StatusCode < 300 {
				deliver := output.begin()
				round, failure = readKernelStream(ctx, t.req.Format, t.client, call, res.StreamID, func(frame []byte) error {
					if marker != "" {
						var used bool
						frame, used = withStreamMarker(t.req.Format, frame, marker)
						if used {
							marker = ""
						}
					}
					return deliver(frame)
				})
			} else if failure == nil {
				failure = providerFailure(res)
			}
		} else {
			failure = call(hostModelExecute, req, &res)
			if failure == nil && (res.StatusCode < 200 || res.StatusCode >= 300) {
				failure = providerFailure(res)
			}
			if failure == nil {
				round, payload, err = kernelJSON(t.req.Format, res.Body, t.client, marker)
				if err != nil {
					if errors.Is(err, errUnfinishedPrivate) {
						failure = kernelError("incomplete_tool_call", "The provider did not complete the compression call; no fold was saved", 422)
					} else {
						failure = kernelError("invalid_response", "CPA returned an invalid model response", 502)
					}
				}
			}
		}
		if failure != nil {
			if !preflight && !round.Visible && !fallback && len(t.groups) == 0 && isOverflow(failure) {
				preflight = true
				if f := t.recoverOverflow(ctx); f != nil {
					return nil, f
				}
				feedback = "Continue the user's task using the saved context."
				phase = "after_fold"
				continue
			}
			return nil, failure
		}
		if len(round.Calls) == 0 {
			return payload, nil
		}
		if fallback {
			return nil, kernelError("protected_replay", "Cannot apply a fold on a protected fallback", 409)
		}
		if len(round.Calls) > 8 {
			return nil, kernelError("compression_limit", "Too many compression operations in one response", 422)
		}
		var receipts []string
		phase = "turn"
		for _, cmd := range round.Calls {
			fields := map[string]any{"session": t.session, "format": t.req.Format, "body": t.original, "name": cmd.Name, "args": cmd.Args}
			var reply struct {
				OK               bool
				Feedback, Marker string
			}
			if err := t.p.kernelCall(ctx, "tool", fields, &reply); err != nil {
				return nil, engineFailure(err)
			}
			if !reply.OK {
				k := cmd.Name + string(cmd.Args)
				if failures[k] {
					return nil, kernelError("compression_refused", reply.Feedback, 422)
				}
				failures[k] = true
			} else if cmd.Name == "compress" {
				phase = "after_fold"
				marker = reply.Marker
			}
			receipts = append(receipts, reply.Feedback)
		}
		if round.External {
			return payload, nil
		}
		feedback = strings.Join(receipts, "\n")
		if stream && round.Visible {
			feedback += " Continue your answer after the text already shown to the user; do not repeat the preamble."
		}
	}
	return nil, kernelError("compression_limit", "Compression did not converge; no further model request was sent", 422)
}

func providerFailure(res hostModelResponse) *rpcError {
	var b struct {
		Error struct{ Code, Message string }
	}
	_ = json.Unmarshal(res.Body, &b)
	f := kernelError("host_model_response", "CPA could not complete the model request", modelStatus(res.StatusCode))
	if isOverflow(kernelError(b.Error.Code, b.Error.Message, f.HTTPStatus)) {
		f.Code = "context_length_exceeded"
		f.Message = "The provider rejected the context length"
	}
	return f
}

// Prefix the first client-visible text delta only. IDs and sequence numbers remain the provider's.
func withStreamMarker(format protocol, frame []byte, marker string) ([]byte, bool) {
	data := bytes.TrimSpace(frame)
	var sse sseFrame
	framed := len(data) > 0 && data[0] != '{'
	if framed {
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			sse.add(bytes.TrimSuffix(line, []byte{'\r'}))
		}
		data = sse.data
	}
	if !bytes.Contains(data, []byte(`"text"`)) && !bytes.Contains(data, []byte(`"content"`)) && format != responses {
		return frame, false
	}
	var o map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&o) != nil {
		return frame, false
	}
	m := o
	k := ""
	switch format {
	case chat:
		choices, _ := o["choices"].([]any)
		if len(choices) == 0 {
			return frame, false
		}
		c, _ := choices[0].(map[string]any)
		m, _ = c["delta"].(map[string]any)
		k = "content"
	case claude:
		m, _ = o["delta"].(map[string]any)
		if m["type"] != "text_delta" {
			return frame, false
		}
		k = "text"
	case responses:
		if o["type"] != "response.output_text.delta" {
			return frame, false
		}
		k = "delta"
	}
	v, _ := m[k].(string)
	if v == "" {
		return frame, false
	}
	m[k] = marker + "\n" + v
	b, _ := json.Marshal(o)
	if framed {
		sse.data = b
		b = sse.eventPayload(sse.event)
	}
	return b, true
}
