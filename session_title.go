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
	"today:", "current working dir", "do not repeat this information",
	"system-reminder", "you are a", "you have ",
}

// titleSeparators split one hint into the clauses a harness joins with its own
// scaffolding: a semicolon, a dash. They are the boundaries a preamble leaves
// before the turn the operator typed, so a title cut inside the preamble can
// still name the work. They are only consulted for a text that is scaffolding
// from its first word: a real title that happens to contain a dash keeps both
// of its halves.
var titleSeparators = []string{"; ", " — "}

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
			if usefulTitle(text) {
				return text
			}
			// The whole text is the injected preamble. A client that joined it
			// to the operator's own turn still leaves that turn readable after
			// a separator.
			for _, clause := range titleClauses(text)[1:] {
				if usefulTitle(clause) {
					return clause
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

// titleClauses splits cleaned conversation text at the punctuation a harness
// puts between its own preamble and the turn the operator typed. Cleaning has
// already collapsed the newlines, so the separators that survive it are the
// semicolon and the dash; the clauses are kept in order.
func titleClauses(text string) []string {
	parts := []string{text}
	for _, separator := range titleSeparators {
		var next []string
		for _, part := range parts {
			next = append(next, strings.Split(part, separator)...)
		}
		parts = next
	}
	clauses := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			clauses = append(clauses, part)
		}
	}
	return clauses
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
