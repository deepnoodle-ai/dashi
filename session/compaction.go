package session

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync/atomic"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
)

// ErrNothingToCompact reports a compaction request with no eligible history. No turn
// was recorded and no model call was made. Package compaction aliases it.
var ErrNothingToCompact = errors.New("compaction: nothing to compact")

// Compact runs one compaction turn without checking the automatic threshold.
// It requires a Compactor and an ended last turn. A non-nil budget overrides
// the compactor's budget. It never consumes queued input or dispatches it.
// A recorded failed or empty summary warns WarningCompactionFailed and keeps
// the previous history. No eligible history returns zero RunResult and
// ErrNothingToCompact. Preparation and recorder failures remain Go errors.
func (s *Session) Compact(
	ctx context.Context,
	budget *dashi.Budget,
	opts ...dashi.RunOption,
) (dashi.RunResult, error) {
	return (&call{s: s, opts: opts}).manualCompact(ctx, budget)
}

// StreamCompact is Compact with events, including the compaction's final
// RunResult. Breaking the range interrupts execution and preserves the record
// for Continue. No queue turn follows it. Each iterator can be consumed once.
func (s *Session) StreamCompact(
	ctx context.Context,
	budget *dashi.Budget,
	opts ...dashi.RunOption,
) iter.Seq2[dashi.Event, error] {
	var started atomic.Bool
	return func(yield func(dashi.Event, error) bool) {
		if !started.CompareAndSwap(false, true) {
			yield(dashi.Event{}, dashi.ErrStreamConsumed)
			return
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		(&call{s: s, opts: opts, yield: yield}).manualCompact(ctx, budget)
	}
}

func (cl *call) manualCompact(ctx context.Context, budget *dashi.Budget) (dashi.RunResult, error) {
	s := cl.s
	s.mu.Lock()
	p, err := s.planManualCompaction(ctx, budget)
	s.mu.Unlock()
	if err == nil {
		err = s.buildCompaction(ctx, p)
	}
	if err != nil {
		return dashi.RunResult{}, cl.fail(err)
	}
	out, lost, err := cl.runCompaction(ctx, *p)
	if lost {
		s.mu.Lock()
		out, err = s.compactionSlot(ctx, *p)
		s.mu.Unlock()
	}
	// A CommitError names the attempted Start even when its write was
	// rejected or unacknowledged. That identity alone is not a turn record.
	if len(out.Turn.Steps) == 0 {
		cl.fail(err)
	} else {
		cl.report(out, err)
	}
	return out, err
}

func (s *Session) planManualCompaction(
	ctx context.Context,
	budget *dashi.Budget,
) (*startPlan, error) {
	s.ready()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Compactor == nil {
		return nil, errors.New("session: Session.Compactor is required")
	}
	if s.ID == "" {
		return nil, ErrNothingToCompact
	}
	last, err := s.findLast(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkExists(last); err != nil {
		return nil, err
	}
	if last == 0 {
		return nil, ErrNothingToCompact
	}
	if busy := s.compactingBusy(); busy != nil {
		return nil, busy
	}
	t, err := s.load(ctx, last)
	if err != nil {
		return nil, err
	}
	if r := s.refine(ctx, t); r.next != dashi.NextNone {
		return nil, &TurnBusyError{
			TurnID: t.turn.ID,
			Origin: t.view.Origin,
			Next:   r.next,
			CallID: resultUnknownCall(t),
		}
	}
	messages, warnings := s.history(ctx, last)
	p := s.reserveCompaction(ctx, last, messages, budget)
	p.warnings = warnings
	return p, nil
}

// compactingBusy returns the *TurnBusyError for the compaction turn this
// handle is building or running, or nil when there is none. The turn after
// it is not free, even before the compaction turn is recorded, so a new
// turn must not plan it, nor ask the Compactor for it again. The caller
// holds mu.
func (s *Session) compactingBusy() *TurnBusyError {
	if s.compacting == "" {
		return nil
	}
	return &TurnBusyError{
		TurnID: s.compacting,
		Origin: dashi.StartCompaction,
		Next:   dashi.NextRunning,
	}
}

// compactionSlot resolves a lost manual reservation without allocating a
// second summary slot. The recorder remains the authority about the winner.
func (s *Session) compactionSlot(ctx context.Context, p startPlan) (dashi.RunResult, error) {
	t, err := s.load(ctx, p.n)
	if err != nil {
		return dashi.RunResult{}, err
	}
	if t.view.Origin != dashi.StartCompaction ||
		t.turn.Steps[0].CommandID != p.command.CommandID {
		return dashi.RunResult{}, dashi.ErrStaleTurn
	}
	r := s.refine(ctx, t)
	if r.next == dashi.NextRunning || r.next == dashi.NextRecover {
		return dashi.RunResult{}, &TurnBusyError{
			TurnID: t.turn.ID,
			Origin: dashi.StartCompaction,
			Next:   r.next,
		}
	}
	out := dashi.RunResult{Turn: t.turn.Clone(), TurnView: t.view}
	if _, ok := t.view.ReplacementHistory(); !ok {
		out.SessionWarnings = append(out.SessionWarnings, closedWarning(t))
	}
	return out, nil
}

// Compactor decides when and how a Session compacts its history.
// Package compaction implements it.
type Compactor interface {
	// Needed reports whether to compact before the next turn. messages is
	// the session's history, as the next turn would receive it, and last is
	// the usage of the last model call of the last turn.
	Needed(messages []llm.Message, last llm.Usage) bool
	// Start builds the compaction turn's Start: a summarizer agent, the
	// summary request as Input, and Compaction with the kept messages,
	// where the summary goes among them, its frame, and its Hash, the
	// dashi.HistoryHash of messages. The session sets the command ID,
	// origin, history, and Through, fills an empty Hash, turns tools
	// off, and applies the caller's Start.Budget when it has one.
	//
	// The session calls Start without holding its lock, so a slow Start,
	// such as one that asks a model which messages to keep, never blocks
	// Steer or Enqueue. Meanwhile the session counts as compacting: Steer
	// and a new Start refuse with a *TurnBusyError, as they do while the
	// compaction turn runs, so the handle calls Start once per compaction
	// turn. ctx ends with the call that asked for the compaction.
	Start(ctx context.Context, messages []llm.Message) (dashi.Start, error)
}

// isCompaction reports whether command c runs a compaction turn.
func isCompaction(turn dashi.Turn, c dashi.Command) bool {
	if start, ok := c.(dashi.Start); ok {
		return start.Origin == dashi.StartCompaction
	}
	return len(turn.Steps) > 0 && turn.Steps[0].Started != nil &&
		turn.Steps[0].Started.Origin == dashi.StartCompaction
}

// lastUsage returns the usage of the last model call of turn that reported
// any: the size of the context at the end of the turn.
func lastUsage(turn dashi.Turn) llm.Usage {
	for i := len(turn.Steps) - 1; i >= 0; i-- {
		m := turn.Steps[i].Model
		if m != nil && m.Result != nil && m.Result.Usage != nil {
			return *m.Result.Usage
		}
	}
	return llm.Usage{}
}

// planCompaction runs the compaction check before turn last+1: when the
// last turn completed and the Compactor asks for it, it reserves the slot
// and returns the plan of a compaction turn, whose Start buildCompaction
// makes outside mu. When needsCompaction is true, the last turn need not
// have completed (see NeedsCompaction). The check is skipped after other
// turns that failed or were canceled, and after a compaction turn, so a
// failing summary is tried at most once per turn. A non-nil budget, the
// caller's Start.Budget, caps the compaction turn too. The caller holds mu.
func (s *Session) planCompaction(
	ctx context.Context,
	last int,
	messages []llm.Message,
	budget *dashi.Budget,
	needsCompaction bool,
) *startPlan {
	if s.Compactor == nil || last == 0 {
		return nil
	}
	t, err := s.load(ctx, last)
	if err != nil || !needsCompaction && t.view.Stop.Kind != dashi.StopCompleted ||
		t.view.Origin == dashi.StartCompaction ||
		!s.Compactor.Needed(messages, lastUsage(t.turn)) {
		return nil
	}
	return s.reserveCompaction(ctx, last, messages, budget)
}

// compactionInput is what a compaction turn's Start is built from, outside
// mu: the history it replaces, the caller's budget, and the agent whose
// model writes the summary unless the Compactor names another.
type compactionInput struct {
	messages []llm.Message
	budget   *dashi.Budget
	agent    dashi.Agent
}

// reserveCompaction marks turn last+1 as this handle's compaction turn, so
// Steer and a new Start refuse while the Compactor builds its Start and
// while it runs, and returns its plan without a command. The caller holds
// mu, and passes the plan to buildCompaction after releasing it.
func (s *Session) reserveCompaction(
	ctx context.Context,
	last int,
	messages []llm.Message,
	budget *dashi.Budget,
) *startPlan {
	id := TurnID(s.ID, last+1)
	s.running[id]++
	s.compacting = id
	return &startPlan{
		n:       last + 1,
		id:      id,
		runner:  s.runner(),
		compact: true,
		summarize: &compactionInput{
			messages: messages,
			budget:   budget,
			agent:    s.agentFor(ctx, dashi.Agent{}, last),
		},
	}
}

// buildCompaction asks the Compactor for a reserved compaction turn's
// Start and completes p.command. It runs without mu, so a slow Compactor
// blocks neither Steer nor Enqueue. When it fails, it gives the slot back,
// as the end of a run would.
func (s *Session) buildCompaction(ctx context.Context, p *startPlan) error {
	in := p.summarize
	c, err := s.Compactor.Start(ctx, llmclone.Messages(in.messages))
	if err == nil && c.Compaction == nil {
		err = errors.New("the compactor's Start has no Compaction")
	}
	if err == nil {
		c.CommandID = "compact:" + p.id
		c.Origin = dashi.StartCompaction
		c.History = nil
		c.Compaction = cloneCompaction(c.Compaction)
		c.Compaction.Through = p.n - 1
		if c.Compaction.Hash == "" {
			// A custom Compactor may leave it empty; the check still
			// applies.
			c.Compaction.Hash, err = dashi.HistoryHash(in.messages)
		}
	}
	if err != nil {
		s.mu.Lock()
		s.stopRunning(p.id)
		s.mu.Unlock()
		return err
	}
	if c.Agent.Model == "" {
		// The summary is written by the session's model, unless the
		// compactor names another.
		a := in.agent
		c.Agent.Provider, c.Agent.API, c.Agent.Model = a.Provider, a.API, a.Model
	}
	c.Agent.Tools, c.Agent.NoTools = nil, true
	if in.budget != nil {
		b := in.budget.Clone()
		c.Budget = &b
	}
	p.command = c
	return nil
}

// compact runs a planned compaction turn. For Stream, it reports the
// turn's RunResult event. A compaction turn that fails or ends without a
// reply adds WarningCompactionFailed to the call, and one that stopped
// without ending is closed first. It reports lost when another worker took
// the slot, so the caller plans again and finds that worker's turn. It
// returns an error only when the call must stop: the consumer broke the
// range, or the context ended; the next Start then closes the turn.
func (cl *call) compact(ctx context.Context, p startPlan) (lost bool, _ error) {
	if err := cl.s.buildCompaction(ctx, &p); err != nil {
		if ctx.Err() != nil {
			return false, cl.fail(ctx.Err())
		}
		// The turn runs without compaction, and its result carries the
		// warning.
		cl.warnings = append(cl.warnings, compactionFailed(fmt.Sprintf(
			"the compaction turn could not start: %v", err,
		)))
		return false, nil
	}
	out, lost, err := cl.runCompaction(ctx, p)
	if lost {
		return true, nil
	}
	// A preparation or recording failure is a warning for the automatic
	// path, which still runs the caller's turn.
	var reported error
	if cl.stopped || ctx.Err() != nil {
		reported = err
	}
	if len(out.Turn.Steps) > 0 {
		cl.report(out, reported)
	} else if reported != nil {
		cl.fail(reported)
	}
	cl.warnings = append(cl.warnings, out.SessionWarnings...)
	if cl.stopped {
		return false, context.Canceled
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	// Automatic compaction is best effort. Manual calls return the error.
	return false, nil
}

func (cl *call) runCompaction(
	ctx context.Context,
	p startPlan,
) (dashi.RunResult, bool, error) {
	s := cl.s
	out, err := cl.exec(ctx, p.runner, dashi.NewTurn(p.id), p.command)
	out, err = s.finish(ctx, p.n, p.id, out, err, p.warnings, nil, false)
	if takenSlot(err) || err == nil && out.Stop.Inferred {
		return out, true, nil
	}
	if cl.stopped {
		return out, false, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return out, false, err
	}
	if _, ok := out.TurnView.ReplacementHistory(); ok && err == nil {
		return out, false, nil
	}
	var warning dashi.Warning
	switch {
	case err != nil:
		warning = compactionFailed(fmt.Sprintf(
			"compaction turn %s failed: %v", p.id, err,
		))
	case out.Next() == dashi.NextNone:
		warning = compactionFailed(fmt.Sprintf(
			"compaction turn %s ended %s without a summary",
			p.id,
			out.Stop.Kind,
		))
	default:
		// Close it now, so a queue turn, which closes nothing, can follow.
		s.mu.Lock()
		t, closeErr := s.load(ctx, p.n)
		var events []dashi.Event
		if closeErr == nil {
			_, events, closeErr = s.closeLast(ctx, p.n, t, cl.opts, cl.yield != nil)
			if closeErr == nil {
				var closed *sessionTurn
				closed, closeErr = s.load(ctx, p.n)
				if closeErr == nil {
					out.Turn, out.TurnView = closed.turn.Clone(), closed.view
				}
			}
		}
		s.mu.Unlock()
		if !cl.forward(events) {
			return out, false, context.Canceled
		}
		if closeErr != nil {
			out.SessionWarnings = append(out.SessionWarnings,
				compactionFailed(fmt.Sprintf("compaction turn %s could not close: %v", p.id, closeErr)))
			return out, false, closeErr
		}
		warning = closedWarning(t)
	}
	out.SessionWarnings = append(out.SessionWarnings, warning)
	return out, false, err
}

// takenSlot reports whether a compaction turn failed because another
// worker took its slot: the same compact: key runs one turn, so a stale-turn
// or reused-command-ID conflict at any step means that worker records it.
func takenSlot(err error) bool {
	return errors.Is(err, dashi.ErrStaleTurn) ||
		errors.Is(err, dashi.ErrCommandIDReused)
}

func compactionFailed(msg string) dashi.Warning {
	return dashi.Warning{
		Code:    WarningCompactionFailed,
		Message: msg + "; the previous history is kept",
	}
}

// checkHash compares a completed compaction turn's hash with the history
// built from the turns it covers, when this handle already holds
// them, and returns a WarningCompactionMismatch while they differ. Loading
// the covered turns only to check would undo the short load. The caller
// holds mu.
func (s *Session) checkHash(ctx context.Context, t *sessionTurn) *dashi.Warning {
	if t.checked {
		return t.mismatch
	}
	f := t.view.Compaction
	if !s.holds(f.Through) {
		return nil
	}
	t.checked = true
	messages, _ := s.history(ctx, f.Through)
	if got, err := dashi.HistoryHash(messages); err != nil || got != f.Hash {
		t.mismatch = &dashi.Warning{
			Code: dashi.WarningCompactionMismatch,
			Message: fmt.Sprintf(
				"compaction turn %s does not match the turns it covers; its "+
					"replacement history is still used",
				t.turn.ID,
			),
		}
	}
	return t.mismatch
}

// cloneCompaction copies a compaction fact, so the session never shares the
// Compactor's messages.
func cloneCompaction(f *dashi.CompactionFact) *dashi.CompactionFact {
	c := *f
	c.Keep = llmclone.Messages(f.Keep)
	return &c
}

// holds reports whether the cache has every turn that building the history
// of turns 1 through n reads. The caller holds mu.
func (s *Session) holds(n int) bool {
	for ; n >= 1; n-- {
		t := s.turns[n]
		if t == nil {
			return false
		}
		if _, ok := t.view.ReplacementHistory(); ok {
			return true
		}
	}
	return true
}
