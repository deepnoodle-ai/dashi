package dashi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

// ToolStatus describes a call's recorded execution state.
type ToolStatus string

const (
	// ToolRequested means the model requested the call.
	ToolRequested ToolStatus = "requested"
	// ToolApprovalPending means a BeforeTool hook deferred the call, which
	// awaits a host answer.
	ToolApprovalPending ToolStatus = "approval_pending"
	// ToolDenied means a hook or the host refused the call.
	ToolDenied ToolStatus = "denied"
	// ToolAllowed means the call may run.
	ToolAllowed ToolStatus = "allowed"
	// ToolStarted means execution began without a settled outcome.
	ToolStarted ToolStatus = "started"
	// ToolWaiting means execution deferred completion.
	ToolWaiting ToolStatus = "waiting"
	// ToolUncertain means execution may have occurred without a known result.
	ToolUncertain ToolStatus = "uncertain"
	// ToolSettled means a final outcome was recorded.
	ToolSettled ToolStatus = "settled"
)

// ToolView projects the current call state from committed facts.
type ToolView struct {
	Call       tool.Call
	Definition tool.Definition
	Status     ToolStatus
	// Verdict is the combined BeforeTool result.
	Verdict ToolVerdict
	Outcome *tool.Outcome
	// ArgsDigest is set while Status is ToolApprovalPending. It identifies
	// Call.Args by JSON meaning; echo it in ApprovalFact.ArgsDigest to approve
	// or deny exactly the arguments shown.
	ArgsDigest string
	// Approval is the host's answer to a deferred call, recorded by Settle.
	// Verdict remains the hooks' deferral.
	Approval *ApprovalFact
}

// TurnView is the current state reconstructed from acknowledged steps.
type TurnView struct {
	ID    string
	Agent Agent
	// OutputSchema is the schema pinned at Start for this turn.
	OutputSchema json.RawMessage
	// Revision is the latest acknowledged step sequence number.
	Revision int64
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
	Spend Spend
	// Stop is why the last run ended. Replay and Outcome project a missing
	// stop as Run does: uncertain for an unsettled model attempt,
	// interrupted for work in flight, waiting for pending work. A hook's
	// view is taken mid-run, so its Stop is empty.
	Stop   Stop
	Output *StructuredOutput
	// Answer is the text of the latest assistant message. On a completed
	// turn it is the final answer, and valid tool-only work can leave it
	// empty; on a waiting turn it is text the model wrote alongside its
	// tool calls.
	Answer string
	// Pending lists the work awaiting a host action or settlement: model
	// attempts, then local calls in call order, then hosted approval
	// requests in the order the model made them.
	Pending []Pending
	// Warnings includes nonfatal conditions from acknowledged steps.
	Warnings []Warning
	// ReadyToContinue means Continue can run without first raising a budget.
	// A limited turn with a spent cap can still accept a Continue that raises it.
	ReadyToContinue bool
	// Tools is keyed by tool call ID and rebuilt from committed steps.
	Tools map[string]ToolView

	// live holds the latest named context item per origin and name that the
	// next model call would still see; see Context.
	live map[string]ContextItem
}

// Context returns the latest context item with this origin and name that the
// next model call would still see, so a BeforeModel hook can tell whether
// the item it would deliver is already current. An empty origin means
// ContextUser.
func (v TurnView) Context(
	origin ContextOrigin,
	name string,
) (ContextItem, bool) {
	item, ok := v.live[contextKey(ContextItem{Origin: origin, Name: name})]
	if !ok {
		return ContextItem{}, false
	}
	return cloneContextItem(item), true
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
	hostedOrder            []string
	hostedAttempts         map[string]string
	commandDigests         map[string]string
	latestResult           *llm.Result
	awaitingClassification bool
	failedModel            string
	// activeHooks is the ActiveHooks of the latest command.
	activeHooks *ActiveHooks
	// answerVetoed is set while the latest answer is one a BeforeFinish hook
	// vetoed; see ContextFact.Vetoed.
	answerVetoed bool
	// deliveredOnKept sizes the context delivered while a final text answer
	// was kept by a stop; see keptFinishCandidate.
	deliveredOnKept int
	// contextIDs maps each recorded context item ID to its item, so a
	// repeated delivery is recognized.
	contextIDs map[string]ContextItem
	// liveSeq holds the step that recorded each live item, and intentSeq
	// the step that recorded each model request, so a NextCallOnly item
	// expires only after a later dispatched call.
	liveSeq   map[string]int64
	intentSeq map[string]int64
}

// Replay verifies a turn's versioned prefix and reconstructs its current view.
// It does not execute any recorded intent.
func Replay(t Turn) (TurnView, error) {
	s, e := replay(t)
	if e != nil {
		return TurnView{}, e
	}
	view := snapshotView(s.TurnView)
	view.Stop = s.projectedStop()
	return view, nil
}

