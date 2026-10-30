package dashi

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
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

// WithHistoryMismatch sets how the Continue commands that Advance and
// Drive run treat a history that differs from the one the turn started
// with: the ones they derive, and a Continue from the inbox that leaves
// OnHistoryMismatch empty. The default is HistoryMismatchWarn; pass
// HistoryMismatchReject to get ErrHistoryMismatch instead of running the
// turn on the wrong history.
func WithHistoryMismatch(m HistoryMismatch) RunOption {
	return func(o *runOptions) {
		o.historyMismatch = m
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
// Recorder must also be a Loader. Pass WithOwner for lease fencing, and
// WithHistory and WithHistoryMismatch for the Continue's prior
// conversation; WithHooks, WithInbox, and WithPause apply to the runs it
// makes.
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
// A turn held by a host limit without Ends, such as a balance too low, is
// asked about on every call: Advance prepares the call the Continue would
// make and asks the BeforeModel and BeforeSend hooks, without sending,
// running tools, or recording anything. While they refuse with the same
// host limit code, Advance returns with nothing written; otherwise it runs
// the Continue. A host lifts a hold by fixing its cause, such as topping
// up the balance, and may signal Ready to skip Drive's poll. A hold the
// recorder made with a LimitError cannot be asked about: it is retried for
// real at most once per Advance call and per inbox signal to Drive.
//
// A command the turn cannot take, such as a result for an unknown call or
// a raise on a turn not stopped at a limit, never fails the turn: it is
// recorded as a WarningInbox, reported to an InboxRefuser, and
// acknowledged. Once the turn has ended, every waiting command except a
// late model result or a steer is refused with ErrTurnEnded, records
// nothing, and is acknowledged. A steer the turn did not record stays in
// the inbox for the next turn (see MemoryInbox.Leftovers). When Advance
// returns, an InboxReleaser gets back the steers no run recorded. A
// failing inbox is warned about once per call.
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
		var unwatch func()
		ready, unwatch = n.Ready(turnID)
		if unwatch != nil {
			defer unwatch()
		}
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
			a.recorderRetry = true
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
	// as of the last inbox read; the next run takes them. The inbox's
	// claim on those no run took is released when the call returns.
	steers []string
	// claimed lists, in order, every steer an inbox read of this call
	// returned, which the inbox may hold claimed for the turn. Those the
	// record does not hold are released when the call returns.
	claimed     []string
	claimedSeen map[string]bool
	// inboxContinued records that the last inbox read ran a Continue.
	inboxContinued bool
	// tried lists the steers a Continue already ran for, so a steer the
	// turn cannot take does not run it again.
	tried map[string]bool
	// inboxFailed records that a failing inbox was already warned about.
	inboxFailed bool
	// warnings ride on the next step this call records.
	warnings []Warning
	// recorderRetry allows one real retry of a hold the recorder made; it
	// is set again by each inbox signal Drive receives.
	recorderRetry bool
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
	if o.historyMismatch != "" && o.historyMismatch != HistoryMismatchWarn &&
		o.historyMismatch != HistoryMismatchReject {
		return nil, errors.New("dashi: invalid history-mismatch behavior")
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

		claimedSeen: map[string]bool{},

		recorderRetry: true,
	}, nil
}

// advance runs passes until the turn must wait or has ended, then releases
// the inbox's claim on the steers no run recorded, so Withdraw and other
// turns can take them again.
func (a *advancer) advance(ctx context.Context) (RunResult, error) {
	a.opts.inDoubt = map[string]bool{}
	out, err := a.passes(ctx)
	a.releaseSteers(ctx, out.Turn)
	return out, err
}

// releaseSteers releases the steers any inbox read of this call returned
// that turn does not record. Without a loaded turn it keeps the claims: a
// steer that might be recorded must not go to another turn. A steer a run left in doubt is
// released as in doubt again, never as unrecorded: turn may be the run's
// copy, which lacks a write whose result is unknown.
func (a *advancer) releaseSteers(ctx context.Context, turn Turn) {
	releaser, ok := a.inbox.(InboxReleaser)
	if !ok || len(a.claimed) == 0 || len(turn.Steps) == 0 {
		return
	}
	state, err := replay(turn)
	if err != nil {
		return
	}
	var unrecorded, inDoubt []string
	for _, id := range a.claimed {
		if _, recorded := state.contextIDs[id]; recorded {
			continue
		}
		if a.opts.inDoubt[id] {
			inDoubt = append(inDoubt, id)
		} else {
			unrecorded = append(unrecorded, id)
		}
	}
	a.claimed, a.claimedSeen = nil, map[string]bool{}
	if len(unrecorded) == 0 && len(inDoubt) == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inboxTimeout)
	defer cancel()
	releaser.Release(rctx, a.turnID, unrecorded, inDoubt)
}

