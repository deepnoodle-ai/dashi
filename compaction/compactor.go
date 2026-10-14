package compaction

import (
	"context"
	"slices"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
)

// Compactor implements session.Compactor. Set it as Session.Compactor, and
// the session runs a compaction turn when the last model call reached
// Policy's due count. A host that keeps its own turns calls StartIfNeeded
// and records the Start with its own Runner. The Compactor chooses what to
// summarize and builds the summary request; the compaction turn makes the
// model call, so its spend and recovery are those of any turn, and its
// Start.Budget caps it. The zero value works, with the defaults of Policy,
// but its window is DefaultWindow (200,000 tokens) whatever the session's
// model is: set Policy.Model to the session model's ModelInfo.
//
// The kept tail and the summary's output allowance are sized from the room
// below the due count after the fixed part: system and developer messages,
// and what the last model call sent besides the history, such as tool
// definitions and instructions. Together they take at most half of that
// room, so the replacement history is well below the due count. When the
// fixed part takes more than half the due count, the due count rises to the
// fixed part plus half of it, up to the window, so compaction keeps room.
type Compactor struct {
	// Policy says when compaction is due. Its due count also sizes the
	// kept tail and the summary.
	Policy Policy

	// Model writes the summary. Empty means the session's model. When the
	// catalog knows Model and its window is smaller than Policy's, the
	// transcript is sized to fit it.
	Model string

	// Prompt replaces DefaultPrompt. The summary is the text inside the
	// reply's last <summary> and </summary> tags, or the whole reply
	// when it has none.
	Prompt string

	// MaxTokens limits the summary output. Zero means 16384. The output
	// allowance is also at most a quarter of the room below the due count,
	// and a quarter of the summary model's window.
	MaxTokens int

	// TailFraction is the share of the room below the due count kept as
	// verbatim recent messages. Zero means DefaultTailFraction. A negative
	// value keeps none, so the summary alone stands for the history.
	TailFraction float64
}

// New returns a Compactor with policy.
func New(policy Policy) *Compactor {
	return &Compactor{Policy: policy}
}

// Needed reports whether to compact messages, the history for the next
// Turn, given last, the usage of the last model call (see LastUsage).
// Compaction is due when that usage, or the estimated size of messages,
// reaches Policy's due count, raised when the fixed part takes more than
// half of it (see Compactor). The size estimate catches tool output added
// after the last model call. History with images, audio, video, or binary
// documents is not size-estimated. It returns false when nothing in
// messages can be summarized, or when the replacement history would itself
// be due, so compaction never repeats after every turn.
func (c *Compactor) Needed(messages []llm.Message, last llm.Usage) bool {
	p := c.plan(messages, last)
	return c.Policy.dueHistory(messages, last, p.dueTokens) &&
		len(p.covered) > 0 && !p.due
}

// StartIfNeeded returns the Start of the compaction turn to run before the
// turn that follows turns (oldest first, as dashi.BuildHistory takes them),
// and whether one is needed:
//   - The last turn has not ended, or is a compaction turn: not needed.
//     Its replacement, summarized or fallback, is the history.
//   - The last turn overflowed (session.NeedsCompaction): needed.
//   - Otherwise: needed when Needed(BuildHistory(turns), LastUsage(last)).
//   - Not needed when nothing is old enough to compact, or when the
//     replacement would itself be due, so compaction never repeats after
//     every turn.
//
// The Start's CommandID is "compact:" and the last turn's ID, so a retry, a
// restarted worker, or a second worker finds the same turn. It makes no
// model call and writes nothing. Its only errors are encoding errors.
func (c *Compactor) StartIfNeeded(ctx context.Context, turns []dashi.Turn) (
	start dashi.Start, needed bool, err error) {
	if len(turns) == 0 {
		return dashi.Start{}, false, nil
	}
	last := turns[len(turns)-1]
	view, err := last.View()
	if err != nil {
		return dashi.Start{}, false, err
	}
	if !view.Stop.Ended || view.Origin == dashi.StartCompaction {
		return dashi.Start{}, false, nil
	}
	history, _ := dashi.BuildHistory(turns)
	usage := LastUsage(last)
	p := c.plan(history, usage)
	if !session.NeedsCompaction(view) && !c.Policy.dueHistory(history, usage, p.dueTokens) {
		return dashi.Start{}, false, nil
	}
	if len(p.covered) == 0 || p.due {
		return dashi.Start{}, false, nil
	}
	start, err = c.start(history, p)
	if err != nil {
		return dashi.Start{}, false, err
	}
	start.CommandID = "compact:" + last.ID
	return start, true, nil
}

