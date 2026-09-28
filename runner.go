package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Runner runs turns for one agent with the long-lived services it needs. It
// holds configuration only, so one value can serve many turns; per-call
// controls are RunOption values, and each turn's state lives in its Turn.
type Runner struct {
	// Agent may be the zero value: see Agent for the defaults. On an existing
	// turn, a zero Agent runs with the turn's recorded agent.
	Agent  Agent
	Client llm.Client
	Tools  tool.Toolset
	// Recorder acknowledges steps. When nil, Run uses an in-memory recorder
	// for that call; pass the returned Turn to a later Continue.
	Recorder Recorder
	// Policy decides model calls and tool calls. When nil, every bound local
	// and external tool is allowed; supply a policy to deny or defer calls.
	Policy Policy
	// Observer receives every committed step, plus provisional deltas and
	// tool progress, which it may drop (see Observer). When it is set, Run requests streamed model output and
	// delivers tool.Report snapshots so the Observer sees work as it happens;
	// Stream always does.
	Observer Observer
	// OnContextDone controls whether an ended host context pauses or cancels.
	OnContextDone ContextDone
}

// RunOption adjusts one Run or Stream call.
type RunOption func(*runOptions)

// runOptions holds per-call controls. The stream fields are set only by Run
// and Stream, so a streamed model request always has a consumer for its
// deltas: an Observer, a Stream range, or both.
type runOptions struct {
	owner      string
	pause      <-chan struct{}
	stream     bool
	streamSink func(Event)
}

// WithOwner forwards owner to recorder commits for host lease fencing.
func WithOwner(owner string) RunOption {
	return func(o *runOptions) {
		o.owner = owner
	}
}

// WithPause requests a stop before the next model or tool call once pause is
// closed. Closing it does not interrupt work already in flight.
func WithPause(pause <-chan struct{}) RunOption {
	return func(o *runOptions) {
		o.pause = pause
	}
}

// ContextDone selects the meaning of an ended Go context for one run.
type ContextDone string

const (
	// ContextInterrupt leaves the turn resumable and is the default.
	ContextInterrupt ContextDone = "interrupt"
	// ContextCancel ends the turn as an explicit Cancel command would.
	ContextCancel ContextDone = "cancel"
)

// HistoryMismatch selects how Continue treats prior history that differs
// from the history fingerprinted at Start.
type HistoryMismatch string

const (
	// HistoryMismatchWarn records WarningHistoryMismatch on the continued step
	// and runs with the supplied history. It is the default.
	HistoryMismatchWarn HistoryMismatch = "warn"
	// HistoryMismatchReject rejects the Continue with ErrHistoryMismatch before
	// any step is committed, so the turn stays resumable once the host supplies
	// the history it started with.
	HistoryMismatchReject HistoryMismatch = "reject"
)

// Stream advances a turn when iteration begins, requesting streamed model
// output and tool progress. Events carry committed steps, model deltas, and
// tool.Report snapshots. A fully read sequence ends with one Outcome event. A
// rejected command yields only an error; a write the recorder did not
// acknowledge yields the Outcome event and its *CommitError together.
// Breaking the range ends the context. By default, the turn records an
// interruption; ContextCancel on the Runner makes the break a final cancel.
// Reload the turn by ID to inspect committed facts, since no final event
// follows a break. A second range yields ErrStreamConsumed instead of
// repeating work.
func (r Runner) Stream(
	ctx context.Context,
	turn Turn,
	command Command,
	opts ...RunOption,
) iter.Seq2[Event, error] {
	var started atomic.Bool
	return func(yield func(Event, error) bool) {
		if !started.CompareAndSwap(false, true) {
			yield(Event{}, ErrStreamConsumed)
			return
		}
		streamCtx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		stopped := false
		o := newRunOptions(opts)
		o.stream = true
		o.streamSink = func(event Event) {
			if stopped {
				return
			}
			if !yield(event, nil) {
				stopped = true
				cancel(errors.New("dashi: stream consumer stopped"))
			}
		}
		out, err := r.run(streamCtx, turn, command, o)
		if stopped {
			return
		}
		if err != nil && !recorded(err) {
			yield(Event{}, err)
			return
		}
		yield(Event{TurnID: out.Turn.ID, Outcome: &out}, err)
	}
}

// Run advances one turn from an explicit command. The Outcome is always the
// latest known state, and the error says whether the command applied:
//
//   - nil: every step was recorded. Execution failures, limits, and waits
//     are results, reported in Outcome.Stop with any partial work.
//   - ErrRevisionConflict or ErrCommandConflict (check with errors.Is, which
//     also matches them inside a *CommitError): another worker owns or
//     advanced the turn, or the command ID was reused. Reload the turn.
//   - *CommitError: the run executed, but CommitError.Step was not saved,
//     or may have been. The Outcome holds the steps this run knows were
//     saved; see CommitError.
//   - ErrRecordInvalid (which also matches ErrInvalidTurn): steps this run
//     recorded fail replay, a bug in the runner or recorder. Something was
//     recorded; the Outcome holds the steps without a view.
//   - any other error: the command was rejected and nothing was recorded.
//     The Outcome is the caller's turn and its view, unchanged. An invalid
//     input turn gives ErrInvalidTurn alone.
//
// Run requests streamed model output only when the Runner has an Observer,
// which is then the only consumer of deltas.
func (r Runner) Run(
	ctx context.Context,
	turn Turn,
	command Command,
	opts ...RunOption,
) (Outcome, error) {
	o := newRunOptions(opts)
	o.stream = r.Observer != nil
	return r.run(ctx, turn, command, o)
}

// run applies a command. A command rejected before any commit returns the
// caller's turn and its view, so a loop that reassigns its Outcome keeps
// the turn.
func (r Runner) run(
	ctx context.Context,
	turn Turn,
	command Command,
	opts runOptions,
) (Outcome, error) {
	out, err := r.apply(ctx, turn, command, opts)
	if err != nil && !recorded(err) {
		out = restingOutcome(turn)
	}
	return out, err
}

// recorded reports whether err came after the run began recording.
func recorded(err error) bool {
	var ce *CommitError
	return errors.As(err, &ce) && ce.Step != nil ||
		errors.Is(err, ErrRecordInvalid)
}

