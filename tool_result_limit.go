package dashi

import (
	"fmt"
	"strings"
	"unicode/utf8"

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
		"[Tool output shortened: %d bytes, %d omitted. To see omitted text, "+
			"ask for a smaller part: a path, a pattern, or a line range. "+
			"Do not repeat a command that changes state only to see its "+
			"output.]",
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
	for _, b := range r.Output.Content {
		if b.Text != nil {
			text.WriteString(b.Text.Text)
		}
	}
	all := text.String()
	if len(all) <= limit {
		return r, Shortened{}, false
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
// text joined is all. The marker goes into the block where the head ends.
// Invalid bytes in kept text become U+FFFD. A block that changes loses its
// citations, since their character ranges no longer match. A text block
// with nothing left is dropped. It also returns the view's text size.
func layoutResult(
	r llm.ToolResult,
	all string,
	headEnd, tailStart int,
	marker string,
) (llm.ToolResult, int) {
	out := r
	out.Output = r.Output.Clone()
	out.Output.Content = nil
	size := 0
	markerPlaced := false
	offset := 0
	for _, b := range r.Output.Content {
		if b.Text == nil {
			out.Output.Content = append(out.Output.Content, b.Clone())
			continue
		}
		start, end := offset, offset+len(b.Text.Text)
		offset = end
		var keep strings.Builder
		if start < headEnd {
			keep.WriteString(all[start:min(end, headEnd)])
		}
		placeMarker := !markerPlaced && end >= headEnd
		if placeMarker {
			keep.WriteString(marker)
			markerPlaced = true
		}
		if end > tailStart {
			keep.WriteString(all[max(start, tailStart):end])
		}
		kept := strings.ToValidUTF8(keep.String(), "\uFFFD")
		if kept == "" {
			continue
		}
		size += len(kept)
		blk := b.Clone()
		if kept != b.Text.Text {
			blk.Text.Text = kept
			blk.Text.Citations = nil
		}
		out.Output.Content = append(out.Output.Content, blk)
	}
	return out, size
}
