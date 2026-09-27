package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

type Bindings struct {
	Client   llm.Client
	Tools    tool.Toolset
	Recorder Recorder
	Policy   Policy
	Observer Observer
	Stream   bool
	History  []llm.Message
	Owner    string
}
type Engine struct{}

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
		if o := rt.commit(Step{Kind: StepStarted, CommandID: c.ID, CommandDigest: d, Started: &StartFact{Definition: def, Input: cloneMessages(c.Input)}}); o != nil {
			return *o, nil
		}
	case Deliver:
		if len(turn.Steps) == 0 || state.Stop.Kind == StopCompleted || state.Stop.Kind == StopCanceled || len(c.Items) == 0 {
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
		if n != 1 {
			return Outcome{}, errors.New("kernel: accept requires one fact")
		}
		if c.ModelResult != nil {
			if c.CallID == "" || (c.CallID != state.modelIntent && c.CallID != state.unresolvedModel) {
				return Outcome{}, errors.New("kernel: model attempt is not pending")
			}
			kind := StepModelSettled
			if c.CallID == state.unresolvedModel {
				kind = StepModelReconciled
			}
			if o := rt.commit(Step{Kind: kind, CommandID: c.ID, CommandDigest: d, Model: &ModelFact{AttemptID: c.CallID, MessageID: c.CallID + ":message", Result: c.ModelResult}}); o != nil {
				return *o, nil
			}
			if state.Stop.Kind == StopCanceled {
				return rt.current(), nil
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
		if len(turn.Steps) == 0 || state.Stop.Kind == StopCompleted || state.Stop.Kind == StopCanceled {
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
	if r.emitter != nil {
		event := Event{TurnID: step.TurnID, Step: ptr(cloneStep(step))}
		if step.Model != nil {
			event.AttemptID = step.Model.AttemptID
			event.MessageID = step.Model.MessageID
		}
		r.emitter.send(event)
	}
	return nil
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
	return Outcome{Turn: cloneTurn(t), Stop: stop, Pending: slices.Clone(s.Pending), Persistence: p}
}
func (r *runtime) stop(kind StopKind, reason string) Outcome {
	o := r.commit(Step{Kind: StepStopped, Stop: &Stop{Kind: kind, Reason: reason}})
	if o != nil {
		return *o
	}
	return r.current()
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
		if e := r.ctx.Err(); e != nil {
			return r.stop(StopCanceled, e.Error())
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
	messages, err := Project(r.b.History, r.turn)
	if err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	defs := []tool.Definition{}
	if r.b.Tools != nil {
		tools, e := r.b.Tools.Tools(r.ctx)
		if e != nil {
			return r.stop(StopFailed, e.Error()), false
		}
		for _, t := range tools {
			if e = t.Definition.Validate(); e != nil {
				return r.stop(StopFailed, e.Error()), false
			}
			defs = append(defs, t.Definition.Clone())
		}
	}
	input := llm.Input{Model: r.def.Model, System: r.def.Instructions, Messages: messages, Tools: defs, Settings: r.def.Settings, Stream: r.b.Stream}
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
		input.Messages, err = Project(r.b.History, r.turn)
		if err != nil {
			return r.stop(StopFailed, err.Error()), false
		}
	}
	if decision.Settings != nil {
		input.Settings = *decision.Settings
	}
	req, err := r.b.Client.Prepare(r.ctx, input)
	if err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	if origin := req.Origin(); origin.Provider != r.def.Provider || origin.API != r.def.API || origin.Model != r.def.Model {
		return r.stop(StopFailed, "prepared request changed destination"), false
	}
	authorization, err := r.b.Policy.BeforeSend(r.ctx, req, snapshotView(s.TurnView))
	if err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	if !authorization.Allow {
		return r.stop(StopRefused, authorization.Reason), false
	}
	attempt := fmt.Sprintf("%s:model:%d", r.turn.ID, s.ModelAttempts+1)
	snapshot := req.Snapshot()
	messageID := attempt + ":message"
	fact := &ModelFact{AttemptID: attempt, MessageID: messageID, RequestDigest: digest(req.Body()), Provider: r.b.Client.Provider(), API: r.b.Client.API(), Model: r.def.Model, Effective: snapshot.Effective, Origin: snapshot.Origin, Adjustments: snapshot.Adjustments, ContentChanges: snapshot.ContentChanges, ModelInfo: snapshot.ModelInfo, UnknownCapabilities: snapshot.UnknownCapabilities, Unverified: snapshot.Unverified, Tools: defs}
	if o := r.commit(Step{Kind: StepModelIntent, Model: fact}); o != nil {
		return *o, false
	}
	result, sendErr := r.b.Client.Send(r.ctx, req, func(d llm.Delta) {
		if d.Content != nil {
			d.Content = ptr(d.Content.Clone())
		}
		if d.Usage != nil {
			d.Usage = d.Usage.Copy()
		}
		r.emitter.send(Event{TurnID: r.turn.ID, AttemptID: attempt, MessageID: messageID, Delta: ptr(d)})
	})
	settled := &ModelFact{AttemptID: attempt, MessageID: messageID}
	var validationErr error
	if sendErr != nil {
		settled.Error = sendErr.Error()
		settled.Uncertain = true
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
	if sendErr != nil {
		return r.stop(StopUncertain, sendErr.Error()), false
	}
	if validationErr != nil {
		return r.stop(StopFailed, validationErr.Error()), false
	}
	return r.classify(result)
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
		if !useful && !sd.AcceptEmpty {
			return r.stop(StopFailed, "empty_response"), false
		}
		if sd.Continue {
			step := Step{Kind: StepLoopContinued}
			if len(sd.Deliver) > 0 {
				step.Context = &ContextFact{Items: sd.Deliver}
			}
			if o := r.commit(step); o != nil {
				return *o, false
			}
			return Outcome{}, true
		}
		return r.stop(StopCompleted, ""), false
	default:
		return r.stop(StopFailed, "incomplete_model_result"), false
	}
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