func (r Runner) apply(
	ctx context.Context,
	turn Turn,
	command Command,
	opts runOptions,
) (Outcome, error) {
	if command == nil {
		return Outcome{}, errors.New("dashi: command is required")
	}
	if r.OnContextDone != "" && r.OnContextDone != ContextInterrupt &&
		r.OnContextDone != ContextCancel {
		return Outcome{}, errors.New("dashi: invalid context-done behavior")
	}
	if r.Client == nil {
		return Outcome{}, errors.New("dashi: client is required")
	}
	// Empty IDs are generated before anything reads them: the turn ID before
	// replay, recorder setup, and loading, and the command ID before its
	// digest, which covers the ID.
	if turn.ID == "" {
		if _, ok := command.(Start); !ok {
			return Outcome{}, errors.New("dashi: turn ID is required for commands other than Start")
		}
		turn.ID = NewID()
	}
	command = withCommandID(command)
	if r.Policy == nil {
		r.Policy = defaultPolicy{}
	}
	state, err := replay(turn)
	if err != nil {
		return Outcome{}, err
	}
	// An agent may leave its destination, model, and revision to defaults. The
	// filled values are pinned like explicit ones, so replay and Continue
	// compare the same resolved agent.
	agent, err := r.agentFor(state, len(turn.Steps) > 0)
	if err != nil {
		return Outcome{}, err
	}
	if r.Recorder == nil {
		r.Recorder = newMemoryRecorderForTurn(turn)
	}
	if len(turn.Steps) > 0 {
		if err := agentChanged(agent, state.Agent); err != nil {
			return Outcome{}, err
		}
	}
	d, err := commandDigest(command)
	if err != nil {
		return Outcome{}, err
	}
	if old, ok := state.commandDigests[command.commandID()]; ok {
		if old != d {
			return Outcome{}, ErrCommandConflict
		}
		if loader, ok := r.Recorder.(Loader); ok {
			saved, loadErr := loader.Load(ctx, turn.ID)
			if loadErr != nil {
				return Outcome{}, loadErr
			}
			current, replayErr := replay(saved)
			if replayErr != nil {
				return Outcome{}, replayErr
			}
			if err := agentChanged(agent, current.Agent); err != nil {
				return Outcome{}, err
			}
			if current.commandDigests[command.commandID()] != d {
				return Outcome{}, ErrRevisionConflict
			}
			return outcome(saved, current), nil
		}
		return outcome(turn, state), nil
	}
	// A caller can retry with an old snapshot after an acknowledgement was lost.
	// Only a turn known not to exist falls back to the caller's snapshot;
	// acting on a snapshot the recorder could not confirm might repeat work.
	if loader, ok := r.Recorder.(Loader); ok {
		saved, loadErr := loader.Load(ctx, turn.ID)
		if loadErr != nil && !errors.Is(loadErr, ErrTurnNotFound) {
			return Outcome{}, loadErr
		}
		if loadErr == nil {
			current, replayErr := replay(saved)
			if replayErr != nil {
				return Outcome{}, replayErr
			}
			if old, found := current.commandDigests[command.commandID()]; found {
				if err := agentChanged(agent, current.Agent); err != nil {
					return Outcome{}, err
				}
				if old != d {
					return Outcome{}, ErrCommandConflict
				}
				return outcome(saved, current), nil
			}
			// The runner trusts the snapshot's recorded agent, and a zero
			// Agent runs with it, so it must be the recorder's.
			if len(turn.Steps) > 0 && len(saved.Steps) > 0 &&
				!sameAgent(state.Agent, current.Agent) {
				return Outcome{}, fmt.Errorf(
					"%w: the turn's recorded agent differs from the recorder's; load the turn from the recorder",
					ErrAgentChanged,
				)
			}
		}
	}
	// Checked after the retry checks, so a retried command on an ended turn
	// still gets its recorded answer.
	if err := state.acceptsAfterEnd(command); err != nil {
		return Outcome{}, err
	}
	runCtx := ctx
	if !agent.Limits.Deadline.IsZero() {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithDeadlineCause(
			ctx,
			agent.Limits.Deadline,
			errTurnDeadline,
		)
		defer cancel()
	}
	rt := runtime{
		ctx:     runCtx,
		turn:    cloneTurn(turn),
		agent:   agent,
		runner:  r,
		opts:    opts,
		emitter: newEmitter(r.Observer),
	}
	defer rt.emitter.close()
	switch c := command.(type) {
	case Start:
		if len(turn.Steps) != 0 || len(c.Input) == 0 {
			return Outcome{}, errors.New("dashi: start requires a new turn and input")
		}
		if c.Budget != nil {
			if err := c.Budget.Validate(); err != nil {
				return Outcome{}, err
			}
		}
		if err := validateHistory(c.History); err != nil {
			return Outcome{}, err
		}
		fingerprint, err := historyFingerprint(c.History)
		if err != nil {
			return Outcome{}, err
		}
		rt.history = cloneMessages(c.History)
		if len(c.OutputSchema) > 0 {
			if _, err := compileOutputSchema(c.OutputSchema); err != nil {
				return Outcome{}, err
			}
			if err := validateOutputToolChoice(agent.Settings); err != nil {
				return Outcome{}, err
			}
		}
		origin := c.Origin
		if origin == "" {
			origin = StartUser
		}
		if o := rt.commit(Step{
			Kind:          StepStarted,
			CommandID:     c.CommandID,
			CommandDigest: d,
			Started: &StartFact{
				Agent:              agent,
				Budget:             cloneBudget(c.Budget),
				OutputSchema:       slices.Clone(c.OutputSchema),
				Origin:             origin,
				HistoryFingerprint: fingerprint,
				Input:              cloneMessages(c.Input),
			},
		}); o != nil {
			return rt.result(*o)
		}
	case Deliver:
		if len(turn.Steps) == 0 ||
			state.Output != nil ||
			len(c.Items) == 0 {
			return Outcome{}, errors.New("dashi: turn cannot receive context")
		}
		if o := rt.commit(Step{
			Kind:          StepDelivered,
			CommandID:     c.CommandID,
			CommandDigest: d,
			Context:       &ContextFact{Items: c.Items},
		}); o != nil {
			return rt.result(*o)
		}
		return rt.result(rt.current())
	case Continue:
		if len(turn.Steps) == 0 {
			return Outcome{}, fmt.Errorf("%w: turn has not started", ErrNotReady)
		}
		if c.OnHistoryMismatch != "" &&
			c.OnHistoryMismatch != HistoryMismatchWarn &&
			c.OnHistoryMismatch != HistoryMismatchReject {
			return Outcome{}, errors.New("dashi: invalid history-mismatch behavior")
		}
		if c.Budget != nil {
			old := Budget{}
			if state.Budget != nil {
				old = *state.Budget
			}
			if err := c.Budget.raisedFrom(old); err != nil {
				return Outcome{}, err
			}
		}
		// History is checked before any path that can commit, so a rejected
		// Continue never changes the record.
		if err := validateHistory(c.History); err != nil {
			return Outcome{}, err
		}
		fingerprint, err := historyFingerprint(c.History)
		if err != nil {
			return Outcome{}, err
		}
		stored := turn.Steps[0].Started.HistoryFingerprint
		mismatch := stored != fingerprint
		if mismatch && c.OnHistoryMismatch == HistoryMismatchReject {
			return Outcome{}, ErrHistoryMismatch
		}
		if state.interruptionLimitReached() {
			step := Step{
				Kind:          StepStopped,
				CommandID:     c.CommandID,
				CommandDigest: d,
				Stop: &Stop{
					Kind:   StopLimited,
					Reason: "interruptions",
					Limit: &LimitDetail{
						Kind:  LimitInterruptions,
						Limit: int64(max(agent.Limits.Interruptions, 0)),
						Used:  int64(state.Interruptions),
					},
				},
			}
			if o := rt.commit(step); o != nil {
				return rt.result(*o)
			}
			return rt.result(rt.current())
		}
		if !state.canContinueWithBudget(c.Budget) {
			return Outcome{}, notReady(state)
		}
		rt.history = cloneMessages(c.History)
		step := Step{
			Kind:          StepContinued,
			CommandID:     c.CommandID,
			CommandDigest: d,
			Budget:        cloneBudget(c.Budget),
		}
		if mismatch {
			step.Warnings = []Warning{
				{
					Code:    WarningHistoryMismatch,
					Message: "prior history differs from turn start",
				},
			}
		}
		if o := rt.commit(step); o != nil {
			return rt.result(*o)
		}
	case Settle:
		if len(turn.Steps) == 0 {
			return Outcome{}, errors.New("dashi: cannot settle before start")
		}
		n := 0
		if c.Outcome != nil {
			n++
		}
		if c.Approval != nil {
			n++
		}
		if c.HostedApproval != nil {
			n++
		}
		if c.ModelResult != nil {
			if err := validateModelResult(*c.ModelResult, agent); err != nil {
				return Outcome{}, err
			}
			n++
		}
		if c.ModelFailure != nil {
			if (c.ModelFailure.Disposition != llm.FailureNotDispatched &&
				c.ModelFailure.Disposition != llm.FailureRejected) ||
				c.ModelFailure.Reason == "" ||
				c.ModelFailure.Evidence == "" {
				return Outcome{}, errors.New("dashi: model failure requires verified nonexecution evidence")
			}
			n++
		}
		if n != 1 {
			return Outcome{}, errors.New("dashi: settle requires one fact")
		}
		if c.ModelResult != nil || c.ModelFailure != nil {
			if c.CallID == "" ||
				(c.CallID != state.modelIntent &&
					c.CallID != state.unresolvedModel) {
				return Outcome{}, errors.New("dashi: model attempt is not pending")
			}
			kind := StepModelSettled
			if c.CallID == state.unresolvedModel {
				kind = StepModelReconciled
			}
			fact := &ModelFact{
				AttemptID: c.CallID,
				MessageID: c.CallID + ":message",
				Result:    c.ModelResult,
			}
			if c.ModelFailure != nil {
				fact.Error = c.ModelFailure.Reason
				fact.Failure = c.ModelFailure.Disposition
				fact.Evidence = c.ModelFailure.Evidence
			}
			setSettlementCharge(state.modelIntents[c.CallID], fact)
			if o := rt.commit(Step{
				Kind:          kind,
				CommandID:     c.CommandID,
				CommandDigest: d,
				Model:         fact,
			}); o != nil {
				return rt.result(*o)
			}
		} else if c.HostedApproval != nil {
			if c.CallID != "" && c.CallID != c.HostedApproval.RequestID {
				return Outcome{}, errors.New("dashi: hosted approval ID mismatch")
			}
			a, ok := state.hosted[c.HostedApproval.RequestID]
			if !ok || a.Origin != c.HostedApproval.Origin {
				return Outcome{}, errors.New("dashi: hosted approval is not pending")
			}
			if o := rt.commit(Step{
				Kind:           StepHostedApprovalSettled,
				CommandID:      c.CommandID,
				CommandDigest:  d,
				HostedApproval: c.HostedApproval,
			}); o != nil {
				return rt.result(*o)
			}
		} else if c.Approval != nil {
			v, ok := state.Tools[c.Approval.CallID]
			if !ok || v.Status != ToolApprovalPending || c.CallID != c.Approval.CallID {
				return Outcome{}, errors.New("dashi: tool approval is not pending")
			}
			if c.Approval.ArgsDigest != ArgsDigest(v.Call.Args) ||
				v.Decision.Revision != c.Approval.PolicyRevision {
				return Outcome{}, errors.New("dashi: stale tool approval")
			}
			if o := rt.commit(Step{
				Kind:          StepApprovalSettled,
				CommandID:     c.CommandID,
				CommandDigest: d,
				Approval:      c.Approval,
			}); o != nil {
				return rt.result(*o)
			}
		} else {
			v, ok := state.Tools[c.CallID]
			if !ok ||
				(v.Status != ToolStarted &&
					v.Status != ToolWaiting &&
					v.Status != ToolUncertain) {
				return Outcome{}, errors.New("dashi: tool result is not pending")
			}
			if v.Call.Name == SubmitOutputTool {
				return Outcome{}, errors.New("dashi: structured output is settled by the runner")
			}
			if e := c.Outcome.Validate(); e != nil {
				return Outcome{}, e
			}
			if c.Outcome.State == tool.StateWaiting ||
				c.Outcome.State == tool.StateDetached {
				return Outcome{}, errors.New("dashi: settle requires a final tool outcome")
			}
			if o := rt.commit(Step{
				Kind:          StepToolSettled,
				CommandID:     c.CommandID,
				CommandDigest: d,
				Tool:          &ToolFact{Call: v.Call, Outcome: c.Outcome},
			}); o != nil {
				return rt.result(*o)
			}
		}
		return rt.result(rt.current())
	case Cancel:
		if len(turn.Steps) == 0 || state.Output != nil {
			return Outcome{}, errors.New("dashi: turn cannot cancel")
		}
		if o := rt.commit(Step{
			Kind:          StepStopped,
			CommandID:     c.CommandID,
			CommandDigest: d,
			Stop:          &Stop{Kind: StopCanceled, Reason: c.Reason},
		}); o != nil {
			return rt.result(*o)
		}
		return rt.result(rt.current())
	default:
		return Outcome{}, errors.New("dashi: unknown command")
	}
	return rt.result(rt.run())
}

