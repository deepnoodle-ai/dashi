package compaction

import (
	"context"
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

// Needed reports whether to compact messages, the history for the next Turn.
// It returns false if nothing in messages can be summarized. Compaction is due when the
// last model call's usage, or the estimated size of messages, reaches
// Policy's share of the window. The size estimate catches tool output added
// after the last model call. History with images, audio, video, or binary
// documents is not size-estimated.
func (c *Compactor) Needed(messages []llm.Message, last llm.Usage) bool {
	return c.Policy.dueHistory(messages, last) && len(withoutAuthoritative(messages)) > 0
}

// Start builds the compaction turn's Start. It keeps the newest messages
// that fit in the tail budget, cutting only where a user message other than
// a tool result starts, and never between a tool call and its result. The
// messages before the cut are sent as a text transcript with the prompt,
// without their reminders, but with agent mail (a reminder with a From).
// Their system and developer messages are kept ahead of the summary, with
// their own role. The Start has origin StartCompaction and no tools, and
// its fact's Hash is the dashi.HistoryHash of messages. The caller sets its
// CommandID and may set Budget; a Session also sets the fact's Through.
// Start makes no model call, so it does not use ctx.
func (c *Compactor) Start(_ context.Context, messages []llm.Message) (dashi.Start, error) {
	window, _ := c.Policy.Window()
	cut := c.cut(messages, window)
	covered := withoutReminders(withoutAuthoritative(messages[:cut]))
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
	hash, err := dashi.HistoryHash(messages)
	if err != nil {
		return dashi.Start{}, err
	}
	kept := authoritative(messages[:cut])
	return dashi.Start{
		Agent: dashi.Agent{
			Model:    c.Model,
			Settings: llm.Settings{MaxTokens: maxTokens},
			NoTools:  true,
		},
		Origin: dashi.StartCompaction,
		Input:  []llm.Message{request},
		Compaction: &dashi.CompactionFact{
			Keep:      append(slices.Clone(kept), messages[cut:]...),
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
// tail fits in the tail budget, or len(messages) to keep none.
func (c *Compactor) cut(messages []llm.Message, window int) int {
	fraction := c.TailFraction
	if fraction == 0 {
		fraction = DefaultTailFraction
	}
	return tailStart(messages, max(0, int(fraction*float64(window))))
}

var _ session.Compactor = (*Compactor)(nil)
