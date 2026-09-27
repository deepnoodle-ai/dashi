package kernel

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

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

type Bindings struct {
	Client     llm.Client
	Tools      tool.Toolset
	Recorder   Recorder
	Policy     Policy
	Observer   Observer
	Stream     bool
	Owner      string
	streamSink func(Event)
}
type Engine struct{}

// Stream advances a turn when iteration begins. A fully read sequence ends
// with one Outcome event; command validation errors appear in the error slot.
// Breaking the range cancels the work. Reload the turn by ID to inspect the
// facts committed during cancellation, since no terminal event follows break.
// A second range yields ErrStreamConsumed instead of repeating work.
func (Engine) Stream(ctx context.Context, turn Turn, def Definition, command Command, b Bindings) iter.Seq2[Event, error] {
	var started atomic.Bool
	return func(yield func(Event, error) bool) {
		if !started.CompareAndSwap(false, true) {
			yield(Event{}, ErrStreamConsumed)
			return
		}
		streamCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		stopped := false
		b.Stream = true
		b.streamSink = func(event Event) {
			if stopped {
				return
			}
			if !yield(event, nil) {
				stopped = true
				cancel()
			}
		}
		out, err := (Engine{}).Apply(streamCtx, turn, def, command, b)
		if stopped {
			return
		}
		if err != nil {
			yield(Event{}, err)
			return
		}
		yield(Event{TurnID: turn.ID, Outcome: &out}, nil)
	}
}

