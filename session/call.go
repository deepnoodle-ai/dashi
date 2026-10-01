package session

import (
	"context"
	"errors"
	"iter"
	"slices"
	"sync/atomic"

	"github.com/deepnoodle-ai/dashi"
)

// Stream is Run with events. It yields the events of every turn the call
// runs, in order: the step that closes a stopped last turn, a compaction
// turn, the command's own turn, and the queue turns that follow it. Each event carries its
// TurnID, and each turn ends with its own RunResult event, with the session's
// warnings; the last RunResult event is the call's result. An error the
// session returns before any turn runs, such as *TurnBusyError, is yielded
// alone.
//
// A retried Start whose turn is running (a duplicate with NextRunning) is
// followed instead, when the recorder is a dashi.StepReader and a
// dashi.Loader: Stream yields that turn's steps as they commit and ends
// with its RunResult event once it stops.
//
// Breaking the range ends the context, which records an interruption (or a
// cancel under dashi.ContextCancel), and no further queue turn starts. The
// handle keeps the steps the turn committed, so a later Continue resumes it
// even when the recorder cannot load turns. A second range yields
// dashi.ErrStreamConsumed.
func (s *Session) Stream(
	ctx context.Context,
	c dashi.Command,
	opts ...dashi.RunOption,
) iter.Seq2[dashi.Event, error] {
	var started atomic.Bool
	return func(yield func(dashi.Event, error) bool) {
		if !started.CompareAndSwap(false, true) {
			yield(dashi.Event{}, dashi.ErrStreamConsumed)
			return
		}
		if err := checkCommand(c); err != nil {
			yield(dashi.Event{}, err)
			return
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		(&call{s: s, opts: opts, yield: yield}).run(ctx, c)
	}
}

// RunQueue runs queued input (see Enqueue) when the session is idle: its
// last turn ended with NextNone, or it has none. It starts a queue turn
// from every queued item and repeats, as Run does after a turn, until the
// queue is empty or a turn stops any other way. It returns the last turn's
// result and true, or, when it ran nothing, the session's last turn's
// result and false: nothing was queued, or the last turn has not ended, so
// the items wait. A queue turn's Start has the command ID "queue:" and the
// sorted item IDs in JSON, origin StartUser when any item came from the
// user and StartScheduled otherwise, and the items' messages as Input, in
// queue order. A queue turn whose key a turn in the idempotency window
// already started is a duplicate: its items were already added to that
// turn, so they leave the queue and nothing runs. A last turn that cannot be read fails the
// check, even under Exclusive, since it may hold the key: RunQueue returns
// the error and runs nothing. While queued items stay reserved for a turn
// the session cannot read, RunQueue returns that error and runs nothing;
// the results of other commands warn WarningQueueBlocked meanwhile.
func (s *Session) RunQueue(
	ctx context.Context,
	opts ...dashi.RunOption,
) (dashi.RunResult, bool, error) {
	cl := &call{s: s, opts: opts}
	out, ran, err := cl.queueTurn(ctx)
	if err != nil {
		return out, ran, err
	}
	if !ran {
		out, err = s.lastResult(ctx)
		return out, false, err
	}
	out, err = cl.dispatch(ctx, out, err)
	return out, true, err
}

// call is one Run, Stream, or RunQueue call: the commands it runs and, for
// Stream, where their events go.
type call struct {
	s    *Session
	opts []dashi.RunOption
	// yield receives events; nil for Run.
	yield func(dashi.Event, error) bool
	// stopped is set once the consumer broke the range.
	stopped bool
	// started is set when the last exec of a Start saw its turn_started
	// step: the engine reports a step only once it committed, so the
	// queued items the Start carried were added to the turn.
	started bool
	// warnings holds session warnings of a compaction turn for the result
	// of the turn it ran before.
	warnings []dashi.Warning
}

func (cl *call) run(ctx context.Context, c dashi.Command) (dashi.RunResult, error) {
	var out dashi.RunResult
	var err error
	switch c := c.(type) {
	case dashi.Start:
		out, err = cl.start(ctx, c)
	case dashi.Continue:
		out, err = cl.command(ctx, c)
	default:
		// Resolve, AddMessages, and Cancel run no model call, so nothing queued
		// runs after them.
		return cl.command(ctx, c)
	}
	return cl.dispatch(ctx, out, err)
}

// dispatch runs queue turns while the last one ended with NextNone and the
// queue holds items. For Run, the result keeps the session warnings of the
// turns before it.
func (cl *call) dispatch(
	ctx context.Context,
	out dashi.RunResult,
	err error,
) (dashi.RunResult, error) {
	for err == nil && !cl.stopped && ctx.Err() == nil &&
		out.Next() == dashi.NextNone {
		next, ran, qerr := cl.queueTurn(ctx)
		if !ran && (qerr == nil || errors.Is(qerr, errReserved)) {
			// Nothing ran, or the queue waits on a reservation, which the
			// result already warns of.
			break
		}
		if cl.yield == nil {
			next.SessionWarnings = append(
				append([]dashi.Warning(nil), out.SessionWarnings...),
				next.SessionWarnings...,
			)
		}
		out, err = next, qerr
	}
	return out, err
}

// start runs a new turn. A Start that loses a race on its turn ID, or a
// keyed Start that finds its command ID taken, reloads and applies the
// rules once more.
func (cl *call) start(ctx context.Context, c dashi.Start) (dashi.RunResult, error) {
	s := cl.s
	fresh := c.CommandID == ""
	if fresh {
		// A generated key cannot match an earlier turn, so the idempotency
		// scan is skipped.
		c.CommandID = dashi.NewID()
	}
	compacted := false
	for attempt := 0; ; {
		s.mu.Lock()
		p, err := s.planStart(ctx, c, fresh, cl.opts, cl.yield != nil, compacted)
		s.mu.Unlock()
		if !cl.forward(p.closing) {
			if err == nil && p.done == nil {
				// The turn was planned but never ran: release it, and put
				// the queued items it carried back.
				s.finish(ctx, p.n, p.id, dashi.RunResult{}, context.Canceled, nil, p.reserved, false)
			}
			return dashi.RunResult{}, context.Canceled
		}
		if p.done != nil {
			out := s.withMemoryWarning(*p.done)
			cl.report(out, err)
			return out, err
		}
		if err != nil {
			var busy *TurnBusyError
			if cl.yield != nil && errors.As(err, &busy) && busy.Duplicate &&
				busy.Next == dashi.NextRunning {
				return cl.follow(ctx, busy.TurnID, err)
			}
			return dashi.RunResult{}, cl.fail(err)
		}
		if p.compact {
			// The compaction turn runs first; then the Start is planned
			// again, after it.
			lost, err := cl.compact(ctx, p)
			if err != nil {
				return dashi.RunResult{}, err
			}
			// A lost slot leaves the check to the next plan, after the
			// other worker's turns.
			compacted = !lost
			continue
		}
		out, err := cl.exec(ctx, p.runner, dashi.NewTurn(p.id), p.command)
		out, err = s.finish(ctx, p.n, p.id, out, err, cl.takeWarnings(p.warnings), p.reserved, cl.started)
		if attempt == 0 && (lostStart(err) || !fresh && takenKey(err)) {
			attempt++
			continue
		}
		// A keyed Start that raced another worker's equal Start to the same
		// turn gets that turn's result from the engine, as a retry. With
		// no stop recorded, the turn may still be running, so plan again:
		// the idempotency check then refuses it as a duplicate.
		if attempt < 2 && !fresh && err == nil && out.Stop.Inferred {
			attempt++
			continue
		}
		cl.report(out, err)
		return out, err
	}
}

// command routes a command other than Start to the last turn.
func (cl *call) command(ctx context.Context, c dashi.Command) (dashi.RunResult, error) {
	s := cl.s
	s.mu.Lock()
	p, err := s.planCommand(ctx, &c)
	s.mu.Unlock()
	if err != nil {
		return dashi.RunResult{}, cl.fail(err)
	}
	out, err := cl.exec(ctx, p.runner, p.turn, c)
	out, err = s.finish(ctx, p.n, p.turn.ID, out, err, p.warnings, nil, false)
	cl.report(out, err)
	return out, err
}

// queueTurn starts one queue turn from every queued item, when the session
// is idle. It reports false when it started none. A queue turn that loses
// the race for its turn ID tries once more.
func (cl *call) queueTurn(ctx context.Context) (dashi.RunResult, bool, error) {
	s := cl.s
	compacted := false
	for attempt := 0; ; {
		s.mu.Lock()
		p, ok, err := s.planQueue(ctx, compacted)
		s.mu.Unlock()
		if errors.Is(err, errReserved) {
			// The queue waits; each result warns WarningQueueBlocked.
			return dashi.RunResult{}, false, err
		}
		if err != nil {
			return dashi.RunResult{}, false, cl.fail(err)
		}
		if !ok {
			return dashi.RunResult{}, false, nil
		}
		if p.compact {
			lost, err := cl.compact(ctx, p)
			if err != nil {
				return dashi.RunResult{}, false, err
			}
			compacted = !lost
			continue
		}
		out, err := cl.exec(ctx, p.runner, dashi.NewTurn(p.id), p.command)
		out, err = s.finish(ctx, p.n, p.id, out, err, cl.takeWarnings(p.warnings), p.reserved, cl.started)
		if attempt == 0 && lostStart(err) {
			attempt++
			continue
		}
		cl.report(out, err)
		return out, true, err
	}
}

// exec runs one command on the engine, with the handle's steering source
// ahead of the caller's options, so an explicit WithSteering still wins,
// except on a compaction turn, which gets no steering.
// For Stream it forwards the events, tagged with the turn ID; the RunResult
// event waits for report, after the session adds its warnings.
func (cl *call) exec(
	ctx context.Context,
	r dashi.Runner,
	turn dashi.Turn,
	c dashi.Command,
) (dashi.RunResult, error) {
	cl.s.mu.Lock()
	source := cl.s.steering()
	cl.s.mu.Unlock()
	opts := append([]dashi.RunOption{dashi.WithSteering(source)}, cl.opts...)
	if isCompaction(turn, c) {
		opts = append(opts, dashi.WithSteering(nil))
	}
	cl.started = false
	if cl.yield == nil {
		out, err := r.Run(ctx, turn, c, opts...)
		cl.started = len(out.Turn.Steps) > 0 &&
			startedBy(&out.Turn.Steps[0], turn.ID, c)
		return out, err
	}
	// A consumer that stops ends the run's context, and the rest of the
	// run is drained unseen, so the session still learns the steps the
	// turn committed, up to its interruption, even when the recorder cannot
	// load them.
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	var out dashi.RunResult
	var runErr error
	have := false
	seen := turn
	seen.Steps = slices.Clone(turn.Steps)
	for ev, err := range r.Stream(runCtx, turn, c, opts...) {
		if ev.Result != nil {
			out, have, runErr = *ev.Result, true, err
			continue
		}
		if err != nil {
			runErr = err
			continue
		}
		if ev.TurnID == "" {
			ev.TurnID = turn.ID
		}
		if ev.Step != nil {
			seen.Steps = append(seen.Steps, *ev.Step)
			if startedBy(ev.Step, turn.ID, c) {
				cl.started = true
			}
		}
		if cl.stopped {
			continue
		}
		if !cl.yield(ev, nil) {
			cl.stopped = true
			stop(errStreamStopped)
		}
	}
	if !have {
		// The engine yielded no RunResult: the turn holds the committed
		// steps this call saw, if any.
		out = dashi.RunResult{Turn: seen}
		if runErr == nil && cl.stopped {
			runErr = errStreamStopped
		}
	}
	return out, runErr
}

// startedBy reports whether step is the turn_started step that Start c
// committed for turn turnID.
func startedBy(step *dashi.Step, turnID string, c dashi.Command) bool {
	start, ok := c.(dashi.Start)
	return ok && step.Kind == dashi.StepTurnStarted && step.TurnID == turnID &&
		step.CommandID == start.CommandID
}

// takeWarnings returns the call's pending compaction warnings, then
// warnings, and clears them.
func (cl *call) takeWarnings(warnings []dashi.Warning) []dashi.Warning {
	out := append(cl.warnings, warnings...)
	cl.warnings = nil
	return out
}

// report yields a turn's RunResult event, or the error alone when nothing
// was recorded.
func (cl *call) report(out dashi.RunResult, err error) {
	if cl.yield == nil || cl.stopped {
		return
	}
	var ev dashi.Event
	if err == nil || dashi.Recorded(err) {
		ev = dashi.Event{TurnID: out.Turn.ID, Result: &out}
	}
	if !cl.yield(ev, err) {
		cl.stopped = true
	}
}

// fail yields an error the session returns before a turn runs.
func (cl *call) fail(err error) error {
	if cl.yield != nil && !cl.stopped && !cl.yield(dashi.Event{}, err) {
		cl.stopped = true
	}
	return err
}

// forward yields buffered events. It reports false once the consumer
// stopped.
func (cl *call) forward(events []dashi.Event) bool {
	for _, ev := range events {
		if cl.stopped {
			break
		}
		if !cl.yield(ev, nil) {
			cl.stopped = true
		}
	}
	return !cl.stopped
}

// follow yields the steps of a running turn this call is a retry of, then
// each new step as it commits, and ends with the turn's RunResult event at
// its next stop, or at once when the loaded turn already rests at one.
// Without a recorder that can read steps, it returns busy.
func (cl *call) follow(
	ctx context.Context,
	turnID string,
	busy error,
) (dashi.RunResult, error) {
	s := cl.s
	s.mu.Lock()
	rec := s.recorder()
	s.mu.Unlock()
	reader, reads := rec.(dashi.StepReader)
	loader, loads := rec.(dashi.Loader)
	if !reads || !loads {
		return dashi.RunResult{}, cl.fail(busy)
	}
	turn, err := loader.Load(ctx, turnID)
	if err != nil {
		return dashi.RunResult{}, cl.fail(err)
	}
	var after int64
	for i := range turn.Steps {
		step := turn.Steps[i]
		after = step.Seq
		if !cl.forward([]dashi.Event{{TurnID: turnID, Seq: step.Seq, Step: &step}}) {
			return dashi.RunResult{}, context.Canceled
		}
	}
	// The turn stopped after the duplicate check. Follow would wait while
	// it rests at a stop that has not ended it, so report it now.
	if n := len(turn.Steps); n > 0 && turn.Steps[n-1].Kind == dashi.StepTurnStopped {
		return cl.reportTurn(turn)
	}
	for step, err := range dashi.Follow(ctx, reader, turnID, after) {
		if err != nil {
			return dashi.RunResult{}, cl.fail(err)
		}
		if !cl.forward([]dashi.Event{{TurnID: turnID, Seq: step.Seq, Step: &step}}) {
			return dashi.RunResult{}, context.Canceled
		}
		if step.Kind == dashi.StepTurnStopped {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return dashi.RunResult{}, err
	}
	turn, err = loader.Load(ctx, turnID)
	if err != nil {
		return dashi.RunResult{}, cl.fail(err)
	}
	return cl.reportTurn(turn)
}

// reportTurn yields the RunResult event of a followed turn.
func (cl *call) reportTurn(turn dashi.Turn) (dashi.RunResult, error) {
	view, err := turn.View()
	if err != nil {
		return dashi.RunResult{}, cl.fail(err)
	}
	out := dashi.RunResult{Turn: turn, TurnView: view}
	cl.report(out, nil)
	return out, nil
}

// collectRun runs a command with Runner.Stream and returns its events,
// including its RunResult event, for a caller that holds the handle lock.
func collectRun(
	ctx context.Context,
	r dashi.Runner,
	turn dashi.Turn,
	c dashi.Command,
	opts []dashi.RunOption,
) (dashi.RunResult, []dashi.Event, error) {
	out := dashi.RunResult{Turn: turn}
	var events []dashi.Event
	var runErr error
	for ev, err := range r.Stream(ctx, turn, c, opts...) {
		if ev.Result != nil {
			out = *ev.Result
		}
		if err != nil {
			runErr = err
			if ev.Result == nil {
				continue
			}
		}
		if ev.TurnID == "" {
			ev.TurnID = turn.ID
		}
		events = append(events, ev)
	}
	return out, events, runErr
}

// lastOutcome returns the current result of the session's last turn, or a
// zero RunResult when it has none.
func (s *Session) lastResult(ctx context.Context) (dashi.RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ID == "" {
		return dashi.RunResult{}, nil
	}
	last, err := s.findLast(ctx)
	if err != nil || last == 0 {
		return dashi.RunResult{}, err
	}
	t, err := s.load(ctx, last)
	if err != nil {
		return dashi.RunResult{}, err
	}
	return dashi.RunResult{Turn: t.turn.Clone(), TurnView: t.view}, nil
}
