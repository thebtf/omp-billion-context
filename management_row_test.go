package main

import (
	"encoding/json"
	"testing"
)

// A field that held nothing but wrapper tags must not keep them: the row would
// otherwise render the markup even though a sibling names the work.
func TestReadableRowClearsAFieldThatIsOnlyTags(t *testing.T) {
	row := map[string]json.RawMessage{
		"id":             json.RawMessage(`"row-1"`),
		"title":          json.RawMessage(`"<job></job>"`),
		"firstBlockHint": json.RawMessage(`"Real work title"`),
	}
	if !readableRow(row) {
		t.Fatal("no change was reported")
	}
	var title, hint, id string
	_ = json.Unmarshal(row["title"], &title)
	_ = json.Unmarshal(row["firstBlockHint"], &hint)
	_ = json.Unmarshal(row["id"], &id)
	if title != "Real work title" {
		t.Fatalf("title = %q, want the informative sibling", title)
	}
	if hint != "Real work title" {
		t.Fatalf("hint = %q", hint)
	}
	if id != "row-1" {
		t.Fatalf("identifier changed: %q", id)
	}
}

func TestReadableRowEmptiesATagOnlyFieldWithNoSibling(t *testing.T) {
	row := map[string]json.RawMessage{"title": json.RawMessage(`"<system-reminder></system-reminder>"`)}
	if !readableRow(row) {
		t.Fatal("a tag-only field must be cleared")
	}
	var title string
	_ = json.Unmarshal(row["title"], &title)
	if title != "" {
		t.Fatalf("title = %q, want it cleared", title)
	}
}

func TestReadableRowLeavesAGoodTitleAlone(t *testing.T) {
	row := map[string]json.RawMessage{
		"title":          json.RawMessage(`"Fix the release protocol"`),
		"firstBlockHint": json.RawMessage(`"<user> another turn"`),
	}
	before := string(row["title"])
	readableRow(row)
	if string(row["title"]) != before {
		t.Fatalf("a good title was rewritten: %s", row["title"])
	}
}
