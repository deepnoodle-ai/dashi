// Package state builds the text a decision model judges, its state, from
// a turn. Sizes are in bytes, never tokens.
package state

import (
	"slices"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Request returns the user's latest request in the turn: the text of the
// last user message in view.Input that is not a named context item, cut to
// its last maxBytes bytes with KeepEnd, since a question usually follows
// whatever the user pasted. Reminder content is not text, so a message of
// reminders only is skipped. It returns "" when the turn has no such
// message, and for a compaction turn, whose Input is the session's summary
// instruction rather than a request from the user.
func Request(view dashi.TurnView, maxBytes int) string {
	if view.Origin == dashi.StartCompaction {
		return ""
	}
	for _, m := range slices.Backward(view.Input) {
		if m.Role != llm.RoleUser || m.Context != nil {
			continue
		}
		if text := m.Text(); text != "" {
			return KeepEnd(text, maxBytes)
		}
	}
	return ""
}

// Cut returns the first maxBytes bytes of text, shortened further so it
// ends on a whole UTF-8 character. A maxBytes of zero or less means no
// limit. KeepEnd keeps the end instead.
func Cut(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

// KeepEnd returns the last maxBytes bytes of text, shortened further so it
// starts on a whole UTF-8 character. A maxBytes of zero or less means no
// limit. Use it where the end of a text matters most, such as a message
// whose question follows a pasted log.
func KeepEnd(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	start := len(text) - maxBytes
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}
