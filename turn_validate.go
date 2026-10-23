package dashi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// recordsActiveHooks reports the steps that record ActiveHooks: the first
// step of the commands that run hooks, Start, Continue, and a Resolve with a
// tool result, which runs AfterTool.
func recordsActiveHooks(step Step) bool {
	return step.Kind == StepTurnStarted || step.Kind == StepTurnContinued ||
		step.Kind == StepToolFinished && step.CommandID != ""
}

// hookVerdicts lists the verdicts each hook point can record.
var hookVerdicts = map[HookPoint][]HookVerdict{
	HookBeforeModel: {VerdictChange, VerdictNoChange, VerdictStop, VerdictFailed},
	HookBeforeSend:  {VerdictReserve, VerdictDeny, VerdictLimit, VerdictFailed},
	HookBeforeTool: {
		VerdictAllow,
		VerdictDeny,
		VerdictNeedsApproval,
		VerdictRewrite,
		VerdictFailed,
	},
	HookAfterTool: {VerdictReplace, VerdictNoChange, VerdictFailed},
	HookBeforeFinish: {
		VerdictAddMessages,
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
		a.AfterTool,
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

// validateHidden checks the tools recorded as hidden from a model call:
// named once each, never the submit tool, and not also offered. The
// reserved final-reply call runs no hides, so it records none.
func validateHidden(fact *ModelFact) error {
	if fact.FinalReplyOnly && len(fact.Hidden) > 0 {
		return errors.New("invalid hidden tools")
	}
	seen := map[string]bool{}
	for _, name := range fact.Hidden {
		offered := slices.ContainsFunc(fact.Tools, func(d tool.Definition) bool {
			return d.Name == name
		})
		if name == "" || name == SubmitOutputTool || seen[name] || offered {
			return errors.New("invalid hidden tools")
		}
		seen[name] = true
	}
	return nil
}

// validateReservation checks the BeforeSend reservation terms recorded on a
// model call's start.
func validateReservation(terms map[string]json.RawMessage) error {
	total := 0
	for name, raw := range terms {
		if name == "" || len(raw) == 0 || !json.Valid(raw) {
			return errors.New("invalid model reservation terms")
		}
		total += len(raw)
	}
	if total > maxReservation {
		return errors.New("model reservation terms exceed 16 KiB")
	}
	return nil
}

func validateStepPayload(step Step) error {
	if step.Budget != nil && step.Kind != StepTurnContinued {
		return errors.New("budget raise requires a continuation step")
	}
	if step.ModelCalls != 0 && step.Kind != StepTurnContinued {
		return errors.New("model-call raise requires a continuation step")
	}
	// Warnings ride on whatever step comes next, except the two that
	// describe their own step.
	for _, warning := range step.Warnings {
		valid := false
		switch warning.Code {
		case WarningHistoryMismatch:
			valid = step.Kind == StepTurnContinued
		case WarningBudgetClamp:
			valid = step.Kind == StepModelCallStarted
		case WarningActiveCompactionUnavailable, WarningActiveCompactionBlocked, WarningActiveCompactionMediaEstimateUnavailable, WarningHookFailed, WarningHooksChanged, WarningMessagesDropped,
			WarningSteering, WarningInbox, WarningToolChoiceCleared, WarningHideIgnored,
			WarningToolNotOffered, WarningTaskBudgetAdjusted:
			valid = true
		case WarningLimitAdjusted:
			valid = step.Kind == StepTurnStarted
		case WarningToolDataDropped:
			valid = step.Kind == StepToolFinished || step.Kind == StepToolWaiting
		case WarningSummaryFallback:
			valid = step.Kind == StepTurnStopped && step.Stop != nil && step.Stop.Ended ||
				step.Kind == StepContextCompacted && step.Compacted != nil && step.Compacted.SummaryFallback != ""
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
	if step.Tool != nil && step.Tool.Result != nil &&
		step.Kind != StepToolFinished {
		return errors.New("only a final result records what the model sees")
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
		if step.Kind == StepToolFinished &&
			(step.Tool == nil || step.Tool.Call.Name != SubmitOutputTool) {
			return errors.New("only a rejected submission finishes with steers")
		}
		for _, item := range step.Context.Items {
			if item.ID == "" {
				return errors.New("a steer requires an ID")
			}
		}
	}
	if step.ActiveHooks != nil {
		if !recordsActiveHooks(step) {
			return errors.New("active hooks require a start, continuation, or final result")
		}
		if err := step.ActiveHooks.validate(); err != nil {
			return err
		}
	}
	count := 0
	for _, present := range []bool{
		step.Started != nil,
		step.Compacted != nil,
		step.Context != nil,
		step.Model != nil,
		step.Tool != nil,
		step.Approval != nil,
		step.ProviderToolApproval != nil,
		step.Output != nil,
		step.Stop != nil,
	} {
		if present {
			count++
		}
	}
	expected := 1
	switch step.Kind {
	case StepTurnContinued:
		expected = 0
	case StepLoopContinued:
		if step.Context == nil {
			expected = 0
		}
	case StepToolFinished:
		// A turned-down submission carries the items to add.
		if step.Context != nil {
			expected = 2
		}
	}
	if count != expected {
		return errors.New("step has unexpected payloads")
	}
	switch step.Kind {
	case StepTurnStarted:
		if step.Started == nil {
			return errors.New("start payload missing")
		}
	case StepMessagesAdded, StepLoopContinued:
		if step.Kind == StepMessagesAdded && step.Context == nil {
			return errors.New("context payload missing")
		}
	case StepTurnContinued:
	case StepModelCallStarted, StepModelCallFinished, StepModelCallFinishedByHost:
		if step.Model == nil {
			return errors.New("model payload missing")
		}
		if !step.Model.CapabilityPolicy.Valid() {
			return errors.New("invalid capability policy")
		}
	case StepToolDecision, StepToolStarted, StepToolWaiting, StepToolFinished:
		if step.Tool == nil {
			return errors.New("tool payload missing")
		}
	case StepApprovalDecided:
		if step.Approval == nil {
			return errors.New("approval payload missing")
		}
	case StepProviderToolApprovalDecided:
		if step.ProviderToolApproval == nil {
			return errors.New("provider tool approval payload missing")
		}
	case StepOutputRecorded:
		if step.Output == nil {
			return errors.New("output payload missing")
		}
	case StepContextCompacted:
		if step.Compacted == nil {
			return errors.New("compaction payload missing")
		}
	case StepTurnStopped:
		if step.Stop == nil {
			return errors.New("stop payload missing")
		}
	default:
		return errors.New("unknown step kind")
	}
	return nil
}

func validateMessage(m llm.Message) error {
	if identity := m.Context; identity != nil {
		if identity.Origin == "" || identity.Name == "" {
			return errors.New("message context requires origin and name")
		}
		m.Context = nil
		return ContextItem{
			Origin: ContextOrigin(identity.Origin), Name: identity.Name, Message: m,
		}.Validate()
	}
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

// Validate returns why item cannot enter a turn as context, a steer, or
// queued input, or nil when it can. The rules match Origin to the message
// role and reminder tier, require a Name for a Snapshot, and allow only
// content blocks and reminders. An empty Origin counts as ContextUser.
func (item ContextItem) Validate() error {
	switch cmp.Or(item.Origin, ContextUser) {
	case ContextUser, ContextRuntime, ContextTool, ContextAgent:
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
	// An agent's message names its sender, so the record and the model
	// agree about who spoke.
	if item.Origin == ContextAgent {
		reminder := messageReminder(item.Message)
		if reminder == nil ||
			reminder.Tier != llm.ReminderContextual ||
			reminder.From == "" {
			return errors.New(
				"agent context requires one contextual reminder with a sender",
			)
		}
	}
	return validateMessage(item.Message)
}