func snapshotView(v TurnView) TurnView {
	b, _ := json.Marshal(v)
	var out TurnView
	_ = json.Unmarshal(b, &out)
	if len(v.live) > 0 {
		out.live = make(map[string]ContextItem, len(v.live))
		for k, item := range v.live {
			out.live[k] = cloneContextItem(item)
		}
	}
	return out
}

func replay(t Turn) (*replayState, error) {
	s := &replayState{
		TurnView:         TurnView{ID: t.ID, Tools: map[string]ToolView{}},
		modelDefs:        map[string]tool.Definition{},
		hosted:           map[string]llm.HostedApprovalRequest{},
		hostedAttempts:   map[string]string{},
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
			return nil, fmt.Errorf(
				"%w: step %d cannot serialize: %v",
				ErrInvalidTurn,
				i+1,
				err,
			)
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
			if old, ok := s.commandDigests[step.CommandID]; ok &&
				old != step.CommandDigest {
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
		if step.Kind == StepStarted || step.Kind == StepContinued {
			s.activeHooks = step.ActiveHooks
		}
		s.trackContext(step)
		if err := s.trackIDs(step); err != nil {
			return nil, fmt.Errorf("%w: step %d: %v", ErrInvalidTurn, i+1, err)
		}
		if !s.awaitingClassification {
			s.deliveredOnKept = 0
		}
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
	s.Pending = s.pendingItems()
	s.Answer = answerText(t.Steps)
	// A vetoed answer never reaches the host through Answer.
	if s.answerVetoed {
		s.Answer = ""
	}
	if err := s.calculateSpend(); err != nil {
		return nil, fmt.Errorf("%w: calculate spend: %v", ErrInvalidTurn, err)
	}
	s.ReadyToContinue = s.canContinue()
	return s, nil
}

func (s *replayState) calculateSpend() error {
	s.Spend = Spend{
		Cost:     map[string]money.Amount{},
		ToolCost: map[string]money.Amount{},
	}
	for _, id := range slices.Sorted(maps.Keys(s.modelIntents)) {
		usage, charge, estimated, counted := attemptSpend(
			s.modelIntents[id],
			s.modelSettlements[id],
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
		if err := addCurrencyCost(
			s.Spend.Cost,
			charge.Currency,
			charge.Total,
		); err != nil {
			return err
		}
		capped := s.Budget != nil && s.Budget.MaxCost != nil
		if charge.Partial() {
			s.Spend.CostPartial++
			if s.Spend.UnpricedRequests == nil {
				s.Spend.UnpricedRequests = map[string]int{}
			}
			for key, count := range charge.UnpricedRequests {
				total, ok := sumCounts(s.Spend.UnpricedRequests[key], count)
				if !ok {
					return errors.New("dashi: unpriced request count overflows")
				}
				s.Spend.UnpricedRequests[key] = total
			}
		}
		// A partial total is a lower bound, so it cannot show that a cap
		// holds. Admission refuses hosted tools under a cap, so a partial
		// charge there is hosted work nothing foresaw.
		if capped && (charge.Currency != s.Budget.MaxCost.Currency ||
			charge.Partial()) {
			s.Spend.CostUnknown++
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.Tools)) {
		view := s.Tools[id]
		if view.Status == ToolSettled && view.Outcome != nil &&
			view.Outcome.Cost != nil {
			cost := view.Outcome.Cost
			if err := addCurrencyCost(
				s.Spend.ToolCost,
				cost.Currency,
				cost.Amount,
			); err != nil {
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
	if s.Stop.Terminal {
		switch step.Kind {
		case StepModelSettled, StepModelReconciled, StepToolSettled:
		default:
			return errors.New("ended turn only accepts late results")
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
			step.Stop.Kind == StopLimited && step.Stop.Limit != nil &&
			(step.Stop.Limit.Kind == LimitDeadline ||
				step.Stop.Limit.Kind == LimitInterruptions ||
				step.Stop.Limit.Kind == LimitModelAttempts && s.finalAnswerOnly ||
				step.Stop.Limit.Kind == LimitOutputTokens ||
				step.Stop.Limit.Kind == LimitTotalTokens ||
				step.Stop.Limit.Kind == LimitCost):
		default:
			return errors.New("turn with recorded output can only complete")
		}
	}
	switch step.Kind {
	case StepStarted:
		if step.Started == nil {
			return errors.New("missing start")
		}
		if e := step.Started.Agent.Validate(); e != nil {
			return e
		}
		// The runner records a resolved agent; only its ID may be empty.
		if a := step.Started.Agent; a.Revision == "" || a.Provider == "" ||
			a.API == "" || a.Model == "" {
			return errors.New("recorded agent requires revision, provider, API, and model")
		}
		outputSchema := step.Started.OutputSchema
		if len(outputSchema) == 0 {
			outputSchema = step.Started.Agent.OutputSchema
		}
		if len(outputSchema) > 0 {
			if _, err := compileOutputSchema(outputSchema); err != nil {
				return err
			}
			if err := validateOutputToolChoice(step.Started.Agent.Settings); err != nil {
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
		if origin != StartUser &&
			origin != StartScheduled &&
			origin != StartOperator {
			return errors.New("invalid start origin")
		}
		for _, m := range step.Started.Input {
			if origin != StartOperator && m.Role != llm.RoleUser {
				return errors.New("user or scheduled start input must use user role")
			}
			if origin == StartOperator &&
				m.Role != llm.RoleUser &&
				m.Role != llm.RoleSystem &&
				m.Role != llm.RoleDeveloper {
				return errors.New("invalid operator start role")
			}
			if err := validateMessage(m); err != nil {
				return err
			}
			if reminder := messageReminder(m); reminder != nil &&
				reminder.Tier == llm.ReminderOperator &&
				origin != StartOperator {
				return errors.New("operator reminder requires operator start origin")
			}
		}
		s.Agent = step.Started.Agent.resolved()
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
		if s.keptFinishCandidate() {
			s.deliveredOnKept += estimatedItemTokens(step.Context.Items)
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
			s.Raises = append(
				s.Raises,
				BudgetRaise{Budget: budget.Clone(), At: step.At},
			)
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
			if step.Context.Vetoed {
				s.answerVetoed = true
			}
		}
	case StepModelIntent:
		if step.Model == nil ||
			step.Model.AttemptID == "" ||
			step.Model.MessageID == "" ||
			step.Model.RequestDigest == "" ||
			s.modelIntent != "" ||
			s.Output != nil ||
			len(s.pending()) > 0 {
			return errors.New("invalid model intent")
		}
		if err := validateAdmission(step.Model.Admission); err != nil {
			return err
		}
		if step.Model.Provider != s.Agent.Provider ||
			step.Model.API != s.Agent.API ||
			step.Model.Model != s.Agent.Model {
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
		if step.Model.FinalAnswerOnly &&
			(s.Agent.Limits.NoReservedFinalAnswer ||
				s.ModelAttempts == 0 ||
				s.ModelAttempts+1 != s.Agent.Limits.ModelAttempts) {
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
				if len(s.OutputSchema) == 0 || d.Kind != tool.KindLocal || !sameJSON(
					d.Schema,
					s.OutputSchema,
				) {
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
		if step.Model == nil ||
			step.Model.AttemptID == "" ||
			step.Model.AttemptID != s.modelIntent ||
			step.Model.MessageID != s.modelMessages[s.modelIntent] {
			return errors.New("model settlement without intent")
		}
		if step.Model.Result == nil && step.Model.Error == "" {
			return errors.New("missing model outcome")
		}
		if err := validateModelAccounting(step.Model); err != nil {
			return err
		}
		if step.Model.Failure != "" &&
			step.Model.Failure != llm.FailureUnknown &&
			step.Model.Failure != llm.FailureRejected &&
			step.Model.Failure != llm.FailureNotDispatched {
			return errors.New("invalid model failure disposition")
		}
		if step.Model.Uncertain &&
			(step.Model.Failure == llm.FailureRejected ||
				step.Model.Failure == llm.FailureNotDispatched) {
			return errors.New("known model failure marked uncertain")
		}
		if step.Model.Interrupted &&
			(step.Model.Failure == llm.FailureRejected ||
				step.Model.Failure == llm.FailureNotDispatched) {
			return errors.New("interrupted model call cannot have known nonexecution")
		}
		if step.Model.Failure == llm.FailureRejected ||
			step.Model.Failure == llm.FailureNotDispatched {
			if step.Model.Error == "" ||
				step.Model.Evidence == "" ||
				(step.Model.Result != nil &&
					(step.Model.Result.ID != "" ||
						len(step.Model.Result.Message.Content) != 0 ||
						step.Model.Result.Usage != nil)) {
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
				!s.Stop.Terminal
			if err := s.recordResult(
				step.Model.AttemptID,
				step.Model.Result,
				actionable,
			); err != nil {
				return err
			}
		}
		if step.Model.Error == "" && !step.Model.Interrupted {
			s.awaitingClassification = true
		}
		if err := s.checkStillOpen(); err != nil {
			return err
		}
	case StepModelReconciled:
		if step.Model == nil ||
			step.Model.AttemptID != s.unresolvedModel ||
			step.Model.MessageID != s.modelMessages[s.unresolvedModel] ||
			(step.Model.Result == nil &&
				step.Model.Error == "") {
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
			if err := s.recordResult(
				step.Model.AttemptID,
				step.Model.Result,
				!s.Stop.Terminal,
			); err != nil {
				return err
			}
			if s.unresolvedInterrupted {
				s.ModelAttempts++
			}
			s.awaitingClassification = true
		}
		s.unresolvedPartial = false
		s.unresolvedInterrupted = false
		if err := s.checkStillOpen(); err != nil {
			return err
		}
	case StepToolDecision:
		s.awaitingClassification = false
		if step.Tool == nil {
			return errors.New("missing tool decision")
		}
		v, ok := s.Tools[step.Tool.Call.ID]
		if !ok || v.Status != ToolRequested {
			return errors.New("tool decision without request")
		}
		v.Verdict = step.Tool.Verdict
		if step.Tool.Call.ID != v.Call.ID || step.Tool.Call.Name != v.Call.Name {
			return errors.New("tool decision changes identity")
		}
		if err := step.Tool.Call.Validate(); err != nil {
			return err
		}
		if !sameJSONValue(step.Tool.Call.Args, v.Call.Args) &&
			!sameJSONValue(step.Tool.Call.RequestedArgs, v.Call.Args) {
			return errors.New("tool rewrite lost requested arguments")
		}
		if step.Tool.Definition.Name != "" && !sameJSONValue(
			step.Tool.Definition,
			v.Definition,
		) {
			return errors.New("tool decision changed definition")
		}
		if v.Call.Name == SubmitOutputTool &&
			(v.Verdict.Kind != DecisionAllow ||
				v.Verdict.Revision != "dashi-output-v1" ||
				len(step.Tool.Call.RequestedArgs) != 0 ||
				!sameJSON(
					v.Call.Args,
					step.Tool.Call.Args,
				)) {
			return errors.New("reserved output tool cannot be rewritten or denied")
		}
		v.Call = step.Tool.Call.Clone()
		switch v.Verdict.Kind {
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
		if !ok || v.Status != ToolAllowed || !sameJSONValue(
			v.Call,
			step.Tool.Call,
		) || (step.Tool.Definition.Name != "" && !sameJSONValue(
			step.Tool.Definition,
			v.Definition,
		)) {
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
		if !ok || !sameJSONValue(
			v.Call,
			step.Tool.Call,
		) || (v.Status != ToolStarted && (v.Status != ToolAllowed || v.Definition.Kind != tool.KindExternal)) {
			return errors.New("tool wait without allowance")
		}
		if step.Tool.Outcome.State != tool.StateWaiting &&
			step.Tool.Outcome.State != tool.StateDetached {
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
		if step.Tool.Vetoed {
			if step.Tool.Call.Name != SubmitOutputTool ||
				s.Tools[step.Tool.Call.ID].Status != ToolStarted ||
				!strings.HasPrefix(step.Tool.Reason, "not accepted: ") ||
				step.Tool.Outcome == nil ||
				step.Tool.Outcome.State != tool.StateFailed {
				return errors.New("only a turned-down submission records a veto")
			}
			s.answerVetoed = true
		}
		if step.Context != nil {
			started := s.Tools[step.Tool.Call.ID].Status == ToolStarted
			if step.Tool.Call.Name != SubmitOutputTool || !started ||
				!strings.HasPrefix(step.Tool.Reason, "not accepted: ") ||
				step.Tool.Outcome == nil ||
				step.Tool.Outcome.State != tool.StateFailed ||
				len(step.Context.Items) == 0 {
				return errors.New("only a turned-down submission carries context")
			}
			for _, item := range step.Context.Items {
				if err := validateContext(item); err != nil {
					return err
				}
			}
		}
		v, ok := s.Tools[step.Tool.Call.ID]
		if !ok || v.Status == ToolSettled || !sameJSONValue(v.Call, step.Tool.Call) {
			return errors.New("tool settlement without pending call")
		}
		if step.Tool.Outcome == nil {
			// No outcome records a result the runner did not observe: a
			// panic, or a start with no result after a crash. A returned
			// error settles with a failed outcome instead. Replay does not
			// second-guess the reason text, so an older or forged record
			// claiming uncertainty for an error still needs Settle.
			if v.Status != ToolStarted && v.Status != ToolWaiting {
				return errors.New("uncertainty without start")
			}
			v.Status = ToolUncertain
		} else {
			if e := step.Tool.Outcome.Validate(); e != nil {
				return e
			}
			if step.Tool.Outcome.State == tool.StateWaiting ||
				step.Tool.Outcome.State == tool.StateDetached {
				return errors.New("wait is not settlement")
			}
			if v.Call.Name == SubmitOutputTool {
				if step.Tool.Outcome.State != tool.StateNotExecuted &&
					v.Status != ToolStarted {
					return errors.New("reserved output tool requires runner start")
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
	case StepApprovalSettled:
		s.awaitingClassification = false
		if step.Approval == nil {
			return errors.New("missing approval")
		}
		v, ok := s.Tools[step.Approval.CallID]
		if !ok || v.Status != ToolApprovalPending {
			return errors.New("approval without pending decision")
		}
		if step.Approval.ArgsDigest != ArgsDigest(v.Call.Args) {
			return errors.New("stale approval")
		}
		// Verdict stays the hooks'; the host's answer is kept beside it,
		// so a denial's reason reaches the model when Continue settles the
		// call.
		answer := *step.Approval
		v.Approval = &answer
		if answer.Approve {
			v.Status = ToolAllowed
		} else {
			v.Status = ToolDenied
		}
		s.Tools[v.Call.ID] = v
	case StepHostedApprovalSettled:
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
		if len(s.OutputSchema) == 0 ||
			s.Output != nil ||
			step.Output == nil ||
			len(s.pending()) > 0 {
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
			if !ok ||
				v.Call.Name != SubmitOutputTool ||
				v.Status != ToolSettled ||
				v.Outcome == nil ||
				v.Outcome.State != tool.StateSucceeded ||
				s.callAttempts[v.Call.ID] != step.Output.AttemptID ||
				!sameJSON(
					v.Call.Args,
					step.Output.Value,
				) {
				return errors.New("structured output does not match an accepted submission")
			}
		case OutputFromText:
			if step.Output.ToolCallID != "" ||
				step.Output.AttemptID == "" ||
				step.Output.AttemptID != s.latestAttempt ||
				!s.awaitingClassification ||
				s.latestResult == nil ||
				s.latestResult.Stop.Kind != llm.StopFinished {
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
		// A candidate answer survives an interruption, and a BeforeFinish
		// failure, so Continue can classify it again without another model
		// call.
		if step.Stop.Kind != StopInterrupted &&
			(step.Stop.Kind != StopFailed ||
				step.Stop.Reason != StopReasonHookFailed) {
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
		if step.Stop.Kind == StopCompleted &&
			len(s.OutputSchema) > 0 &&
			s.Output == nil {
			return errors.New("completed turn lacks structured output")
		}
		s.Stop = *step.Stop
		if step.Stop.Kind == StopLimited {
			// A cost stop is terminal when spend includes an unknown cost.
			if err := s.calculateSpend(); err != nil {
				return err
			}
		}
		if s.terminal() != step.Stop.Terminal {
			return fmt.Errorf(
				"stop terminal=%t, but the turn's state gives %t",
				step.Stop.Terminal,
				s.terminal(),
			)
		}
		if step.Stop.Terminal {
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
	for _, id := range s.hostedOrder {
		if _, ok := s.hosted[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// pendingItems describes the work a host must act on. Calls that are only
// requested, allowed, or denied are left out: Continue alone runs them.
func (s *replayState) pendingItems() []Pending {
	var out []Pending
	for _, id := range []string{s.modelIntent, s.unresolvedModel} {
		if id != "" {
			out = append(out, Pending{Kind: PendingModel, ID: id, AttemptID: id})
		}
	}
	for _, id := range s.callOrder {
		v := s.Tools[id]
		kind := PendingTool
		switch v.Status {
		case ToolApprovalPending:
			kind = PendingApproval
		case ToolStarted, ToolWaiting, ToolUncertain:
		default:
			continue
		}
		out = append(out, Pending{
			Kind:      kind,
			ID:        id,
			AttemptID: s.callAttempts[id],
			Name:      v.Call.Name,
			Args:      v.Call.Args,
			Tool:      &v,
		})
	}
	for _, id := range s.hostedOrder {
		a, ok := s.hosted[id]
		if !ok {
			continue
		}
		out = append(out, Pending{
			Kind:      PendingApproval,
			ID:        id,
			AttemptID: s.hostedAttempts[id],
			Name:      a.Name,
			Args:      a.Arguments,
			Hosted:    &a,
		})
	}
	return out
}

// projectedStop is the stop a caller sees for a turn at rest. A record
// without a stop step ended mid-run, so the stop is inferred as Run would
// have recorded it.
func (s *replayState) projectedStop() Stop {
	switch {
	case s.Stop.Kind != "":
		return s.Stop
	case s.unresolvedModel != "":
		return Stop{
			Kind:   StopUncertain,
			Reason: "model attempt needs reconciliation",
		}
	case s.modelIntent != "" || hasStartedTool(s):
		return Stop{Kind: StopInterrupted, Reason: "worker stopped before result"}
	case len(s.Pending) > 0:
		return Stop{Kind: StopWaiting}
	case len(s.pending()) > 0:
		// Requested or authorized calls wait only for a worker.
		return Stop{Kind: StopInterrupted, Reason: "worker stopped before result"}
	}
	return Stop{}
}

func (s *replayState) recordResult(
	attempt string,
	result *llm.Result,
	actionable bool,
) error {
	s.latestResult = result
	s.latestAttempt = attempt
	if !actionable {
		return nil
	}
	s.answerVetoed = false
	if err := validateModelResult(*result, s.Agent); err != nil {
		return err
	}
	if result.Stop.Kind == llm.StopPause {
		s.ProviderPauses++
	}
	if result.Stop.Kind == llm.StopToolUse ||
		result.Stop.Kind == llm.StopFinished {
		for _, part := range result.Message.Content {
			if part.Kind != llm.ContentToolCall || part.ToolCall == nil {
				continue
			}
			c := part.ToolCall.Clone()
			if _, ok := s.Tools[c.ID]; ok {
				return errors.New("repeated tool call ID")
			}
			s.Tools[c.ID] = ToolView{
				Call:       c,
				Definition: s.modelDefs[c.Name],
				Status:     ToolRequested,
			}
			s.callAttempts[c.ID] = attempt
			s.callOrder = append(s.callOrder, c.ID)
		}
	}
	if result.Stop.Kind == llm.StopToolUse ||
		result.Stop.Kind == llm.StopFinished {
		for _, part := range result.Message.Content {
			if part.Kind == llm.ContentHostedApprovalRequest &&
				part.HostedApprovalRequest != nil {
				a := part.HostedApprovalRequest.Clone()
				if _, ok := s.hosted[a.ID]; ok {
					return errors.New("repeated hosted approval ID")
				}
				// A provider may reuse an ID once its earlier request was
				// answered. The attempt tells the two requests apart, so
				// each gets its own answer command.
				s.hosted[a.ID] = a
				s.hostedAttempts[a.ID] = attempt
				s.hostedOrder = slices.DeleteFunc(
					s.hostedOrder,
					func(id string) bool { return id == a.ID },
				)
				s.hostedOrder = append(s.hostedOrder, a.ID)
			}
		}
	}
	return nil
}

func (s *replayState) canContinue() bool {
	return s.canContinueWithBudget(nil)
}

func (s *replayState) canContinueWithBudget(raise *Budget) bool {
	if s.Revision == 0 || s.terminal() || s.unresolvedModel != "" ||
		len(s.hosted) > 0 {
		return false
	}
	if s.Interruptions > max(s.Agent.Limits.Interruptions, 0) {
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
			after, ok := used.Add(limit.CostAtRisk)
			if !known || !ok || limit.CostAtRisk > 0 &&
				after > budget.MaxCost.Amount ||
				limit.CostAtRisk == 0 && used >= budget.MaxCost.Amount {
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

// terminal reports whether the current stop ends the turn for good: no
// settlement or raise lets Continue run it again. It reads only the stop,
// the agent's fixed limits, and counts that never shrink (model attempts
// and unknown costs), so a recorded true stays true. A recorded false stays
// accurate because only model results raise those counts, and replay
// rejects one that would flip a limited stop (checkStillOpen). Cost stops
// read Spend, which the caller must have calculated.
func (s *replayState) terminal() bool {
	switch s.Stop.Kind {
	case StopCompleted, StopCanceled:
		return true
	case StopLimited:
	default:
		return false
	}
	if s.ModelAttempts >= s.Agent.Limits.ModelAttempts {
		return true
	}
	if s.Stop.Limit == nil {
		return false
	}
	switch s.Stop.Limit.Kind {
	case LimitDeadline, LimitInterruptions:
		return true
	case LimitCost:
		return s.Spend.CostUnknown > 0
	}
	return false
}

// acceptsAfterEnd rejects a command on an ended turn unless it is a Settle
// reporting the result of work that had already started.
func (s *replayState) acceptsAfterEnd(command Command) error {
	if !s.Stop.Terminal {
		return nil
	}
	if c, ok := command.(Settle); ok &&
		c.Approval == nil && c.HostedApproval == nil {
		if c.ModelResult != nil || c.ModelFailure != nil {
			if c.CallID != "" &&
				(c.CallID == s.modelIntent || c.CallID == s.unresolvedModel) {
				return nil
			}
		} else if v, ok := s.Tools[c.CallID]; ok &&
			(v.Status == ToolStarted || v.Status == ToolWaiting ||
				v.Status == ToolUncertain) {
			return nil
		}
	}
	if s.Stop.Reason != "" {
		return fmt.Errorf("%w: %s (%s)", ErrTurnEnded, s.Stop.Kind, s.Stop.Reason)
	}
	return fmt.Errorf("%w: %s", ErrTurnEnded, s.Stop.Kind)
}

// checkLateModelStep refuses, before anything is saved, a late model result
// that replay would reject because it makes an open limited stop terminal
// (checkStillOpen). A host recorder need not replay what it commits, so
// without this the step would be stored and the turn would fail replay from
// then on. It replays only when the turn rests at an open limited stop.
func checkLateModelStep(turn Turn, s *replayState, step Step) error {
	if s.Stop.Kind != StopLimited || s.Stop.Terminal {
		return nil
	}
	step.Version = 1
	step.TurnID = turn.ID
	step.Seq = int64(len(turn.Steps) + 1)
	candidate := turn
	candidate.Steps = append(slices.Clone(turn.Steps), step)
	if _, err := replay(candidate); err != nil {
		return fmt.Errorf("dashi: late model result refused: %w", err)
	}
	return nil
}

// checkStillOpen enforces that a turn becomes non-continuable only through
// a recorded stop: a late model result after a limited stop that was not
// terminal must not make it terminal, or the recorded Terminal would be
// stale. The runner records uncertain before any budget stop while a model
// call is unresolved, so no record it writes fails this.
func (s *replayState) checkStillOpen() error {
	if s.Stop.Kind != StopLimited || s.Stop.Terminal {
		return nil
	}
	if err := s.calculateSpend(); err != nil {
		return err
	}
	if s.terminal() {
		return errors.New("late model result ended the turn without a stop")
	}
	return nil
}

func (s *replayState) interruptionLimitReached() bool {
	if s.Revision == 0 ||
		s.Interruptions < max(s.Agent.Limits.Interruptions, 0) {
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

// trackContext keeps the named items the next model call would see: the
// latest item per origin and name, less NextCallOnly items once a model call
// that was dispatched follows them, as Project does.
func (s *replayState) trackContext(step Step) {
	switch step.Kind {
	case StepDelivered, StepLoopContinued, StepToolSettled:
		if step.Context == nil {
			return
		}
		for _, item := range step.Context.Items {
			if item.Name == "" {
				continue
			}
			if s.live == nil {
				s.live = map[string]ContextItem{}
				s.liveSeq = map[string]int64{}
			}
			key := contextKey(item)
			s.live[key] = cloneContextItem(item)
			s.liveSeq[key] = step.Seq
		}
	case StepModelIntent:
		if s.intentSeq == nil {
			s.intentSeq = map[string]int64{}
		}
		s.intentSeq[step.Model.AttemptID] = step.Seq
	case StepModelSettled, StepModelReconciled:
		if !modelDispatched(step.Model) {
			return
		}
		intent := s.intentSeq[step.Model.AttemptID]
		for key, item := range s.live {
			if item.NextCallOnly && s.liveSeq[key] < intent {
				delete(s.live, key)
			}
		}
	}
}

// keptFinishCandidate reports a final text answer kept by a stop before it
// was accepted: a BeforeFinish failure, an interruption, or a worker that
// stopped with no stop recorded. Context delivered while it is kept is
// counted in deliveredOnKept, and Continue decides at run time whether the
// model can be asked again.
func (s *replayState) keptFinishCandidate() bool {
	if !s.awaitingClassification {
		return false
	}
	switch s.Stop.Kind {
	case StopInterrupted, "":
		return true
	case StopFailed:
		return s.Stop.Reason == StopReasonHookFailed
	}
	return false
}

// trackIDs records the step's context item IDs. An ID may appear once in a
// turn: the runner skips a repeat before recording it.
func (s *replayState) trackIDs(step Step) error {
	for _, item := range step.Items() {
		if item.ID == "" {
			continue
		}
		if _, ok := s.contextIDs[item.ID]; ok {
			return fmt.Errorf("repeated context item ID %q", item.ID)
		}
		if s.contextIDs == nil {
			s.contextIDs = map[string]ContextItem{}
		}
		s.contextIDs[item.ID] = item
	}
	return nil
}

// roomForAnyCall reports whether one more model call fits ModelAttempts,
// the reserved final-answer call included. A call in flight counts as used.
func (s *replayState) roomForAnyCall() bool {
	used := s.ModelAttempts
	if s.modelIntent != "" {
		used++
	}
	return used < s.Agent.Limits.ModelAttempts
}

// roomForNormalCall reports whether one more model call fits before the
// reserved final-answer call, or within ModelAttempts when there is no
// reserve. A model call in flight, requested but not settled, counts as
// used, since ModelAttempts counts only settled calls.
func (s *replayState) roomForNormalCall() bool {
	used := s.ModelAttempts
	if s.modelIntent != "" {
		used++
	}
	limit := s.Agent.Limits.ModelAttempts
	if s.Agent.Limits.NoReservedFinalAnswer {
		return used < limit
	}
	return used+1 < limit
}

// hookVerdicts lists the verdicts each hook point can record.
var hookVerdicts = map[HookPoint][]HookVerdict{
	HookBeforeModel: {VerdictChange, VerdictStop, VerdictFailed},
	HookBeforeSend:  {VerdictAdmit, VerdictDeny, VerdictLimit, VerdictFailed},
	HookBeforeTool: {
		VerdictAllow,
		VerdictDeny,
		VerdictDefer,
		VerdictRewrite,
		VerdictFailed,
	},
	HookBeforeFinish: {
		VerdictDeliver,
		VerdictReject,
		VerdictAcceptEmpty,
		VerdictFailed,
	},
}

func validateHookResult(result HookResult) error {
	verdicts, ok := hookVerdicts[result.Point]
	if !ok {
		return errors.New("invalid hook point")
	}
	if result.Name == "" || !slices.Contains(verdicts, result.Verdict) {
		return errors.New("invalid hook result")
	}
	if result.Pass < 0 || result.Pass > 0 && result.Point != HookBeforeTool {
		return errors.New("hook result pass is only for before_tool")
	}
	if (result.Error != "") != (result.Verdict == VerdictFailed) {
		return errors.New("a hook error goes with a failed verdict")
	}
	return nil
}

// validate checks that each point lists unique, nonempty hook names.
func (a ActiveHooks) validate() error {
	for _, names := range [][]string{
		a.BeforeModel,
		a.BeforeSend,
		a.BeforeTool,
		a.BeforeFinish,
	} {
		seen := map[string]bool{}
		for _, name := range names {
			if name == "" || seen[name] {
				return errors.New("invalid active hook names")
			}
			seen[name] = true
		}
	}
	return nil
}

// validateAdmission checks the BeforeSend admissions recorded on an intent.
func validateAdmission(admission map[string]json.RawMessage) error {
	total := 0
	for name, raw := range admission {
		if name == "" || len(raw) == 0 || !json.Valid(raw) {
			return errors.New("invalid model admission metadata")
		}
		total += len(raw)
	}
	if total > maxAdmission {
		return errors.New("model admission metadata exceeds 16 KiB")
	}
	return nil
}

func cloneContextItem(item ContextItem) ContextItem {
	item.Message = item.Message.Clone()
	return item
}

func validateStepPayload(step Step) error {
	if step.Budget != nil && step.Kind != StepContinued {
		return errors.New("budget raise requires a continuation step")
	}
	// Warnings ride on whatever step comes next, except the two that
	// describe their own step.
	for _, warning := range step.Warnings {
		valid := false
		switch warning.Code {
		case WarningHistoryMismatch:
			valid = step.Kind == StepContinued
		case WarningBudgetClamp:
			valid = step.Kind == StepModelIntent
		case WarningHookFailed, WarningHooksChanged, WarningDeliveryDropped,
			WarningSteering:
			valid = true
		}
		if !valid || warning.Message == "" {
			return errors.New("invalid warning")
		}
	}
	for _, result := range step.HookResults {
		if err := validateHookResult(result); err != nil {
			return err
		}
	}
	if step.Kind != StepLoopContinued && step.Context != nil &&
		step.Context.Vetoed {
		return errors.New("only a loop continuation records a veto")
	}
	if step.Context != nil && step.Context.Steering {
		if step.Context.Vetoed {
			return errors.New("a steer is not a veto")
		}
		// The runner records steers; no command does.
		if step.CommandID != "" {
			return errors.New("a command cannot record steers")
		}
		if step.Kind == StepToolSettled &&
			(step.Tool == nil || step.Tool.Call.Name != SubmitOutputTool) {
			return errors.New("only a rejected submission settles with steers")
		}
		for _, item := range step.Context.Items {
			if item.ID == "" {
				return errors.New("a steer requires an ID")
			}
		}
	}
	if step.ActiveHooks != nil {
		if step.Kind != StepStarted && step.Kind != StepContinued {
			return errors.New("active hooks require a start or continuation step")
		}
		if err := step.ActiveHooks.validate(); err != nil {
			return err
		}
	}
	count := 0
	for _, present := range []bool{
		step.Started != nil,
		step.Context != nil,
		step.Model != nil,
		step.Tool != nil,
		step.Approval != nil,
		step.HostedApproval != nil,
		step.Output != nil,
		step.Stop != nil,
	} {
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
	case StepToolSettled:
		// A turned-down submission carries the items to deliver.
		if step.Context != nil {
			expected = 2
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
		if !step.Model.CapabilityPolicy.Valid() {
			return errors.New("invalid capability policy")
		}
	case StepToolDecision, StepToolStarted, StepToolWaiting, StepToolSettled:
		if step.Tool == nil {
			return errors.New("tool payload missing")
		}
	case StepApprovalSettled:
		if step.Approval == nil {
			return errors.New("approval payload missing")
		}
	case StepHostedApprovalSettled:
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
		if part.Kind == llm.ContentReminder &&
			(m.Role != llm.RoleUser ||
				len(m.Content) != 1) {
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
	switch cmp.Or(item.Origin, ContextUser) {
	case ContextUser, ContextRuntime, ContextTool:
		if item.Message.Role != llm.RoleUser {
			return errors.New("untrusted context must use user role")
		}
	case ContextOperator:
		if item.Message.Role != llm.RoleUser &&
			item.Message.Role != llm.RoleSystem &&
			item.Message.Role != llm.RoleDeveloper {
			return errors.New("operator context has invalid role")
		}
	default:
		return errors.New("invalid context origin")
	}
	if item.Snapshot && item.Name == "" {
		return errors.New("snapshot context requires a name")
	}
	// Context is input from the application or a person, never a turn's own
	// machinery: a tool result or call here would pair with nothing, and
	// providers reject it.
	for _, part := range item.Message.Content {
		if part.Kind != llm.ContentBlock && part.Kind != llm.ContentReminder {
			return fmt.Errorf(
				"context may carry only content blocks and reminders, not %s",
				part.Kind,
			)
		}
	}
	if reminder := messageReminder(item.Message); reminder != nil &&
		reminder.Tier == llm.ReminderOperator &&
		item.Origin != ContextOperator {
		return errors.New("operator reminder requires operator context origin")
	}
	return validateMessage(item.Message)
}