type runtime struct {
	ctx     context.Context
	turn    Turn
	history []llm.Message
	agent   Agent
	runner  Runner
	opts    runOptions
	emitter *emitter
	// err is the recording failure that ended the run, if any.
	err error
}

// result pairs an Outcome with the recording failure that ended the run.
func (r *runtime) result(out Outcome) (Outcome, error) {
	return out, r.err
}

func validateHistory(history []llm.Message) error {
	for i, message := range history {
		if err := validateMessage(message); err != nil {
			return fmt.Errorf("dashi: history message %d: %w", i+1, err)
		}
	}
	return nil
}

var errTurnDeadline = errors.New("dashi: turn deadline reached")

func (r *runtime) contextStop() Outcome {
	cause := context.Cause(r.ctx)
	if errors.Is(cause, errTurnDeadline) {
		return r.stop(StopLimited, "deadline")
	}
	if cause == nil {
		cause = r.ctx.Err()
	}
	if cause == nil {
		cause = context.Canceled
	}
	if r.runner.OnContextDone == ContextCancel {
		return r.stop(StopCanceled, cause.Error())
	}
	return r.stop(StopInterrupted, cause.Error())
}

func (r *runtime) pauseRequested() bool {
	if r.opts.pause == nil {
		return false
	}
	select {
	case <-r.opts.pause:
		return true
	default:
		return false
	}
}

