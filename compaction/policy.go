package compaction

import (
	"encoding/json"

	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
)

const (
	// DefaultWindow is the context window in tokens when the model's window
	// is unknown.
	DefaultWindow = 200_000

	// DefaultFraction is the share of the window that makes compaction due.
	DefaultFraction = 0.2
)

// Policy decides when compaction is due: at the due count, a number of
// tokens that the last model call, or the history's estimated size, reaches.
// The zero value works: the due count is DefaultFraction of the model's
// window, or of DefaultWindow when the model states none.
type Policy struct {
	// Model supplies the context window through Capabilities.ContextWindow.
	Model llm.ModelInfo

	// Fraction is the share of the window that makes compaction due. Zero
	// means DefaultFraction. A negative value turns automatic compaction
	// off, even with DueTokens; Session.Compact still compacts. A value
	// above 1 counts as 1.
	Fraction float64

	// DueTokens, when positive, is the due count itself, and Fraction then
	// only turns compaction off. When the model states its window, a larger
	// value counts as the window. Zero or negative means unset.
	DueTokens int
}

// Window returns the context window in tokens. When the model does not state
// one, it returns DefaultWindow and false, so the host can warn once.
func (p Policy) Window() (tokens int, known bool) {
	if w := p.Model.Capabilities.ContextWindow; w > 0 {
		return w, true
	}
	return DefaultWindow, false
}

// Due reports whether the last model call of the last Turn used at least
// the due count. It counts all input, including cached input, plus output,
// because the next call must fit both. Due does not check the count of
// Turns, so a manual or automatic Compact can still return ErrNothingToCompact.
func (p Policy) Due(last llm.Usage) bool {
	if p.Fraction < 0 {
		return false
	}
	used := usedTokens(last)
	return used > 0 && used >= p.dueTokens()
}

// dueTokens returns the due count, whether or not automatic compaction is
// off: DueTokens when set, else Fraction of the window.
func (p Policy) dueTokens() int {
	window, known := p.Window()
	if p.DueTokens > 0 {
		if known {
			return min(p.DueTokens, window)
		}
		return p.DueTokens
	}
	fraction := p.Fraction
	if fraction <= 0 {
		fraction = DefaultFraction
	}
	return int(min(fraction, 1) * float64(window))
}

// dueHistory reports whether the last call's usage, or the history's
// estimated size, reaches due tokens, unless automatic compaction is off.
// The estimate counts tool output added after the last model call. History
// with images, audio, video, or binary documents uses only the last call's
// usage, since its byte size says little about tokens.
func (p Policy) dueHistory(messages []llm.Message, last llm.Usage, due int) bool {
	if p.Fraction < 0 {
		return false
	}
	reached := func(used int) bool { return used > 0 && used >= due }
	if reached(usedTokens(last)) {
		return true
	}
	if compactiontext.HasMedia(messages) {
		return false
	}
	b, err := json.Marshal(messages)
	return err == nil && reached(len(b)/bytesPerToken)
}