// Apply advances one turn from an explicit command. A Go error means command
// validation failed before a new fact was acknowledged. Once a commit succeeds,
// execution failures are represented in Outcome, retaining partial work.
func (Engine) Apply(ctx context.Context, turn Turn, def Definition, command Command, b Bindings) (Outcome, error) {
	if command == nil || command.commandID() == "" {
		return Outcome{}, errors.New("kernel: command ID is required")
	}
	if err := def.Validate(); err != nil {
		return Outcome{}, err
	}
	if turn.ID == "" || b.Client == nil || b.Recorder == nil {
		return Outcome{}, errors.New("kernel: turn ID, client, and recorder are required")
	}
	if b.Client.Provider() != def.Provider || b.Client.API() != def.API {
		return Outcome{}, errors.New("kernel: client does not match accepted definition")
	}
	if b.Policy == nil {
		b.Policy = denyToolPolicy{}
	}
	state, err := replay(turn)
	if err != nil {
		return Outcome{}, err
	}
	if len(turn.Steps) > 0 && digest(def) != digest(state.Definition) {
		return Outcome{}, ErrDefinitionChanged
	}
	d, err := commandDigest(command)
	if err != nil {
		return Outcome{}, err
	}
	if old, ok := state.commandDigests[command.commandID()]; ok {
		if old != d {
			return Outcome{}, ErrCommandConflict
		}
		if loader, ok := b.Recorder.(interface{ Load(string) (Turn, error) }); ok {
			saved, loadErr := loader.Load(turn.ID)
			if loadErr != nil {
				return Outcome{}, loadErr
			}
			current, replayErr := replay(saved)
			if replayErr != nil {
				return Outcome{}, replayErr
			}
			if digest(def) != digest(current.Definition) {
				return Outcome{}, ErrDefinitionChanged
			}
			if current.commandDigests[command.commandID()] != d {
				return Outcome{}, ErrRevisionConflict
			}
			return outcome(saved, current, PersistenceAcknowledged), nil
		}
		return outcome(turn, state, PersistenceAcknowledged), nil
	}
	// A caller can retry with an old snapshot after an acknowledgement was lost.
	// The built-in recorder exposes Load; other recorders may provide it too.
	if loader, ok := b.Recorder.(interface{ Load(string) (Turn, error) }); ok {
		if saved, loadErr := loader.Load(turn.ID); loadErr == nil {
			current, replayErr := replay(saved)
			if replayErr != nil {
				return Outcome{}, replayErr
			}
			if old, found := current.commandDigests[command.commandID()]; found {
				if digest(def) != digest(current.Definition) {
					return Outcome{}, ErrDefinitionChanged
				}
				if old != d {
					return Outcome{}, ErrCommandConflict
				}
				return outcome(saved, current, PersistenceAcknowledged), nil
			}
		}
	}
	rt := runtime{ctx: ctx, turn: cloneTurn(turn), def: def, b: b, emitter: newEmitter(b.Observer)}
	defer rt.emitter.close()
	switch c := command.(type) {
	case Start:
		if len(turn.Steps) != 0 || len(c.Input) == 0 {
			return Outcome{}, errors.New("kernel: start requires a new turn and input")
		}
		if len(c.OutputSchema) > 0 {
			if _, err := compileOutputSchema(c.OutputSchema); err != nil {
				return Outcome{}, err
			}
			if err := validateOutputToolChoice(def.Settings); err != nil {
				return Outcome{}, err
			}
		}
		origin := c.Origin
		if origin == "" {
			origin = StartUser
		}
		if o := rt.commit(Step{Kind: StepStarted, CommandID: c.ID, CommandDigest: d, Started: &StartFact{Definition: def, OutputSchema: slices.Clone(c.OutputSchema), Origin: origin, History: cloneMessages(c.History), Input: cloneMessages(c.Input)}}); o != nil {
			return *o, nil
		}
	case Deliver:
		if len(turn.Steps) == 0 || state.Stop.Kind == StopCompleted || state.Stop.Kind == StopCanceled || state.Output != nil || len(c.Items) == 0 {
			return Outcome{}, errors.New("kernel: turn cannot receive context")
		}
		if o := rt.commit(Step{Kind: StepDelivered, CommandID: c.ID, CommandDigest: d, Context: &ContextFact{Items: c.Items}}); o != nil {
			return *o, nil
		}
		return rt.current(), nil
	case Continue:
		if !state.canContinue() {
			return Outcome{}, errors.New("kernel: turn cannot continue")
		}
		if o := rt.commit(Step{Kind: StepContinued, CommandID: c.ID, CommandDigest: d}); o != nil {
			return *o, nil
		}
	case Accept:
		if len(turn.Steps) == 0 {
			return Outcome{}, errors.New("kernel: cannot accept before start")
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
			if err := validateModelResult(*c.ModelResult, def); err != nil {
				return Outcome{}, err
			}
			n++
		}
		if c.ModelFailure != nil {
			if (c.ModelFailure.Disposition != llm.FailureNotDispatched && c.ModelFailure.Disposition != llm.FailureRejected) || c.ModelFailure.Reason == "" || c.ModelFailure.Evidence == "" {
				return Outcome{}, errors.New("kernel: model failure requires verified nonexecution evidence")
			}
			n++
		}
		if n != 1 {
			return Outcome{}, errors.New("kernel: accept requires one fact")
		}
		if c.ModelResult != nil || c.ModelFailure != nil {
			if c.CallID == "" || (c.CallID != state.modelIntent && c.CallID != state.unresolvedModel) {
				return Outcome{}, errors.New("kernel: model attempt is not pending")
			}
			kind := StepModelSettled
			if c.CallID == state.unresolvedModel {
				kind = StepModelReconciled
			}
			fact := &ModelFact{AttemptID: c.CallID, MessageID: c.CallID + ":message", Result: c.ModelResult}
			if c.ModelFailure != nil {
				fact.Error = c.ModelFailure.Reason
				fact.Failure = c.ModelFailure.Disposition
				fact.Evidence = c.ModelFailure.Evidence
			}
			if o := rt.commit(Step{Kind: kind, CommandID: c.ID, CommandDigest: d, Model: fact}); o != nil {
				return *o, nil
			}
			if state.Stop.Kind == StopCanceled {
				return rt.current(), nil
			}
			if c.ModelFailure != nil {
				return rt.stop(StopFailed, c.ModelFailure.Reason), nil
			}
			classified, again := rt.classify(*c.ModelResult)
			if !again {
				return classified, nil
			}
			return rt.run(), nil
		} else if c.HostedApproval != nil {
			if c.CallID != "" && c.CallID != c.HostedApproval.RequestID {
				return Outcome{}, errors.New("kernel: hosted approval ID mismatch")
			}
			a, ok := state.hosted[c.HostedApproval.RequestID]
			if !ok || a.Origin != c.HostedApproval.Origin {
				return Outcome{}, errors.New("kernel: hosted approval is not pending")
			}
			if o := rt.commit(Step{Kind: StepHostedApproval, CommandID: c.ID, CommandDigest: d, HostedApproval: c.HostedApproval}); o != nil {
				return *o, nil
			}
		} else if c.Approval != nil {
			v, ok := state.Tools[c.Approval.CallID]
			if !ok || v.Status != ToolApprovalPending || c.CallID != c.Approval.CallID {
				return Outcome{}, errors.New("kernel: tool approval is not pending")
			}
			if digest(v.Call.Args) != c.Approval.ArgsDigest || v.Decision.Revision != c.Approval.PolicyRevision {
				return Outcome{}, errors.New("kernel: stale tool approval")
			}
			if o := rt.commit(Step{Kind: StepApproval, CommandID: c.ID, CommandDigest: d, Approval: c.Approval}); o != nil {
				return *o, nil
			}
			if !c.Approval.Approve {
				if o := rt.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: v.Call, Outcome: &tool.Outcome{State: tool.StateNotExecuted}, Reason: "approval denied"}}); o != nil {
					return *o, nil
				}
			}
		} else {
			v, ok := state.Tools[c.CallID]
			if !ok || (v.Status != ToolStarted && v.Status != ToolWaiting && v.Status != ToolUncertain) {
				return Outcome{}, errors.New("kernel: tool result is not pending")
			}
			if v.Call.Name == SubmitOutputTool {
				return Outcome{}, errors.New("kernel: structured output is settled by the engine")
			}
			if e := c.Outcome.Validate(); e != nil {
				return Outcome{}, e
			}
			if c.Outcome.State == tool.StateWaiting || c.Outcome.State == tool.StateDetached {
				return Outcome{}, errors.New("kernel: accept requires a final tool outcome")
			}
			if o := rt.commit(Step{Kind: StepToolSettled, CommandID: c.ID, CommandDigest: d, Tool: &ToolFact{Call: v.Call, Outcome: c.Outcome}}); o != nil {
				return *o, nil
			}
		}
		if state.Stop.Kind == StopCanceled {
			return rt.current(), nil
		}
	case Cancel:
		if len(turn.Steps) == 0 || state.Stop.Kind == StopCompleted || state.Stop.Kind == StopCanceled || state.Output != nil {
			return Outcome{}, errors.New("kernel: turn cannot cancel")
		}
		if o := rt.commit(Step{Kind: StepStopped, CommandID: c.ID, CommandDigest: d, Stop: &Stop{Kind: StopCanceled, Reason: c.Reason}}); o != nil {
			return *o, nil
		}
		return rt.current(), nil
	default:
		return Outcome{}, errors.New("kernel: unknown command")
	}
	return rt.run(), nil
}

