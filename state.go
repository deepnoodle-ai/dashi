package kernel

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

type ToolStatus string

const (
	ToolRequested       ToolStatus = "requested"
	ToolApprovalPending ToolStatus = "approval_pending"
	ToolDenied          ToolStatus = "denied"
	ToolAllowed         ToolStatus = "allowed"
	ToolStarted         ToolStatus = "started"
	ToolWaiting         ToolStatus = "waiting"
	ToolUncertain       ToolStatus = "uncertain"
	ToolSettled         ToolStatus = "settled"
)

type ToolView struct {
	Call       tool.Call
	Definition tool.Definition
	Status     ToolStatus
	Decision   ToolDecision
	Outcome    *tool.Outcome
}
type TurnView struct {
	ID             string
	Definition     Definition
	Revision       int64
	ModelAttempts  int
	ProviderPauses int
	Usage          *llm.Usage
	Stop           Stop
	Pending        []string
	Tools          map[string]ToolView
}
type replayState struct {
	TurnView
	modelIntent            string
	unresolvedModel        string
	modelUsages            map[string]*llm.Usage
	modelDefs              map[string]tool.Definition
	callOrder              []string
	hosted                 map[string]llm.HostedApprovalRequest
	commandDigests         map[string]string
	latestResult           *llm.Result
	awaitingClassification bool
}

