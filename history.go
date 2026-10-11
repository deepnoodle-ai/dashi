package dashi

import (
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
)

// BuildHistory returns the history built from turns, which are in
// order, oldest first: what the next turn sends as Start.History, or as
// Continue.History for the turn after them. It is the rule a session.Session
// uses, for a host that keeps its own turns.
//
// It walks back from the newest turn. At the newest compaction turn that
// completed with a summary, it uses that turn's replacement history and
// stops, so turns before it may be left out of turns. Every other turn
// gives its TurnMessages. A compaction turn that failed replaces nothing.
// A turn whose view or messages cannot be built is left out with a
// WarningTurnUnreadable, as a history that loses one old turn is still worth
// sending. When the compaction turn used has a Hash and turns holds turns
// before it, they are checked against it (compaction.Compactor and
// session.Session set it; a host with its own compactor sets it with
// HistoryHash); a mismatch warns WarningCompactionMismatch, and the
// replacement history is still used.
//
// The result shares message content with the turns, so the caller must not
// change it.
func BuildHistory(turns []Turn) ([]llm.Message, []Warning) {
	at := historyReader(turns)
	check := func(i int, h historyTurn) *Warning {
		f := h.view.Compaction
		if f.Hash == "" || i == 0 {
			return nil
		}
		covered, _ := buildHistory(i, at, nil)
		return hashMismatch(h.id, f.Hash, covered)
	}
	return buildHistory(len(turns), at, check)
}

// historyReader caches projections for one construction. Both policies use
// the same reader and replacement selection rule.
func historyReader(turns []Turn) func(int) (historyTurn, error) {
	views := make([]*TurnView, len(turns))
	messages := make([][]llm.Message, len(turns))
	read := make([]bool, len(turns))
	errors := make([]error, len(turns))
	return func(i int) (historyTurn, error) {
		t := turns[i]
		h := historyTurn{
			id: t.ID,
			messages: func() ([]llm.Message, error) {
				if !read[i] {
					messages[i], errors[i] = TurnMessages(t)
					read[i] = true
				}
				return messages[i], errors[i]
			},
		}
		if views[i] == nil {
			view, err := t.View()
			if err != nil {
				return h, err
			}
			views[i] = &view
		}
		h.view = *views[i]
		return h, nil
	}
}

// historyTurn is one turn as the history rule reads it.
type historyTurn struct {
	id       string
	view     TurnView
	messages func() ([]llm.Message, error)
}

// buildHistory is the history rule of BuildHistory over n turns, which
// at reads by index, newest first. An error from at makes the turn
// unreadable; its historyTurn still carries the ID. check, when not nil,
// returns a warning about the compaction turn whose replacement is used.
func buildHistory(
	n int,
	at func(i int) (historyTurn, error),
	check func(i int, h historyTurn) *Warning,
) ([]llm.Message, []Warning) {
	var parts [][]llm.Message // newest first
	var warnings []Warning    // newest first
	for i := n - 1; i >= 0; i-- {
		h, err := at(i)
		if err == nil && h.view.Origin == StartCompaction {
			msgs, ok := h.view.ReplacementHistory()
			if !ok {
				continue // a failed compaction replaces nothing
			}
			if check != nil {
				if w := check(i, h); w != nil {
					warnings = append(warnings, *w)
				}
			}
			parts = append(parts, msgs)
			break
		}
		var msgs []llm.Message
		if err == nil {
			msgs, err = h.messages()
		}
		if err != nil {
			warnings = append(warnings, unreadable(h.id, err))
			continue
		}
		parts = append(parts, msgs)
	}
	var out []llm.Message
	for i := len(parts) - 1; i >= 0; i-- {
		out = append(out, parts[i]...)
	}
	slices.Reverse(warnings)
	return out, warnings
}

func unreadable(turnID string, err error) Warning {
	return Warning{
		Code: WarningTurnUnreadable,
		Message: fmt.Sprintf(
			"turn %s was left out of the history: %v",
			turnID,
			err,
		),
	}
}

// hashMismatch returns a WarningCompactionMismatch when covered, the
// history before compaction turn id, does not have hash.
func hashMismatch(id, hash string, covered []llm.Message) *Warning {
	if got, err := historyHash(covered); err == nil && got == hash {
		return nil
	}
	return &Warning{
		Code: WarningCompactionMismatch,
		Message: fmt.Sprintf(
			"compaction turn %s does not match the turns it covers; its "+
				"replacement history is still used",
			id,
		),
	}
}

// ReplacementHistory returns the history a compaction turn replaces the
// history of the turns it covers with: CompactionFact.Keep with the summary,
// after its Frame, at SummaryAt. It returns false for any other turn and for
// a compaction turn that did not complete with a summary, which replaces
// nothing. The result shares message content with v.
func (v TurnView) ReplacementHistory() ([]llm.Message, bool) {
	f := v.Compaction
	answer := compactiontext.Summary(v.Reply)
	if f == nil || v.Stop.Kind != StopCompleted || answer == "" {
		return nil, false
	}
	summary := answer
	if f.Frame != "" {
		summary = f.Frame + "\n\n" + answer
	}
	out := make([]llm.Message, 0, len(f.Keep)+1)
	out = append(out, f.Keep[:f.SummaryAt]...)
	out = append(out, llm.UserMessage(llm.Text(summary)))
	return append(out, f.Keep[f.SummaryAt:]...), true
}
