package dashi

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
)

// WithHistory supplies the prior conversation for the Continue commands
// that Advance and Drive run: the ones they derive, and a Continue from the
// inbox that carries no History. Pass the history the turn started with,
// such as the history pinned when the turn was admitted.
func WithHistory(history []llm.Message) RunOption {
	return func(o *runOptions) {
		o.history = llmclone.Messages(history)
	}
}

// driveBackstop bounds Drive's wait for the inbox's signal, so a missed
// signal delays a command by at most this long. A variable so tests can
// shorten it.
var driveBackstop = 15 * time.Second

// continuePrefix starts the command ID of each Continue that Advance
// derives. Inbox command IDs may not use it.
const continuePrefix = "continue:"

// errReservedID refuses an inbox command whose ID uses continuePrefix.
var errReservedID = errors.New(
	`dashi: command IDs starting with "continue:" are reserved for Advance`,
)

// Advance loads a saved turn, applies the commands waiting in its inbox,
// and runs it until it must wait. RunResult.Next says why it returned. The
// Recorder must also be a Loader. Pass WithOwner for lease fencing and
// WithHistory for the Continue's prior conversation; WithHooks, WithInbox,
// and WithPause apply to the runs it makes.
//
// Each pass applies the inbox's commands in order. Resolve, AddMessages,
// and a Continue that raises a limit apply as Run applies them; a Cancel
// ends the turn and the pass. A command the record already holds is only
// acknowledged. Steers wait for the next run of the turn, which picks them
// up from the inbox. When the turn's Next is NextContinue, or NextRecover
// with ReadyToContinue, Advance runs a Continue whose command ID,
// "continue:<LastSeq>", comes from the record, so a retry after a crash
// derives the same command and needs no outbox. Inbox command IDs that
// start with "continue:" are refused. Otherwise Advance records a stop, if
// the last step is not one, repeating the turn's current stop (usually
// StopWaiting), so a watcher of stops sees the exact pending work. It
// passes again while the inbox has something new, and never continues from
// a stop it recorded itself unless an input arrived after it, so a turn
// that cannot progress returns instead of spinning. A turn past its
// Limits.Deadline gets its deadline stop.
//
// A command the turn cannot take, such as a result for an unknown call or
// a raise on a turn not stopped at a limit, never fails the turn: it is
// recorded as a WarningInbox, reported to an InboxRefuser, and
// acknowledged. Once the turn has ended, every waiting command except a
// late model result is refused with ErrTurnEnded, records nothing, and is
// acknowledged. A failing inbox is warned about once per call.
//
// Advance leaves the turn at a stop step whenever it returns nil. The
// errors are those of Run, plus the Loader's; ErrStaleTurn means another
// worker holds the turn, and a context error means ctx ended (the run in
// flight records an interrupted stop).
func (r Runner) Advance(
	ctx context.Context,
	turnID string,
	opts ...RunOption,
) (RunResult, error) {
	a, err := r.newAdvancer(turnID, newRunOptions(opts))
	if err != nil {
		return RunResult{}, err
	}
	return a.advance(ctx)
}