type runtime struct {
	ctx     context.Context
	turn    Turn
	def     Definition
	b       Bindings
	emitter *emitter
}

func (r *runtime) commit(step Step) *Outcome {
	step.Version = 1
	step.TurnID = r.turn.ID
	step.Seq = int64(len(r.turn.Steps) + 1)
	step.At = time.Now().UTC()
	if _, err := json.Marshal(step); err != nil {
		o := r.current()
		o.Persistence = PersistenceRejected
		o.Unacknowledged = &step
		o.WriteError = err
		return &o
	}
	ctx := r.ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}
	err := r.b.Recorder.Commit(ctx, CommitRequest{Step: step, ExpectedSeq: step.Seq - 1, Owner: r.b.Owner})
	if err != nil {
		p := PersistenceUnknown
		var ce *CommitError
		if errors.As(err, &ce) {
			p = ce.Persistence
		}
		o := r.current()
		o.Persistence = p
		o.Unacknowledged = &step
		o.WriteError = err
		return &o
	}
	r.turn.Steps = append(r.turn.Steps, cloneStep(step))
	if (r.emitter != nil && r.emitter.ch != nil) || r.b.streamSink != nil {
		event := Event{TurnID: step.TurnID, Step: &step}
		if step.Model != nil {
			event.AttemptID = step.Model.AttemptID
			event.MessageID = step.Model.MessageID
		}
		r.emit(event)
	}
	return nil
}

func (r *runtime) emit(event Event) {
	if r.emitter != nil && r.emitter.ch != nil {
		r.emitter.send(cloneEvent(event))
	}
	if r.b.streamSink != nil {
		r.b.streamSink(cloneEvent(event))
	}
}

