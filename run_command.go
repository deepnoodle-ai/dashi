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
	if r.OnContextDone != "" && r.OnContextDone != ContextInterrupt &&
		r.OnContextDone != ContextCancel {
		return RunResult{}, errors.New("dashi: invalid context-done behavior")
	}
	// Empty IDs are generated before anything reads them: the turn ID before
	// replay, recorder setup, and loading, and the command ID before its
	// digest, which covers the ID.
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
	// A Start's agent may leave its destination, model, and revision to
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
	// A Start's digest covers its resolved agent, so a retry that spells out
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
		if loader, ok := r.Recorder.(Loader); ok {
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
	if loader, ok := r.Recorder.(Loader); ok {
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
	// still gets its recorded answer.
	if err := state.acceptsAfterEnd(command); err != nil {
		return RunResult{}, err
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
		ctx:              runCtx,
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
			return RunResult{}, errors.New("dashi: start requires a new turn and input")
		}
		if c.Budget != nil {
			if err := c.Budget.Validate(); err != nil {
				return RunResult{}, err
			}
		}
		if c.Parent != nil {
			if err := c.Parent.Validate(); err != nil {
				return RunResult{}, err
			}
		}
		if err := validateHistory(c.History); err != nil {
			return RunResult{}, err
		}
		hash, err := historyHash(c.History)
		if err != nil {
			return RunResult{}, err
		}
		rt.history = llmclone.Messages(c.History)
		if len(c.OutputSchema) > 0 {
			if _, err := compileOutputSchema(c.OutputSchema); err != nil {
				return RunResult{}, err
			}
			if err := validateOutputToolChoice(agent.Settings); err != nil {
				return RunResult{}, err
			}
		}
		origin := c.Origin
		if origin == "" {
			origin = StartUser
		}
		if err := validateCompaction(origin, c.Compaction); err != nil {
			return RunResult{}, fmt.Errorf("dashi: %w", err)
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
		if err := r.checkModelClient(opts, state); err != nil {
			return RunResult{}, err
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
				Parent:                 cloneCallRef(c.Parent),
				Compaction:             c.Compaction.clone(),
			},
		}); o != nil {
			return rt.result(*o)
		}
	case AddMessages:
		if len(turn.Steps) == 0 ||
			state.Output != nil ||
			len(c.Items) == 0 {
			return RunResult{}, errors.New("dashi: turn cannot receive context")
		}
		items, err := newMessageItems(state, c.Items)
		if err != nil {
			return RunResult{}, err
		}
		if len(items) == 0 {
			// Every item is already recorded: a retried delivery.
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
			return RunResult{}, fmt.Errorf("%w: turn has not started", ErrNotReady)
		}
		if c.OnHistoryMismatch != "" &&
			c.OnHistoryMismatch != HistoryMismatchWarn &&
			c.OnHistoryMismatch != HistoryMismatchReject {
			return RunResult{}, errors.New("dashi: invalid history-mismatch behavior")
		}
		if c.Budget != nil {
			old := Budget{}
			if state.Budget != nil {
				old = *state.Budget
			}
			if err := c.Budget.raisedFrom(old); err != nil {
				return RunResult{}, err
			}
		}
		// History is checked before any path that can commit, so a rejected
		// Continue never changes the record.
		if err := validateHistory(c.History); err != nil {
			return RunResult{}, err
		}
		hash, err := historyHash(c.History)
		if err != nil {
			return RunResult{}, err
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
			return RunResult{}, fmt.Errorf(
				"dashi: model-attempt raise must exceed the current limit of %d",
				state.modelCallCap,
			)
		}
		if !state.canContinueWith(c.Budget, c.ModelCalls) {
			return RunResult{}, notReady(state)
		}
		if err := r.checkModelClient(opts, state); err != nil {
			return RunResult{}, err
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
			return RunResult{}, errors.New("dashi: cannot settle before start")
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
				return RunResult{}, err
			}
			n++
		}
		if c.ModelFailure != nil {
			if (c.ModelFailure.SendStatus != llm.SendStatusNotSent &&
				c.ModelFailure.SendStatus != llm.SendStatusProviderRejected) ||
				c.ModelFailure.Reason == "" ||
				c.ModelFailure.Evidence == "" {
				return RunResult{}, errors.New("dashi: model failure requires verified nonexecution evidence")
			}
			n++
		}
		if n != 1 {
			return RunResult{}, errors.New("dashi: settle requires one fact")
		}
		if c.ModelResult != nil || c.ModelFailure != nil {
			if c.CallID == "" ||
				(c.CallID != state.modelCallStart &&
					c.CallID != state.unresolvedModel) {
				return RunResult{}, errors.New("dashi: model attempt is not pending")
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
			setFinishCharge(state.startedModelCalls[c.CallID], fact)
			step := Step{
				Kind:        kind,
				CommandID:   c.CommandID,
				CommandHash: d,
				Model:       fact,
			}
			if err := checkLateResultStep(turn, state, step); err != nil {
				return RunResult{}, err
			}
			if o := rt.commit(step); o != nil {
				return rt.result(*o)
			}
		} else if c.ProviderToolApproval != nil {
			if c.CallID != "" && c.CallID != c.ProviderToolApproval.RequestID {
				return RunResult{}, errors.New("dashi: provider tool approval ID mismatch")
			}
			a, ok := state.providerTool[c.ProviderToolApproval.RequestID]
			if !ok || a.Origin != c.ProviderToolApproval.Origin {
				return RunResult{}, errors.New("dashi: provider tool approval is not pending")
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
				return RunResult{}, errors.New("dashi: tool approval is not pending")
			}
			if c.Approval.ArgsHash != ArgsHash(v.Call.Args) {
				return RunResult{}, errors.New("dashi: stale tool approval")
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
				return RunResult{}, errors.New("dashi: tool result is not pending")
			}
			if v.Call.Name == SubmitOutputTool {
				return RunResult{}, errors.New("dashi: structured output is settled by the runner")
			}
			if e := c.Outcome.Validate(); e != nil {
				return RunResult{}, e
			}
			if c.Outcome.State == tool.StateWaiting ||
				c.Outcome.State == tool.StateDetached {
				return RunResult{}, errors.New("dashi: settle requires a final tool outcome")
			}
			fact := &ToolFact{Call: v.Call, Outcome: c.Outcome}
			// The turn deadline limits the turn's own work, not the host
			// recording a result, so the hooks run under the command's
			// context.
			rt.afterTool(ctx, fact)
			step := Step{
				Kind:        StepToolFinished,
				CommandID:   c.CommandID,
				CommandHash: d,
				Tool:        fact,
			}
			if err := checkLateResultStep(turn, state, step); err != nil {
				return RunResult{}, err
			}
			if o := rt.commit(step); o != nil {
				return rt.result(*o)
			}
		}
		return rt.result(rt.current())
	case Cancel:
		// A turn with no steps has not started: a host that admitted work
		// and never ran it cancels its own admission.
		if len(turn.Steps) == 0 {
			return RunResult{}, errors.New("dashi: cannot cancel: turn has not started")
		}
		if state.Output != nil {
			return RunResult{}, errors.New("dashi: turn cannot cancel")
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
		return RunResult{}, errors.New("dashi: unknown command")
	}
	return rt.result(rt.run())
}

// errNoModelClient rejects a run that would prepare or send a model request
// without a Client.
var errNoModelClient = errors.New("dashi: client is required to call the model")

// checkModelClient rejects, before anything is recorded, a Start or Continue
// that would call the model without a Client. Only an already-closed pause
// stops the run before its next model call, so such a run needs no Client
// unless a summary from active compaction must still be applied, which
// prepares a request.
func (r Runner) checkModelClient(opts runOptions, state *replayState) error {
	if r.Client != nil {
		return nil
	}
	if !pauseClosed(opts.pause) || state.pendingCompaction != "" {
		return errNoModelClient
	}
	return nil
}

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
