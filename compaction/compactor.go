package compaction

import (
	"slices"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
)

// Compactor implements session.Compactor. Set it as Session.Compactor, and
// the session runs a compaction turn when the last model call used
// Policy's share of the window. A host that keeps its own turns calls
// Needed and Start itself and runs the Start with its own Runner. The
// Compactor chooses what to summarize and builds the summary request; the
// compaction turn makes the model call, so its spend and recovery are those
// of any turn, and its Start.Budget caps it. The zero value works, with the defaults of Policy,
// but its window is DefaultWindow (128,000 tokens) whatever the session's
// model is: set Policy.Model to the session model's ModelInfo.
type Compactor struct {
	// Policy says when compaction is due. Its window also sizes the kept
	// tail and the transcript.
	Policy Policy

	// Model writes the summary. Empty means the session's model. When the
	// catalog knows Model and its window is smaller than Policy's, the
	// transcript is sized to fit it.
	Model string

	// Prompt replaces DefaultPrompt. The summary is the text inside the
	// reply's last <summary> and </summary> tags, or the whole reply
	// when it has none.
	Prompt string

	// MaxTokens limits the summary output. Zero means 16384.
	MaxTokens int

	// TailFraction is the share of the window kept as verbatim recent
	// messages. Zero means DefaultTailFraction. A negative value keeps
	// none, so the summary alone stands for the history.
	TailFraction float64
}

// New returns a Compactor with policy.
func New(policy Policy) *Compactor {
	return &Compactor{Policy: policy}
}

// Needed reports whether Policy.Due(last) and projected, the history the
// next turn would get, has a message to summarize.
func (c *Compactor) Needed(projected []llm.Message, last llm.Usage) bool {
	return c.Policy.Due(last) && len(withoutAuthoritative(projected)) > 0
}

// Start builds the compaction turn's Start. It keeps the newest messages
// that fit in the tail budget, cutting only where a user message other than
// a tool result starts, and never between a tool call and its result. The
// messages before the cut are sent as a text transcript with the prompt,
// without their reminders, but with agent mail (a reminder with a From).
// Their system and developer messages are kept ahead of the summary, with
// their own role. The Start has origin StartCompaction and no tools, and
// its fact's Hash is the dashi.HistoryHash of projected. The caller sets its
// CommandID and may set Budget; a Session also sets the fact's Through.
func (c *Compactor) Start(projected []llm.Message) (dashi.Start, error) {
	window, _ := c.Policy.Window()
	cut := c.cut(projected, window)
	covered := withoutReminders(withoutAuthoritative(projected[:cut]))
	if len(covered) == 0 {
		return dashi.Start{}, ErrNothingToCompact
	}
	request, maxTokens, err := summaryRequest(
		covered,
		c.Prompt,
		c.summaryWindow(window),
		c.MaxTokens,
	)
	if err != nil {
		return dashi.Start{}, err
	}
	hash, err := dashi.HistoryHash(projected)
	if err != nil {
		return dashi.Start{}, err
	}
	kept := authoritative(projected[:cut])
	return dashi.Start{
		Agent: dashi.Agent{
			Model:    c.Model,
			Settings: llm.Settings{MaxTokens: maxTokens},
			NoTools:  true,
		},
		Origin: dashi.StartCompaction,
		Input:  []llm.Message{request},
		Compaction: &dashi.CompactionFact{
			Keep:      append(slices.Clone(kept), projected[cut:]...),
			SummaryAt: len(kept),
			Frame:     summaryFrame,
			Hash:      hash,
		},
	}, nil
}

// summaryWindow returns the window of the summary call: the smaller of
// window, the session model's, and Model's catalog window.
func (c *Compactor) summaryWindow(window int) int {
	if c.Model == "" {
		return window
	}
	for _, provider := range catalog.Providers() {
		if e, ok := catalog.Find(provider, c.Model); ok {
			if w := e.Capabilities.ContextWindow; w > 0 {
				return min(window, w)
			}
			break
		}
	}
	return window
}

// cut returns the index of the first kept message: the earliest cut whose
// tail fits in the tail budget, or len(projected) to keep none.
func (c *Compactor) cut(projected []llm.Message, window int) int {
	fraction := c.TailFraction
	if fraction == 0 {
		fraction = DefaultTailFraction
	}
	budget := max(0, int(fraction*float64(window)))
	sizes := make([]int, len(projected)+1)
	for i := len(projected) - 1; i >= 0; i-- {
		sizes[i] = sizes[i+1] + estimate(projected[i])
	}
	var starts []int
	for i, m := range projected {
		if m.Role == llm.RoleUser && !slices.ContainsFunc(m.Content, isResult) {
			starts = append(starts, i)
		}
	}
	return chooseCut(projected, starts, sizes, budget)
}

func isResult(c llm.Content) bool {
	return c.ToolResult != nil
}

var _ session.Compactor = (*Compactor)(nil)