func (r *runtime) pauseStop() Outcome {
	return r.stop(StopInterrupted, "pause requested")
}

func (r *runtime) failureOrContext(err error) Outcome {
	if r.ctx.Err() != nil {
		return r.contextStop()
	}
	return r.stop(StopFailed, err.Error())
}

func safeRepeat(def tool.Definition) bool {
	return def.Annotations.ReadOnly != nil &&
		*def.Annotations.ReadOnly ||
		def.Annotations.Idempotent != nil && *def.Annotations.Idempotent
}

func unsafeHosted(defs []tool.Definition) bool {
	for _, def := range defs {
		if def.Kind == tool.KindProviderHosted && !safeRepeat(def) {
			return true
		}
	}
	return false
}

// sameAgent compares agents after both were resolved, so a caller's zero limit
// matches the default recorded for it.
func sameAgent(a, b Agent) bool {
	return sameJSONValue(a, b)
}

func (r *runtime) commit(step Step) *Outcome {
	step.Version = 1
	step.TurnID = r.turn.ID
	step.Seq = int64(len(r.turn.Steps) + 1)
	step.At = time.Now().UTC()
	if step.Kind == StepStopped && step.Stop != nil {
		stop := *step.Stop
		stop.Terminal = r.terminalAfter(stop)
		step.Stop = &stop
	}
	if _, err := json.Marshal(step); err != nil {
		r.err = &CommitError{
			Persistence: PersistenceRejected,
			Step:        &step,
			Err:         err,
		}
		o := r.current()
		return &o
	}
	ctx := r.ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}
	err := r.runner.Recorder.Commit(
		ctx,
		CommitRequest{
			Step:        step,
			ExpectedSeq: step.Seq - 1,
			Owner:       r.opts.owner,
		},
	)
	if err != nil {
		r.err = commitError(step, err)
		o := r.current()
		return &o
	}
	r.turn.Steps = append(r.turn.Steps, cloneStep(step))
	if r.emitter.active() || r.opts.streamSink != nil {
		event := Event{TurnID: step.TurnID, Step: &step}
		if step.Model != nil {
			event.AttemptID = step.Model.AttemptID
			event.MessageID = step.Model.MessageID
		}
		r.emit(event)
	}
	return nil
}

// terminalAfter reports whether recording stop would end the turn. A stop
// changes none of the state terminal reads except the stop itself.
func (r *runtime) terminalAfter(stop Stop) bool {
	state, err := replay(r.turn)
	if err != nil {
		// The commit's own replay check reports the broken record.
		return false
	}
	state.Stop = stop
	return state.terminal()
}

func (r *runtime) emit(event Event) {
	if r.emitter.active() {
		if event.Progress != nil {
			r.emitter.sendProgress(cloneEvent(event))
		} else {
			r.emitter.send(cloneEvent(event))
		}
	}
	if r.opts.streamSink != nil {
		r.opts.streamSink(cloneEvent(event))
	}
}

func cloneEvent(event Event) Event {
	if event.Step != nil {
		step := cloneStep(*event.Step)
		event.Step = &step
	}
	if event.Progress != nil {
		event.Progress = cloneToolProgress(event.Progress)
	}
	if event.Delta != nil {
		delta := *event.Delta
		if delta.Content != nil {
			content := delta.Content.Clone()
			delta.Content = &content
		}
		if delta.Usage != nil {
			delta.Usage = delta.Usage.Copy()
		}
		event.Delta = &delta
	}
	return event
}

func (r *runtime) current() Outcome {
	s, e := replay(r.turn)
	if e != nil {
		if r.err == nil {
			r.err = fmt.Errorf("%w: %w: %v", ErrRecordInvalid, ErrInvalidTurn, e)
		}
		return Outcome{Turn: cloneTurnSafe(r.turn)}
	}
	return outcome(r.turn, s)
}

