package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSessionTitleDropsConversationTags(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"user wrapper", `<user> FULFaccept5ae09ecdaf: inspect the card`, "FULFaccept5ae09ecdaf: inspect the card"},
		{"system reminder", "<system-reminder>\nToday: 2026-11-27\n</system-reminder>", "Today: 2026-11-27"},
		{"job wrapper", "<job> Background subagent. It... </job>", "Background subagent. It..."},
		{"chat wrapper with inner markup", "<chat> <user> привет </user> </chat>", "привет"},
		{"unterminated wrapper", "<chat> <user> show me the diff", "show me the diff"},
		{"plain text", "plain conversation title", "plain conversation title"},
		{"directory in a real title", "Current working directory detection is broken", "Current working directory detection is broken"},
		{"title with internal separators", "Today: 2026-11-28 — BILI-010 — fix the list; keep full IDs", "BILI-010 — fix the list; keep full IDs"},
		{"directory path before title", `Current working dir D:\work — Fix title parsing`, "Fix title parsing"},
		{"only tags", "<job></job>", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionTitle(tc.raw); got != tc.want {
				t.Fatalf("sessionTitle(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestSessionTitleKeepsAWholeTitleThatContainsADash(t *testing.T) {
	const raw = "BILI-010 — fix the session list"
	if got := sessionTitle(raw); got != raw {
		t.Fatalf("sessionTitle(%q) = %q, want it whole", raw, got)
	}
}

func TestSessionTitleCutsOnlyTheScaffoldingBeforeTheTurn(t *testing.T) {
	const raw = "Today: 2026-11-28 — make the list readable"
	if got := sessionTitle(raw); got != "make the list readable" {
		t.Fatalf("sessionTitle(%q) = %q, want the turn after the preamble", raw, got)
	}
}

// The list decides how much fits and the tooltip keeps the whole string, so the
// bridge must not shorten a title on its way through.
func TestSessionTitleNeverShortensALongTitle(t *testing.T) {
	raw := strings.TrimSpace(strings.Repeat("readable naming ", 20))
	if got := sessionTitle(raw); got != raw {
		t.Fatalf("a %d-rune title came back as %d runes", len(raw), len(got))
	}
}

// Every session of a project opens with the same working directory and the same
// instruction reminder; only the date tells them apart.
func TestSessionTitleDropsTheRepeatedPreambleTail(t *testing.T) {
	raw := "<system-reminder> Today: 2026-11-28; current working dir D:\\work; do not repeat this information"
	if got := sessionTitle(raw); got != "Today: 2026-11-28" {
		t.Fatalf("sessionTitle(%q) = %q", raw, got)
	}
}

func TestSessionTitleKeepsOrdinaryComparisonOperators(t *testing.T) {
	const raw = "why is a > b and x<y in this diff"
	got := sessionTitle(raw)
	if strings.Contains(got, "  ") || got != strings.TrimSpace(got) {
		t.Fatalf("sessionTitle collapsed text badly: %q", got)
	}
	if !strings.Contains(got, "b") || !strings.Contains(got, "diff") {
		t.Fatalf("sessionTitle dropped prose: %q", got)
	}
}

func TestWithReadableTitlesRewritesOnlyDisplayStrings(t *testing.T) {
	page := map[string]any{
		"sessions": []map[string]any{{
			"id": "abc", "live": true, "requests": 9,
			"firstBlockHint": "<user> FULFaccept5ae09ecdaf: inspect the card",
			"title":          "<job> Background subagent",
		}},
		"total": 1,
	}
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	out := withReadableTitles(body)
	var decoded struct {
		Sessions []map[string]any `json:"sessions"`
		Total    int              `json:"total"`
	}
	if json.Unmarshal(out, &decoded) != nil || decoded.Total != 1 || len(decoded.Sessions) != 1 {
		t.Fatalf("payload changed shape: %s", out)
	}
	row := decoded.Sessions[0]
	if row["id"] != "abc" || row["live"] != true || row["requests"] != float64(9) {
		t.Fatalf("non-display fields changed: %v", row)
	}
	// A generic job title says nothing, so the real turn wins; the hint loses
	// only its markup.
	if row["title"] != "FULFaccept5ae09ecdaf: inspect the card" {
		t.Fatalf("title = %v, want the informative turn", row["title"])
	}
	if row["firstBlockHint"] != "FULFaccept5ae09ecdaf: inspect the card" {
		t.Fatalf("firstBlockHint = %v", row["firstBlockHint"])
	}
}

func TestWithReadableTitlesPassesUnrecognizedPayloadThrough(t *testing.T) {
	for _, body := range []string{`{"sessions":"not-an-array"}`, `not json`, `{}`} {
		if got := string(withReadableTitles([]byte(body))); got != body {
			t.Fatalf("payload was rewritten: %q", got)
		}
	}
}

func TestSkipReportAnnouncesOncePerInterval(t *testing.T) {
	var report skipReport
	start := time.Unix(0, 0)
	if count, announce := report.record(missingCallerScope, chat, start); !announce || count != 1 {
		t.Fatalf("first record = (%d, %v)", count, announce)
	}
	if _, announce := report.record(missingCallerScope, chat, start.Add(time.Minute)); announce {
		t.Fatal("repeat inside the interval must stay quiet")
	}
	count, announce := report.record(missingCallerScope, chat, start.Add(skipNoticeInterval+time.Second))
	if !announce || count != 3 {
		t.Fatalf("third record = (%d, %v)", count, announce)
	}
	counts, total, _ := report.detail()
	if counts["missing_caller_scope"] != 3 || total != 3 {
		t.Fatalf("snapshot = %v total=%d", counts, total)
	}
}

func TestSkipReportExcludesOrdinaryTrafficFromTheWarningTotal(t *testing.T) {
	var report skipReport
	now := time.Unix(0, 0)
	report.record(otherEndpoint, chat, now)
	report.record(missingSessionID, chat, now)
	counts, total, _ := report.detail()
	if counts["other_endpoint"] != 1 || counts["missing_session_id"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
	if total != 1 {
		t.Fatalf("total = %d, want only actionable bypasses counted", total)
	}
}