// Replay verifies a turn's versioned prefix and reconstructs its current view.
// It does not execute any recorded intent.
func Replay(t Turn) (TurnView, error) {
	s, e := replay(t)
	if e != nil {
		return TurnView{}, e
	}
	return snapshotView(s.TurnView), nil
}
func snapshotView(v TurnView) TurnView {
	b, _ := json.Marshal(v)
	var out TurnView
	_ = json.Unmarshal(b, &out)
	return out
}
func replay(t Turn) (*replayState, error) {
	s := &replayState{TurnView: TurnView{ID: t.ID, Tools: map[string]ToolView{}}, modelDefs: map[string]tool.Definition{}, hosted: map[string]llm.HostedApprovalRequest{}, commandDigests: map[string]string{}, modelUsages: map[string]*llm.Usage{}}
	if t.ID == "" {
		return nil, fmt.Errorf("%w: empty ID", ErrInvalidTurn)
	}
	for i, step := range t.Steps {
		if _, err := json.Marshal(step); err != nil {
			return nil, fmt.Errorf("%w: step %d cannot serialize: %v", ErrInvalidTurn, i+1, err)
		}
		if step.Version != 1 || step.TurnID != t.ID || step.Seq != int64(i+1) {
			return nil, fmt.Errorf("%w: invalid step header at %d", ErrInvalidTurn, i+1)
		}
		if err := validateStepPayload(step); err != nil {
			return nil, fmt.Errorf("%w: step %d: %v", ErrInvalidTurn, i+1, err)
		}
		if step.CommandID != "" {
			if step.CommandDigest == "" {
				return nil, fmt.Errorf("%w: command digest missing", ErrInvalidTurn)
			}
			if old, ok := s.commandDigests[step.CommandID]; ok && old != step.CommandDigest {
				return nil, ErrCommandConflict
			}
			s.commandDigests[step.CommandID] = step.CommandDigest
		}
		if i == 0 && step.Kind != StepStarted {
			return nil, fmt.Errorf("%w: first step is not start", ErrInvalidTurn)
		}
		if i > 0 && step.Kind == StepStarted {
			return nil, fmt.Errorf("%w: duplicate start", ErrInvalidTurn)
		}
		if err := s.apply(step); err != nil {
			return nil, fmt.Errorf("%w: step %d: %v", ErrInvalidTurn, i+1, err)
		}
		s.Revision = step.Seq
	}
	s.Pending = s.pending()
	for _, usage := range s.modelUsages {
		if usage != nil {
			if s.Usage == nil {
				s.Usage = &llm.Usage{}
			}
			s.Usage.Add(usage)
		}
	}
	return s, nil
}
func (s *replayState) apply(step Step) error {
	switch step.Kind {
	case StepStarted:
		if step.Started == nil {
			return errors.New("missing start")
		}
		if e := step.Started.Definition.Validate(); e != nil {
			return e
		}
		if len(step.Started.Input) == 0 {
			return errors.New("missing start input")
		}
		for _, m := range step.Started.Input {
			if m.Role != llm.RoleUser {
				return errors.New("start input must be user messages")
			}
			if err := validateMessage(m); err != nil {
				return err
			}
		}
		s.Definition = step.Started.Definition
	case StepDelivered:
		if step.Context == nil || len(step.Context.Items) == 0 {
			return errors.New("missing context")
		}
		for _, item := range step.Context.Items {
			if err := validateContext(item); err != nil {
				return err
			}
		}
	case StepContinued:
		if !s.canContinue() {
			return errors.New("turn cannot continue")
		}
		s.Stop = Stop{}
	case StepLoopContinued:
		if !s.awaitingClassification || len(s.pending()) > 0 {
			return errors.New("loop continuation without final candidate")
		}
		s.awaitingClassification = false
		if step.Context != nil && len(step.Context.Items) == 0 {
			return errors.New("empty continuation context")
		}
		if step.Context != nil {
			for _, item := range step.Context.Items {
				if err := validateContext(item); err != nil {
					return err
				}
			}
		}
	case StepModelIntent:
		if step.Model == nil || step.Model.AttemptID == "" || step.Model.RequestDigest == "" || s.modelIntent != "" || len(s.pending()) > 0 {
			return errors.New("invalid model intent")
		}
		s.modelIntent = step.Model.AttemptID
		s.awaitingClassification = false
		s.ModelAttempts++
		s.modelDefs = map[string]tool.Definition{}
		for _, d := range step.Model.Tools {
			if _, ok := s.modelDefs[d.Name]; ok {
				return errors.New("duplicate tool definition")
			}
			s.modelDefs[d.Name] = d
		}
	case StepModelSettled:
		if step.Model == nil || step.Model.AttemptID == "" || step.Model.AttemptID != s.modelIntent {
			return errors.New("model settlement without intent")
		}
		if step.Model.Result == nil && step.Model.Error == "" {
			return errors.New("missing model outcome")
		}
		s.modelIntent = ""
		if step.Model.Uncertain {
			s.unresolvedModel = step.Model.AttemptID
		}
		if step.Model.Result != nil {
			if err := s.recordResult(step.Model.AttemptID, step.Model.Result, step.Model.Error == "" && s.Stop.Kind != StopCanceled); err != nil {
				return err
			}
		}
		if step.Model.Error == "" {
			s.awaitingClassification = true
		}
	case StepModelReconciled:
		if step.Model == nil || step.Model.AttemptID != s.unresolvedModel || step.Model.Result == nil || step.Model.Error != "" {
			return errors.New("model reconciliation without uncertain attempt")
		}
		s.unresolvedModel = ""
		if err := s.recordResult(step.Model.AttemptID, step.Model.Result, s.Stop.Kind != StopCanceled); err != nil {
			return err
		}
		s.awaitingClassification = true
	case StepToolDecision:
		s.awaitingClassification = false
		if step.Tool == nil {
			return errors.New("missing tool decision")
		}
		v, ok := s.Tools[step.Tool.Call.ID]
		if !ok || v.Status != ToolRequested {
			return errors.New("tool decision without request")
		}
		v.Decision = step.Tool.Decision
		if step.Tool.Call.ID != v.Call.ID || step.Tool.Call.Name != v.Call.Name {
			return errors.New("tool decision changes identity")
		}
		v.Call = step.Tool.Call.Clone()
		switch v.Decision.Kind {
		case DecisionAllow:
			v.Status = ToolAllowed
		case DecisionDefer:
			v.Status = ToolApprovalPending
		case DecisionDeny:
			v.Status = ToolDenied
		default:
			return errors.New("invalid tool decision")
		}
		s.Tools[v.Call.ID] = v
	case StepToolStarted:
		s.awaitingClassification = false
		if step.Tool == nil {
			return errors.New("missing tool start")
		}
		v, ok := s.Tools[step.Tool.Call.ID]
		if !ok || v.Status != ToolAllowed {
			return errors.New("tool start without allowance")
		}
		v.Status = ToolStarted
		s.Tools[v.Call.ID] = v
	case StepToolWaiting:
		s.awaitingClassification = false
		if step.Tool == nil || step.Tool.Outcome == nil {
			return errors.New("missing tool wait")
		}
		v, ok := s.Tools[step.Tool.Call.ID]
		if !ok || (v.Status != ToolAllowed && v.Status != ToolStarted) {
			return errors.New("tool wait without allowance")
		}
		if step.Tool.Outcome.State != tool.StateWaiting && step.Tool.Outcome.State != tool.StateDetached {
			return errors.New("invalid wait outcome")
		}
		v.Status = ToolWaiting
		v.Outcome = step.Tool.Outcome
		s.Tools[v.Call.ID] = v
	case StepToolSettled:
		s.awaitingClassification = false
		if step.Tool == nil {
			return errors.New("missing tool settlement")
		}
		v, ok := s.Tools[step.Tool.Call.ID]
		if !ok || v.Status == ToolSettled {
			return errors.New("tool settlement without pending call")
		}
		if step.Tool.Outcome == nil {
			if v.Status != ToolStarted && v.Status != ToolWaiting {
				return errors.New("uncertainty without start")
			}
			v.Status = ToolUncertain
		} else {
			if e := step.Tool.Outcome.Validate(); e != nil {
				return e
			}
			if step.Tool.Outcome.State == tool.StateWaiting || step.Tool.Outcome.State == tool.StateDetached {
				return errors.New("wait is not settlement")
			}
			v.Status = ToolSettled
			v.Outcome = step.Tool.Outcome
		}
		s.Tools[v.Call.ID] = v
	case StepApproval:
		s.awaitingClassification = false
		if step.Approval == nil {
			return errors.New("missing approval")
		}
		v, ok := s.Tools[step.Approval.CallID]
		if !ok || v.Status != ToolApprovalPending {
			return errors.New("approval without pending decision")
		}
		if digest(v.Call.Args) != step.Approval.ArgsDigest || v.Decision.Revision != step.Approval.PolicyRevision {
			return errors.New("stale approval")
		}
		if step.Approval.Approve {
			v.Status = ToolAllowed
		} else {
			v.Status = ToolDenied
		}
		s.Tools[v.Call.ID] = v
	case StepHostedApproval:
		s.awaitingClassification = false
		if step.HostedApproval == nil {
			return errors.New("missing hosted approval")
		}
		a, ok := s.hosted[step.HostedApproval.RequestID]
		if !ok || a.Origin != step.HostedApproval.Origin {
			return errors.New("hosted approval without matching request")
		}
		delete(s.hosted, a.ID)
	case StepStopped:
		s.awaitingClassification = false
		if step.Stop == nil {
			return errors.New("missing stop")
		}
		s.Stop = *step.Stop
	default:
		return fmt.Errorf("unknown step kind %q", step.Kind)
	}
	return nil
}
func (s *replayState) pending() []string {
	var out []string
	if s.modelIntent != "" {
		out = append(out, s.modelIntent)
	}
	if s.unresolvedModel != "" {
		out = append(out, s.unresolvedModel)
	}
	for _, id := range s.callOrder {
		if s.Tools[id].Status != ToolSettled {
			out = append(out, id)
		}
	}
	for id := range s.hosted {
		out = append(out, id)
	}
	return out
}

