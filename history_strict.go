package dashi

import (
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/dashi/llm"
)

// HistoryError identifies the supplied Turn that prevents strict history
// construction. Err retains the replay, message, or source-hash failure.
type HistoryError struct {
	TurnID string
	Err    error
}

// Error reports the affected Turn and the failed integrity check.
func (e *HistoryError) Error() string {
	return fmt.Sprintf("dashi: history turn %s: %v", e.TurnID, e.Err)
}

// Unwrap retains the cause for errors.Is and errors.As.
func (e *HistoryError) Unwrap() error {
	return e.Err
}

// ErrCompactionMismatch means a supplied summary's source hash does not
// match the history built from the supplied Turns before it.
var ErrCompactionMismatch = errors.New("dashi: compaction source hash mismatch")

// BuildHistoryStrict builds history with BuildHistory's replacement rule,
// checking every supplied Turn in order, oldest first. Unlike BuildHistory,
// it returns nil history and a *HistoryError for unreadable records,
// unbuildable ordinary messages, or mismatched summary source hashes. It
// also checks supplied records hidden by later successful summaries.
// Recorded hook or budget notices do not cause construction to fail.
//
// Turns before a trusted applied summary may be omitted. A first summary has
// no supplied source to check. Valid historical summaries with no Hash remain
// readable, but carry no source hash proof. Failed or empty summaries replace
// nothing. Construction never changes the supplied records.
func BuildHistoryStrict(turns []Turn) ([]llm.Message, error) {
	at := historyReader(turns)
	for i := range turns {
		h, err := at(i)
		if err != nil {
			return nil, &HistoryError{TurnID: h.id, Err: err}
		}
		if h.view.Origin != StartCompaction {
			if _, err := h.messages(); err != nil {
				return nil, &HistoryError{TurnID: h.id, Err: err}
			}
			continue
		}
		if _, ok := h.view.ReplacementHistory(); !ok {
			continue
		}
		if h.view.Compaction.Hash == "" || i == 0 {
			continue
		}
		// Every preceding record and summary hash passed already. Reusing
		// the common selection rule cannot conceal an unchecked failure.
		covered, _ := buildHistory(i, at, nil)
		hash, err := historyHash(covered)
		if err == nil && hash != h.view.Compaction.Hash {
			err = ErrCompactionMismatch
		}
		if err != nil {
			return nil, &HistoryError{TurnID: h.id, Err: err}
		}
	}
	history, _ := buildHistory(len(turns), at, nil)
	return history, nil
}
