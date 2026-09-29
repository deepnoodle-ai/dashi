package dashi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Compactor decides when and how a Session compacts its projected history.
// Package compaction implements it.
type Compactor interface {
	// Needed reports whether to compact before the next turn. projected is
	// the projected history, and last is the usage of the last model call
	// of the last turn.
	Needed(projected []llm.Message, last llm.Usage) bool
	// Start builds the compaction turn's Start: a summarizer agent, the
	// summary request as Input, and Compaction with the kept messages,
	// where the summary goes among them, and its frame. The session sets
	// the command ID, origin, history, Through, and Digest, and turns tools
	// off.
	Start(projected []llm.Message) (Start, error)
}

// CompactionFact is recorded in the start fact of a compaction turn. With
// the turn's answer it forms the replacement history: Keep[:SummaryAt], a
// user message of Frame and the answer, then Keep[SummaryAt:]. This is the
// one place a turn records earlier messages, so a reader with no Compactor
// builds the same history.
type CompactionFact struct {
	// Through is the last turn covered.
	Through int `json:"through"`
	// Digest is the history fingerprint of the projected history replaced.
	Digest string `json:"digest"`
	// Keep holds the messages the replacement history keeps, which need not
	// be a contiguous tail.
	Keep []llm.Message `json:"keep,omitempty"`
	// SummaryAt is the index in Keep where the summary goes.
	SummaryAt int `json:"summary_at"`
	// Frame is the text that introduces the summary.
	Frame string `json:"frame"`
}

func (f *CompactionFact) clone() *CompactionFact {
	if f == nil {
		return nil
	}
	c := *f
	c.Keep = cloneMessages(f.Keep)
	return &c
}

// isCompaction reports whether command c runs a compaction turn.
func isCompaction(turn Turn, c Command) bool {
	if start, ok := c.(Start); ok {
		return start.Origin == StartCompaction
	}
	return len(turn.Steps) > 0 && turn.Steps[0].Started != nil &&
		turn.Steps[0].Started.Origin == StartCompaction
}

// validateCompaction checks that a compaction fact comes with, and only
// with, origin StartCompaction, and that it is well formed.
func validateCompaction(origin StartOrigin, f *CompactionFact) error {
	switch {
	case f == nil && origin == StartCompaction:
		return errors.New("compaction start requires a compaction fact")
	case f == nil:
		return nil
	case origin != StartCompaction:
		return errors.New("compaction fact requires origin compaction")
	case f.Through < 0 || f.SummaryAt < 0 || f.SummaryAt > len(f.Keep):
		return errors.New("compaction fact is out of range")
	}
	return validateHistory(f.Keep)
}