// Start builds the compaction turn's Start. It keeps the newest messages
// that fit in the tail budget, cutting only where a user message other than
// a tool result starts, and never between a tool call and its result. The
// messages before the cut are sent as a text transcript with the prompt,
// without their reminders, but with agent mail (a reminder with a From).
// Their system and developer messages are kept ahead of the summary, with
// their own role. last is the usage of the last model call (see
// LastUsage); zero means no estimate of tool definitions.
//
// The fact's SummaryFallback stands in for the summary if the turn ends
// without one: a note that the earlier messages were removed, then their
// user messages, the names of the tools called, and the agent's last
// reply, shortened to half the summary's output allowance. A transcript too
// large for the summary model is shortened, and if needed its middle
// items dropped, so Start never fails on size.
//
// The Start has origin StartCompaction and no tools, and its fact's Hash is
// the dashi.HistoryHash of messages. The caller sets its CommandID and may
// set Budget; a Session also sets the fact's Through. Start makes no model
// call, so it does not use ctx. It returns ErrNothingToCompact when
// messages hold nothing to summarize, or when the fixed part leaves no room
// below the window for a useful replacement.
func (c *Compactor) Start(
	_ context.Context,
	messages []llm.Message,
	last llm.Usage,
) (dashi.Start, error) {
	p := c.plan(messages, last)
	if len(p.covered) == 0 || p.due {
		return dashi.Start{}, ErrNothingToCompact
	}
	return c.start(messages, p)
}

// start builds the Start of plan p for messages.
func (c *Compactor) start(messages []llm.Message, p plan) (dashi.Start, error) {
	request := summaryRequest(p.covered, c.Prompt, p.summaryWindow, p.allowance)
	hash, err := dashi.HistoryHash(messages)
	if err != nil {
		return dashi.Start{}, err
	}
	removed := len(withoutAuthoritative(messages[:p.cut]))
	kept := authoritative(messages[:p.cut])
	return dashi.Start{
		Agent: dashi.Agent{
			Model:    c.Model,
			Settings: llm.Settings{MaxTokens: p.allowance},
			NoTools:  true,
		},
		Origin: dashi.StartCompaction,
		Input:  []llm.Message{request},
		Compaction: &dashi.CompactionFact{
			Keep:            append(slices.Clone(kept), messages[p.cut:]...),
			SummaryAt:       len(kept),
			Frame:           summaryFrame,
			Hash:            hash,
			SummaryFallback: compactiontext.Fallback(p.covered, removed, p.allowance/2*bytesPerToken),
		},
	}, nil
}

// plan is how one compaction of a history is sized and cut.
type plan struct {
	// cut is the index of the first message of the kept tail.
	cut int
	// covered is what the summary covers: the messages before cut, without
	// system and developer messages and without reminders.
	covered []llm.Message
	// allowance is the summary's output allowance in tokens.
	allowance int
	// summaryWindow is the summary model's window.
	summaryWindow int
	// dueTokens is the due count, raised when the fixed part takes more
	// than half of it.
	dueTokens int
	// due is set when the replacement history would itself be due.
	due bool
}

// plan sizes the compaction of messages. The room is the due count less
// the fixed part: system and developer messages, and what the last call
// sent besides the history (tool definitions, instructions), estimated as
// its input less the history's estimate. The output allowance is at most a
// quarter of the room, and the kept tail at most TailFraction of it and
// half of it less the allowance.
//
// When the fixed part takes more than half the due count, as many tool
// definitions can, the due count rises to the fixed part plus half the due
// count, up to the window, so compaction still has room instead of turning
// itself off.
func (c *Compactor) plan(messages []llm.Message, last llm.Usage) plan {
	window, _ := c.Policy.Window()
	due := c.Policy.dueTokens()
	extra := max(0, last.TotalInputTokens()-estimateAll(messages))
	fixed := extra + estimateAll(authoritative(messages))
	if fixed > due/2 {
		due = max(due, min(window, fixed+due/2))
	}
	room := max(0, due-fixed)

	p := plan{summaryWindow: c.summaryWindow(window), dueTokens: due}
	p.allowance = c.MaxTokens
	if p.allowance <= 0 {
		p.allowance = defaultSummaryTokens
	}
	p.allowance = max(1, min(p.allowance, room/4, p.summaryWindow/4))

	fraction := c.TailFraction
	if fraction == 0 {
		fraction = DefaultTailFraction
	}
	tail := min(int(max(0, fraction)*float64(room)), room/2-p.allowance)
	p.cut = tailStart(messages, max(0, tail))
	p.covered = withoutReminders(withoutAuthoritative(messages[:p.cut]))

	kept := estimateAll(authoritative(messages[:p.cut])) + estimateAll(messages[p.cut:])
	p.due = extra+kept+p.allowance >= due
	return p
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

var _ session.Compactor = (*Compactor)(nil)
