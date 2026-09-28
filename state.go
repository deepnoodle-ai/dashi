package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

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
	// ArgsDigest is set while Status is ToolApprovalPending. It identifies
	// Call.Args by JSON meaning; echo it in ApprovalFact.ArgsDigest to approve
	// or deny exactly the arguments shown.
	ArgsDigest string
}

// TurnView is the current state reconstructed from acknowledged steps.
type TurnView struct {
	ID           string
	Definition   Definition
	OutputSchema json.RawMessage
	Revision     int64
	// ModelAttempts counts settled calls that were not interrupted.
	ModelAttempts int
	// Interruptions counts resumes after a context stop or worker crash.
	Interruptions  int
	ProviderPauses int
	// Budget is the current cap, including any recorded raises.
	Budget *Budget
	// Raises preserves the authorized cap changes in order.
	Raises []BudgetRaise
	// Spend is rebuilt from committed model and tool facts.
	Spend   Spend
	Stop    Stop
	Output  *StructuredOutput
	Pending []string
	// Warnings includes nonfatal conditions from acknowledged steps.
	Warnings []Warning
	// ReadyToContinue means Continue can run without first raising a budget.
	// A limited turn with a spent cap can still accept a Continue that raises it.
	ReadyToContinue bool
	Tools           map[string]ToolView
}
type replayState struct {
	TurnView
	// modelCalls counts every requested call so retried calls get distinct IDs.
	modelCalls             int
	modelIntent            string
	unresolvedModel        string
	unresolvedPartial      bool
	unresolvedInterrupted  bool
	modelIntents           map[string]*ModelFact
	modelSettlements       map[string]*ModelFact
	modelMessages          map[string]string
	callAttempts           map[string]string
	latestAttempt          string
	finalAnswerOnly        bool
	modelDefs              map[string]tool.Definition
	callOrder              []string
	hosted                 map[string]llm.HostedApprovalRequest
	commandDigests         map[string]string
	latestResult           *llm.Result
	awaitingClassification bool
	failedModel            string
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
	s := &replayState{
		TurnView: TurnView{
			ID:    t.ID,
			Tools: map[string]ToolView{},
		},
		modelDefs:        map[string]tool.Definition{},
		hosted:           map[string]llm.HostedApprovalRequest{},
		commandDigests:   map[string]string{},
		modelIntents:     map[string]*ModelFact{},
		modelSettlements: map[string]*ModelFact{},
		modelMessages:    map[string]string{},
		callAttempts:     map[string]string{},
	}
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
		s.Warnings = append(s.Warnings, step.Warnings...)
		s.Revision = step.Seq
	}
	// Derive ArgsDigest from the final status, so no transition out of
	// approval can leave a stale digest behind.
	for id, v := range s.Tools {
		v.ArgsDigest = ""
		if v.Status == ToolApprovalPending {
			v.ArgsDigest = ArgsDigest(v.Call.Args)
		}
		s.Tools[id] = v
	}
	s.Pending = s.pending()
	if err := s.calculateSpend(); err != nil {
		return nil, fmt.Errorf("%w: calculate spend: %v", ErrInvalidTurn, err)
	}
	s.ReadyToContinue = s.canContinue()
	return s, nil
}