// replacement returns the replacement history of a compaction turn, and
// false when the turn did not complete with an answer, so it replaces
// nothing.
func replacement(v TurnView) ([]llm.Message, bool) {
	f := v.Compaction
	answer := strings.TrimSpace(v.Answer)
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

// lastUsage returns the usage of the last model call of turn that reported
// any: the size of the context at the end of the turn.
func lastUsage(turn Turn) llm.Usage {
	for i := len(turn.Steps) - 1; i >= 0; i-- {
		m := turn.Steps[i].Model
		if m != nil && m.Result != nil && m.Result.Usage != nil {
			return *m.Result.Usage
		}
	}
	return llm.Usage{}
}

// planCompaction runs the compaction check before turn last+1: when the
// last turn completed and the Compactor asks for it, it returns the plan of
// a compaction turn. A Compactor that cannot build its Start gives a
// WarningCompactionFailed instead, and the turn runs without compaction.
// The check is skipped after a turn that failed or was canceled, and after
// a compaction turn, so a failing summary is tried at most once per
// completed turn. The caller holds mu.
func (s *Session) planCompaction(
	ctx context.Context,
	last int,
	projected []llm.Message,
) (*startPlan, []Warning) {
	if s.Compactor == nil || last == 0 {
		return nil, nil
	}
	t, err := s.load(ctx, last)
	if err != nil || t.view.Stop.Kind != StopCompleted ||
		t.view.Origin == StartCompaction ||
		!s.Compactor.Needed(projected, lastUsage(t.turn)) {
		return nil, nil
	}
	c, err := s.Compactor.Start(cloneMessages(projected))
	if err == nil && c.Compaction == nil {
		err = errors.New("the compactor's Start has no Compaction")
	}
	digest, ferr := historyFingerprint(projected)
	if err = errors.Join(err, ferr); err != nil {
		return nil, []Warning{compactionFailed(fmt.Sprintf(
			"the compaction turn could not start: %v",
			err,
		))}
	}
	id := TurnID(s.ID, last+1)
	c.CommandID = "compact:" + id
	c.Origin = StartCompaction
	c.History = nil
	c.Compaction = c.Compaction.clone()
	c.Compaction.Through = last
	c.Compaction.Digest = digest
	if c.Agent.Model == "" {
		// The summary is written by the session's model, unless the
		// compactor names another.
		a := s.agentFor(ctx, Agent{}, last)
		c.Agent.Provider, c.Agent.API, c.Agent.Model = a.Provider, a.API, a.Model
	}
	c.Agent.Tools, c.Agent.NoTools = nil, true
	s.running[id]++
	s.compacting = id
	return &startPlan{
		n:       last + 1,
		id:      id,
		command: c,
		runner:  s.runner(),
		compact: true,
	}, nil
}

// compact runs a planned compaction turn. For Stream, it reports the
// turn's Outcome event. A compaction turn that fails or ends without an
// answer adds WarningCompactionFailed to the call, and one that stopped
// without ending is closed first. It returns an error only when the call
// must stop: the consumer broke the range, or the context ended; the next
// Start then closes the turn.
func (cl *call) compact(ctx context.Context, p startPlan) error {
	s := cl.s
	out, err := cl.exec(ctx, p.runner, NewTurn(p.id), p.command)
	out, err = s.finish(ctx, p.n, p.id, out, err, nil, nil, false)
	if lostStart(err) || takenKey(err) || err == nil && out.Stop.Projected {
		// Another worker took this slot; planning again finds its turn.
		return nil
	}
	if err == nil {
		cl.report(out, nil)
		if cl.yield == nil {
			cl.warnings = append(cl.warnings, out.SessionWarnings...)
		}
	}
	if cl.stopped {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := replacement(out.TurnView); ok && err == nil {
		return nil
	}
	var warning Warning
	switch {
	case err != nil:
		warning = compactionFailed(fmt.Sprintf(
			"compaction turn %s failed: %v", p.id, err,
		))
	case out.Next() == NextNone:
		warning = compactionFailed(fmt.Sprintf(
			"compaction turn %s ended %s without a summary",
			p.id,
			out.Stop.Kind,
		))
	default:
		// Close it now, so a queue turn, which closes nothing, can follow.
		s.mu.Lock()
		t, err := s.load(ctx, p.n)
		var events []Event
		if err == nil {
			_, events, err = s.closeLast(ctx, p.n, t, cl.opts, cl.yield != nil)
		}
		s.mu.Unlock()
		if !cl.forward(events) {
			return context.Canceled
		}
		if err != nil {
			// The next Start closes it, with this warning.
			return nil
		}
		warning = closedWarning(t)
	}
	cl.warnings = append(cl.warnings, warning)
	return nil
}

func compactionFailed(msg string) Warning {
	return Warning{
		Code:    WarningCompactionFailed,
		Message: msg + "; the previous projected history is kept",
	}
}

// checkDigest compares a completed compaction turn's digest with the
// projected history of the turns it covers, when this handle already holds
// them, and returns a WarningCompactionMismatch while they differ. Loading
// the covered turns only to check would undo the short load. The caller
// holds mu.
func (s *Session) checkDigest(ctx context.Context, t *sessionTurn) *Warning {
	if t.checked {
		return t.mismatch
	}
	f := t.view.Compaction
	if !s.holds(f.Through) {
		return nil
	}
	t.checked = true
	projected, _ := s.history(ctx, f.Through)
	if digest, err := historyFingerprint(projected); err == nil &&
		digest == f.Digest {
		return nil
	}
	t.mismatch = &Warning{
		Code: WarningCompactionMismatch,
		Message: fmt.Sprintf(
			"compaction turn %s does not match the turns it covers; its "+
				"replacement history is still used",
			t.turn.ID,
		),
	}
	return t.mismatch
}

// holds reports whether the cache has every turn that the projected
// history of turns 1 through n reads. The caller holds mu.
func (s *Session) holds(n int) bool {
	for ; n >= 1; n-- {
		t := s.turns[n]
		if t == nil {
			return false
		}
		if _, ok := replacement(t.view); ok {
			return true
		}
	}
	return true
}