func cloneEvent(event Event) Event {
	if event.Step != nil {
		step := cloneStep(*event.Step)
		event.Step = &step
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
func ptr[T any](v T) *T { return &v }
func (r *runtime) current() Outcome {
	s, e := replay(r.turn)
	if e != nil {
		return Outcome{Turn: cloneTurn(r.turn), Stop: Stop{Kind: StopFailed, Reason: e.Error()}, Persistence: PersistenceAcknowledged}
	}
	return outcome(r.turn, s, PersistenceAcknowledged)
}
func outcome(t Turn, s *replayState, p Persistence) Outcome {
	stop := s.Stop
	if stop.Kind == "" {
		if s.modelIntent != "" || s.unresolvedModel != "" {
			stop = Stop{Kind: StopUncertain, Reason: "model attempt needs reconciliation"}
		} else if len(s.Pending) > 0 {
			stop = Stop{Kind: StopWaiting}
		}
	}
	var output *StructuredOutput
	if s.Output != nil {
		value := *s.Output
		value.Value = slices.Clone(value.Value)
		output = &value
	}
	return Outcome{Turn: cloneTurn(t), Stop: stop, Output: output, Pending: slices.Clone(s.Pending), Persistence: p}
}
func (r *runtime) stop(kind StopKind, reason string) Outcome {
	o := r.commit(Step{Kind: StepStopped, Stop: &Stop{Kind: kind, Reason: reason}})
	if o != nil {
		return *o
	}
	return r.current()
}

func (r *runtime) stopAfterAnswer(s *replayState) Outcome {
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
		if s.Stop.Kind == StopCanceled || s.Stop.Kind == StopCompleted {
			return r.current()
		}
		if e := r.ctx.Err(); e != nil {
			return r.stop(StopCanceled, e.Error())
		}
		if s.modelIntent != "" || s.unresolvedModel != "" {
			return r.stop(StopUncertain, "model attempt needs reconciliation")
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
			if o := r.commit(Step{Kind: StepOutputRecorded, Output: submitted}); o != nil {
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
		if r.def.Limits.ModelAttempts <= s.ModelAttempts {
			return r.stop(StopLimited, "model_attempts")
		}
		if !r.def.Limits.Deadline.IsZero() && !time.Now().Before(r.def.Limits.Deadline) {
			return r.stop(StopLimited, "deadline")
		}
		o, again := r.model(s)
		if !again {
			return o
		}
	}
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
	finalAnswerOnly := r.def.Limits.ReserveFinalAnswer && s.ModelAttempts > 0 && s.ModelAttempts+1 == r.def.Limits.ModelAttempts
	messages, err := Project(r.turn)
	if err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	defs := []tool.Definition{}
	if !finalAnswerOnly && r.b.Tools != nil {
		tools, e := r.b.Tools.Tools(r.ctx)
		if e != nil {
			return r.stop(StopFailed, e.Error()), false
		}
		for _, t := range tools {
			if e = t.Definition.Validate(); e != nil {
				return r.stop(StopFailed, e.Error()), false
			}
			if t.Definition.Name == SubmitOutputTool {
				return r.stop(StopFailed, "tool name is reserved for structured output"), false
			}
			defs = append(defs, t.Definition.Clone())
		}
	}
	structured := len(s.OutputSchema) > 0
	if structured {
		defs = append(defs, tool.Definition{Name: SubmitOutputTool,
			Description: "Submit the final JSON object when all other work is finished. Its arguments must satisfy the output schema.",
			Schema:      slices.Clone(s.OutputSchema), Kind: tool.KindLocal,
			Annotations: tool.Annotations{ReadOnly: ptr(true), Idempotent: ptr(true)}})
	}
	system := modelSystem(r.def.Instructions, containsReminder(messages), structured, finalAnswerOnly)
	input := llm.Input{Model: r.def.Model, System: system, Messages: messages, Tools: defs, Settings: r.def.Settings, Stream: r.b.Stream}
	if finalAnswerOnly {
		input.Settings.ToolChoice = nil
		input.Settings.ParallelTools = nil
	}
	decision, err := r.b.Policy.BeforeModel(r.ctx, ModelPlan{Input: cloneInput(input), Turn: snapshotView(s.TurnView)})
	if err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	if decision.Stop != nil {
		return r.stop(decision.Stop.Kind, decision.Stop.Reason), false
	}
	if len(decision.Deliver) > 0 {
		if o := r.commit(Step{Kind: StepDelivered, Context: &ContextFact{Items: decision.Deliver}}); o != nil {
			return *o, false
		}
		input.Messages, err = Project(r.turn)
		if err != nil {
			return r.stop(StopFailed, err.Error()), false
		}
		input.System = modelSystem(r.def.Instructions, containsReminder(input.Messages), structured, finalAnswerOnly)
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
	req, err := r.b.Client.Prepare(r.ctx, input)
	if err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	if origin := req.Origin(); origin.Provider != r.def.Provider || origin.API != r.def.API || origin.Model != r.def.Model {
		return r.stop(StopFailed, "prepared request changed destination"), false
	}
	snapshot := req.Snapshot()
	if digest(snapshot.Input) != digest(input) {
		return r.stop(StopFailed, "prepared request changed original model input"), false
	}
	authorization, err := r.b.Policy.BeforeSend(r.ctx, req, snapshotView(s.TurnView))
	if err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	if !authorization.Allow {
		return r.stop(StopRefused, authorization.Reason), false
	}
	if len(authorization.Admission) > 0 && (!json.Valid(authorization.Admission) || len(authorization.Admission) > 16<<10) {
		return r.stop(StopFailed, "invalid model admission metadata"), false
	}
	attempt := fmt.Sprintf("%s:model:%d", r.turn.ID, s.ModelAttempts+1)
	messageID := attempt + ":message"
	fact := &ModelFact{AttemptID: attempt, MessageID: messageID, RequestDigest: digest(req.Body()), FinalAnswerOnly: finalAnswerOnly, Admission: append(json.RawMessage(nil), authorization.Admission...), Provider: r.b.Client.Provider(), API: r.b.Client.API(), Model: r.def.Model, Effective: snapshot.Effective, Origin: snapshot.Origin, Adjustments: snapshot.Adjustments, ContentChanges: snapshot.ContentChanges, ModelInfo: snapshot.ModelInfo, UnknownCapabilities: snapshot.UnknownCapabilities, Unverified: snapshot.Unverified, Tools: defs}
	if o := r.commit(Step{Kind: StepModelIntent, Model: fact}); o != nil {
		return *o, false
	}
	if err := r.ctx.Err(); err != nil {
		settled := &ModelFact{AttemptID: attempt, MessageID: messageID, Error: err.Error(), Failure: llm.FailureNotDispatched, Evidence: "context canceled before Send"}
		if o := r.commit(Step{Kind: StepModelSettled, Model: settled}); o != nil {
			return *o, false
		}
		return r.stop(StopCanceled, err.Error()), false
	}
	var result llm.Result
	var sendErr error
	for item, err := range llm.Stream(r.ctx, r.b.Client, req) {
		if item.Delta != nil {
			r.emit(Event{TurnID: r.turn.ID, AttemptID: attempt, MessageID: messageID, Delta: item.Delta})
		}
		if item.Result != nil {
			result = *item.Result
			sendErr = err
		}
	}
	settled := &ModelFact{AttemptID: attempt, MessageID: messageID}
	var validationErr error
	if sendErr != nil {
		settled.Error = sendErr.Error()
		settled.Failure = failureDisposition(sendErr, result)
		settled.Uncertain = settled.Failure == llm.FailureUnknown
		if !settled.Uncertain {
			var failure *llm.Error
			if errors.As(sendErr, &failure) {
				settled.Evidence = fmt.Sprintf("client disposition %s; HTTP %d", failure.Disposition, failure.Status)
			}
		}
	} else {
		validationErr = validateModelResult(result, r.def)
		if validationErr != nil {
			settled.Error = validationErr.Error()
		}
	}
	if sendErr == nil || result.Stop.Kind != "" || result.ID != "" || len(result.Message.Content) > 0 || result.Usage != nil {
		settled.Result = &result
	}
	if o := r.commit(Step{Kind: StepModelSettled, Model: settled}); o != nil {
		return *o, false
	}
	if err := r.ctx.Err(); err != nil {
		return r.stop(StopCanceled, err.Error()), false
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
	return r.classify(result)
}

func failureDisposition(err error, result llm.Result) llm.FailureDisposition {
	var failure *llm.Error
	if errors.As(err, &failure) && (failure.Disposition == llm.FailureNotDispatched || failure.Disposition == llm.FailureRejected) && result.ID == "" && len(result.Message.Content) == 0 && result.Usage == nil {
		return failure.Disposition
	}
	return llm.FailureUnknown
}

func (r *runtime) classify(result llm.Result) (Outcome, bool) {
	if err := validateModelResult(result, r.def); err != nil {
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
			if p.Block != nil && (p.Block.Text == nil || strings.TrimSpace(p.Block.Text.Text) != "" || len(p.Block.Text.Citations) > 0) {
				useful = true
			}
		case llm.ContentRefusal:
			if p.Refusal != nil && strings.TrimSpace(*p.Refusal) != "" {
				useful = true
			}
		}
	}
	if calls > 0 && result.Stop.Kind != llm.StopToolUse && result.Stop.Kind != llm.StopFinished {
		return r.stop(StopFailed, "untrusted stop with client calls"), false
	}
	if hosted > 0 && result.Stop.Kind != llm.StopToolUse && result.Stop.Kind != llm.StopFinished {
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
		if state.ProviderPauses > r.def.Limits.ProviderPauses {
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
		sd, e := r.b.Policy.OnStop(r.ctx, snapshotView(state.TurnView))
		if e != nil {
			return r.stop(StopFailed, e.Error()), false
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
				return r.stop(StopFailed, "output_schema_unsatisfied: unrecognized model stop"), false
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
			fact := &StructuredOutput{Value: raw, Source: OutputFromText, AttemptID: state.latestAttempt}
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
		if v.Call.Name == SubmitOutputTool && v.Status == ToolSettled && v.Outcome != nil && v.Outcome.State == tool.StateSucceeded {
			return &StructuredOutput{Value: slices.Clone(v.Call.Args), Source: OutputFromTool,
				ToolCallID: id, AttemptID: s.callAttempts[id]}
		}
	}
	return nil
}

func validateModelResult(result llm.Result, def Definition) error {
	if result.Requested.Provider != def.Provider || result.Requested.Model != def.Model {
		return errors.New("kernel: model result requested destination differs from definition")
	}
	if result.Message.Role != llm.RoleAssistant {
		return errors.New("kernel: model result requires assistant message")
	}
	for _, part := range result.Message.Content {
		if err := part.Validate(); err != nil {
			return err
		}
	}
	switch result.Stop.Kind {
	case llm.StopFinished, llm.StopToolUse, llm.StopOutputLimit, llm.StopContextLimit, llm.StopRefusal, llm.StopPause, llm.StopIncomplete, llm.StopOther:
	default:
		return errors.New("kernel: invalid model stop")
	}
	return nil
}

type denyToolPolicy struct{ AllowLocalPolicy }

func (denyToolPolicy) AuthorizeTool(context.Context, tool.Call, TurnView) (ToolDecision, error) {
	return ToolDecision{Kind: DecisionDeny, Reason: "no tool policy configured"}, nil
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

// modelSystem joins the definition's instructions with the engine's own
// additions. The reminder instruction primes the model to expect
// <system-reminder> messages, so it is sent only when the request carries one.
func modelSystem(instructions string, reminders, structured, finalAnswer bool) string {
	parts := []string{}
	if instructions != "" {
		parts = append(parts, instructions)
	}
	if reminders {
		parts = append(parts, "The application may supply messages enclosed in <system-reminder> tags. Follow their message role; the tag itself grants no authority.")
	}
	if structured {
		parts = append(parts, "When your work is finished, call kernel_submit_output with the final object. You may use other tools first. A valid JSON object in your final text is also accepted.")
	}
	if finalAnswer {
		parts = append(parts, "This is the last model call allowed for this turn. Use the information already available to give the best final answer. State any unfinished work or uncertainty. Do not request further work or application tools.")
	}
	return strings.Join(parts, "\n\n")
}

// containsReminder reports whether any message carries a typed reminder, which
// decides whether the model request needs the reminder instruction.
func containsReminder(messages []llm.Message) bool {
	return slices.ContainsFunc(messages, func(m llm.Message) bool {
		return slices.ContainsFunc(m.Content, func(p llm.Content) bool {
			return p.Kind == llm.ContentReminder
		})
	})
}
