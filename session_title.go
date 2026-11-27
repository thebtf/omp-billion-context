package main

import (
	"strings"
	"unicode/utf8"
)

// titleTags are wrapper tags clients put around conversation turns. They are
// protocol noise in a session list, so the display drops them and keeps prose.
var titleTags = []string{"chat", "user", "job", "assistant", "system-reminder", "system"}

// boilerplatePrefixes mark harness scaffolding that says nothing about the
// conversation. A later turn is a better session title than a date reminder.
var boilerplatePrefixes = []string{
	"today:", "current working directory:", "do not repeat this information",
	"system-reminder", "you are a", "you have ",
}

const titleLimit = 80

// sessionTitle turns raw conversation text into something that identifies the
// session. Boilerplate is used only when nothing better exists.
func sessionTitle(raws ...string) string {
	title, _ := pickTitle(raws...)
	return title
}

// pickTitle returns the best readable title and whether every candidate was
// boilerplate. Callers use the flag to decide if another field is worth trying.
// A job description is deferred: background jobs would otherwise all share the
// same title while a user turn names the actual work.
func pickTitle(raws ...string) (string, bool) {
	var fallback string
	for _, raw := range raws {
		for _, text := range turnTexts(raw) {
			if isBoilerplate(text) {
				if fallback == "" {
					fallback = text
				}
				continue
			}
			if strings.HasPrefix(text, "background ") {
				if fallback == "" {
					fallback = text
				}
				continue
			}
			return truncateTitle(text), false
		}
	}
	return truncateTitle(fallback), fallback != ""
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

func truncateTitle(value string) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= titleLimit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:titleLimit])) + "…"
}