// Drive is Advance in a loop: between passes it waits for the inbox's
// signal (InboxNotifier), a 15-second backstop poll, or the turn's
// Limits.Deadline. It returns the turn's result with a nil error once the
// turn has ended, and with ctx's error when ctx ends, which is how a host
// stops it when its lease on the turn is lost. Drive does not retry a
// failed model call: the turn rests at NextFix, with the failure's
// category and retry-after on its ModelFact, until the host puts a
// Continue in the inbox.
func (r Runner) Drive(
	ctx context.Context,
	turnID string,
	opts ...RunOption,
) (RunResult, error) {
	a, err := r.newAdvancer(turnID, newRunOptions(opts))
	if err != nil {
		return RunResult{}, err
	}
	var ready <-chan struct{}
	if n, ok := a.inbox.(InboxNotifier); ok {
		ready = n.Ready(turnID)
	}
	for {
		out, err := a.advance(ctx)
		if err != nil || out.Stop.Ended {
			return out, err
		}
		wait := driveBackstop
		if d := out.Agent.Limits.Deadline; !d.IsZero() {
			wait = max(min(wait, time.Until(d)), 0)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-ready:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// advancer holds the state of one Advance or Drive call.
type advancer struct {
	runner Runner
	opts   runOptions
	inbox  Inbox
	loader Loader
	turnID string
	// selfStop is the Seq of the last stop this call recorded, which it
	// does not continue from without new input.
	selfStop int64
	// handled lists the inbox commands this call already applied, refused,
	// or acknowledged, so an inbox that fails to acknowledge cannot loop it.
	handled map[string]bool
	// steers lists the IDs of the waiting steers the record does not hold,
	// as of the last inbox read; the next run takes them.
	steers []string
	// tried lists the steers a Continue already ran for, so a steer the
	// turn cannot take does not run it again.
	tried map[string]bool
	// inboxFailed records that a failing inbox was already warned about.
	inboxFailed bool
	// warnings ride on the next step this call records.
	warnings []Warning
}

func (r Runner) newAdvancer(turnID string, o runOptions) (*advancer, error) {
	if turnID == "" {
		return nil, errors.New("dashi: turn ID is required")
	}
	loader, ok := r.Recorder.(Loader)
	if !ok {
		return nil, errors.New("dashi: Advance needs a Recorder that is also a Loader")
	}
	if r.OnContextDone != "" && r.OnContextDone != ContextInterrupt &&
		r.OnContextDone != ContextCancel {
		return nil, errors.New("dashi: invalid context-done behavior")
	}
	if err := JoinHooks(r.Hooks, o.hooks).validate(); err != nil {
		return nil, err
	}
	if !o.inboxSet {
		o.inbox, o.inboxSet = r.Inbox, true
	}
	o.record = false
	o.fresh = true
	o.stream = r.Observer != nil
	return &advancer{
		runner:  r,
		opts:    o,
		inbox:   o.inbox,
		loader:  loader,
		turnID:  turnID,
		handled: map[string]bool{},
		tried:   map[string]bool{},
	}, nil
}

// advance runs passes until the turn must wait or has ended.
func (a *advancer) advance(ctx context.Context) (RunResult, error) {
	for {
		if err := ctx.Err(); err != nil {
			return RunResult{}, err
		}
		turn, err := a.load(ctx)
		if err != nil {
			return RunResult{}, err
		}
		turn, applied, err := a.applyInbox(ctx, turn)
		if err != nil {
			return unchangedResult(turn), err
		}
		view, err := turn.View()
		if err != nil {
			return unchangedResult(turn), err
		}
		if view.Stop.Ended {
			if applied {
				continue // refuse what arrived meanwhile
			}
			return unchangedResult(turn), nil
		}
		if d := view.Agent.Limits.Deadline; !d.IsZero() && !time.Now().Before(d) {
			return a.recordStop(ctx, turn, nil)
		}
		if a.shouldContinue(view, turn, applied) {
			out, err := a.runContinue(ctx, turn, view)
			if err != nil || out.LastSeq <= view.LastSeq {
				// On no new step, as when an inbox command already took
				// the derived ID, return rather than spin.
				return out, err
			}
			if last := lastStep(out.Turn); last.Kind == StepTurnStopped {
				a.selfStop = last.Seq
			}
			continue
		}
		out := unchangedResult(turn)
		if lastStep(turn).Kind != StepTurnStopped || len(a.warnings) > 0 {
			out, err = a.recordStop(ctx, turn, restingStop(view))
			if err != nil {
				return out, err
			}
			a.selfStop = out.LastSeq
		}
		if !applied {
			return out, nil
		}
	}
}

func (a *advancer) load(ctx context.Context) (Turn, error) {
	turn, err := a.loader.Load(ctx, a.turnID)
	if err != nil {
		return Turn{}, err
	}
	if len(turn.Steps) == 0 {
		return Turn{}, fmt.Errorf("%w: %s", ErrTurnNotFound, a.turnID)
	}
	turn.ID = a.turnID
	return turn, nil
}

// shouldContinue reports whether Advance runs a Continue now: the turn is
// ready, and it is not resting at a stop this call recorded with no input
// since. A steer no Continue of this call ran for counts as input, once.
func (a *advancer) shouldContinue(view TurnView, turn Turn, applied bool) bool {
	switch view.Next() {
	case NextContinue:
	case NextRecover:
		// A Continue also records the stop of a turn out of resumes.
		if !view.ReadyToContinue {
			state, err := replay(turn)
			if err != nil || !state.resumeLimitReached() {
				return false
			}
		}
	default:
		return false
	}
	if view.LastSeq != a.selfStop || applied {
		return true
	}
	fresh := false
	for _, id := range a.steers {
		if !a.tried[id] {
			a.tried[id] = true
			fresh = true
		}
	}
	return fresh
}

// runContinue runs the Continue derived from the record. It takes the
// waiting steers.
func (a *advancer) runContinue(
	ctx context.Context,
	turn Turn,
	view TurnView,
) (RunResult, error) {
	for _, id := range a.steers {
		a.tried[id] = true
	}
	c := Continue{
		CommandID: continuePrefix + strconv.FormatInt(view.LastSeq, 10),
		History:   llmclone.Messages(a.opts.history),
	}
	return a.run(ctx, turn, c, nil)
}

// run applies one command to the freshly loaded turn, with the call's
// options, its pending warnings, and the inbox command ID it records.
func (a *advancer) run(
	ctx context.Context,
	turn Turn,
	c Command,
	delivered []string,
) (RunResult, error) {
	o := a.opts
	o.delivered = delivered
	o.warnings = a.warnings
	out, err := a.runner.run(ctx, turn, c, o)
	if out.LastSeq > int64(len(turn.Steps)) {
		// The warnings rode on the first step the command recorded.
		a.warnings = nil
	}
	return out, err
}

// applyInbox applies the inbox's waiting commands to turn, in order, and
// reports whether it handled any it had not handled before. A Cancel ends
// the turn; the commands after it are refused. An error the turn did not
// cause, such as a failed write or another worker's lease, stops it.
func (a *advancer) applyInbox(ctx context.Context, turn Turn) (Turn, bool, error) {
	a.steers = nil
	if a.inbox == nil {
		return turn, false, nil
	}
	commands, err := a.inbox.Waiting(ctx, a.turnID)
	if err != nil {
		if ctx.Err() != nil {
			return turn, false, ctx.Err()
		}
		if !a.inboxFailed {
			a.inboxFailed = true
			a.warn(fmt.Sprintf("inbox failed: %v", err))
		}
		return turn, false, nil
	}
	applied := false
	for _, c := range commands {
		id := CommandIDOf(c)
		if id == "" {
			// It cannot be recorded or acknowledged by ID.
			if !a.handled[""] {
				a.handled[""] = true
				a.warn(fmt.Sprintf("inbox command of type %T skipped: it has no command ID", c))
			}
			continue
		}
		if a.handled[id] {
			continue
		}
		state, err := replay(turn)
		if err != nil {
			return turn, applied, err
		}
		if strings.HasPrefix(id, continuePrefix) {
			a.refuse(ctx, id, state, errReservedID)
			applied = true
			continue
		}
		if _, ok := c.(Steer); ok {
			_, recorded := state.contextIDs[id]
			switch {
			case recorded:
				a.acknowledge(ctx, id, state)
			case state.Stop.Ended:
				a.refuse(ctx, id, state, state.acceptsAfterEnd(c))
				applied = true
			default:
				a.steers = append(a.steers, id)
			}
			continue
		}
		if v, ok := c.(Continue); ok && len(v.History) == 0 {
			v.History = llmclone.Messages(a.opts.history)
			c = v
		}
		out, err := a.run(ctx, turn, c, []string{id})
		if err != nil && !isRefusal(err) {
			return out.Turn, applied, err
		}
		turn = out.Turn
		applied = true
		if err != nil {
			a.refuse(ctx, id, state, err)
			continue
		}
		a.acknowledge(ctx, id, state)
		if _, ran := c.(Continue); ran {
			// The run took steers and may have seen newer commands:
			// fetch again.
			return turn, true, nil
		}
	}
	return turn, applied, nil
}

// refuse reports a command the turn did not take and acknowledges it. On
// an open turn the refusal is also recorded as a WarningInbox.
func (a *advancer) refuse(ctx context.Context, id string, state *replayState, reason error) {
	if !state.Stop.Ended {
		a.warn(fmt.Sprintf("inbox command %s not applied: %v", id, reason))
	}
	if refuser, ok := a.inbox.(InboxRefuser); ok {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inboxTimeout)
		refuser.Refused(rctx, a.turnID, id, reason)
		cancel()
	}
	a.acknowledge(ctx, id, state)
}

// acknowledge tells the inbox a command is handled. A failure is warned
// about on an open turn; the command is not applied twice either way.
func (a *advancer) acknowledge(ctx context.Context, id string, state *replayState) {
	a.handled[id] = true
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inboxTimeout)
	defer cancel()
	if err := a.inbox.Delivered(actx, a.turnID, []string{id}); err != nil &&
		!state.Stop.Ended {
		a.warn(fmt.Sprintf("inbox acknowledgment failed: %v", err))
	}
}

func (a *advancer) warn(message string) {
	a.warnings = append(a.warnings, Warning{Code: WarningInbox, Message: message})
}

// recordStop records stop, or the deadline stop when stop is nil, with
// the call's pending warnings.
func (a *advancer) recordStop(ctx context.Context, turn Turn, stop *Stop) (RunResult, error) {
	state, err := replay(turn)
	if err != nil {
		return unchangedResult(turn), err
	}
	r := a.runner
	rt := runtime{
		ctx:             ctx,
		commitCtx:       ctx,
		turn:            cloneTurn(turn),
		agent:           state.Agent,
		runner:          r,
		opts:            a.opts,
		emitter:         newEmitter(r.Observer),
		hooks:           JoinHooks(r.Hooks, a.opts.hooks),
		activeHooks:     r.Hooks.active(),
		lastHooks:       state.activeHooks,
		pendingWarnings: slices.Clone(a.warnings),
	}
	defer rt.emitter.close()
	var out RunResult
	if stop == nil {
		out = rt.stop(StopLimited, string(LimitDeadline))
	} else {
		out = rt.stopWith(stop)
	}
	if rt.err == nil {
		a.warnings = nil
	}
	return out, rt.err
}

// restingStop is the stop Advance records for a turn that cannot go on:
// the turn's current stop again, or the inferred one, so Next is
// unchanged and the stop is recorded.
func restingStop(view TurnView) *Stop {
	stop := Stop{
		Kind:   view.Stop.Kind,
		Reason: view.Stop.Reason,
		RefID:  view.Stop.RefID,
	}
	if view.Stop.Limit != nil {
		limit := *view.Stop.Limit
		stop.Limit = &limit
	}
	if stop.Kind == "" {
		stop.Kind = StopInterrupted
		stop.Reason = "worker stopped before result"
	}
	return &stop
}

// lastStep returns the turn's last step, or nil for a turn with no steps.
func lastStep(t Turn) *Step {
	if len(t.Steps) == 0 {
		return nil
	}
	return &t.Steps[len(t.Steps)-1]
}

// FollowStops yields the turn's view now, then at each later stop, and
// ends after the stop that ends the turn. It reads like Follow, through r,
// and extends one in-memory turn, but builds a view only at stops, so a
// burst of steps yields one view and a reader never acts on an
// intermediate state. A turn is stopped when its last step is a stop step
// and working otherwise; a working turn's first view is for display, never
// for action. Late model results after the ending stop are not read.
//
// A read error, including ErrCursorAhead, or a record that fails replay is
// yielded once and ends the sequence. On an unknown turn it waits for the
// first step.
func FollowStops(
	ctx context.Context,
	r StepReader,
	turnID string,
) iter.Seq2[TurnView, error] {
	return func(yield func(TurnView, error) bool) {
		turn := Turn{ID: turnID}
		first := true
		resting, woke := false, false
		for ctx.Err() == nil {
			cursor := int64(len(turn.Steps))
			steps, err := r.StepsAfter(ctx, turnID, cursor, followPage)
			if err != nil {
				if ctx.Err() == nil {
					yield(TurnView{}, err)
				}
				return
			}
			full := len(steps) >= followPage
			for _, step := range steps {
				if step.TurnID != turnID || step.Seq != int64(len(turn.Steps))+1 {
					yield(TurnView{}, discontinuous(turnID, int64(len(turn.Steps)), step))
					return
				}
				turn.Steps = append(turn.Steps, step)
				if first || step.Kind != StepTurnStopped {
					continue
				}
				if !yieldView(turn, yield) || step.Stop != nil && step.Stop.Ended {
					return
				}
			}
			if full {
				continue
			}
			if first && len(turn.Steps) > 0 {
				first = false
				if !yieldView(turn, yield) {
					return
				}
				if last := lastStep(turn); last.Stop != nil && last.Stop.Ended {
					return
				}
			}
			if last := lastStep(turn); last != nil {
				resting = last.Kind == StepTurnStopped
			}
			if woke && len(steps) == 0 {
				sleep(ctx, followFloor)
			}
			woke = followWait(ctx, r, turnID, int64(len(turn.Steps)), resting)
		}
	}
}

// yieldView builds turn's view and yields it, or yields the replay error.
// It reports whether to go on.
func yieldView(turn Turn, yield func(TurnView, error) bool) bool {
	view, err := turn.View()
	if err != nil {
		yield(TurnView{}, err)
		return false
	}
	return yield(view, nil)
}