func hasStartedTool(s *replayState) bool {
	for _, view := range s.Tools {
		if view.Status == ToolStarted {
			return true
		}
	}
	return false
}

func (r *runtime) stop(kind StopKind, reason string) Outcome {
	stop := &Stop{Kind: kind, Reason: reason}
	if kind == StopLimited {
		state, err := replay(r.turn)
		if err == nil {
			switch reason {
			case "model_attempts":
				stop.Limit = &LimitDetail{
					Kind:  LimitModelAttempts,
					Limit: int64(r.agent.Limits.ModelAttempts),
					Used:  int64(state.ModelAttempts),
				}
			case "interruptions":
				stop.Limit = &LimitDetail{
					Kind:  LimitInterruptions,
					Limit: int64(max(r.agent.Limits.Interruptions, 0)),
					Used:  int64(state.Interruptions),
				}
			case "provider_pauses":
				stop.Limit = &LimitDetail{
					Kind:  LimitProviderPauses,
					Limit: int64(max(r.agent.Limits.ProviderPauses, 0)),
					Used:  int64(state.ProviderPauses),
				}
			case "deadline":
				stop.Limit = &LimitDetail{
					Kind:  LimitDeadline,
					Limit: r.agent.Limits.Deadline.Unix(),
					Used:  time.Now().Unix(),
				}
			}
		}
	}
	return r.stopWith(stop)
}

func (r *runtime) stopWith(stop *Stop) Outcome {
	o := r.commit(Step{Kind: StepStopped, Stop: stop})
	if o != nil {
		return *o
	}
	return r.current()
}

func (r *runtime) stopAfterAnswer(s *replayState) Outcome {
	if r.ctx.Err() != nil {
		return r.contextStop()
	}
	if s.finalAnswerOnly {
		return r.stop(StopLimited, "model_attempts")
	}
	return r.stop(StopCompleted, "")
}

func (r *runtime) run() Outcome {
	for {
		s, e := replay(r.turn)
		if e != nil {
			return r.stop(StopFailed, e.Error())
		}
		if s.Stop.Terminal {
			return r.current()
		}
		if s.Interruptions > max(r.agent.Limits.Interruptions, 0) {
			return r.stop(StopLimited, "interruptions")
		}
		if r.ctx.Err() != nil {
			return r.contextStop()
		}
		if s.modelIntent != "" {
			if o := r.recoverModelIntent(s); o != nil {
				return *o
			}
			continue
		}
		if s.unresolvedModel != "" {
			return r.stop(StopUncertain, "model attempt needs reconciliation")
		}
		if stop := budgetOvershotStop(s); stop != nil {
			return r.stopWith(stop)
		}
		if len(s.callOrder) > 0 && hasOpenTools(s) {
			if o, done := r.tools(s); done {
				return o
			}
			continue
		}
		if len(s.hosted) > 0 {
			return r.stop(StopWaiting, "provider approval pending")
		}
		if s.Output != nil {
			return r.stopAfterAnswer(s)
		}
		if submitted := submittedOutput(s); submitted != nil {
			if o := r.commit(Step{
				Kind:   StepOutputRecorded,
				Output: submitted,
			}); o != nil {
				return *o
			}
			return r.stopAfterAnswer(s)
		}
		if s.awaitingClassification && s.latestResult != nil {
			o, again := r.classify(*s.latestResult)
			if !again {
				return o
			}
			continue
		}
		if s.failedModel != "" {
			return r.stop(StopFailed, s.failedModel)
		}
		if stop := budgetSpentStop(s); stop != nil {
			return r.stopWith(stop)
		}
		if r.agent.Limits.ModelAttempts <= s.ModelAttempts {
			return r.stop(StopLimited, "model_attempts")
		}
		if !r.agent.Limits.Deadline.IsZero() &&
			!time.Now().Before(r.agent.Limits.Deadline) {
			return r.stop(StopLimited, "deadline")
		}
		if r.pauseRequested() {
			return r.pauseStop()
		}
		o, again := r.model(s)
		if !again {
			return o
		}
	}
}

func (r *runtime) recoverModelIntent(s *replayState) *Outcome {
	unsafe := false
	for _, def := range s.modelDefs {
		if def.Kind == tool.KindProviderHosted && !safeRepeat(def) {
			unsafe = true
			break
		}
	}
	fact := &ModelFact{
		AttemptID:   s.modelIntent,
		MessageID:   s.modelMessages[s.modelIntent],
		Error:       "worker stopped before model result",
		Interrupted: true,
		Uncertain:   unsafe,
	}
	setSettlementCharge(s.modelIntents[s.modelIntent], fact)
	if o := r.commit(Step{Kind: StepModelSettled, Model: fact}); o != nil {
		return o
	}
	if unsafe {
		o := r.stop(StopUncertain, "provider-hosted tool may have run")
		return &o
	}
	return nil
}

func hasOpenTools(s *replayState) bool {
	for _, id := range s.callOrder {
		if s.Tools[id].Status != ToolSettled {
			return true
		}
	}
	return false
}

