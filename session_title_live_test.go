package main

import (
	"encoding/json"
	"testing"
)

// These rows come from a live CPA overview: bili falls back to the first block
// hint, which is raw conversation text, so wrapper tags became the title.
func TestWithReadableTitlesOnObservedRows(t *testing.T) {
	body := `{"sessions":[
		{"id":"39042b4e0104785cbe0d182c9ea49914859da3647c9cd6dcd4ce752f283a1d6f","live":true,"requests":3,
		 "firstBlockHint":"<system-reminder> Today: 202..."},
		{"id":"345161a76bf5284362e96ccd8fbd72a9788f4f014607e8f0d2b8b8e6dcf47f0a","live":true,
		 "firstBlockHint":"<user> FULFaccept5ae09ecdaf..."},
		{"id":"4ebb79facfff5ad0078e3524a139369ed3be8ce1d89844077713e9ed70aa9b0f","live":true,
		 "firstBlockHint":"<job> Background subagent. It..."},
		{"id":"41e4ad9c49a98734601904ffc1520a9dd59fb2cdd43a32842c9cdfa8db0b8c8a","live":true,
		 "firstBlockHint":"<chat> <user> я тебя сейчас у..."},
		{"id":"5c8402e4e147a087054af0de40dfe78afe4d375c49299175bc3a570ec4f8d6a2","live":true,
		 "firstBlockHint":"plain conversation with no wrapper"}
	],"total":5}`
	out := withReadableTitles([]byte(body))
	var decoded struct {
		Sessions []map[string]any `json:"sessions"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if decoded.Total != 5 || len(decoded.Sessions) != 5 {
		t.Fatalf("page shape changed: %s", out)
	}
	wants := []string{
		"Today: 202...",
		"FULFaccept5ae09ecdaf...",
		"Background subagent. It...",
		"я тебя сейчас у...",
		"plain conversation with no wrapper",
	}
	for i, want := range wants {
		row := decoded.Sessions[i]
		if got := row["firstBlockHint"]; got != want {
			t.Fatalf("row %d title = %q, want %q", i, got, want)
		}
		if row["id"] == "" || row["live"] != true {
			t.Fatalf("row %d lost identity: %v", i, row)
		}
	}
	if decoded.Sessions[1]["requests"] != nil {
		// rows without the field must not gain one
		t.Fatalf("row 1 gained a field: %v", decoded.Sessions[1])
	}
}
