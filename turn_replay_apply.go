package dashi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func (s *replayState) apply(step Step) error {
	if s.Stop.Kind == StopCompleted {
		return errors.New("completed turn cannot change")
	}
	if s.Stop.Terminal {
		switch step.Kind {
		case StepModelCallFinished, StepModelCallFinishedByHost, StepToolFinished:
		default:
			return errors.New("ended turn only accepts late results")
		}
	}
	// Once validated output is recorded, execution has no more work. A
	// context stop may still intervene before the completion stop is saved;
	// Continue then completes the existing output without another model call.
	if s.Output != nil {
		switch {
		case step.Kind == StepTurnContinued:
		case step.Kind == StepTurnStopped && step.Stop != nil && step.Stop.Kind == StopCompleted:
		case step.Kind == StepTurnStopped && step.Stop != nil &&
			step.Stop.Kind == StopInterrupted:
		case step.Kind == StepTurnStopped && step.Stop != nil &&
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
	case StepTurnStarted:
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
			origin != StartOperator &&
			origin != StartCompaction {
			return errors.New("invalid start origin")
		}
		if err := validateCompaction(origin, step.Started.Compaction); err != nil {
			return err
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
		if step.Started.Parent != nil {
			if err := step.Started.Parent.Validate(); err != nil {
				return err
			}
		}
		s.Parent = cloneCallRef(step.Started.Parent)
		s.Origin = origin
		s.Compaction = step.Started.Compaction.clone()
		s.Agent = step.Started.Agent.resolved()
		s.ActiveCompactionWindow = step.Started.ActiveCompactionWindow
		if s.ActiveCompactionWindow < 0 || s.ActiveCompactionWindow > 0 && (s.ActiveCompactionWindow < 1024 || s.Agent.ActiveCompaction == nil || s.Agent.ActiveCompaction.Disabled || origin == StartCompaction) {
			return errors.New("invalid active compaction window")
		}
		s.attemptCap = s.Agent.Limits.ModelAttempts
		if step.Started.Budget != nil {
			if err := step.Started.Budget.Validate(); err != nil {
				return err
			}
			budget := step.Started.Budget.Clone()
			s.Budget = &budget
		}
		s.OutputSchema = append(json.RawMessage(nil), outputSchema...)
	case StepMessagesAdded:
		if step.Context == nil || len(step.Context.Items) == 0 {
			return errors.New("missing context")
		}
		if s.keptFinishCandidate() {
			s.deliveredOnKept += estimatedItemTokens(step.Context.Items)
		}
		for _, item := range step.Context.Items {
			if err := item.Validate(); err != nil {
				return err
			}
		}
	case StepTurnContinued:
		if err := s.calculateSpend(); err != nil {
			return err
		}
		if !s.canContinueWith(step.Budget, step.ModelAttempts) {
			return errors.New("turn cannot continue")
		}
		if step.ModelAttempts != 0 {
			if step.ModelAttempts <= s.attemptCap {
				return errors.New("model-attempt raise must exceed the cap")
			}
			s.attemptCap = step.ModelAttempts
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
		if s.resumeCounts() {
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
				if err := item.Validate(); err != nil {
					return err
				}
			}
			if step.Context.Vetoed {
				s.answerVetoed = true
			}
		}
	case StepModelCallStarted:
		if step.Model == nil ||
			step.Model.AttemptID == "" ||
			step.Model.MessageID == "" ||
			step.Model.RequestDigest == "" ||
			s.modelIntent != "" ||
			s.Output != nil ||
			len(s.pending()) > 0 {
			return errors.New("invalid model intent")
		}
		if err := s.validateCompactionIntent(step); err != nil {
			return err
		}
		if err := validateAdmission(step.Model.Admission); err != nil {
			return err
		}
		if err := validateHidden(step.Model); err != nil {
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
		if step.Model.FinalReplyOnly &&
			(s.Agent.Limits.NoReservedFinalReply ||
				s.ModelAttempts == 0 ||
				s.ModelAttempts+1 != s.attemptCap) {
			return errors.New("final-answer intent is not the last model attempt")
		}
		s.modelIntent = step.Model.AttemptID
		s.modelIntents[step.Model.AttemptID] = step.Model
		s.modelOrder = append(s.modelOrder, step.Model.AttemptID)
		s.modelCalls++
		s.finalAnswerOnly = step.Model.FinalReplyOnly
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
				if len(s.OutputSchema) == 0 || d.Kind != tool.KindLocal || !canonjson.Equal(
					d.Schema,
					s.OutputSchema,
				) {
					return errors.New("invalid reserved output tool")
				}
				outputTool = true
			} else if step.Model.FinalReplyOnly {
				return errors.New("final-answer intent offers an application tool")
			}
			s.modelDefs[d.Name] = d
		}
		if len(s.OutputSchema) > 0 && !outputTool && step.Model.Purpose != ModelPurposeCompaction {
			return errors.New("output schema requires reserved output tool")
		}
	case StepModelCallFinished:
		if step.Model == nil ||
			step.Model.AttemptID == "" ||
			step.Model.AttemptID != s.modelIntent ||
			step.Model.MessageID != s.modelMessages[s.modelIntent] {
			return errors.New("model settlement without intent")
		}
		if step.Model.Result == nil && step.Model.Error == "" {
			return errors.New("missing model outcome")
		}
		if step.Model.ErrorReason != "" && step.Model.Error == "" {
			return errors.New("model error reason without error")
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
		if step.Model.Interrupted != InterruptNone &&
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
		if err := s.validateCompactionSettlement(step.Model); err != nil {
			return err
		}
		s.modelIntent = ""
		s.modelSettlements[step.Model.AttemptID] = step.Model
		if step.Model.Interrupted == InterruptNone {
			s.ModelAttempts++
		}
		if step.Model.Uncertain {
			s.unresolvedModel = step.Model.AttemptID
			s.unresolvedInterrupted = step.Model.Interrupted != InterruptNone
			s.unresolvedPartial = step.Model.Result != nil && (step.Model.Result.ID != "" || len(step.Model.Result.Message.Content) > 0 || step.Model.Result.Usage != nil)
		} else if step.Model.Error != "" &&
			step.Model.Interrupted == InterruptNone &&
			step.Model.Failure != llm.FailureRejected &&
			step.Model.Failure != llm.FailureNotDispatched {
			s.failedModel = step.Model.Error
		}
		if step.Model.Result != nil && s.modelIntents[step.Model.AttemptID].Purpose != ModelPurposeCompaction {
			actionable := step.Model.Error == "" &&
				step.Model.Interrupted == InterruptNone &&
				!s.Stop.Terminal
			if err := s.recordResult(
				step.Model.AttemptID,
				step.Model.Result,
				actionable,
			); err != nil {
				return err
			}
		}
		if step.Model.Error == "" &&
			step.Model.Interrupted == InterruptNone {
			if s.modelIntents[step.Model.AttemptID].Purpose == ModelPurposeCompaction {
				s.pendingCompaction = step.Model.AttemptID
			} else {
				s.compactionUsed = false
				s.awaitingClassification = true
			}
		}
		if err := s.checkStillOpen(); err != nil {
			return err
		}
	case StepModelCallFinishedByHost:
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
		if err := s.validateCompactionSettlement(step.Model); err != nil {
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
			if s.modelIntents[step.Model.AttemptID].Purpose != ModelPurposeCompaction {
				if err := s.recordResult(
					step.Model.AttemptID,
					step.Model.Result,
					!s.Stop.Terminal,
				); err != nil {
					return err
				}
			}
			if s.unresolvedInterrupted {
				s.ModelAttempts++
			}
			if s.modelIntents[step.Model.AttemptID].Purpose == ModelPurposeCompaction {
				s.pendingCompaction = step.Model.AttemptID
			} else {
				s.compactionUsed = false
				s.awaitingClassification = true
			}
		}
		s.unresolvedPartial = false
		s.unresolvedInterrupted = false
		if err := s.checkStillOpen(); err != nil {
			return err
		}
	case StepContextCompacted:
		if err := s.applyContextCompaction(step); err != nil {
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
		if !canonjson.SameValue(step.Tool.Call.Args, v.Call.Args) &&
			!canonjson.SameValue(step.Tool.Call.RequestedArgs, v.Call.Args) {
			return errors.New("tool rewrite lost requested arguments")
		}
		if step.Tool.Definition.Name != "" && !canonjson.SameValue(
			step.Tool.Definition,
			v.Definition,
		) {
			return errors.New("tool decision changed definition")
		}
		if v.Call.Name == SubmitOutputTool &&
			(v.Verdict.Kind != DecisionAllow ||
				v.Verdict.Revision != "dashi-output-v1" ||
				len(step.Tool.Call.RequestedArgs) != 0 ||
				!canonjson.Equal(
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
		if !ok || v.Status != ToolAllowed || !canonjson.SameValue(
			v.Call,
			step.Tool.Call,
		) || (step.Tool.Definition.Name != "" && !canonjson.SameValue(
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
		if !ok || !canonjson.SameValue(
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
	case StepToolFinished:
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
				if err := item.Validate(); err != nil {
					return err
				}
			}
		}
		v, ok := s.Tools[step.Tool.Call.ID]
		if !ok || v.Status == ToolFinished || !canonjson.SameValue(v.Call, step.Tool.Call) {
			return errors.New("tool settlement without pending call")
		}
		if step.Tool.Outcome == nil {
			// No outcome records a result the runner did not observe: a
			// panic, or a start with no result after a crash. A returned
			// error settles with a failed outcome instead. Replay does not
			// second-guess the reason text, so an older or forged record
			// claiming uncertainty for an error still needs Resolve.
			if v.Status != ToolStarted && v.Status != ToolWaiting {
				return errors.New("uncertainty without start")
			}
			v.Status = ToolResultUnknown
		} else {
			if e := step.Tool.Outcome.Validate(); e != nil {
				return e
			}
			if step.Tool.Outcome.State == tool.StateWaiting ||
				step.Tool.Outcome.State == tool.StateDetached {
				return errors.New("wait is not settlement")
			}
			if v.Call.Name == SubmitOutputTool {
				// A steer never skips the submission; the finish point
				// decides a waiting steer for it.
				if step.Tool.Reason == reasonSteering {
					return errors.New("a steer never skips the reserved output tool")
				}
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
			case ToolAllowed:
				// Only an urgent steer keeps an allowed call from starting.
				if step.Tool.Outcome.State != tool.StateNotExecuted ||
					step.Tool.Reason != reasonSteering {
					return errors.New("only a steer settles an allowed call without running it")
				}
			case ToolStarted, ToolWaiting, ToolResultUnknown:
			default:
				return errors.New("tool result without execution or denial")
			}
			if step.Tool.Result != nil {
				// Only a tool's or host's content passes AfterTool.
				if v.Call.Name == SubmitOutputTool ||
					(v.Status != ToolStarted && v.Status != ToolWaiting &&
						v.Status != ToolResultUnknown) ||
					step.Tool.Result.CallID != v.Call.ID {
					return errors.New("result replacement without a tool result")
				}
				if err := validToolResult(*step.Tool.Result); err != nil {
					return err
				}
				v.result = new(llmclone.ToolResult(*step.Tool.Result))
				if sh := step.Tool.Shortened; sh != nil {
					if sh.Limit <= 0 || sh.ShownBytes < 0 ||
						sh.OriginalBytes <= sh.ShownBytes {
						return errors.New("invalid shortened record")
					}
					v.shortened = new(*sh)
				}
			} else if step.Tool.Shortened != nil {
				return errors.New("shortened without a result")
			}
			v.steered = (v.Status == ToolRequested || v.Status == ToolAllowed) &&
				step.Tool.Outcome.State == tool.StateNotExecuted &&
				step.Tool.Reason == reasonSteering
			v.Status = ToolFinished
			v.Outcome = step.Tool.Outcome
		}
		if step.Tool.Result != nil && step.Tool.Outcome == nil {
			return errors.New("result replacement without an outcome")
		}
		s.Tools[v.Call.ID] = v
		if err := s.checkStillOpen(); err != nil {
			return err
		}
	case StepApprovalDecided:
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
	case StepProviderToolApprovalDecided:
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
				v.Status != ToolFinished ||
				v.Outcome == nil ||
				v.Outcome.State != tool.StateSucceeded ||
				s.callAttempts[v.Call.ID] != step.Output.AttemptID ||
				!canonjson.Equal(
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
			if err != nil || !canonjson.Equal(parsed, step.Output.Value) {
				return errors.New("structured output differs from final model text")
			}
		default:
			return errors.New("invalid structured output source")
		}
		value := *step.Output
		value.Value = append(json.RawMessage(nil), value.Value...)
		s.Output = &value
		s.awaitingClassification = false
	case StepTurnStopped:
		if step.Stop == nil {
			return errors.New("missing stop")
		}
		if step.Stop.Projected {
			return errors.New("a recorded stop cannot be projected")
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
			StopCanceled, StopFailed, StopResultUnknown, StopInterrupted:
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
			// Cost and usage completeness determine prefix terminality.
			if err := s.calculateSpend(); err != nil {
				return err
			}
		}
		if s.stopTerminal(*step.Stop) != step.Stop.Terminal {
			return fmt.Errorf(
				"stop terminal=%t, but the turn's state gives %t",
				step.Stop.Terminal,
				s.stopTerminal(*step.Stop),
			)
		}
		if step.Stop.Terminal {
			// The stop is the evidence that calls without a start could not
			// execute. Started calls remain pending for late reconciliation.
			for _, id := range s.callOrder {
				v := s.Tools[id]
				switch v.Status {
				case ToolRequested, ToolApprovalPending, ToolAllowed, ToolDenied:
					v.Status = ToolFinished
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