func (r *runtime) model(s *replayState) (Outcome, bool) {
	finalAnswerOnly := !r.agent.Limits.NoReservedFinalAnswer &&
		s.ModelAttempts > 0 &&
		s.ModelAttempts+1 == r.agent.Limits.ModelAttempts
	messages, err := projectWithHistory(r.turn, r.history)
	if err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	defs := []tool.Definition{}
	if !finalAnswerOnly && r.runner.Tools != nil {
		tools, e := r.runner.Tools.Tools(r.ctx)
		if e != nil {
			return r.failureOrContext(e), false
		}
		for _, t := range tools {
			if e = t.Definition.Validate(); e != nil {
				return r.stop(StopFailed, e.Error()), false
			}
			if t.Definition.Name == SubmitOutputTool {
				return r.stop(
					StopFailed,
					"tool name is reserved for structured output",
				), false
			}
			defs = append(defs, t.Definition.Clone())
		}
	}
	structured := len(s.OutputSchema) > 0
	if structured {
		defs = append(
			defs,
			tool.Definition{
				Name:        SubmitOutputTool,
				Description: "Submit the final JSON object when all other work is finished. Its arguments must satisfy the output schema.",
				Schema:      slices.Clone(s.OutputSchema),
				Kind:        tool.KindLocal,
				Annotations: tool.Annotations{ReadOnly: new(true), Idempotent: new(true)},
			},
		)
	}
	system := modelSystem(
		r.agent.Instructions,
		containsReminder(messages),
		structured,
		finalAnswerOnly,
	)
	input := llm.Input{
		Model:    r.agent.Model,
		System:   system,
		Messages: messages,
		Tools:    defs,
		Settings: r.agent.Settings,
		Stream:   r.opts.stream,
	}
	if finalAnswerOnly {
		input.Settings.ToolChoice = nil
		input.Settings.ParallelTools = nil
	}
	decision, err := r.runner.Policy.BeforeModel(
		r.ctx,
		ModelPlan{Input: cloneInput(input), Turn: snapshotView(s.TurnView)},
	)
	if err != nil {
		return r.failureOrContext(err), false
	}
	if decision.Stop != nil {
		if decision.Stop.Limit != nil {
			if decision.Stop.Kind != StopLimited ||
				decision.Stop.Limit.Validate() != nil {
				return r.stop(StopFailed, "invalid policy limit"), false
			}
		}
		return r.stopWith(decision.Stop), false
	}
	if len(decision.Deliver) > 0 {
		if o := r.commit(Step{
			Kind:    StepDelivered,
			Context: &ContextFact{Items: decision.Deliver},
		}); o != nil {
			return *o, false
		}
		input.Messages, err = projectWithHistory(r.turn, r.history)
		if err != nil {
			return r.stop(StopFailed, err.Error()), false
		}
		input.System = modelSystem(
			r.agent.Instructions,
			containsReminder(input.Messages),
			structured,
			finalAnswerOnly,
		)
	}
	if decision.Settings != nil {
		input.Settings = *decision.Settings
	}
	if finalAnswerOnly {
		// The turn-wide call limit wins over a forced application-tool choice.
		// BeforeSend still authorizes the actual prepared request.
		input.Settings.ToolChoice = nil
		input.Settings.ParallelTools = nil
	}
	if len(s.OutputSchema) > 0 {
		if err := validateOutputToolChoice(input.Settings); err != nil {
			return r.stop(StopFailed, err.Error()), false
		}
	}
	var budgetWarnings []Warning
	if s.Budget != nil && s.Budget.MaxOutputTokens > 0 {
		remaining := s.Budget.MaxOutputTokens - s.Spend.Usage.OutputTokens
		if remaining <= 0 {
			return r.stopWith(budgetSpentStop(s)), false
		}
		if input.Settings.MaxTokens == 0 || input.Settings.MaxTokens > remaining {
			old := input.Settings.MaxTokens
			input.Settings.MaxTokens = remaining
			budgetWarnings = append(
				budgetWarnings,
				Warning{
					Code: WarningBudgetClamp,
					Message: fmt.Sprintf(
						"max_tokens %d bounded to %d by the turn output-token budget",
						old,
						remaining,
					),
				},
			)
		}
	}
	req, err := r.runner.Client.Prepare(r.ctx, input)
	if err != nil {
		return r.failureOrContext(err), false
	}
	if origin := req.Origin(); origin.Provider != r.agent.Provider ||
		origin.API != r.agent.API ||
		origin.Model != r.agent.Model {
		return r.stop(StopFailed, "prepared request changed destination"), false
	}
	snapshot := req.Snapshot()
	if !sameJSONValue(snapshot.Input, input) {
		return r.stop(
			StopFailed,
			"prepared request changed original model input",
		), false
	}
	atRisk, remainingCost, costStop := checkSendBudget(s, req, r.agent.Model)
	if costStop != nil {
		return r.stopWith(costStop), false
	}
	authorization, err := r.runner.Policy.BeforeSend(
		r.ctx,
		SendPlan{
			Request:   req,
			Turn:      snapshotView(s.TurnView),
			AtRisk:    atRisk,
			Remaining: remainingCost,
		},
	)
	if err != nil {
		return r.failureOrContext(err), false
	}
	if !authorization.Allow {
		if authorization.Limit != nil {
			limit := *authorization.Limit
			if limit.Kind != LimitHost || limit.Validate() != nil {
				return r.stop(StopFailed, "invalid host limit"), false
			}
			return r.stopWith(&Stop{
				Kind:   StopLimited,
				Reason: authorization.Reason,
				Limit:  &limit,
			}), false
		}
		return r.stop(StopRefused, authorization.Reason), false
	}
	if len(authorization.Admission) > 0 &&
		(!json.Valid(authorization.Admission) ||
			len(authorization.Admission) > 16<<10) {
		return r.stop(StopFailed, "invalid model admission metadata"), false
	}
	if r.ctx.Err() != nil {
		return r.contextStop(), false
	}
	if r.pauseRequested() {
		return r.pauseStop(), false
	}
	attempt := fmt.Sprintf("%s:model:%d", r.turn.ID, s.modelCalls+1)
	messageID := attempt + ":message"
	fact := &ModelFact{
		AttemptID:       attempt,
		MessageID:       messageID,
		RequestDigest:   digest(req.Body()),
		FinalAnswerOnly: finalAnswerOnly,
		Admission: append(
			json.RawMessage(nil),
			authorization.Admission...,
		),
		AtRisk:               atRisk,
		EstimatedInputTokens: llm.EstimatedInputTokens(req),
		Provider:             r.runner.Client.Provider(),
		API:                  r.runner.Client.API(),
		Model:                r.agent.Model,
		Effective:            snapshot.Effective,
		Origin:               snapshot.Origin,
		Adjustments:          snapshot.Adjustments,
		ContentChanges:       snapshot.ContentChanges,
		ModelInfo:            snapshot.ModelInfo,
		CapabilityPolicy:     snapshot.CapabilityPolicy,
		Unverified:           snapshot.Unverified,
		Tools:                defs,
	}
	if o := r.commit(Step{
		Kind:     StepModelIntent,
		Model:    fact,
		Warnings: budgetWarnings,
	}); o != nil {
		return *o, false
	}
	if err := r.ctx.Err(); err != nil || r.pauseRequested() {
		reason := "pause requested"
		if err != nil {
			reason = context.Cause(r.ctx).Error()
		}
		settled := &ModelFact{
			AttemptID:   attempt,
			MessageID:   messageID,
			Error:       reason,
			Interrupted: true,
			NoDispatch:  true,
		}
		if o := r.commit(Step{Kind: StepModelSettled, Model: settled}); o != nil {
			return *o, false
		}
		if err != nil {
			return r.contextStop(), false
		}
		return r.pauseStop(), false
	}
	var result llm.Result
	var sendErr error
	var partialText strings.Builder
	var partialContent []llm.Content
	var observedUsage *llm.Usage
	for item, err := range llm.Stream(
		r.ctx,
		r.runner.Client,
		llm.Call{
			Request: req,
			// The client may key durable work by this ID; see llm.Attempt.
			Attempt: llm.Attempt{TurnID: r.turn.ID, AttemptID: attempt},
		},
	) {
		if item.Delta != nil {
			if item.Delta.Usage != nil {
				if observedUsage == nil {
					observedUsage = &llm.Usage{}
				}
				observedUsage.Absorb(item.Delta.Usage)
			}
			if item.Delta.Kind == llm.DeltaText {
				partialText.WriteString(item.Delta.Text)
			}
			if item.Delta.Content != nil {
				partialContent = append(partialContent, item.Delta.Content.Clone())
			}
			if r.ctx.Err() == nil {
				r.emit(Event{
					TurnID:    r.turn.ID,
					AttemptID: attempt,
					MessageID: messageID,
					Delta:     item.Delta,
				})
			}
		}
		if item.Result != nil {
			result = *item.Result
			sendErr = err
		}
	}
	settled := &ModelFact{AttemptID: attempt, MessageID: messageID}
	if r.ctx.Err() != nil {
		settled.Interrupted = true
		settled.Error = context.Cause(r.ctx).Error()
		settled.Uncertain = unsafeHosted(defs)
		if len(result.Message.Content) == 0 {
			switch {
			case partialText.Len() > 0:
				result.Message = llm.AssistantMessage(llm.Text(partialText.String()))
			case len(partialContent) > 0:
				result.Message = llm.AssistantMessage(partialContent...)
			}
		}
		if observedUsage != nil {
			if result.Usage == nil {
				result.Usage = observedUsage
			} else {
				result.Usage.Absorb(observedUsage)
			}
		}
		if result.ID != "" || len(result.Message.Content) > 0 ||
			result.Usage != nil {
			settled.Result = &result
		}
		setSettlementCharge(fact, settled)
		if o := r.commit(Step{Kind: StepModelSettled, Model: settled}); o != nil {
			return *o, false
		}
		if settled.Uncertain && r.runner.OnContextDone != ContextCancel &&
			!errors.Is(context.Cause(r.ctx), errTurnDeadline) {
			return r.stop(StopUncertain, "provider-hosted tool may have run"), false
		}
		return r.contextStop(), false
	}
	var validationErr error
	if sendErr != nil {
		settled.Error = sendErr.Error()
		settled.Failure = failureDisposition(sendErr, result)
		settled.Uncertain = settled.Failure == llm.FailureUnknown
		if !settled.Uncertain {
			if failure, ok := errors.AsType[*llm.Error](sendErr); ok {
				settled.Evidence = fmt.Sprintf(
					"client disposition %s; HTTP %d",
					failure.Disposition,
					failure.Status,
				)
			}
		}
	} else {
		validationErr = validateModelResult(result, r.agent)
		if validationErr != nil {
			settled.Error = validationErr.Error()
		}
	}
	if sendErr == nil ||
		result.Stop.Kind != "" ||
		result.ID != "" ||
		len(result.Message.Content) > 0 ||
		result.Usage != nil {
		settled.Result = &result
	}
	setSettlementCharge(fact, settled)
	if o := r.commit(Step{Kind: StepModelSettled, Model: settled}); o != nil {
		return *o, false
	}
	if r.ctx.Err() != nil {
		return r.contextStop(), false
	}
	if sendErr != nil {
		if settled.Uncertain {
			return r.stop(StopUncertain, sendErr.Error()), false
		}
		return r.stop(StopFailed, sendErr.Error()), false
	}
	if validationErr != nil {
		return r.stop(StopFailed, validationErr.Error()), false
	}
	current, replayErr := replay(r.turn)
	if replayErr != nil {
		return r.stop(StopFailed, replayErr.Error()), false
	}
	if stop := budgetOvershotStop(current); stop != nil {
		return r.stopWith(stop), false
	}
	return r.classify(result)
}

