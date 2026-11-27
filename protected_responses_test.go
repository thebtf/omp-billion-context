package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestProtectedGroupIntegrity(t *testing.T) {
	const r = `{"type":"reasoning","id":"r1","encrypted_content":"cipher","summary":[],"serial":9007199254740993}`
	const c = `{"type":"function_call","id":"fc1","name":"lookup","arguments":"{}","call_id":"c1"}`
	const o = `{"type":"function_call_output","output":"result","call_id":"c1"}`
	const user = `{"role":"user","content":"next turn"}`
	triple := strings.Join([]string{r, c, o}, ",")
	second := strings.NewReplacer("r1", "r2", "fc1", "fc2", "c1", "c2").Replace(triple)
	original := "[" + triple + "," + second + "," + user + "]"
	groups, ok := protectedInput(json.RawMessage(original))
	if !ok || len(groups) != 2 {
		t.Fatal("complete groups rejected")
	}
	body := func(input string) []byte {
		return []byte(`{"reasoning":{"context":"current_turn"},"input":` + input + `}`)
	}
	for _, tc := range []struct {
		name, input string
		want        bool
	}{
		{"group untouched after folded prefix", "[" + user + "," + original[1:], true},
		{"rewritten JSON without value change", strings.Replace(original, `"id":"r1","encrypted_content":"cipher"`, `"encrypted_content":"cipher", "id" : "r1"`, 1), true},
		{"changed ciphertext", strings.Replace(original, "cipher", "new", 1), false},
		{"changed large integer", strings.Replace(original, "9007199254740993", "9007199254740992", 1), false},
		{"missing output", strings.Replace(original, ","+o, "", 1), false},
		{"duplicate output", strings.Replace(original, o, o+","+o, 1), false},
		{"orphaned output", strings.Replace(original, `"output":"result","call_id":"c1"`, `"output":"result","call_id":"wrong"`, 1), false},
		{"different arguments", strings.Replace(original, `"arguments":"{}"`, `"arguments":"null"`, 1), false},
		{"duplicate reasoning id", strings.Replace(original, "r2", "r1", 1), false},
		{"group order changed", "[" + second + "," + triple + "," + user + "]", false},
		{"protected group dropped", "[" + triple + "," + user + "]", false},
		{"group inserted", "[" + triple + "," + original[1:], false},
		{"current user removed", strings.Replace(original, ","+user, "", 1), false},
		{"provider reference mixed in", "[{\"type\":\"item_reference\",\"id\":\"state\"}," + original[1:], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := groups.intact(body(tc.input)); got != tc.want {
				t.Fatalf("intact=%t, want %t", got, tc.want)
			}
		})
	}
	for _, field := range []string{`"previous_response_id":"prior",`, `"background":true,`, `"conversation":{"id":"server"},`} {
		raw := body(original)
		if groups.intact(append([]byte("{"+field), raw[1:]...)) {
			t.Fatal("accepted server-owned continuation")
		}
	}
}

func TestAmbiguousReasoningModeNeverEntersProtectedReplay(t *testing.T) {
	meta := map[string]any{"caller_scope": "client", "request_path": paths[responses]}
	h := http.Header{"X-Session-Id": {"s"}}
	_, _, groups := classify("m", responses, h, meta, []byte(protectedFixture))
	if len(groups) != 1 {
		t.Fatal("control not admitted")
	}
	for _, pair := range [][2]string{{`"context":"current_turn"`, `"Context":"current_turn"`}, {`"context":"current_turn"`, `"context":"all_turns","Context":"current_turn"`}, {`"context":"current_turn"`, `"context":"all_turns","context":"current_turn"`}, {`"reasoning":`, `"Reasoning":`}} {
		wire := []byte(strings.Replace(protectedFixture, pair[0], pair[1], 1))
		reason, _, _ := classify("m", responses, h, meta, wire)
		if reason == compressible || groups.intact(wire) {
			t.Fatal("ambiguous protected context accepted")
		}
	}
}