func (s *replayState) recordResult(attempt string, result *llm.Result, actionable bool) error {
	s.latestResult = result
	if result.Usage != nil {
		s.modelUsages[attempt] = result.Usage.Copy()
	}
	if !actionable {
		return nil
	}
	if err := validateModelResult(*result, s.Definition); err != nil {
		return err
	}
	if result.Stop.Kind == llm.StopPause {
		s.ProviderPauses++
	}
	if result.Stop.Kind == llm.StopToolUse || result.Stop.Kind == llm.StopFinished {
		for _, part := range result.Message.Content {
			if part.Kind != llm.ContentToolCall || part.ToolCall == nil {
				continue
			}
			c := part.ToolCall.Clone()
			if _, ok := s.Tools[c.ID]; ok {
				return errors.New("repeated tool call ID")
			}
			s.Tools[c.ID] = ToolView{Call: c, Definition: s.modelDefs[c.Name], Status: ToolRequested}
			s.callOrder = append(s.callOrder, c.ID)
		}
	}
	for _, part := range result.Message.Content {
		if part.Kind == llm.ContentHostedApprovalRequest && part.HostedApprovalRequest != nil {
			a := part.HostedApprovalRequest.Clone()
			if _, ok := s.hosted[a.ID]; ok {
				return errors.New("repeated hosted approval ID")
			}
			s.hosted[a.ID] = a
		}
	}
	return nil
}

