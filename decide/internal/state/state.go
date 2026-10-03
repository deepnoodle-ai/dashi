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
// maxBytes with Cut. Reminder content is not text, so a message of
// reminders only is skipped. It returns "" when the turn has no such
// message.
func Request(view dashi.TurnView, maxBytes int) string {
	for _, m := range slices.Backward(view.Input) {
		if m.Role != llm.RoleUser || m.Context != nil {
			continue
		}
		if text := m.Text(); text != "" {
			return Cut(text, maxBytes)
		}
	}
	return ""
}

// Cut returns the first maxBytes bytes of text, shortened further so it
// ends on a whole UTF-8 character. A maxBytes of zero or less means no
// limit.
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