func failureDisposition(err error, result llm.Result) llm.FailureDisposition {
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || result.ID != "" || len(result.Message.Content) > 0 ||
		result.Usage != nil {
		return llm.FailureUnknown
	}
	if failure.Disposition == llm.FailureNotDispatched ||
		failure.Disposition == llm.FailureRejected {
		return failure.Disposition
	}
	return llm.FailureUnknown
}

func (r *runtime) classify(result llm.Result) (Outcome, bool) {
	if r.ctx.Err() != nil {
		return r.contextStop(), false
	}
	if err := validateModelResult(result, r.agent); err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	calls := 0
	hosted := 0
	useful := false
	for _, p := range result.Message.Content {
		switch p.Kind {
		case llm.ContentToolCall:
			calls++
		case llm.ContentHostedApprovalRequest:
			hosted++
		case llm.ContentBlock:
			if p.Block != nil &&
				(p.Block.Text == nil ||
					strings.TrimSpace(p.Block.Text.Text) != "" ||
					len(p.Block.Text.Citations) > 0) {
				useful = true
			}
		case llm.ContentRefusal:
			if p.Refusal != nil && strings.TrimSpace(*p.Refusal) != "" {
				useful = true
			}
		}
	}
	if calls > 0 &&
		result.Stop.Kind != llm.StopToolUse &&
		result.Stop.Kind != llm.StopFinished {
		return r.stop(StopFailed, "untrusted stop with client calls"), false
	}
	if hosted > 0 &&
		result.Stop.Kind != llm.StopToolUse &&
		result.Stop.Kind != llm.StopFinished {
		return r.stop(StopFailed, "untrusted stop with hosted approval"), false
	}
	if calls > 0 {
		return Outcome{}, true
	}
	if hosted > 0 {
		return r.stop(StopWaiting, "provider approval pending"), false
	}
	switch result.Stop.Kind {
	case llm.StopPause:
		state, _ := replay(r.turn)
		if state.ProviderPauses > max(r.agent.Limits.ProviderPauses, 0) {
			return r.stop(StopLimited, "provider_pauses"), false
		}
		if o := r.commit(Step{Kind: StepLoopContinued}); o != nil {
			return *o, false
		}
		return Outcome{}, true
	case llm.StopRefusal:
		return r.stop(StopRefused, result.Stop.Raw), false
	case llm.StopOutputLimit:
		return r.stop(StopLimited, "output"), false
	case llm.StopContextLimit:
		return r.stop(StopLimited, "context"), false
	case llm.StopFinished, llm.StopOther:
		state, _ := replay(r.turn)
		sd, e := r.runner.Policy.OnStop(r.ctx, snapshotView(state.TurnView))
		if e != nil {
			return r.failureOrContext(e), false
		}
		if r.ctx.Err() != nil {
			return r.contextStop(), false
		}
		if !useful && !sd.AcceptEmpty && len(state.OutputSchema) == 0 {
			return r.stop(StopFailed, "empty_response"), false
		}
		if sd.Continue && !state.finalAnswerOnly {
			step := Step{Kind: StepLoopContinued}
			if len(sd.Deliver) > 0 {
				step.Context = &ContextFact{Items: sd.Deliver}
			}
			if o := r.commit(step); o != nil {
				return *o, false
			}
			return Outcome{}, true
		}
		if len(state.OutputSchema) > 0 {
			if result.Stop.Kind != llm.StopFinished {
				return r.stop(
					StopFailed,
					"output_schema_unsatisfied: unrecognized model stop",
				), false
			}
			schema, err := compileOutputSchema(state.OutputSchema)
			if err != nil {
				return r.stop(StopFailed, err.Error()), false
			}
			raw, err := parseOutputText(result.Message.Text(), schema)
			if err != nil {
				return r.stop(StopFailed, "output_schema_unsatisfied: "+err.Error()), false
			}
			state, _ := replay(r.turn)
			fact := &StructuredOutput{
				Value:     raw,
				Source:    OutputFromText,
				AttemptID: state.latestAttempt,
			}
			if o := r.commit(Step{Kind: StepOutputRecorded, Output: fact}); o != nil {
				return *o, false
			}
			return r.stopAfterAnswer(state), false
		}
		return r.stopAfterAnswer(state), false
	default:
		return r.stop(StopFailed, "incomplete_model_result"), false
	}
}