func (s *replayState) canContinue() bool {
	if s.Revision == 0 || s.Stop.Kind == StopCompleted || s.Stop.Kind == StopCanceled || s.modelIntent != "" || s.unresolvedModel != "" || len(s.hosted) > 0 {
		return false
	}
	for _, id := range s.callOrder {
		switch s.Tools[id].Status {
		case ToolSettled, ToolRequested, ToolAllowed, ToolDenied:
		default:
			return false
		}
	}
	return true
}

func validateStepPayload(step Step) error {
	count := 0
	for _, present := range []bool{step.Started != nil, step.Context != nil, step.Model != nil, step.Tool != nil, step.Approval != nil, step.HostedApproval != nil, step.Stop != nil} {
		if present {
			count++
		}
	}
	expected := 1
	switch step.Kind {
	case StepContinued:
		expected = 0
	case StepLoopContinued:
		if step.Context == nil {
			expected = 0
		}
	}
	if count != expected {
		return errors.New("step has unexpected payloads")
	}
	switch step.Kind {
	case StepStarted:
		if step.Started == nil {
			return errors.New("start payload missing")
		}
	case StepDelivered, StepLoopContinued:
		if step.Kind == StepDelivered && step.Context == nil {
			return errors.New("context payload missing")
		}
	case StepContinued:
	case StepModelIntent, StepModelSettled, StepModelReconciled:
		if step.Model == nil {
			return errors.New("model payload missing")
		}
	case StepToolDecision, StepToolStarted, StepToolWaiting, StepToolSettled:
		if step.Tool == nil {
			return errors.New("tool payload missing")
		}
	case StepApproval:
		if step.Approval == nil {
			return errors.New("approval payload missing")
		}
	case StepHostedApproval:
		if step.HostedApproval == nil {
			return errors.New("hosted approval payload missing")
		}
	case StepStopped:
		if step.Stop == nil {
			return errors.New("stop payload missing")
		}
	default:
		return errors.New("unknown step kind")
	}
	return nil
}
func validateMessage(m llm.Message) error {
	switch m.Role {
	case llm.RoleUser, llm.RoleAssistant, llm.RoleSystem, llm.RoleDeveloper:
	default:
		return errors.New("invalid message role")
	}
	if len(m.Content) == 0 {
		return errors.New("empty message")
	}
	for _, part := range m.Content {
		if err := part.Validate(); err != nil {
			return err
		}
	}
	return nil
}
func validateContext(item ContextItem) error {
	switch item.Origin {
	case "user", "operator", "library", "tool":
	default:
		return errors.New("invalid context origin")
	}
	if item.Snapshot && item.Name == "" {
		return errors.New("snapshot context requires a name")
	}
	if item.Origin == "user" && item.Message.Role != llm.RoleUser {
		return errors.New("user context cannot claim higher authority")
	}
	return validateMessage(item.Message)
}
