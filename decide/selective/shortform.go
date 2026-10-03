package selective

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

const (
	// shortTextBytes is the largest text DefaultShortForm leaves whole.
	shortTextBytes = 1024

	// shortEndLines and shortEndBytes bound each kept end of a cut text.
	shortEndLines = 8
	shortEndBytes = 400
)

// DefaultShortForm writes a smaller copy of a segment in code. It keeps the
// assistant messages and the tool call IDs, cuts each tool result or agent
// message larger than 1 KB to its first and last lines with a note saying
// what was cut, and turns images, documents, audio, and video into a note.
// It returns false when nothing was made smaller.
func DefaultShortForm(segment []llm.Message) ([]llm.Message, bool) {
	out := make([]llm.Message, len(segment))
	changed := false
	for i, m := range segment {
		parts := make([]llm.Content, len(m.Content))
		for j, c := range m.Content {
			parts[j] = c
			switch {
			case c.Block != nil:
				if note, ok := mediaNote(*c.Block); ok {
					parts[j] = llm.Text(note)
					changed = true
				}
			case c.ToolResult != nil:
				r := *c.ToolResult
				blocks := make([]content.Block, len(r.Output.Content))
				cut := false
				for k, b := range r.Output.Content {
					blocks[k] = b
					if note, ok := mediaNote(b); ok {
						blocks[k], cut = content.TextBlock(note), true
					} else if b.Text != nil {
						if s, ok := cutText(b.Text.Text); ok {
							blocks[k], cut = content.TextBlock(s), true
						}
					}
				}
				if cut {
					r.Output = tool.Output{Content: blocks}
					parts[j].ToolResult = &r
					changed = true
				}
			case c.Reminder != nil:
				if s, ok := cutText(c.Reminder.Content); ok {
					r := *c.Reminder
					r.Content = s
					parts[j].Reminder = &r
					changed = true
				}
			}
		}
		m.Content = parts
		out[i] = m
	}
	return out, changed
}

// mediaNote returns the note that stands for a media block.
func mediaNote(b content.Block) (string, bool) {
	switch {
	case b.Image != nil:
		return "[image omitted in the short form]", true
	case b.Document != nil:
		return "[document omitted in the short form: " + b.Document.Title + "]", true
	case b.Audio != nil:
		return "[audio omitted in the short form]", true
	case b.Video != nil:
		return "[video omitted in the short form]", true
	}
	return "", false
}

// cutText keeps the first and last lines of a large text and says what was
// cut between them. It returns false when the text is small enough.
func cutText(s string) (string, bool) {
	if len(s) <= shortTextBytes {
		return s, false
	}
	lines := strings.SplitAfter(s, "\n")
	head, first := 0, 0
	for first < len(lines) && first < shortEndLines && head+len(lines[first]) <= shortEndBytes {
		head += len(lines[first])
		first++
	}
	if first == 0 {
		head = runeStart(s, shortEndBytes)
	}
	tail := len(s)
	for last := len(lines) - 1; last >= first && len(lines)-last <= shortEndLines &&
		len(s)-tail+len(lines[last]) <= shortEndBytes; last-- {
		tail -= len(lines[last])
	}
	if tail == len(s) {
		tail = runeStart(s, max(head, len(s)-shortEndBytes))
	}
	if tail <= head {
		return s, false
	}
	middle := s[head:tail]
	note := fmt.Sprintf("[... %d lines, %d bytes cut in the short form ...]\n",
		strings.Count(middle, "\n"), len(middle))
	return s[:head] + note + s[tail:], true
}

// runeStart returns the largest rune boundary at or before i.
func runeStart(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// render writes messages as plain text for Decide, as the stock compactor
// writes its transcript: tool calls and results become labeled text,
// reasoning, opaque provider content, and reminders without a sender stay
// out, and media becomes a note.
func render(messages []llm.Message) string {
	return strings.Join(compactiontext.Transcript(messages), "\n\n")
}
