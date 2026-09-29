package compaction

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// minItemBytes is the smallest size that fitTranscript shrinks an item to.
// A smaller cut loses the meaning of the item and saves little.
const minItemBytes = 1024

// renderTranscript writes messages as plain text for the summarizer. Tool
// calls and results become labeled text, so the summarizer request needs no
// tool definitions and no provider-specific replay data. Reasoning and opaque
// provider content stay out. Media becomes a short marker. A reminder with a
// sender (From) is agent mail and becomes its own item; other reminders stay
// out.
func renderTranscript(messages []llm.Message) []string {
	var items []string
	for _, m := range messages {
		var text []string
		flush := func() {
			if len(text) > 0 {
				items = append(items, "["+string(m.Role)+"]\n"+
					strings.Join(text, "\n"))
				text = nil
			}
		}
		for _, c := range m.Content {
			switch {
			case c.Block != nil:
				text = append(text, blockText(*c.Block))
			case c.Refusal != nil:
				text = append(text, *c.Refusal)
			case c.ToolCall != nil:
				flush()
				items = append(items, fmt.Sprintf(
					"[assistant tool call %s id=%s]\n%s",
					c.ToolCall.Name,
					c.ToolCall.ID,
					c.ToolCall.Args,
				))
			case c.ToolResult != nil:
				flush()
				label := "tool result"
				if c.ToolResult.IsError {
					label = "tool error"
				}
				items = append(items, fmt.Sprintf(
					"[%s id=%s]\n%s",
					label,
					c.ToolResult.CallID,
					blocksText(c.ToolResult.Output.Content),
				))
			case c.HostedUse != nil:
				flush()
				items = append(items, fmt.Sprintf(
					"[provider tool %s]\n%s",
					c.HostedUse.Name,
					c.HostedUse.Input,
				))
			case c.HostedResult != nil:
				flush()
				items = append(items, "[provider tool result]\n"+
					blocksText(c.HostedResult.Output))
			case c.Reminder != nil && c.Reminder.From != "":
				flush()
				items = append(items, fmt.Sprintf(
					"[message from %s]\n%s",
					c.Reminder.From,
					c.Reminder.Content,
				))
			}
		}
		flush()
	}
	return items
}

func blocksText(blocks []content.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, blockText(b))
	}
	return strings.Join(parts, "\n")
}

func blockText(b content.Block) string {
	switch {
	case b.Text != nil:
		return b.Text.Text
	case b.Image != nil:
		return "[image omitted]"
	case b.Document != nil:
		return "[document omitted: " + b.Document.Title + "]"
	case b.Audio != nil:
		return "[audio omitted]"
	case b.Video != nil:
		return "[video omitted]"
	case b.Resource != nil:
		data, _ := json.Marshal(b.Resource)
		return string(data)
	}
	return ""
}

// fitTranscript shortens the largest items until the total is at most
// budget bytes. It cuts the middle of an item and keeps its start and end.
// It shortens and never drops an item, so the order and the count of items
// stay the same. It returns an error when the smallest allowed items still
// exceed the budget.
func fitTranscript(items []string, budget int) ([]string, error) {
	total := 0
	for _, s := range items {
		total += len(s)
	}
	if total <= budget {
		return items, nil
	}
	floor := 0
	for _, s := range items {
		floor += min(len(s), minItemBytes)
	}
	if floor > budget {
		return nil, fmt.Errorf(
			"compaction: history is too large for the summarizer model "+
				"(%d items need at least %d bytes; the limit is %d)",
			len(items), floor, budget,
		)
	}
	// Find the largest per-item limit that fits, by bisection.
	low, high := minItemBytes, total
	for low < high {
		mid := (low + high + 1) / 2
		sum := 0
		for _, s := range items {
			sum += min(len(s), mid)
		}
		if sum <= budget {
			low = mid
		} else {
			high = mid - 1
		}
	}
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = truncateMiddle(s, low)
	}
	return out, nil
}

// truncateMiddle keeps the start and end of s and marks the cut. The result
// may exceed limit by the length of the marker.
func truncateMiddle(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	head := cutBefore(s, limit/2)
	tail := cutAfter(s, len(s)-limit/2)
	marker := fmt.Sprintf("\n[... %d bytes cut ...]\n", tail-head)
	return s[:head] + marker + s[tail:]
}

// cutBefore returns the largest rune boundary at or before i.
func cutBefore(s string, i int) int {
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// cutAfter returns the smallest rune boundary at or after i.
func cutAfter(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