// passes runs passes until the turn must wait or has ended.
func (a *advancer) passes(ctx context.Context) (RunResult, error) {
	// ownStop is the Seq of the last stop this call recorded. A hold it
	// recorded itself is not asked about again without new input.
	var ownStop int64
	// last is the turn as of the end of the previous pass, so an error
	// before the next pass loads still releases the steers it holds.
	var last Turn
	for {
		if err := ctx.Err(); err != nil {
			return unchangedResult(last), err
		}
		turn, err := a.load(ctx)
		if err != nil {
			return unchangedResult(last), err
		}
		turn, applied, err := a.applyInbox(ctx, turn)
		if err != nil {
			return unchangedResult(turn), err
		}
		view, err := turn.View()
		if err != nil {
			return unchangedResult(turn), err
		}
		// fresh is input no Continue has run for. An inbox Continue ran
		// for everything before it, and its stop is this call's own.
		fresh := applied
		if a.inboxContinued {
			fresh = false
			if last := lastStep(turn); last.Kind == StepTurnStopped {
				a.selfStop, ownStop = last.Seq, last.Seq
			}
		}
		if view.Stop.Ended {
			if applied {
				last = turn
				continue // refuse what arrived meanwhile
			}
			return unchangedResult(turn), nil
		}
		if d := view.Agent.Limits.Deadline; !d.IsZero() && !time.Now().Before(d) {
			return a.recordStop(ctx, turn, nil)
		}
		run := a.shouldContinue(view, turn, fresh)
		if !run && heldStop(view) && (view.LastSeq != ownStop || fresh) {
			// A hold lifts when BeforeSend allows the call again: ask.
			if run, err = a.askSend(ctx, turn, view); err != nil {
				return unchangedResult(turn), err
			}
		}
		if run {
			out, err := a.runContinue(ctx, turn, view)
			if err != nil || out.LastSeq <= view.LastSeq {
				// On no new step, as when an inbox command already took
				// the derived ID, return rather than spin.
				return out, err
			}
			if step := lastStep(out.Turn); step.Kind == StepTurnStopped {
				a.selfStop, ownStop = step.Seq, step.Seq
			}
			last = out.Turn
			continue
		}
		out := unchangedResult(turn)
		if lastStep(turn).Kind != StepTurnStopped || len(a.warnings) > 0 {
			out, err = a.recordStop(ctx, turn, restingStop(view))
			if err != nil {
				return out, err
			}
			a.selfStop, ownStop = out.LastSeq, out.LastSeq
		}
		if !applied {
			return out, nil
		}
		last = out.Turn
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
	next := view.Next()
	switch {
	case next == NextContinue:
	case next == NextRecover && view.ReadyToContinue:
	case next == NextRecover, next == NextFix:
		// A Continue also records the ending stop of a turn out of
		// resumes, after a crash or a recorded interruption.
		state, err := replay(turn)
		if err != nil || !state.resumeLimitReached() {
			return false
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
	return a.run(ctx, turn, a.derivedContinue(view), nil)
}

// run applies one command to the freshly loaded turn, with the call's
// options, its pending warnings, and the inbox command ID it records.
func (a *advancer) run(
	ctx context.Context,
	turn Turn,
	c Command,
	delivered []DeliveredCommand,
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
	a.inboxContinued = false
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
	// The inbox may claim every steer it returned, including those after
	// a command that ends this read early.
	_, releases := a.inbox.(InboxReleaser)
	for _, c := range commands {
		if !releases {
			break
		}
		if s, ok := c.(Steer); ok && s.Item.ID != "" && !a.claimedSeen[s.Item.ID] {
			a.claimedSeen[s.Item.ID] = true
			a.claimed = append(a.claimed, s.Item.ID)
		}
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
			if recorded {
				a.acknowledge(ctx, id, state)
			} else {
				// A steer the turn has not recorded stays in the inbox,
				// even once the turn has ended: it may be meant for the
				// next turn, which takes it from the inbox's leftovers.
				a.steers = append(a.steers, id)
			}
			continue
		}
		if v, ok := c.(Continue); ok {
			if len(v.History) == 0 {
				v.History = llmclone.Messages(a.opts.history)
			}
			if v.OnHistoryMismatch == "" {
				v.OnHistoryMismatch = a.opts.historyMismatch
			}
			c = v
		}
		out, err := a.run(ctx, turn, c, []DeliveredCommand{{ID: id, Kind: CommandKindOf(c)}})
		if err != nil && !isRefusal(err) {
			return out.Turn, applied, err
		}
		if err == nil && out.LastSeq <= int64(len(turn.Steps)) {
			// The turn already records this command, as when its
			// acknowledgement was lost: nothing ran now.
			a.acknowledge(ctx, id, state)
			continue
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
			for _, id := range a.steers {
				a.tried[id] = true
			}
			a.inboxContinued = true
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

// recordStop records stop with the call's pending warnings. A nil stop
// means the deadline passed: recorded output completes the turn, and
// otherwise the turn stops at the deadline limit.
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
	switch {
	case stop == nil && state.Output != nil:
		// Output recorded before the deadline completes the turn, as in
		// contextStop.
		out = rt.completeReply(state)
	case stop == nil:
		out = rt.stop(StopLimited, string(LimitDeadline))
	default:
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

// FollowNext yields the turn's view whenever what a host does next
// changes: the view now, a working view once work starts after a stop,
// and a view at each later stop. It ends after the stop that ends the
// turn. It reads like Follow, through r, and extends one in-memory turn,
// but builds a view only at those points, so a burst of steps yields at
// most one working view and a reader never acts on an intermediate state.
//
// A turn is stopped when its last step is a stop step, and working
// otherwise. A working view has no Stop, so its Next is NextRunning: it is
// for display ("running"), never for action. A watcher cannot tell a live
// worker from a crashed one, so FollowNext never yields NextRecover: when
// the view now is of a working turn, it is a working view. Two kinds of
// turn whose last step is not a stop are not working: a saved Start, whose
// view now reads NextContinue, and an ended turn with late model results,
// whose view now reads NextNone (the late results are not read). After a
// view that is not working, the first steps other than stops yield one
// working view, unless a stop follows them in the same read; so a stop
// comes between any two working views. A host that tracks workers reads
// Turn.View for NextRecover instead.
//
// A read error, including ErrCursorAhead, or a record that fails replay is
// yielded once and ends the sequence. On an unknown turn it waits for the
// first step.
func FollowNext(
	ctx context.Context,
	r StepReader,
	turnID string,
) iter.Seq2[TurnView, error] {
	return func(yield func(TurnView, error) bool) {
		turn := Turn{ID: turnID}
		first := true
		// quiet means the last view yielded was not a working view, and
		// worked that steps other than stops arrived after it.
		quiet, worked := false, false
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
				if step.Kind != StepTurnStopped {
					worked = true
					continue
				}
				if first {
					continue
				}
				view, ok := yieldView(turn, false, yield)
				if !ok || view.Stop.Ended {
					return
				}
				quiet, worked = true, false
			}
			if full {
				continue
			}
			if first && len(turn.Steps) > 0 {
				first = false
				view, ok := yieldView(turn, nowWorking(turn), yield)
				if !ok || view.Stop.Ended {
					return
				}
				quiet, worked = view.Next() != NextRunning, false
			}
			if quiet && worked {
				if _, ok := yieldView(turn, true, yield); !ok {
					return
				}
				quiet, worked = false, false
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

// nowWorking reports whether FollowNext's view now of turn is a working
// view: its last step is not a stop, and it is neither a saved Start nor
// an ended turn, the cases where View infers a stop that gives NextRecover.
func nowWorking(turn Turn) bool {
	view, err := turn.View()
	return err == nil && view.Next() == NextRecover
}

// yieldView builds turn's view and yields it, or yields the replay error.
// A working view has no Stop, so it reads as NextRunning. It reports the
// view and whether to go on.
func yieldView(
	turn Turn,
	working bool,
	yield func(TurnView, error) bool,
) (TurnView, bool) {
	view, err := turn.View()
	if err != nil {
		yield(TurnView{}, err)
		return TurnView{}, false
	}
	if working {
		view.Stop = Stop{}
	}
	return view, yield(view, nil)
}
