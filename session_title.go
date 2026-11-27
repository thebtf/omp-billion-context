package main

import (
	"strings"
	"unicode/utf8"
)

// titleTags are wrapper tags clients put around conversation turns. They are
// protocol noise in a session list, so the display drops them and keeps prose.
var titleTags = []string{"chat", "user", "job", "assistant", "system-reminder", "system"}

const titleLimit = 80

// sessionTitle turns a raw block hint into something readable. It prefers prose
// inside a conversational wrapper, otherwise strips tags from the whole string.
func sessionTitle(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, tag := range titleTags {
		if prose := innerTag(raw, tag); prose != "" {
			return truncateTitle(prose)
		}
	}
	return truncateTitle(stripTags(raw))
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

func truncateTitle(value string) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= titleLimit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:titleLimit])) + "…"
}
