package main

import (
	"strings"
	"unicode/utf8"
)

// titleTags are wrapper tags clients put around conversation turns. They are
// protocol noise in a session list, so the display drops them and keeps prose.
var titleTags = []string{"chat", "user", "job", "assistant", "system-reminder", "system"}

// boilerplatePrefixes mark text that cannot name a session: the date line, the
// working directory and the instruction reminder a client injects into the
// first turn of every conversation. They repeat on every row, so a sibling
// field is preferred whenever one exists.
var boilerplatePrefixes = []string{
	"today:", "do not repeat this information",
	"system-reminder", "you are a", "you have ",
}

// sessionTitle turns raw conversation text into something that identifies the
// session. Wrapper tags are removed and whitespace collapsed by turnTexts, so
// the result is one continuous line. A text that says something is returned
// whole; only one that opens with the harness preamble every session shares is
// cut at its first clause that says something instead. Nothing is shortened
// here: the list decides how much fits on screen and the row tooltip keeps the
// whole string, so truncating in the bridge would destroy both.
func sessionTitle(raws ...string) string {
	for _, raw := range raws {
		texts := turnTexts(raw)
		for _, text := range texts {
			for rest := text; rest != ""; rest = afterTitleClause(rest) {
				if usefulTitle(rest) {
					return rest
				}
			}
		}
		// Nothing on this field names the work. Its cleaned text still dates the
		// session and tells rows apart better than an empty cell, so it is the
		// fallback — reduced to the part that differs between sessions.
		for _, text := range texts {
			if date := compactPreamble(strings.TrimSpace(text)); date != "" {
				return date
			}
		}
	}
	return ""
}

// sharedPreamble are the phrases a client repeats at the start of every session
// of a project: the working directory and the instruction reminder. They never
// tell two rows apart, so a title keeps only what precedes them.
var sharedPreamble = []string{"current working dir", "do not repeat this information"}

// compactPreamble drops the repeated tail of a harness preamble, leaving the
// piece that names this row — normally its date. A preamble that opens with a
// shared phrase has nothing of its own and yields "".
func compactPreamble(text string) string {
	lower := strings.ToLower(text)
	if len(lower) != len(text) {
		return text
	}
	cut := len(text)
	for _, marker := range sharedPreamble {
		if index := strings.Index(lower, marker); index >= 0 && index < cut {
			cut = index
		}
	}
	if cut == 0 {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(text[:cut]), ";:,- ")
}

// usefulTitle reports whether a display string names this session's work. The
// harness preamble repeats verbatim on every row of a client, and a generic job
// marker says little, so a sibling field that names the work wins over either.
func usefulTitle(text string) bool {
	text = strings.TrimSpace(text)
	return text != "" && !isBoilerplate(text) && !isGenericJob(text)
}

// Remove only the leading clause: punctuation within the actual title is content.
func afterTitleClause(text string) string {
	start := len(text)
	for _, sep := range []string{"; ", " — "} {
		if i := strings.Index(text, sep); i >= 0 && i+len(sep) < start {
			start = i + len(sep)
		}
	}
	return strings.TrimSpace(text[start:])
}

// turnTexts returns every readable segment of one raw field, in order.
func turnTexts(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var texts []string
	for _, tag := range titleTags {
		if prose := innerTag(raw, tag); prose != "" {
			texts = append(texts, prose)
		}
	}
	if cleaned := stripTags(raw); cleaned != "" {
		texts = append(texts, cleaned)
	}
	return texts
}

func isGenericJob(text string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "background ")
}

func isBoilerplate(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, marker := range []string{"current working directory", "current working dir"} {
		if rest, ok := strings.CutPrefix(lower, marker); ok {
			if rest == "" || strings.HasPrefix(rest, ":") {
				return true
			}
			if strings.HasPrefix(rest, " ") {
				path := strings.TrimSpace(rest)
				if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "\\") || len(path) >= 3 && path[1] == ':' && (path[2] == '/' || path[2] == '\\') {
					return true
				}
			}
		}
	}
	// bili clips automatic titles in the middle of the directory marker.
	if part, ok := strings.CutSuffix(lower, "…"); ok && strings.HasPrefix(part, "current working dir") && strings.HasPrefix("current working directory:", part) {
		return true
	}
	for _, prefix := range boilerplatePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// innerTag returns the text inside the first <tag> element, ignoring a closing
// marker when the summary was cut mid-turn.
func innerTag(raw, tag string) string {
	open := "<" + tag + ">"
	index := strings.Index(raw, open)
	if index < 0 {
		return ""
	}
	rest := raw[index+len(open):]
	if end := strings.Index(rest, "</"+tag+">"); end >= 0 {
		rest = rest[:end]
	}
	return stripTags(rest)
}

// stripTags removes the known conversation wrappers and collapses whitespace.
// Only recognized names are removed, so prose such as "x<y" or "<3" survives.
func stripTags(raw string) string {
	var out strings.Builder
	out.Grow(len(raw))
	for index := 0; index < len(raw); {
		if raw[index] == '<' {
			if width := knownTagWidth(raw[index:]); width > 0 {
				if out.Len() > 0 && !endsWithSpace(out.String()) {
					out.WriteByte(' ')
				}
				index += width
				continue
			}
		}
		out.WriteByte(raw[index])
		index++
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

// knownTagWidth reports the length of a recognized opening or closing tag.
func knownTagWidth(text string) int {
	for _, tag := range titleTags {
		if strings.HasPrefix(text, "<"+tag+">") {
			return len(tag) + 2
		}
		if strings.HasPrefix(text, "</"+tag+">") {
			return len(tag) + 3
		}
	}
	return 0
}

func endsWithSpace(value string) bool {
	if value == "" {
		return false
	}
	last, _ := utf8.DecodeLastRuneInString(value)
	return last == ' '
}
