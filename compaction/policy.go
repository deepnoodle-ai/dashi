package compaction

import (
	"encoding/json"

	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
)

const (
	// DefaultWindow is the context window in tokens when the model's window
	// is unknown.
	DefaultWindow = 128_000

	// DefaultFraction is the share of the window that makes compaction due.
	DefaultFraction = 0.8
)

// Policy decides when compaction is due. The zero value works, with a window
// of DefaultWindow and a fraction of DefaultFraction.
type Policy struct {
	// Model supplies the context window through Capabilities.ContextWindow.
	Model llm.ModelInfo

	// Fraction is the share of the window that makes compaction due. Zero
	// means DefaultFraction. A negative value turns automatic compaction off.
	// A value above 1 counts as 1.
	Fraction float64
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
// Fraction of the window. It counts all input, including cached input, plus
// output, because the next call must fit both. Due does not check the count of
// Turns, so a manual or automatic Compact can still return ErrNothingToCompact.
func (p Policy) Due(last llm.Usage) bool {
	if p.Fraction < 0 {
		return false
	}
	fraction := p.Fraction
	if fraction == 0 {
		fraction = DefaultFraction
	}
	fraction = min(fraction, 1)
	window, _ := p.Window()
	used := usedTokens(last)
	return used > 0 && float64(used) >= fraction*float64(window)
}

// dueHistory also sees output produced after the last model call. Encoded
// text bytes conservatively trigger compaction; they do not prove token fit.
// Media history retains the reported-usage trigger without a byte estimate.
func (p Policy) dueHistory(messages []llm.Message, last llm.Usage) bool {
	if p.Due(last) {
		return true
	}
	if compactiontext.HasMedia(messages) {
		return false
	}
	b, err := json.Marshal(messages)
	return err == nil && p.Due(llm.Usage{InputTokens: len(b)})
}