func submittedOutput(s *replayState) *StructuredOutput {
	if len(s.OutputSchema) == 0 || len(s.pending()) > 0 {
		return nil
	}
	for _, id := range s.callOrder {
		v := s.Tools[id]
		if v.Call.Name == SubmitOutputTool &&
			v.Status == ToolSettled &&
			v.Outcome != nil &&
			v.Outcome.State == tool.StateSucceeded {
			return &StructuredOutput{
				Value:      slices.Clone(v.Call.Args),
				Source:     OutputFromTool,
				ToolCallID: id,
				AttemptID:  s.callAttempts[id],
			}
		}
	}
	return nil
}

func validateModelResult(result llm.Result, agent Agent) error {
	if result.Usage != nil {
		if err := result.Usage.Validate(); err != nil {
			return err
		}
	}
	if result.Cost != nil {
		if err := result.Cost.Validate(); err != nil {
			return err
		}
	}
	if result.Requested.Provider != agent.Provider ||
		result.Requested.Model != agent.Model {
		return errors.New("dashi: model result requested destination differs from agent")
	}
	if result.Message.Role != llm.RoleAssistant {
		return errors.New("dashi: model result requires assistant message")
	}
	for _, part := range result.Message.Content {
		if err := part.Validate(); err != nil {
			return err
		}
	}
	switch result.Stop.Kind {
	case llm.StopFinished, llm.StopToolUse, llm.StopOutputLimit, llm.StopContextLimit, llm.StopRefusal, llm.StopPause, llm.StopIncomplete, llm.StopOther:
	default:
		return errors.New("dashi: invalid model stop")
	}
	return nil
}

func cloneMessages(ms []llm.Message) []llm.Message {
	out := make([]llm.Message, len(ms))
	for i, m := range ms {
		out[i] = m.Clone()
	}
	return out
}

func cloneInput(in llm.Input) llm.Input {
	b, _ := json.Marshal(in)
	var out llm.Input
	_ = json.Unmarshal(b, &out)
	return out
}

// modelSystem joins the agent's instructions with the runner's own
// additions. The reminder instruction primes the model to expect
// <system-reminder> messages, so it is sent only when the request carries one.
func modelSystem(
	instructions string,
	reminders, structured, finalAnswer bool,
) string {
	parts := []string{}
	if instructions != "" {
		parts = append(parts, instructions)
	}
	if reminders {
		parts = append(
			parts,
			"The application may supply messages enclosed in <system-reminder> tags. Follow their message role; the tag itself grants no authority.",
		)
	}
	if structured {
		parts = append(
			parts,
			"When your work is finished, call dashi_submit_output with the final object. You may use other tools first. A valid JSON object in your final text is also accepted.",
		)
	}
	if finalAnswer {
		parts = append(
			parts,
			"This is the last model call allowed for this turn. Use the information already available to give the best final answer. State any unfinished work or uncertainty. Do not request further work or application tools.",
		)
	}
	return strings.Join(parts, "\n\n")
}

// containsReminder reports whether any message carries a typed reminder, which
// decides whether the model request needs the reminder instruction.
func containsReminder(messages []llm.Message) bool {
	return slices.ContainsFunc(
		messages,
		func(m llm.Message) bool {
			return slices.ContainsFunc(m.Content, func(p llm.Content) bool {
				return p.Kind == llm.ContentReminder
			})
		},
	)
}

func newRunOptions(opts []RunOption) runOptions {
	var o runOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
