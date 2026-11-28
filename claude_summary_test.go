package main

import "testing"

func TestAuxiliarySummaryUsesOnlyVisibleCompletedText(t *testing.T) {
	for _, c := range []struct {
		format     protocol
		body, want string
	}{
		{claude, `{"type":"message","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"public"}]}`, "public"},
		{claude, `{"type":"message","stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`, ""},
		{claude, `{"type":"message","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"hidden"}]}`, ""},
		{responses, `{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`, ""},
		{chat, `{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`, ""},
	} {
		if got := visibleAnswer(c.format, []byte(c.body)); got != c.want {
			t.Fatalf("%s summary=%q, want %q", c.format, got, c.want)
		}
	}
}
