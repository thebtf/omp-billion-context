package main

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Several provider turns can make one client turn. Envelopes and content indices belong to the client stream.
type continuationStream struct {
	format         protocol
	emit           func([]byte) error
	started        bool
	id             string
	next, sequence int
	prior, current []any
	retained       int
}

func (c *continuationStream) begin() func([]byte) error {
	continuing, base := c.started, c.next
	c.prior = append(c.prior, c.current...)
	c.current = nil
	return func(frame []byte) error {
		var body map[string]any
		var event sseFrame
		data := frame
		if c.format != chat {
			for _, line := range bytes.Split(frame, []byte{'\n'}) {
				event.add(bytes.TrimSuffix(line, []byte{'\r'}))
			}
			data = event.data
		}
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber()
		if d.Decode(&body) != nil {
			return errors.New("invalid client stream frame")
		}
		kind := wireText(body, "type")
		changed := false
		switch c.format {
		case chat:
			if c.id == "" {
				c.id = wireText(body, "id")
			}
			if continuing && c.id != "" {
				body["id"] = c.id
				changed = true
			}
			if continuing {
				if choices, ok := body["choices"].([]any); ok && len(choices) > 0 {
					o, _ := choices[0].(map[string]any)
					delta := wireObject(o, "delta")
					if delta != nil {
						delete(delta, "role")
						changed = true
					}
				}
			}
		case claude:
			if kind == "message_start" && continuing {
				return nil
			}
			if _, has := body["index"]; has {
				i := wireIndex(body, "index") + base
				if i >= c.next {
					c.next = i + 1
				}
				if base != 0 {
					body["index"] = i
					changed = true
				}
			}
		case responses:
			if continuing && (kind == "response.created" || kind == "response.in_progress") {
				return nil
			}
			res := wireObject(body, "response")
			if c.id == "" {
				c.id = wireText(res, "id")
			}
			if continuing && c.id != "" {
				if res != nil {
					res["id"] = c.id
					changed = true
				}
				if body["response_id"] != nil {
					body["response_id"] = c.id
					changed = true
				}
			}
			if _, has := body["sequence_number"]; has {
				n := wireIndex(body, "sequence_number")
				if continuing && n < c.sequence {
					n = c.sequence
					body["sequence_number"] = n
					changed = true
				}
				c.sequence = n + 1
			}
			if _, has := body["output_index"]; has {
				i := wireIndex(body, "output_index") + base
				if i >= c.next {
					c.next = i + 1
				}
				if base != 0 {
					body["output_index"] = i
					changed = true
				}
			}
			if kind == "response.output_item.done" && body["item"] != nil {
				c.retained += len(data)
				if c.retained > maxResponseBytes {
					return errors.New("retained stream output too large")
				}
				c.current = append(c.current, body["item"])
			}
			if continuing && res != nil && (kind == "response.completed" || kind == "response.incomplete") {
				if current, ok := res["output"].([]any); ok {
					items := make([]any, 0, len(c.prior)+len(current))
					items = append(items, c.prior...)
					res["output"] = append(items, current...)
					changed = true
				}
			}
		}
		c.started = true
		if !changed {
			return c.emit(frame)
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		if c.format != chat {
			event.data = encoded
			encoded = event.eventPayload(kind)
		}
		return c.emit(encoded)
	}
}
