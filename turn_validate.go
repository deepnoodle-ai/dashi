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
// step of the commands that run hooks, Start, Continue, and a Settle with a
// tool result, which runs AfterTool.
func recordsActiveHooks(step Step) bool {
	return step.Kind == StepStarted || step.Kind == StepContinued ||
		step.Kind == StepToolSettled && step.CommandID != ""
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
	HookAfterTool: {VerdictReplace, VerdictFailed},
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
// reserved final-answer call runs no hides, so it records none.
func validateHidden(fact *ModelFact) error {
	if fact.FinalAnswerOnly && len(fact.Hidden) > 0 {
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

func validateStepPayload(step Step) error {
	if step.Budget != nil && step.Kind != StepContinued {
		return errors.New("budget raise requires a continuation step")
	}
	if step.ModelAttempts != 0 && step.Kind != StepContinued {
		return errors.New("model-attempt raise requires a continuation step")
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
		case WarningActiveCompactionUnavailable, WarningActiveCompactionBlocked, WarningActiveCompactionMediaEstimateUnavailable, WarningHookFailed, WarningHooksChanged, WarningDeliveryDropped,
			WarningSteering, WarningToolChoiceCleared, WarningHideIgnored,
			WarningToolNotOffered:
			valid = true
		case WarningLimitAdjusted:
			valid = step.Kind == StepStarted
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
		step.Kind != StepToolSettled {
		return errors.New("only a settlement records what the model sees")
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
		if !recordsActiveHooks(step) {
			return errors.New("active hooks require a start, continuation, or settled result")
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
	case StepContextCompacted:
		if step.Compacted == nil {
			return errors.New("compaction payload missing")
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
	if identity := m.Context; identity != nil {
		if identity.Origin == "" || identity.Name == "" {
			return errors.New("message context requires origin and name")
		}
		m.Context = nil
		return validateContext(ContextItem{
			Origin: ContextOrigin(identity.Origin), Name: identity.Name, Message: m,
		})
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

func validateContext(item ContextItem) error {
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
