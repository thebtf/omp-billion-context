package main

import (
	"encoding/json"
	"testing"
)

// The Overview page renders its list from recent, not from the
// Sessions endpoint, so that array needs the same display cleanup.
func TestWithBypassSummaryCleansOverviewRecentRows(t *testing.T) {
	p := newPlugin(nil)
	defer p.shutdown()
	body := `{"requests":7,"recent":[
		{"id":"a1","live":true,"requests":3,"firstBlockHint":"<system-reminder> Today: 202-11-27; current working dir"},
		{"id":"b2","live":true,"title":"<job> Background subagent. It...","firstBlockHint":"Real work title"},
		{"id":"c3","live":false,"title":"plain title"}
	],"notices":[]}`
	out := p.withBypassSummary([]byte(body))

	var decoded struct {
		Requests int              `json:"requests"`
		Recent   []map[string]any `json:"recent"`
		Notices  []any            `json:"notices"`
		Compress json.RawMessage  `json:"cpaCompression"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if decoded.Requests != 7 || decoded.Notices == nil || len(decoded.Compress) == 0 {
		t.Fatalf("payload shape changed: %s", out)
	}
	if len(decoded.Recent) != 3 {
		t.Fatalf("rows = %d", len(decoded.Recent))
	}
	first := decoded.Recent[0]
	// The date line is the only part of the injected preamble that differs
	// between sessions, so the repeated working-directory tail is dropped.
	if first["firstBlockHint"] != "Today: 202-11-27" {
		t.Fatalf("first hint = %v", first["firstBlockHint"])
	}
	if first["id"] != "a1" || first["live"] != true || first["requests"] != float64(3) {
		t.Fatalf("non-display fields changed: %v", first)
	}
	second := decoded.Recent[1]
	if second["title"] != "Real work title" {
		t.Fatalf("a generic job title must yield to the real turn, got %v", second["title"])
	}
	if decoded.Recent[2]["title"] != "plain title" {
		t.Fatalf("a good title was overwritten: %v", decoded.Recent[2]["title"])
	}
}

func TestWithBypassSummaryToleratesMissingRecent(t *testing.T) {
	p := newPlugin(nil)
	defer p.shutdown()
	for _, body := range []string{`{"sessions":0}`, `{"recent":"nope"}`, `{"recent":null}`, `{}`} {
		if out := p.withBypassSummary([]byte(body)); json.Valid(out) == false {
			t.Fatalf("payload is not valid JSON: %s", out)
		}
	}
}
