package dashi

import (
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
)

// ProjectHistory returns the projected history of turns, which are in
// order, oldest first: what the next turn sends as Start.History, or as
// Continue.History for the turn after them. It is the rule a Session uses,
// for a host that keeps its own turns.
//
// It walks back from the newest turn. At the newest compaction turn that
// completed with a summary, it uses that turn's replacement history and
// stops, so turns before it may be left out of turns. Every other turn
// gives its ProjectContext. A compaction turn that failed replaces nothing.
// A turn that cannot be replayed or projected is left out with a
// WarningTurnUnreadable, as a history that loses one old turn is still worth
// sending. When the compaction turn used has a Digest and turns holds turns
// before it, they are checked against it (compaction.Compactor and a
// Session set it; a host with its own compactor sets it with
// HistoryFingerprint); a mismatch warns
// WarningCompactionMismatch, and the replacement history is still used.
//
// The result shares message content with the turns, so the caller must not
// change it.
func ProjectHistory(turns []Turn) ([]llm.Message, []Warning) {
	views := make([]*TurnView, len(turns))
	at := func(i int) (historyTurn, error) {
		t := turns[i]
		h := historyTurn{
			id:       t.ID,
			messages: func() ([]llm.Message, error) { return ProjectContext(t) },
		}
		if views[i] == nil {
			view, err := Replay(t)
			if err != nil {
				return h, err
			}
			views[i] = &view
		}
		h.view = *views[i]
		return h, nil
	}
	check := func(i int, h historyTurn) *Warning {
		f := h.view.Compaction
		if f.Digest == "" || i == 0 {
			return nil
		}
		covered, _ := projectHistory(i, at, nil)
		return digestMismatch(h.id, f.Digest, covered)
	}
	return projectHistory(len(turns), at, check)
}

// historyTurn is one turn as the history rule reads it.
type historyTurn struct {
	id       string
	view     TurnView
	messages func() ([]llm.Message, error)
}

// projectHistory is the history rule of ProjectHistory over n turns, which
// at reads by index, newest first. An error from at makes the turn
// unreadable; its historyTurn still carries the ID. check, when not nil,
// returns a warning about the compaction turn whose replacement is used.
func projectHistory(
	n int,
	at func(i int) (historyTurn, error),
	check func(i int, h historyTurn) *Warning,
) ([]llm.Message, []Warning) {
	var parts [][]llm.Message // newest first
	var warnings []Warning    // newest first
	for i := n - 1; i >= 0; i-- {
		h, err := at(i)
		if err == nil && h.view.Origin == StartCompaction {
			msgs, ok := replacement(h.view)
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

// digestMismatch returns a WarningCompactionMismatch when covered, the
// projected history before compaction turn id, does not have digest.
func digestMismatch(id, digest string, covered []llm.Message) *Warning {
	if got, err := historyFingerprint(covered); err == nil && got == digest {
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

// replacement returns the replacement history of a compaction turn, and
// false when the turn did not complete with a summary (summaryText), so it
// replaces nothing.
func replacement(v TurnView) ([]llm.Message, bool) {
	f := v.Compaction
	answer := compactiontext.Summary(v.Answer)
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
