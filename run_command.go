package dashi

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func (r Runner) apply(
	ctx context.Context,
	turn Turn,
	command Command,
	opts runOptions,
) (RunResult, error) {
	if command == nil {
		return RunResult{}, errors.New("dashi: command is required")
	}
	if _, ok := command.(Steer); ok {
		return RunResult{}, errors.New(
			"dashi: a steer reaches a turn through its inbox (Runner.Inbox or WithInbox)",
		)
	}
	if !opts.inboxSet {
		opts.inbox = r.Inbox
	}
	if r.OnContextDone != "" && r.OnContextDone != ContextInterrupt &&
		r.OnContextDone != ContextCancel {
		return RunResult{}, errors.New("dashi: invalid context-done behavior")
	}
	// Empty IDs are generated before anything reads them: the turn ID before
	// the view is built, recorder setup, and loading, and the command ID
	// before its hash, which covers the ID.
	if turn.ID == "" {
		if _, ok := command.(Start); !ok {
			return RunResult{}, errors.New("dashi: turn ID is required for commands other than Start")
		}
		turn.ID = NewID()
	}
	command = normalizeCommand(command)
	hooks := JoinHooks(r.Hooks, opts.hooks)
	if err := hooks.validate(); err != nil {
		return RunResult{}, err
	}
	state, err := replay(turn)
	if err != nil {
		return RunResult{}, err
	}
	// A Start's agent may leave its destination, model, and version to
	// defaults. The filled values are pinned like explicit ones, so a retried
	// Start compares the same resolved agent. Other commands use the recorded
	// agent.
	agent, err := r.agentFor(command, state)
	if err != nil {
		return RunResult{}, err
	}
	_, isStart := command.(Start)
	implicitRecorder := r.Recorder == nil
	if implicitRecorder {
		r.Recorder = newMemoryRecorderForTurn(turn)
	}
	if isStart && len(turn.Steps) > 0 {
		if err := agentChanged(agent, state.Agent); err != nil {
			return RunResult{}, err
		}
	}
	// A Start's hash covers its filled-in agent, so a retry that spells out
	// a default, such as the client's provider, is the same command.
	hashed := command
	if c, ok := command.(Start); ok {
		c.Agent = agent
		hashed = c
	}
	d, err := commandHash(hashed)
	if err != nil {
		return RunResult{}, err
	}
	if old, ok := state.commandHashes[command.commandID()]; ok {
		if old != d {
			return RunResult{}, ErrCommandIDReused
		}
		// A retry: return the recorded result, from the recorder's copy
		// unless the turn was just loaded (Advance).
		if loader, ok := r.Recorder.(Loader); ok && !opts.fresh {
			saved, loadErr := loader.Load(ctx, turn.ID)
			if loadErr != nil {
				return RunResult{}, loadErr
			}
			current, replayErr := replay(saved)
			if replayErr != nil {
				return RunResult{}, replayErr
			}
			if isStart {
				if err := agentChanged(agent, current.Agent); err != nil {
					return RunResult{}, err
				}
			}
			if current.commandHashes[command.commandID()] != d {
				return RunResult{}, ErrStaleTurn
			}
			return outcome(saved, current), nil
		}
		return outcome(turn, state), nil
	}
	// A caller can retry with an old snapshot after an acknowledgement was lost.
	// Only a turn known not to exist falls back to the caller's snapshot;
	// acting on a snapshot the recorder could not confirm might repeat work.
	// Advance passes a turn it just loaded, so it skips the second load.
	if loader, ok := r.Recorder.(Loader); ok && !opts.fresh {
		saved, loadErr := loader.Load(ctx, turn.ID)
		if loadErr != nil && !errors.Is(loadErr, ErrTurnNotFound) {
			return RunResult{}, loadErr
		}
		if loadErr == nil {
			current, replayErr := replay(saved)
			if replayErr != nil {
				return RunResult{}, replayErr
			}
			if old, found := current.commandHashes[command.commandID()]; found {
				if isStart {
					if err := agentChanged(agent, current.Agent); err != nil {
						return RunResult{}, err
					}
				}
				if old != d {
					return RunResult{}, ErrCommandIDReused
				}
				return outcome(saved, current), nil
			}
			// Commands on an existing turn run with the snapshot's
			// recorded agent, so it must be the recorder's.
			if len(turn.Steps) > 0 && len(saved.Steps) > 0 &&
				!sameAgent(state.Agent, current.Agent) {
				return RunResult{}, fmt.Errorf(
					"%w: the turn's recorded agent differs from the recorder's; load the turn from the recorder",
					ErrAgentChanged,
				)
			}
		}
	}
	// Checked after the retry checks, so a retried command on an ended turn
	// still gets its recorded result.
	if err := state.acceptsAfterEnd(command); err != nil {
		return refused(err)
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
	var cancelRun context.CancelCauseFunc
	if opts.inbox != nil && !opts.record {
		runCtx, cancelRun = context.WithCancelCause(runCtx)
		defer cancelRun(nil)
	}
	rt := runtime{
		ctx:              runCtx,
		commitCtx:        ctx,
		cancelRun:        cancelRun,
		pendingDelivered: slices.Clone(opts.delivered),
		pendingWarnings:  slices.Clone(opts.warnings),
		turn:             cloneTurn(turn),
		agent:            agent,
		runner:           r,
		opts:             opts,
		implicitRecorder: implicitRecorder,
		emitter:          newEmitter(r.Observer),
		hooks:            hooks,
		activeHooks:      r.Hooks.active(),
		lastHooks:        state.activeHooks,
	}
	defer rt.emitter.close()
	defer rt.endSteering()
	switch c := command.(type) {
	case Start:
		if len(turn.Steps) != 0 || len(c.Input) == 0 {
			return refused(errors.New("dashi: start requires a new turn and input"))
		}
		if c.Budget != nil {
			if err := c.Budget.Validate(); err != nil {
				return refused(err)
			}
		}
		if c.Parent != nil {
			if err := c.Parent.Validate(); err != nil {
				return refused(err)
			}
		}
		if err := validateHistory(c.History); err != nil {
			return refused(err)
		}
		hash, err := historyHash(c.History)
		if err != nil {
			return refused(err)
		}
		rt.history = llmclone.Messages(c.History)
		if len(c.OutputSchema) > 0 {
			if _, err := compileOutputSchema(c.OutputSchema); err != nil {
				return refused(err)
			}
			if err := validateOutputToolChoice(agent.Settings); err != nil {
				return refused(err)
			}
		}
		origin := c.Origin
		if origin == "" {
			origin = StartUser
		}
		if err := validateCompaction(origin, c.Compaction); err != nil {
			return refused(fmt.Errorf("dashi: %w", err))
		}
		items, err := newMessageItems(state, c.Items)
		if err != nil {
			return refused(err)
		}
		if len(items) == 0 {
			items = nil
		}
		if n := c.Agent.Limits.ToolResultBytes; n > 0 && n < minToolResultBytes {
			rt.pendingWarnings = append(rt.pendingWarnings, Warning{
				Code: WarningLimitAdjusted,
				Message: fmt.Sprintf(
					"Limits.ToolResultBytes %d is below the minimum; "+
						"the turn uses %d",
					n,
					minToolResultBytes,
				),
			})
		}
		window, err := activeWindow(agent, r.Client, origin)
		if err != nil {
			return RunResult{}, err
		}
		if agent.ActiveCompaction != nil && !agent.ActiveCompaction.Disabled && origin != StartCompaction && window == 0 {
			rt.pendingWarnings = append(rt.pendingWarnings, Warning{Code: WarningActiveCompactionUnavailable, Message: "Active context protection has no recorded window; set ActiveCompactionPolicy.Window or provide model context evidence."})
		}
		if r.Client == nil && !opts.record {
			return RunResult{}, errNoModelClient
		}
		if o := rt.commit(Step{
			Kind:        StepTurnStarted,
			CommandID:   c.CommandID,
			CommandHash: d,
			Started: &StartFact{
				Agent:                  agent,
				ActiveCompactionWindow: window,
				Budget:                 cloneBudget(c.Budget),
				OutputSchema:           slices.Clone(c.OutputSchema),
				Origin:                 origin,
				HistoryHash:            hash,
				Input:                  llmclone.Messages(c.Input),
				Items:                  items,
				Parent:                 cloneCallRef(c.Parent),
				Compaction:             c.Compaction.clone(),
			},
		}); o != nil {
			return rt.result(*o)
		}
		if opts.record {
			return rt.result(rt.current())
		}
	case AddMessages:
		if len(turn.Steps) == 0 ||
			state.Output != nil ||
			len(c.Items) == 0 {
			return refused(errors.New("dashi: turn cannot receive context"))
		}
		items, err := newMessageItems(state, c.Items)
		if err != nil {
			return refused(err)
		}
		if len(items) == 0 {
			// Every item is already recorded: a retried AddMessages.
			return rt.result(rt.current())
		}
		step := Step{
			Kind:        StepMessagesAdded,
			CommandID:   c.CommandID,
			CommandHash: d,
			Context:     &ContextFact{Items: items},
		}
		if o := rt.commit(step); o != nil {
			return rt.result(*o)
		}
		return rt.result(rt.current())
	case Continue:
		if len(turn.Steps) == 0 {
			return refused(fmt.Errorf("%w: turn has not started", ErrNotReady))
		}
		if c.OnHistoryMismatch != "" &&
			c.OnHistoryMismatch != HistoryMismatchWarn &&
			c.OnHistoryMismatch != HistoryMismatchReject {
			return refused(errors.New("dashi: invalid history-mismatch behavior"))
		}
		if agent.Limits.NoRaise && (c.Budget != nil || c.ModelCalls != 0) {
			return refused(errors.New(
				"dashi: the agent's limits cannot be raised (Limits.NoRaise)",
			))
		}
		if c.Budget != nil {
			old := Budget{}
			if state.Budget != nil {
				old = *state.Budget
			}
			if err := c.Budget.raisedFrom(old); err != nil {
				return refused(err)
			}
		}
		// History is checked before any path that can commit, so a rejected
		// Continue never changes the record.
		if err := validateHistory(c.History); err != nil {
			return refused(err)
		}
		hash, err := historyHash(c.History)
		if err != nil {
			return refused(err)
		}
		stored := turn.Steps[0].Started.HistoryHash
		mismatch := stored != hash
		if mismatch && c.OnHistoryMismatch == HistoryMismatchReject {
			return RunResult{}, ErrHistoryMismatch
		}
		if state.resumeLimitReached() {
			step := Step{
				Kind:        StepTurnStopped,
				CommandID:   c.CommandID,
				CommandHash: d,
				Stop: &Stop{
					Kind:   StopLimited,
					Reason: string(LimitResumes),
					Limit: &LimitDetail{
						Kind:  LimitResumes,
						Limit: int64(max(agent.Limits.Resumes, 0)),
						Used:  int64(state.Resumes),
					},
				},
			}
			if o := rt.commit(step); o != nil {
				return rt.result(*o)
			}
			return rt.result(rt.current())
		}
		if c.ModelCalls < 0 ||
			c.ModelCalls != 0 && c.ModelCalls <= state.modelCallCap {
			return refused(fmt.Errorf(
				"dashi: model-call raise must exceed the current limit of %d",
				state.modelCallCap,
			))
		}
		if !state.canContinueWith(c.Budget, c.ModelCalls) {
			return refused(notReady(state))
		}
		if r.Client == nil {
			return RunResult{}, errNoModelClient
		}
		rt.history = llmclone.Messages(c.History)
		step := Step{
			Kind:        StepTurnContinued,
			CommandID:   c.CommandID,
			CommandHash: d,
			Budget:      cloneBudget(c.Budget),
			ModelCalls:  c.ModelCalls,
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
	case Resolve:
		if len(turn.Steps) == 0 {
			return refused(errors.New("dashi: cannot resolve before start"))
		}
		n := 0
		if c.Outcome != nil {
			n++
		}
		if c.Approval != nil {
			n++
		}
		if c.ProviderToolApproval != nil {
			n++
		}
		if c.ModelResult != nil {
			if err := validateModelResult(*c.ModelResult, agent); err != nil {
				return refused(err)
			}
			n++
		}
		if c.ModelFailure != nil {
			if (c.ModelFailure.SendStatus != llm.SendStatusNotSent &&
				c.ModelFailure.SendStatus != llm.SendStatusProviderRejected) ||
				c.ModelFailure.Reason == "" ||
				c.ModelFailure.Evidence == "" {
				return refused(errors.New("dashi: model failure requires verified nonexecution evidence"))
			}
			n++
		}
		if n != 1 {
			return refused(errors.New("dashi: resolve requires one fact"))
		}
		if c.ModelResult != nil || c.ModelFailure != nil {
			if c.CallID == "" ||
				(c.CallID != state.modelCallStart &&
					c.CallID != state.unresolvedModel) {
				return refused(errors.New("dashi: model call is not pending"))
			}
			kind := StepModelCallFinished
			if c.CallID == state.unresolvedModel {
				kind = StepModelCallFinishedByHost
			}
			fact := &ModelFact{
				ModelCallID: c.CallID,
				MessageID:   c.CallID + ":message",
				Result:      c.ModelResult,
			}
			if c.ModelFailure != nil {
				fact.Error = c.ModelFailure.Reason
				fact.SendStatus = c.ModelFailure.SendStatus
				fact.Evidence = c.ModelFailure.Evidence
			}
			step := Step{
				Kind:        kind,
				CommandID:   c.CommandID,
				CommandHash: d,
				Model:       fact,
			}
			if err := checkLateResultStep(turn, state, step); err != nil {
				return refused(err)
			}
			if o := rt.commit(step); o != nil {
				return rt.result(*o)
			}
		} else if c.ProviderToolApproval != nil {
			if c.CallID != "" && c.CallID != c.ProviderToolApproval.RequestID {
				return refused(errors.New("dashi: provider tool approval ID mismatch"))
			}
			a, ok := state.providerTool[c.ProviderToolApproval.RequestID]
			if !ok || a.Origin != c.ProviderToolApproval.Origin {
				return refused(errors.New("dashi: provider tool approval is not pending"))
			}
			if o := rt.commit(Step{
				Kind:                 StepProviderToolApprovalDecided,
				CommandID:            c.CommandID,
				CommandHash:          d,
				ProviderToolApproval: c.ProviderToolApproval,
			}); o != nil {
				return rt.result(*o)
			}
		} else if c.Approval != nil {
			v, ok := state.Tools[c.Approval.CallID]
			if !ok || v.Status != ToolApprovalPending || c.CallID != c.Approval.CallID {
				return refused(errors.New("dashi: tool approval is not pending"))
			}
			if c.Approval.ArgsHash != ArgsHash(v.Call.Args) {
				return refused(errors.New("dashi: stale tool approval"))
			}
			if o := rt.commit(Step{
				Kind:        StepApprovalDecided,
				CommandID:   c.CommandID,
				CommandHash: d,
				Approval:    c.Approval,
			}); o != nil {
				return rt.result(*o)
			}
		} else {
			v, ok := state.Tools[c.CallID]
			if !ok ||
				(v.Status != ToolStarted &&
					v.Status != ToolWaiting &&
					v.Status != ToolResultUnknown) {
				return refused(errors.New("dashi: tool result is not pending"))
			}
			if v.Call.Name == SubmitOutputTool {
				return refused(errors.New("dashi: structured output is recorded by the runner"))
			}
			if e := c.Outcome.Validate(); e != nil {
				return refused(e)
			}
			if c.Outcome.State == tool.StateWaiting ||
				c.Outcome.State == tool.StateDetached {
				return refused(errors.New("dashi: resolve requires a final tool outcome"))
			}
			out, warnings := dropBadData(*c.Outcome, c.CallID)
			fact := &ToolFact{Call: v.Call, Outcome: &out}
			// The turn deadline limits the turn's own work, not the host
			// recording a result, so the hooks run under the command's
			// context.
			rt.afterTool(ctx, fact)
			step := Step{
				Kind:        StepToolFinished,
				CommandID:   c.CommandID,
				CommandHash: d,
				Tool:        fact,
				Warnings:    warnings,
			}
			if err := checkLateResultStep(turn, state, step); err != nil {
				return refused(err)
			}
			if o := rt.commit(step); o != nil {
				return rt.result(*o)
			}
		}
		return rt.result(rt.current())
	case Cancel:
		// A turn with no steps has not started: a host that saved work to
		// run later and never ran it withdraws it itself.
		if len(turn.Steps) == 0 {
			return refused(errors.New("dashi: cannot cancel: turn has not started"))
		}
		if state.Output != nil {
			return refused(errCancelAfterOutput)
		}
		if o := rt.commit(Step{
			Kind:        StepTurnStopped,
			CommandID:   c.CommandID,
			CommandHash: d,
			Stop:        &Stop{Kind: StopCanceled, Reason: c.Reason},
		}); o != nil {
			return rt.result(*o)
		}
		return rt.result(rt.current())
	default:
		return refused(errors.New("dashi: unknown command"))
	}
	return rt.result(rt.run())
}

// errNoModelClient rejects, before anything is recorded, a Start or Continue
// run without a Client, which could prepare or send a model request.
var errNoModelClient = errors.New("dashi: client is required to call the model")

func pauseClosed(pause <-chan struct{}) bool {
	if pause == nil {
		return false
	}
	select {
	case <-pause:
		return true
	default:
		return false
	}
}

func validateHistory(history []llm.Message) error {
	for i, message := range history {
		if err := validateMessage(message); err != nil {
			return fmt.Errorf("dashi: history message %d: %w", i+1, err)
		}
	}
	return nil
}

// refusedError marks a command the turn cannot take: invalid, not pending,
// or arriving after the turn ended. Nothing was recorded. Any other error
// from apply is a failure of the runner, the recorder, or the context.
type refusedError struct{ err error }

func (e *refusedError) Error() string { return e.err.Error() }
func (e *refusedError) Unwrap() error { return e.err }

// refused returns err marked as a refusal of the command.
func refused(err error) (RunResult, error) {
	return RunResult{}, &refusedError{err: err}
}

// isRefusal reports whether err refused a command, as opposed to failing.
// ErrCommandIDReused and ErrHistoryMismatch are returned bare, so callers
// can compare them with ==.
func isRefusal(err error) bool {
	if _, ok := errors.AsType[*CommitError](err); ok {
		return false
	}
	_, ok := errors.AsType[*refusedError](err)
	return ok || errors.Is(err, ErrCommandIDReused) ||
		errors.Is(err, ErrHistoryMismatch)
}