func (s *replayState) calculateSpend() error {
	s.Spend = Spend{
		Cost:     map[string]float64{},
		ToolCost: map[string]float64{},
	}
	for _, id := range slices.Sorted(maps.Keys(s.modelIntents)) {
		usage, charge, estimated, counted := attemptSpend(
			s.modelIntents[id], s.modelSettlements[id],
		)
		if !counted {
			continue
		}
		if usage != nil {
			if err := s.Spend.Usage.Add(usage); err != nil {
				return err
			}
		}
		if estimated {
			s.Spend.Estimated++
		}
		if charge == nil {
			s.Spend.CostUnknown++
			continue
		}
		if err := addCurrencyCost(s.Spend.Cost, charge.Currency, charge.Total); err != nil {
			return err
		}
		if s.Budget != nil && s.Budget.MaxCost != nil &&
			charge.Currency != s.Budget.MaxCost.Currency {
			s.Spend.CostUnknown++
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.Tools)) {
		view := s.Tools[id]
		if view.Status == ToolSettled && view.Outcome != nil &&
			view.Outcome.Cost != nil {
			cost := view.Outcome.Cost
			if err := addCurrencyCost(s.Spend.ToolCost, cost.Currency, cost.Amount); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *replayState) apply(step Step) error {
	if s.Stop.Kind == StopCompleted {
		return errors.New("completed turn cannot change")
	}
	if s.Stop.Kind == StopCanceled {
		switch step.Kind {
		case StepModelSettled, StepModelReconciled, StepToolSettled:
		default:
			return errors.New("canceled turn only accepts late results")
		}
	}
	// Once validated output is recorded, execution has no more work. A
	// context stop may still intervene before the completion stop is saved;
	// Continue then completes the existing output without another model call.
	if s.Output != nil {
		switch {
		case step.Kind == StepContinued:
		case step.Kind == StepStopped && step.Stop != nil && step.Stop.Kind == StopCompleted:
		case step.Kind == StepStopped && step.Stop != nil &&
			step.Stop.Kind == StopInterrupted:
		case step.Kind == StepStopped && step.Stop != nil &&
			step.Stop.Kind == StopLimited &&
			(step.Stop.Reason == "deadline" ||
				step.Stop.Reason == "interruptions" ||
				step.Stop.Reason == "model_attempts" && s.finalAnswerOnly ||
				step.Stop.Limit != nil &&
					(step.Stop.Limit.Kind == LimitOutputTokens ||
						step.Stop.Limit.Kind == LimitTotalTokens ||
						step.Stop.Limit.Kind == LimitCost)):
		default:
			return errors.New("turn with recorded output can only complete")
		}
	}
	switch step.Kind {
	case StepStarted:
		if step.Started == nil {
			return errors.New("missing start")
		}
		if e := step.Started.Definition.Validate(); e != nil {
			return e
		}
		outputSchema := step.Started.OutputSchema
		if len(outputSchema) == 0 {
			outputSchema = step.Started.Definition.OutputSchema
		}
		if len(outputSchema) > 0 {
			if _, err := compileOutputSchema(outputSchema); err != nil {
				return err
			}
			if err := validateOutputToolChoice(step.Started.Definition.Settings); err != nil {
				return err
			}
		}
		if len(step.Started.Input) == 0 {
			return errors.New("missing start input")
		}
		origin := step.Started.Origin
		if origin == "" {
			origin = StartUser
		}
		if origin != StartUser && origin != StartScheduled && origin != StartOperator {
			return errors.New("invalid start origin")
		}
		for _, m := range step.Started.Input {
			if origin != StartOperator && m.Role != llm.RoleUser {
				return errors.New("user or scheduled start input must use user role")
			}
			if origin == StartOperator && m.Role != llm.RoleUser && m.Role != llm.RoleSystem && m.Role != llm.RoleDeveloper {
				return errors.New("invalid operator start role")
			}
			if err := validateMessage(m); err != nil {
				return err
			}
			if reminder := messageReminder(m); reminder != nil && reminder.Tier == llm.ReminderOperator && origin != StartOperator {
				return errors.New("operator reminder requires operator start origin")
			}
		}
		s.Definition = step.Started.Definition
		if step.Started.Budget != nil {
			if err := step.Started.Budget.Validate(); err != nil {
				return err
			}
			budget := step.Started.Budget.Clone()
			s.Budget = &budget
		}
		s.OutputSchema = append(json.RawMessage(nil), outputSchema...)
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
		if err := s.calculateSpend(); err != nil {
			return err
		}
		if !s.canContinueWithBudget(step.Budget) {
			return errors.New("turn cannot continue")
		}
		if step.Budget != nil {
			if step.At.IsZero() {
				return errors.New("budget raise requires a recorded time")
			}
			old := Budget{}
			if s.Budget != nil {
				old = *s.Budget
			}
			if err := step.Budget.raisedFrom(old); err != nil {
				return err
			}
			budget := step.Budget.Clone()
			s.Budget = &budget
			s.Raises = append(s.Raises, BudgetRaise{
				Budget: budget.Clone(),
				At:     step.At,
			})
		}
		if s.Stop.Kind == StopInterrupted || s.Stop.Kind == "" {
			s.Interruptions++
		}
		if s.Stop.Kind == StopFailed {
			s.failedModel = ""
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
		if step.Model == nil || step.Model.AttemptID == "" || step.Model.MessageID == "" || step.Model.RequestDigest == "" || s.modelIntent != "" || s.Output != nil || len(s.pending()) > 0 {
			return errors.New("invalid model intent")
		}
		if len(step.Model.Admission) > 0 && (!json.Valid(step.Model.Admission) || len(step.Model.Admission) > 16<<10) {
			return errors.New("invalid model admission metadata")
		}
		if step.Model.Provider != s.Definition.Provider || step.Model.API != s.Definition.API || step.Model.Model != s.Definition.Model {
			return errors.New("model intent changed destination")
		}
		if step.Model.EstimatedInputTokens < 0 {
			return errors.New("negative estimated input")
		}
		if step.Model.AtRisk != nil {
			if err := step.Model.AtRisk.Validate(); err != nil {
				return err
			}
		}
		if step.Model.FinalAnswerOnly && (!s.Definition.Limits.ReserveFinalAnswer || s.ModelAttempts == 0 || s.ModelAttempts+1 != s.Definition.Limits.ModelAttempts) {
			return errors.New("final-answer intent is not the last model attempt")
		}
		s.modelIntent = step.Model.AttemptID
		s.modelIntents[step.Model.AttemptID] = step.Model
		s.modelCalls++
		s.finalAnswerOnly = step.Model.FinalAnswerOnly
		s.unresolvedPartial = false
		if _, exists := s.modelMessages[step.Model.AttemptID]; exists {
			return errors.New("repeated model attempt ID")
		}
		s.modelMessages[step.Model.AttemptID] = step.Model.MessageID
		s.awaitingClassification = false
		s.modelDefs = map[string]tool.Definition{}
		outputTool := false
		for _, d := range step.Model.Tools {
			if err := d.Validate(); err != nil {
				return err
			}
			if _, ok := s.modelDefs[d.Name]; ok {
				return errors.New("duplicate tool definition")
			}
			if d.Name == SubmitOutputTool {
				if len(s.OutputSchema) == 0 || d.Kind != tool.KindLocal || !sameJSON(d.Schema, s.OutputSchema) {
					return errors.New("invalid reserved output tool")
				}
				outputTool = true
			} else if step.Model.FinalAnswerOnly {
				return errors.New("final-answer intent offers an application tool")
			}
			s.modelDefs[d.Name] = d
		}
		if len(s.OutputSchema) > 0 && !outputTool {
			return errors.New("output schema requires reserved output tool")
		}
	case StepModelSettled:
		if step.Model == nil || step.Model.AttemptID == "" || step.Model.AttemptID != s.modelIntent || step.Model.MessageID != s.modelMessages[s.modelIntent] {
			return errors.New("model settlement without intent")
		}
		if step.Model.Result == nil && step.Model.Error == "" {
			return errors.New("missing model outcome")
		}
		if err := validateModelAccounting(step.Model); err != nil {
			return err
		}
		if step.Model.Failure != "" && step.Model.Failure != llm.FailureUnknown && step.Model.Failure != llm.FailureRejected && step.Model.Failure != llm.FailureNotDispatched {
			return errors.New("invalid model failure disposition")
		}
		if step.Model.Uncertain && (step.Model.Failure == llm.FailureRejected || step.Model.Failure == llm.FailureNotDispatched) {
			return errors.New("known model failure marked uncertain")
		}
		if step.Model.Interrupted &&
			(step.Model.Failure == llm.FailureRejected ||
				step.Model.Failure == llm.FailureNotDispatched) {
			return errors.New("interrupted model call cannot have known nonexecution")
		}
		if step.Model.Failure == llm.FailureRejected || step.Model.Failure == llm.FailureNotDispatched {
			if step.Model.Error == "" || step.Model.Evidence == "" || (step.Model.Result != nil && (step.Model.Result.ID != "" || len(step.Model.Result.Message.Content) != 0 || step.Model.Result.Usage != nil)) {
				return errors.New("known model failure lacks evidence or has partial result")
			}
		}
		s.modelIntent = ""
		s.modelSettlements[step.Model.AttemptID] = step.Model
		if !step.Model.Interrupted {
			s.ModelAttempts++
		}
		if step.Model.Uncertain {
			s.unresolvedModel = step.Model.AttemptID
			s.unresolvedInterrupted = step.Model.Interrupted
			s.unresolvedPartial = step.Model.Result != nil && (step.Model.Result.ID != "" || len(step.Model.Result.Message.Content) > 0 || step.Model.Result.Usage != nil)
		} else if step.Model.Error != "" && !step.Model.Interrupted &&
			step.Model.Failure != llm.FailureRejected &&
			step.Model.Failure != llm.FailureNotDispatched {
			s.failedModel = step.Model.Error
		}
		if step.Model.Result != nil {
			actionable := step.Model.Error == "" && !step.Model.Interrupted &&
				s.Stop.Kind != StopCanceled
			if err := s.recordResult(
				step.Model.AttemptID, step.Model.Result, actionable,
			); err != nil {
				return err
			}
		}
		if step.Model.Error == "" && !step.Model.Interrupted {
			s.awaitingClassification = true
		}
	case StepModelReconciled:
		if step.Model == nil || step.Model.AttemptID != s.unresolvedModel || step.Model.MessageID != s.modelMessages[s.unresolvedModel] || (step.Model.Result == nil && step.Model.Error == "") {
			return errors.New("model reconciliation without uncertain attempt")
		}
		if err := validateModelAccounting(step.Model); err != nil {
			return err
		}
		s.unresolvedModel = ""
		s.modelSettlements[step.Model.AttemptID] = step.Model
		// Verified nonexecution clears uncertainty; Continue may retry.
		if step.Model.Error != "" &&
			(step.Model.Result != nil || s.unresolvedPartial ||
				(step.Model.Failure != llm.FailureRejected &&
					step.Model.Failure != llm.FailureNotDispatched) ||
				step.Model.Evidence == "") {
			return errors.New("model failure reconciliation lacks evidence")
		}
		if step.Model.Error == "" {
			if err := s.recordResult(step.Model.AttemptID, step.Model.Result, s.Stop.Kind != StopCanceled); err != nil {
				return err
			}
			if s.unresolvedInterrupted {
				s.ModelAttempts++
			}
			s.awaitingClassification = true
		}
		s.unresolvedPartial = false
		s.unresolvedInterrupted = false
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
		if err := step.Tool.Call.Validate(); err != nil {
			return err
		}
		if !sameJSONValue(step.Tool.Call.Args, v.Call.Args) && !sameJSONValue(step.Tool.Call.RequestedArgs, v.Call.Args) {
			return errors.New("tool rewrite lost requested arguments")
		}
		if step.Tool.Definition.Name != "" && !sameJSONValue(step.Tool.Definition, v.Definition) {
			return errors.New("tool decision changed definition")
		}
		if v.Call.Name == SubmitOutputTool && (v.Decision.Kind != DecisionAllow || v.Decision.Revision != "kernel-output-v1" || len(v.Decision.Rewrite) != 0 || len(step.Tool.Call.RequestedArgs) != 0 || !sameJSON(v.Call.Args, step.Tool.Call.Args)) {
			return errors.New("reserved output tool cannot be rewritten or denied")
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
		if !ok || v.Status != ToolAllowed || !sameJSONValue(v.Call, step.Tool.Call) || (step.Tool.Definition.Name != "" && !sameJSONValue(step.Tool.Definition, v.Definition)) {
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
		if !ok || !sameJSONValue(v.Call, step.Tool.Call) || (v.Status != ToolStarted && (v.Status != ToolAllowed || v.Definition.Kind != tool.KindExternal)) {
			return errors.New("tool wait without allowance")
		}
		if step.Tool.Outcome.State != tool.StateWaiting && step.Tool.Outcome.State != tool.StateDetached {
			return errors.New("invalid wait outcome")
		}
		if v.Call.Name == SubmitOutputTool {
			return errors.New("reserved output tool cannot wait for host execution")
		}
		if err := step.Tool.Outcome.Validate(); err != nil {
			return err
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
		if !ok || v.Status == ToolSettled || !sameJSONValue(v.Call, step.Tool.Call) {
			return errors.New("tool settlement without pending call")
		}
		if step.Tool.Outcome == nil {
			// No outcome records a result the engine did not observe: a
			// panic, or a start with no result after a crash. A returned
			// error settles with a failed outcome instead. Replay does not
			// second-guess the reason text, so an older or forged record
			// claiming uncertainty for an error still needs Accept.
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
			if v.Call.Name == SubmitOutputTool {
				if step.Tool.Outcome.State != tool.StateNotExecuted && v.Status != ToolStarted {
					return errors.New("reserved output tool requires engine start")
				}
				if step.Tool.Outcome.State == tool.StateSucceeded {
					schema, err := compileOutputSchema(s.OutputSchema)
					if err != nil {
						return err
					}
					if err := validateOutputValue(schema, v.Call.Args); err != nil {
						return err
					}
				}
			}
			switch v.Status {
			case ToolRequested, ToolDenied:
				if step.Tool.Outcome.State != tool.StateNotExecuted {
					return errors.New("unexecuted tool cannot have an execution result")
				}
			case ToolStarted, ToolWaiting, ToolUncertain:
			default:
				return errors.New("tool result without execution or denial")
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
		if !digestMatches(step.Approval.ArgsDigest, v.Call.Args) || v.Decision.Revision != step.Approval.PolicyRevision {
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
	case StepOutputRecorded:
		if len(s.OutputSchema) == 0 || s.Output != nil || step.Output == nil || len(s.pending()) > 0 {
			return errors.New("structured output without an eligible turn")
		}
		schema, err := compileOutputSchema(s.OutputSchema)
		if err != nil {
			return err
		}
		if err := validateOutputValue(schema, step.Output.Value); err != nil {
			return err
		}
		switch step.Output.Source {
		case OutputFromTool:
			v, ok := s.Tools[step.Output.ToolCallID]
			if !ok || v.Call.Name != SubmitOutputTool || v.Status != ToolSettled || v.Outcome == nil || v.Outcome.State != tool.StateSucceeded || s.callAttempts[v.Call.ID] != step.Output.AttemptID || !sameJSON(v.Call.Args, step.Output.Value) {
				return errors.New("structured output does not match an accepted submission")
			}
		case OutputFromText:
			if step.Output.ToolCallID != "" || step.Output.AttemptID == "" || step.Output.AttemptID != s.latestAttempt || !s.awaitingClassification || s.latestResult == nil || s.latestResult.Stop.Kind != llm.StopFinished {
				return errors.New("structured output does not match a final model answer")
			}
			parsed, err := parseOutputText(s.latestResult.Message.Text(), schema)
			if err != nil || !sameJSON(parsed, step.Output.Value) {
				return errors.New("structured output differs from final model text")
			}
		default:
			return errors.New("invalid structured output source")
		}
		value := *step.Output
		value.Value = append(json.RawMessage(nil), value.Value...)
		s.Output = &value
		s.awaitingClassification = false
	case StepStopped:
		if step.Stop == nil {
			return errors.New("missing stop")
		}
		if step.Stop.Limit != nil {
			if step.Stop.Kind != StopLimited {
				return errors.New("limit detail requires limited stop")
			}
			if err := step.Stop.Limit.Validate(); err != nil {
				return err
			}
		}
		if step.Stop.Kind != StopInterrupted {
			s.awaitingClassification = false
		}
		switch step.Stop.Kind {
		case StopCompleted, StopWaiting, StopLimited, StopRefused,
			StopCanceled, StopFailed, StopUncertain, StopInterrupted:
		default:
			return errors.New("invalid stop kind")
		}
		if step.Stop.Kind == StopCompleted && len(s.pending()) > 0 {
			return errors.New("completed turn has pending work")
		}
		if step.Stop.Kind == StopCompleted && len(s.OutputSchema) > 0 && s.Output == nil {
			return errors.New("completed turn lacks structured output")
		}
		if step.Stop.Kind == StopCanceled {
			// The stop is the evidence that calls without a start could not
			// execute. Started calls remain pending for late reconciliation.
			for _, id := range s.callOrder {
				v := s.Tools[id]
				switch v.Status {
				case ToolRequested, ToolApprovalPending, ToolAllowed, ToolDenied:
					v.Status = ToolSettled
					v.Outcome = &tool.Outcome{State: tool.StateNotExecuted}
					s.Tools[id] = v
				}
			}
			clear(s.hosted)
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
	s.latestAttempt = attempt
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
			s.callAttempts[c.ID] = attempt
			s.callOrder = append(s.callOrder, c.ID)
		}
	}
	if result.Stop.Kind == llm.StopToolUse || result.Stop.Kind == llm.StopFinished {
		for _, part := range result.Message.Content {
			if part.Kind == llm.ContentHostedApprovalRequest && part.HostedApprovalRequest != nil {
				a := part.HostedApprovalRequest.Clone()
				if _, ok := s.hosted[a.ID]; ok {
					return errors.New("repeated hosted approval ID")
				}
				s.hosted[a.ID] = a
			}
		}
	}
	return nil
}

func (s *replayState) canContinue() bool {
	return s.canContinueWithBudget(nil)
}

func (s *replayState) canContinueWithBudget(raise *Budget) bool {
	if s.Revision == 0 || s.Stop.Kind == StopCompleted ||
		s.Stop.Kind == StopCanceled || s.unresolvedModel != "" ||
		len(s.hosted) > 0 {
		return false
	}
	if s.Interruptions > s.Definition.Limits.interruptionLimit() {
		return false
	}
	if s.interruptionLimitReached() {
		return false
	}
	if s.Stop.Kind == StopUncertain {
		// An explicit uncertain stop is evidence that the host must report
		// the outcome. A missing stop means the worker may have crashed and
		// Continue may first establish which work can safely be repeated.
		if s.modelIntent != "" {
			return false
		}
		for _, id := range s.callOrder {
			if s.Tools[id].Status == ToolStarted {
				return false
			}
		}
	}
	if s.Stop.Kind == StopLimited &&
		(s.Stop.Reason == "deadline" || s.Stop.Reason == "interruptions" ||
			s.ModelAttempts >= s.Definition.Limits.ModelAttempts) {
		return false
	}
	if s.Stop.Kind == StopLimited && s.Stop.Limit != nil {
		limit := s.Stop.Limit
		budget := s.Budget
		if raise != nil {
			budget = raise
		}
		switch limit.Kind {
		case LimitCost:
			if s.Spend.CostUnknown > 0 {
				return false
			}
			if limit.UnpricedModel != "" {
				break
			}
			if budget == nil || budget.MaxCost == nil {
				return false
			}
			used, known := s.Spend.costIn(budget.MaxCost.Currency)
			if !known || limit.AtRisk > 0 &&
				used+limit.AtRisk > budget.MaxCost.Amount ||
				limit.AtRisk == 0 && used >= budget.MaxCost.Amount {
				return false
			}
		case LimitOutputTokens:
			if budget == nil || budget.MaxOutputTokens <=
				s.Spend.Usage.OutputTokens {
				return false
			}
		case LimitTotalTokens:
			total := s.Spend.Usage.TotalInputTokens() +
				s.Spend.Usage.OutputTokens
			if budget == nil || budget.MaxTotalTokens <= total {
				return false
			}
		}
	}
	for _, id := range s.callOrder {
		v := s.Tools[id]
		switch v.Status {
		case ToolSettled, ToolRequested, ToolAllowed, ToolDenied:
		case ToolStarted:
		default:
			return false
		}
	}
	return true
}

func (s *replayState) interruptionLimitReached() bool {
	if s.Revision == 0 || s.Interruptions <
		s.Definition.Limits.interruptionLimit() {
		return false
	}
	return s.Stop.Kind == StopInterrupted || s.Stop.Kind == ""
}

func validateModelAccounting(fact *ModelFact) error {
	if fact.NoDispatch && (!fact.Interrupted || fact.Result != nil ||
		fact.Charge != nil || fact.Uncertain) {
		return errors.New("known nondispatch cannot have a model result or charge")
	}
	if fact.Result != nil {
		if fact.Result.Usage != nil {
			if err := fact.Result.Usage.Validate(); err != nil {
				return err
			}
		}
		if fact.Result.Cost != nil {
			if err := fact.Result.Cost.Validate(); err != nil {
				return err
			}
		}
	}
	if fact.Charge != nil {
		if err := fact.Charge.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateStepPayload(step Step) error {
	if step.Budget != nil && step.Kind != StepContinued {
		return errors.New("budget raise requires a continuation step")
	}
	if len(step.Warnings) > 0 && step.Kind != StepContinued &&
		step.Kind != StepModelIntent {
		return errors.New("warnings require a continuation or model intent step")
	}
	for _, warning := range step.Warnings {
		if warning.Message == "" ||
			(warning.Code != WarningHistoryMismatch &&
				warning.Code != WarningBudgetClamp) ||
			(warning.Code == WarningHistoryMismatch &&
				step.Kind != StepContinued) ||
			(warning.Code == WarningBudgetClamp &&
				step.Kind != StepModelIntent) {
			return errors.New("invalid warning")
		}
	}
	count := 0
	for _, present := range []bool{step.Started != nil, step.Context != nil, step.Model != nil, step.Tool != nil, step.Approval != nil, step.HostedApproval != nil, step.Output != nil, step.Stop != nil} {
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
	case StepOutputRecorded:
		if step.Output == nil {
			return errors.New("output payload missing")
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
		if part.Kind == llm.ContentReminder && (m.Role != llm.RoleUser || len(m.Content) != 1) {
			return errors.New("reminder requires a standalone user message")
		}
	}
	return nil
}
func messageReminder(m llm.Message) *llm.Reminder {
	if len(m.Content) == 1 && m.Content[0].Kind == llm.ContentReminder {
		return m.Content[0].Reminder
	}
	return nil
}
func validateContext(item ContextItem) error {
	switch item.Origin {
	case "user", "library", "tool":
		if item.Message.Role != llm.RoleUser {
			return errors.New("untrusted context must use user role")
		}
	case "operator":
		if item.Message.Role != llm.RoleUser && item.Message.Role != llm.RoleSystem && item.Message.Role != llm.RoleDeveloper {
			return errors.New("operator context has invalid role")
		}
	default:
		return errors.New("invalid context origin")
	}
	if item.Snapshot && item.Name == "" {
		return errors.New("snapshot context requires a name")
	}
	if reminder := messageReminder(item.Message); reminder != nil && reminder.Tier == llm.ReminderOperator && item.Origin != "operator" {
		return errors.New("operator reminder requires operator context origin")
	}
	return validateMessage(item.Message)
}
