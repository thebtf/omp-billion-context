package main

import (
	"encoding/json"
	"testing"
)

// The Overview page renders its list from overview.recent, not from the
// Sessions endpoint, so that array needs the same display cleanup.
func TestWithBypassSummaryCleansOverviewRecentRows(t *testing.T) {
	p := newPlugin(nil)
	defer p.shutdown()
	body := `{"overview":{"requests":7,"recent":[
		{"id":"a1","live":true,"requests":3,"firstBlockHint":"<system-reminder> Today: 202-11-27; current working dir"},
		{"id":"b2","live":true,"title":"<job> Background subagent. It...","firstBlockHint":"Real work title"},
		{"id":"c3","live":false,"title":"plain title"}
	]},"notices":[]}`
	out := p.withBypassSummary([]byte(body))

	var decoded struct {
		Overview struct {
			Requests int              `json:"requests"`
			Recent   []map[string]any `json:"recent"`
		} `json:"overview"`
		Notices  []any           `json:"notices"`
		Compress json.RawMessage `json:"cpaCompression"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if decoded.Overview.Requests != 7 || decoded.Notices == nil || len(decoded.Compress) == 0 {
		t.Fatalf("payload shape changed: %s", out)
	}
	if len(decoded.Overview.Recent) != 3 {
		t.Fatalf("rows = %d", len(decoded.Overview.Recent))
	}
	first := decoded.Overview.Recent[0]
	if first["firstBlockHint"] != "Today: 202-11-27; current working dir" {
		t.Fatalf("first hint = %v", first["firstBlockHint"])
	}
	if first["id"] != "a1" || first["live"] != true || first["requests"] != float64(3) {
		t.Fatalf("non-display fields changed: %v", first)
	}
	second := decoded.Overview.Recent[1]
	if second["title"] != "Real work title" {
		t.Fatalf("a generic job title must yield to the real turn, got %v", second["title"])
	}
	if decoded.Overview.Recent[2]["title"] != "plain title" {
		t.Fatalf("a good title was overwritten: %v", decoded.Overview.Recent[2]["title"])
	}
}

func TestWithBypassSummaryToleratesMissingRecent(t *testing.T) {
	p := newPlugin(nil)
	defer p.shutdown()
	for _, body := range []string{`{"overview":{}}`, `{"overview":{"recent":"nope"}}`, `{"overview":"not-an-object"}`, `{}`} {
		if out := p.withBypassSummary([]byte(body)); json.Valid(out) == false {
			t.Fatalf("payload is not valid JSON: %s", out)
		}
	}
}
