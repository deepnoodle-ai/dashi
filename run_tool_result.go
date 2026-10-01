package dashi

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Limit for the text of one tool result in the messages the model sees. The
// minimum keeps room for the marker and some text on each side of it.
const (
	defaultToolResultBytes = 64 << 10
	minToolResultBytes     = 1 << 10
)

// maxOutcomeData bounds tool.Outcome.Data, as tool.Report bounds
// Progress.Data.
const maxOutcomeData = 64 << 10

// dropBadData returns o without Data that is not valid JSON or exceeds
// maxOutcomeData, and the warning to record when it dropped it. A tool's
// detail for the host is never worth failing the call over.
func dropBadData(o tool.Outcome, callID string) (tool.Outcome, []Warning) {
	if len(o.Data) == 0 || len(o.Data) <= maxOutcomeData && json.Valid(o.Data) {
		return o, nil
	}
	n := len(o.Data)
	o.Data = nil
	return o, []Warning{{
		Code: WarningToolDataDropped,
		Message: fmt.Sprintf(
			"tool call %s returned %d bytes of Data that is not valid JSON or exceeds %d bytes; the outcome is recorded without it",
			callID, n, maxOutcomeData,
		),
	}}
}

// Shortened records how the result limit changed one call's result in the
// messages the model sees. Sizes count text bytes after the AfterTool hooks
// ran, so a redactor can make them differ from the tool's own output.
type Shortened struct {
	// OriginalBytes is the text size before shortening.
	OriginalBytes int `json:"original_bytes"`
	// ShownBytes is how much of that text the model sees. The
	// marker is not counted, but it fits inside Limit with the kept text.
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
	// Empty text blocks carry nothing, but providers still join them. Drop
	// them first. If that is enough, no text is omitted, so the view has no
	// Shortened record (the zero Limit tells the caller).
	nonEmpty := 0
	for _, b := range r.Output.Content {
		if b.Text != nil && b.Text.Text != "" {
			nonEmpty++
		}
	}
	if len(all)+providerSeparators(nonEmpty) <= limit {
		view, _ := layoutResult(r, 0, 0, "")
		return view, Shortened{}, true
	}
	// The marker's numbers are at most as long as these. Start with a budget
	// that ignores separators, because the blocks that stay are not known
	// yet. Then measure the layout, which counts only the separators of the
	// blocks it keeps, and shrink the budget by the excess until the view
	// fits. Invalid bytes become three-byte U+FFFD, so the kept text can also
	// grow after the cut. At least one byte is always omitted.
	budget := limit - len(toolResultMarker(len(all), len(all)))
	budget = min(budget, len(all)-1)
	for {
		headEnd, tailStart := cutOffsets(all, budget)
		shown := headEnd + len(all) - tailStart
		marker := toolResultMarker(len(all), len(all)-shown)
		view, size := layoutResult(r, headEnd, tailStart, marker)
		if size <= limit || budget <= 0 {
			return view, Shortened{
				OriginalBytes: len(all),
				ShownBytes:    shown,
				Limit:         limit,
			}, true
		}
		// Each kept byte removed frees at most 1+maxSeparatorBytes bytes of
		// size (its own and the separator of a block that becomes empty),
		// so this step never cuts more than needed.
		budget -= max((size-limit)/(1+maxSeparatorBytes), 1)
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

// layoutResult builds the shortened view from the text blocks of r, which
// are one sequence of text. It keeps the text before headEnd and from
// tailStart on, in the blocks where it was, and puts the marker at headEnd.
// Text blocks left empty are dropped. Non-text blocks stay in order and are
// copied. A text block that changes loses its citations, since their
// character ranges no longer match, and invalid bytes in kept text become
// U+FFFD. It also returns the size the view has for providers: its text plus
// the separators they add between text blocks.
func layoutResult(
	r llm.ToolResult,
	headEnd, tailStart int,
	marker string,
) (llm.ToolResult, int) {
	tailStart = max(tailStart, headEnd)
	out := r
	// Copy nothing else: only the blocks kept are cloned below.
	out.Output.Content = make([]content.Block, 0, len(r.Output.Content))
	// No provider sends Display to the model, so the view drops it.
	out.Output.Display = ""
	offset, size, texts := 0, 0, 0
	placed := marker == ""
	for _, b := range r.Output.Content {
		if b.Text == nil {
			out.Output.Content = append(out.Output.Content, b.Clone())
			continue
		}
		t := b.Text.Text
		n := len(t)
		h := min(max(headEnd-offset, 0), n)
		tl := min(max(tailStart-offset, h), n)
		offset += n
		piece := t[:h]
		if !placed && headEnd <= offset {
			piece += marker
			placed = true
		}
		piece = strings.ToValidUTF8(piece+t[tl:], "\uFFFD")
		if piece == "" {
			continue
		}
		if piece == t {
			out.Output.Content = append(out.Output.Content, b.Clone())
		} else {
			out.Output.Content = append(out.Output.Content, content.TextBlock(piece))
		}
		size += len(piece)
		texts++
	}
	return out, size + providerSeparators(texts)
}
