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
	if s.Stop.Ended {
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
				step.Stop.Limit.Kind == LimitResumes ||
				step.Stop.Limit.Kind == LimitModelCalls && s.finalReplyOnly ||
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
		if a := step.Started.Agent; a.Version == "" || a.Provider == "" ||
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
		if err := validateStartInput(origin, step.Started.Input); err != nil {
			return err
		}
		if _, err := newMessageItems(s, step.Started.Context); err != nil {
			return err
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
		s.modelCallCap = s.Agent.Limits.ModelCalls
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
		if !s.canContinueWith(step.Budget, step.ModelCalls) {
			return errors.New("turn cannot continue")
		}
		if step.ModelCalls != 0 {
			if step.ModelCalls <= s.modelCallCap {
				return errors.New("model-call raise must exceed the cap")
			}
			s.modelCallCap = step.ModelCalls
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
			s.Resumes++
		}
		if s.Stop.Kind == StopFailed {
			s.failedModel = ""
		}
		s.Stop = Stop{}
		s.Failure = nil
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
			step.Model.ModelCallID == "" ||
			step.Model.MessageID == "" ||
			step.Model.RequestHash == "" ||
			s.modelCallStart != "" ||
			s.Output != nil ||
			len(s.pending()) > 0 {
			return errors.New("invalid model call start")
		}
		if err := s.validateCompactionCallStart(step); err != nil {
			return err
		}
		if err := validateReservation(step.Model.Reservation); err != nil {
			return err
		}
		if err := validateHidden(step.Model); err != nil {
			return err
		}
		if step.Model.Provider != s.Agent.Provider ||
			step.Model.API != s.Agent.API ||
			step.Model.Model != s.Agent.Model {
			return errors.New("model call start changed destination")
		}
		if step.Model.EstimatedInputTokens < 0 {
			return errors.New("negative estimated input")
		}
		if step.Model.WorstCaseCost != nil {
			if err := step.Model.WorstCaseCost.Validate(); err != nil {
				return err
			}
		}
		if step.Model.FinalReplyOnly &&
			(s.Agent.Limits.NoReservedFinalReply ||
				s.ModelCalls == 0 ||
				s.ModelCalls+1 != s.modelCallCap) {
			return errors.New("final-reply call start is not the last model call")
		}
		s.modelCallStart = step.Model.ModelCallID
		s.startedModelCalls[step.Model.ModelCallID] = step.Model
		s.modelOrder = append(s.modelOrder, step.Model.ModelCallID)
		s.modelCalls++
		s.finalReplyOnly = step.Model.FinalReplyOnly
		s.unresolvedPartial = false
		if _, exists := s.modelMessages[step.Model.ModelCallID]; exists {
			return errors.New("repeated model call ID")
		}
		s.modelMessages[step.Model.ModelCallID] = step.Model.MessageID
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
				return errors.New("final-reply call start offers an application tool")
			}
			s.modelDefs[d.Name] = d
		}
		if len(s.OutputSchema) > 0 && !outputTool && step.Model.Purpose != ModelPurposeCompaction {
			return errors.New("output schema requires reserved output tool")
		}
	case StepModelCallFinished:
		if step.Model == nil ||
			step.Model.ModelCallID == "" ||
			step.Model.ModelCallID != s.modelCallStart ||
			step.Model.MessageID != s.modelMessages[s.modelCallStart] {
			return errors.New("model result without a call start")
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
		if step.Model.SendStatus != "" &&
			step.Model.SendStatus != llm.SendStatusUnknown &&
			step.Model.SendStatus != llm.SendStatusProviderRejected &&
			step.Model.SendStatus != llm.SendStatusNotSent {
			return errors.New("invalid model failure send status")
		}
		if step.Model.ResultUnknown &&
			(step.Model.SendStatus == llm.SendStatusProviderRejected ||
				step.Model.SendStatus == llm.SendStatusNotSent) {
			return errors.New("known model failure marked result unknown")
		}
		if step.Model.Interrupted != InterruptNone &&
			(step.Model.SendStatus == llm.SendStatusProviderRejected ||
				step.Model.SendStatus == llm.SendStatusNotSent) {
			return errors.New("interrupted model call cannot have known nonexecution")
		}
		if step.Model.SendStatus == llm.SendStatusProviderRejected ||
			step.Model.SendStatus == llm.SendStatusNotSent {
			if step.Model.Error == "" ||
				step.Model.Evidence == "" ||
				(step.Model.Result != nil &&
					(step.Model.Result.ID != "" ||
						len(step.Model.Result.Message.Content) != 0 ||
						step.Model.Result.Usage != nil)) {
				return errors.New("known model failure lacks evidence or has partial result")
			}
		}
		if err := s.validateCompactionFinish(step.Model); err != nil {
			return err
		}
		s.modelCallStart = ""
		s.Failure = cloneFailure(step.Model.Failure)
		s.modelFinishes[step.Model.ModelCallID] = step.Model
		if step.Model.Interrupted == InterruptNone {
			s.ModelCalls++
		}
		if step.Model.ResultUnknown {
			s.unresolvedModel = step.Model.ModelCallID
			s.unresolvedInterrupted = step.Model.Interrupted != InterruptNone
			s.unresolvedPartial = step.Model.Result != nil && (step.Model.Result.ID != "" || len(step.Model.Result.Message.Content) > 0 || step.Model.Result.Usage != nil)
		} else if step.Model.Error != "" &&
			step.Model.Interrupted == InterruptNone &&
			step.Model.SendStatus != llm.SendStatusProviderRejected &&
			step.Model.SendStatus != llm.SendStatusNotSent {
			s.failedModel = step.Model.Error
		}
		if step.Model.Result != nil && s.startedModelCalls[step.Model.ModelCallID].Purpose != ModelPurposeCompaction {
			actionable := step.Model.Error == "" &&
				step.Model.Interrupted == InterruptNone &&
				!s.Stop.Ended
			if err := s.recordResult(
				step.Model.ModelCallID,
				step.Model.Result,
				actionable,
			); err != nil {
				return err
			}
		}
		if step.Model.Error == "" &&
			step.Model.Interrupted == InterruptNone {
			if s.startedModelCalls[step.Model.ModelCallID].Purpose == ModelPurposeCompaction {
				s.failedModel = ""
				s.pendingCompaction = step.Model.ModelCallID
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
			step.Model.ModelCallID != s.unresolvedModel ||
			step.Model.MessageID != s.modelMessages[s.unresolvedModel] ||
			(step.Model.Result == nil &&
				step.Model.Error == "") {
			return errors.New("host-supplied model result without an unknown-result call")
		}
		if err := validateModelAccounting(step.Model); err != nil {
			return err
		}
		if err := s.validateCompactionFinish(step.Model); err != nil {
			return err
		}
		s.unresolvedModel = ""
		s.Failure = nil
		s.modelFinishes[step.Model.ModelCallID] = step.Model
		// Proof that the call was never sent clears the unknown result;
		// Continue may retry.
		if step.Model.Error != "" &&
			(step.Model.Result != nil || s.unresolvedPartial ||
				(step.Model.SendStatus != llm.SendStatusProviderRejected &&
					step.Model.SendStatus != llm.SendStatusNotSent) ||
				step.Model.Evidence == "") {
			return errors.New("host-supplied model failure lacks evidence")
		}
		if step.Model.Error == "" {
			if s.startedModelCalls[step.Model.ModelCallID].Purpose != ModelPurposeCompaction {
				if err := s.recordResult(
					step.Model.ModelCallID,
					step.Model.Result,
					!s.Stop.Ended,
				); err != nil {
					return err
				}
			}
			if s.unresolvedInterrupted {
				s.ModelCalls++
			}
			if s.startedModelCalls[step.Model.ModelCallID].Purpose == ModelPurposeCompaction {
				s.pendingCompaction = step.Model.ModelCallID
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
				v.Verdict.RulesVersion != "dashi-output-v1" ||
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
		case DecisionNeedsApproval:
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
			return errors.New("missing tool result")
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
			return errors.New("tool result without pending call")
		}
		if step.Tool.Outcome == nil {
			// No outcome records a result the runner did not observe: a
			// panic, or a start with no result after a crash. A returned
			// error finishes with a failed outcome instead. View does not
			// second-guess the reason text, so an older or forged record
			// claiming an unknown result for an error still needs Resolve.
			if v.Status != ToolStarted && v.Status != ToolWaiting {
				return errors.New("unknown result without start")
			}
			v.Status = ToolResultUnknown
		} else {
			if e := step.Tool.Outcome.Validate(); e != nil {
				return e
			}
			if step.Tool.Outcome.State == tool.StateWaiting ||
				step.Tool.Outcome.State == tool.StateDetached {
				return errors.New("wait is not a final result")
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
					return errors.New("only a steer finishes an allowed call without running it")
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
		if step.Approval.ArgsHash != ArgsHash(v.Call.Args) {
			return errors.New("stale approval")
		}
		// Verdict stays the hooks'; the host's decision is kept beside it,
		// so a denial's reason reaches the model when Continue finishes the
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
		if step.ProviderToolApproval == nil {
			return errors.New("missing provider tool approval")
		}
		a, ok := s.providerTool[step.ProviderToolApproval.RequestID]
		if !ok || a.Origin != step.ProviderToolApproval.Origin {
			return errors.New("provider tool approval without matching request")
		}
		delete(s.providerTool, a.ID)
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
		case OutputSourceToolCall:
			v, ok := s.Tools[step.Output.ToolCallID]
			if !ok ||
				v.Call.Name != SubmitOutputTool ||
				v.Status != ToolFinished ||
				v.Outcome == nil ||
				v.Outcome.State != tool.StateSucceeded ||
				s.modelCallOfTool[v.Call.ID] != step.Output.ModelCallID ||
				!canonjson.Equal(
					v.Call.Args,
					step.Output.Value,
				) {
				return errors.New("structured output does not match an accepted submission")
			}
		case OutputSourceText:
			if step.Output.ToolCallID != "" ||
				step.Output.ModelCallID == "" ||
				step.Output.ModelCallID != s.latestModelCall ||
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
		if step.Stop.Inferred {
			return errors.New("a recorded stop cannot be inferred")
		}
		if step.Stop.Limit != nil {
			if step.Stop.Kind != StopLimited {
				return errors.New("limit detail requires limited stop")
			}
			if err := step.Stop.Limit.Validate(); err != nil {
				return err
			}
		}
		// A candidate reply survives an interruption, and a BeforeFinish
		// failure, so Continue can classify it again without another model
		// call.
		if step.Stop.Kind != StopInterrupted &&
			(step.Stop.Kind != StopFailed ||
				step.Stop.Reason != StopReasonHookFailed) {
			s.awaitingClassification = false
		}
		switch step.Stop.Kind {
		case StopReady:
			if s.LastSeq != 1 || s.modelCalls != 0 ||
				len(s.pending()) != 0 || step.Stop.Reason != "" {
				return errors.New("ready stop requires admission before execution")
			}
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
		if step.Stop.Failure != nil {
			s.Failure = cloneFailure(step.Stop.Failure)
		} else if step.Stop.Kind != StopFailed &&
			step.Stop.Kind != StopResultUnknown {
			s.Failure = nil
		}
		if step.Stop.Kind == StopLimited {
			// Whether cost and usage are complete decides whether the
			// steps so far end the turn.
			if err := s.calculateSpend(); err != nil {
				return err
			}
		}
		if s.stopEnds(*step.Stop) != step.Stop.Ended {
			return fmt.Errorf(
				"stop ended=%t, but the turn's state gives %t",
				step.Stop.Ended,
				s.stopEnds(*step.Stop),
			)
		}
		if step.Stop.Ended {
			// The stop is the evidence that calls without a start could not
			// run. Started calls stay pending until a late result arrives
			// or the host resolves them.
			for _, id := range s.callOrder {
				v := s.Tools[id]
				switch v.Status {
				case ToolRequested, ToolApprovalPending, ToolAllowed, ToolDenied:
					v.Status = ToolFinished
					v.Outcome = &tool.Outcome{State: tool.StateNotExecuted}
					s.Tools[id] = v
				}
			}
			clear(s.providerTool)
		}
	default:
		return fmt.Errorf("unknown step kind %q", step.Kind)
	}
	return nil
}
