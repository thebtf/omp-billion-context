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

// A rename the operator typed is the most deliberate identity a session has: it
// outranks the first-turn hint, and its full length must survive the bridge so
// the row tooltip and the details page can show all of it.
func TestReadableRowPromotesAHostTitleWithoutShorteningIt(t *testing.T) {
	long := "Rename the durable session list so the title fills the column instead of a hex stub"
	row := map[string]json.RawMessage{
		"id":             json.RawMessage(`"row-9"`),
		"hostTitle":      json.RawMessage(`"` + long + `"`),
		"firstBlockHint": json.RawMessage(`"<system-reminder> Today: 2026-11-28; current working dir D:\\work"`),
	}
	if !readableRow(row) {
		t.Fatal("no change was reported")
	}
	var title, hostTitle, id string
	_ = json.Unmarshal(row["title"], &title)
	_ = json.Unmarshal(row["hostTitle"], &hostTitle)
	_ = json.Unmarshal(row["id"], &id)
	if title != long || hostTitle != long {
		t.Fatalf("title = %q, hostTitle = %q, want the full rename", title, hostTitle)
	}
	if id != "row-9" {
		t.Fatalf("identifier changed: %q", id)
	}
}

// A row that only carries the preamble every session shares still has to be
// distinguishable, so the date is kept and the repeated tail is dropped.
func TestReadableRowKeepsOnlyTheDateOfAPreambleOnlyRow(t *testing.T) {
	row := map[string]json.RawMessage{
		"firstBlockHint": json.RawMessage(`"<system-reminder> Today: 2026-11-28; current working dir D:\\work; you have 3 files"`),
	}
	readableRow(row)
	var hint string
	_ = json.Unmarshal(row["firstBlockHint"], &hint)
	if hint != "Today: 2026-11-28" {
		t.Fatalf("hint = %q, want only the part that dates the session", hint)
	}
}

// Display cleanup must not rewrite a payload it cannot improve.
func TestReadableRowReportsNoChangeForAnUntouchedRow(t *testing.T) {
	row := map[string]json.RawMessage{
		"id":    json.RawMessage(`"row-3"`),
		"title": json.RawMessage(`"plain title"`),
	}
	if readableRow(row) {
		t.Fatalf("a clean row was reported as changed: %v", row)
	}
}
