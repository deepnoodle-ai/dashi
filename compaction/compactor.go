package compaction

import (
	"slices"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Compactor implements dashi.Compactor. Set it as Session.Compactor, and
// the session runs a compaction turn when the last model call used
// Policy's share of the window. The Compactor chooses what to summarize and
// builds the summary request; the session's turn makes the model call, so
// its spend, budget, and recovery are those of any turn. The zero value
// works, with the defaults of Policy.
type Compactor struct {
	// Policy says when compaction is due. Its window also sizes the kept
	// tail and the transcript.
	Policy Policy

	// Model writes the summary. Empty means the session's model.
	Model string

	// Prompt replaces DefaultPrompt. It must ask for the summary between
	// <summary> and </summary> tags.
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

// Needed reports whether Policy.Due(last) and the projected history has a
// message to summarize.
func (c *Compactor) Needed(projected []llm.Message, last llm.Usage) bool {
	return c.Policy.Due(last) && len(withoutAuthoritative(projected)) > 0
}

// Start builds the compaction turn's Start. It keeps the newest messages
// that fit in the tail budget, cutting only where a user message other than
// a tool result starts, and never between a tool call and its result. The
// messages before the cut are sent as a text transcript with the prompt,
// without their reminders. Their system and developer messages are kept
// ahead of the summary, with their own role.
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
		window,
		c.MaxTokens,
	)
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
		Input: []llm.Message{request},
		Compaction: &dashi.CompactionFact{
			Keep:      append(slices.Clone(kept), projected[cut:]...),
			SummaryAt: len(kept),
			Frame:     summaryFrame,
		},
	}, nil
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
	cut, _ := chooseCut(projected, starts, 0, sizes, budget)
	return cut
}

func isResult(c llm.Content) bool {
	return c.ToolResult != nil
}

var _ dashi.Compactor = (*Compactor)(nil)
