package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

type replayItem map[string]any

type protectedGroups [][3]replayItem

func (item replayItem) str(key string) string {
	s, _ := item[key].(string)
	return s
}

// UseNumber makes a changed large integer distinguishable without parsing it as
// a float. Different numeric spellings may conservatively trigger full replay.
func protectedInput(input json.RawMessage) (protectedGroups, bool) {
	if len(input) == 0 || input[0] != '[' {
		return nil, true
	}
	var items []replayItem
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if dec.Decode(&items) != nil {
		return nil, false
	}
	var groups protectedGroups
	ids := map[string]bool{}
	calls := map[string]int{}
	outputs := map[string]int{}
	for i, item := range items {
		if id := item.str("id"); id != "" {
			if ids[id] {
				return nil, false
			}
			ids[id] = true
		}
		switch item.str("type") {
		case "function_call":
			calls[item.str("call_id")]++
		case "function_call_output":
			outputs[item.str("call_id")]++
		case "", "message", "reasoning":
		default:
			return nil, false
		}
		if _, has := item["encrypted_content"]; !has {
			continue
		}
		if item.str("type") != "reasoning" || item.str("id") == "" || item.str("encrypted_content") == "" || i+2 >= len(items) {
			return nil, false
		}
		call, result := items[i+1], items[i+2]
		id := call.str("call_id")
		if call.str("type") != "function_call" || call.str("name") == "" || id == "" || result.str("type") != "function_call_output" || result.str("call_id") != id {
			return nil, false
		}
		if _, ok := call["arguments"].(string); !ok {
			return nil, false
		}
		if result["output"] == nil {
			return nil, false
		}
		groups = append(groups, [3]replayItem{item, call, result})
	}
	for _, group := range groups {
		id := group[1].str("call_id")
		if calls[id] != 1 || outputs[id] != 1 {
			return nil, false
		}
	}
	return groups, true
}

// Go's struct decoder matches keys case-insensitively and accepts duplicates.
// A provider is not required to resolve those ambiguities the same way.
func exactReplayField(object []byte, name string) (json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(object))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	seen := map[string]bool{}
	var field, scratch json.RawMessage
	for d.More() {
		tok, err := d.Token()
		key, ok := tok.(string)
		if err != nil || !ok || seen[key] || strings.ToLower(key) != key {
			return nil, false
		}
		seen[key] = true
		if key == name {
			if err := d.Decode(&field); err != nil {
				return nil, false
			}
		} else if err := d.Decode(&scratch); err != nil {
			return nil, false
		}
	}
	_, err := d.Token()
	return field, err == nil
}

func currentTurn(raw json.RawMessage) bool {
	value, ok := exactReplayField(raw, "context")
	var mode string
	return ok && json.Unmarshal(value, &mode) == nil && mode == "current_turn"
}

func (groups protectedGroups) intact(body []byte) bool {
	var replay struct {
		Input        json.RawMessage `json:"input"`
		Previous     string          `json:"previous_response_id"`
		Conversation json.RawMessage `json:"conversation"`
		Background   bool            `json:"background"`
	}
	reasoning, exact := exactReplayField(body, "reasoning")
	if !exact || !currentTurn(reasoning) || json.Unmarshal(body, &replay) != nil || replay.Previous != "" || replay.Background || (len(replay.Conversation) != 0 && string(replay.Conversation) != "null") {
		return false
	}
	got, ok := protectedInput(replay.Input)
	if !ok || !reflect.DeepEqual(got, groups) {
		return false
	}
	// Internal fold receipts may follow the latest user message. The protected
	// reasoning must still belong to an earlier turn in the candidate replay.
	var items []json.RawMessage
	if json.Unmarshal(replay.Input, &items) != nil {
		return false
	}
	for i := len(items) - 1; i >= 0; i-- {
		var kind struct{ Type string }
		if json.Unmarshal(items[i], &kind) != nil || strings.EqualFold(kind.Type, "reasoning") {
			return false
		}
		if newUserTurn(items[i]) {
			return true
		}
	}
	return false
}
