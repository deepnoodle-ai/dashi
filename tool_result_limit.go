package dashi

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Limit for the text of one tool result in the model view. The minimum keeps
// room for the marker and some text on each side of it.
const (
	defaultToolResultBytes = 64 << 10
	minToolResultBytes     = 1 << 10
)

// Shortened records how the result limit changed the model view of one
// call. Sizes count text bytes after the AfterTool hooks ran, so a redactor
// can make them differ from the tool's own output.
type Shortened struct {
	// OriginalBytes is the text size before shortening.
	OriginalBytes int `json:"original_bytes"`
	// ShownBytes is how much of that text the view keeps. The marker is not
	// counted, but it fits inside Limit with the kept text.
	ShownBytes int `json:"shown_bytes"`
	// Limit is the limit in force when Dashi shortened the result.
	Limit int `json:"limit"`
}

// resolveToolResultBytes returns the limit Dashi records for an agent: zero
// becomes the default, a negative value stays -1 (no limit), and a value
// below the minimum is raised.
func resolveToolResultBytes(n int) int {
	switch {
	case n == 0:
		return defaultToolResultBytes
	case n < 0:
		return -1
	}
	return max(n, minToolResultBytes)
}

// toolResultMarker is the text placed where Dashi removed the middle of a
// result. It never tells the model to run the call again, because a call can
// change state.
func toolResultMarker(original, omitted int) string {
	return fmt.Sprintf(
		"[Tool output shortened: %d bytes, %d omitted. "+
			"The omitted middle was not shown to you: make no claims about it. "+
			"To see omitted text, ask for a smaller part: a path, a "+
			"pattern, or a line range. Do not repeat a command that "+
			"changes state only to see its output.]",
		original,
		omitted,
	)
}

// shortenResult applies limit to the text blocks of r. It returns false and
// leaves r as it is when the limit is off or the text fits. It never changes
// r. The text blocks are one sequence: the view keeps the first half and the
// last half of the budget, with the marker between them, so it holds at most
// limit bytes of text. Non-text blocks stay in place and do not count.
func shortenResult(
	r llm.ToolResult,
	limit int,
) (llm.ToolResult, Shortened, bool) {
	if limit <= 0 {
		return r, Shortened{}, false
	}
	var text strings.Builder
	size := 0
	for _, b := range r.Output.Content {
		if b.Text != nil {
			size += len(b.Text.Text)
		}
	}
	text.Grow(size)
	blocks := 0
	for _, b := range r.Output.Content {
		if b.Text != nil {
			text.WriteString(b.Text.Text)
			blocks++
		}
	}
	all := text.String()
	if len(all)+providerSeparators(blocks) <= limit {
		return r, Shortened{}, false
	}
	if len(all) <= limit {
		// Only the separators that providers add between text blocks push
		// the request over the limit. One merged block has none.
		view, _ := layoutResult(r, all, len(all), len(all), "")
		return view, Shortened{
			OriginalBytes: len(all),
			ShownBytes:    len(all),
			Limit:         limit,
		}, true
	}
	// The marker's numbers are at most as long as these, so this budget is
	// safe. Invalid bytes become three-byte U+FFFD, so the kept text can
	// grow after the cut. Shrink the budget until the view fits.
	budget := limit - len(toolResultMarker(len(all), len(all)))
	for {
		headEnd, tailStart := cutOffsets(all, budget)
		shown := headEnd + len(all) - tailStart
		marker := toolResultMarker(len(all), len(all)-shown)
		view, size := layoutResult(r, all, headEnd, tailStart, marker)
		if size <= limit || budget <= 0 {
			return view, Shortened{
				OriginalBytes: len(all),
				ShownBytes:    shown,
				Limit:         limit,
			}, true
		}
		budget -= size - limit
	}
}

// maxSeparatorBytes is the longest text a provider adds between two text
// blocks of one tool result: Chat Completions joins with one newline, and
// Gemini and Responses join with two.
const maxSeparatorBytes = 2

// providerSeparators returns the most bytes providers add between n text
// blocks.
func providerSeparators(n int) int {
	return max(n-1, 0) * maxSeparatorBytes
}

// cutOffsets returns where the kept head ends and the kept tail starts in s
// for a budget of bytes. Each cut moves inward to a rune boundary.
func cutOffsets(s string, budget int) (int, int) {
	budget = max(budget, 0)
	headEnd := budget / 2
	tailStart := len(s) - (budget - budget/2)
	for headEnd > 0 && !utf8.RuneStart(s[headEnd]) {
		headEnd--
	}
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	return headEnd, tailStart
}

// layoutResult builds the shortened view from the text blocks of r, whose
// text joined is all. The kept head, the marker, and the kept tail become one
// text block at the place of the first text block, so providers add no
// separators. Invalid bytes in kept text become U+FFFD, and the merged block
// has no citations, since their character ranges no longer match. Non-text
// blocks stay in order and are copied. It also returns the view's text size.
func layoutResult(
	r llm.ToolResult,
	all string,
	headEnd, tailStart int,
	marker string,
) (llm.ToolResult, int) {
	kept := strings.ToValidUTF8(
		all[:headEnd]+marker+all[max(tailStart, headEnd):],
		"\uFFFD",
	)
	out := r
	// Copy nothing else: only the blocks kept are cloned below.
	out.Output.Content = make([]content.Block, 0, len(r.Output.Content))
	// No provider sends Display to the model, so the view drops it.
	out.Output.Display = ""
	placed := false
	for _, b := range r.Output.Content {
		if b.Text == nil {
			out.Output.Content = append(out.Output.Content, b.Clone())
			continue
		}
		if !placed && kept != "" {
			out.Output.Content = append(out.Output.Content, content.TextBlock(kept))
		}
		placed = true
	}
	return out, len(kept)
}
