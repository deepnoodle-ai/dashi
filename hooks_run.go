package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

// maxHookError bounds the hook error text, including a panic's stack, kept
// on the record and in warnings, which are persisted with every turn.
const maxHookError = 4 << 10

// maxHookReason bounds a BeforeFinish reason, which the model sees and the
// record keeps.
const maxHookReason = 1 << 10

// maxRewrites is how many BeforeTool rewrites one call may take before it is
// denied; rewrites must converge.
const maxRewrites = 3

// beforeModel runs the BeforeModel chain, applying each hook's settings and
// hides to input. It returns the items to deliver, the tools hidden from
// this call in the order they were first hidden, or a hook's stop. A hook
// that fails or answers invalidly is skipped with a warning, since
// BeforeModel only shapes the request. The returned Outcome is set when the
// run's context ended during a hook.
func (r *runtime) beforeModel(
	s *replayState,
	input *llm.Input,
	finalAnswerOnly bool,
) ([]ContextItem, []string, *Stop, *Outcome) {
	var deliver []ContextItem
	var hidden []string
	ignored := hideIgnored{}
	bound := map[string]bool{}
	for _, def := range input.Tools {
		bound[def.Name] = true
	}
	mark := r.hookMark()
	for _, h := range r.hooks.BeforeModel {
		d, err := callHook(r.ctx, h, ModelPlan{
			Input: cloneInput(*input),
			Turn:  snapshotView(s.TurnView),
		})
		// The candidate input is this hook's settings and hides applied on
		// top of every earlier hook's, so validation sees what the call
		// would send whatever the hooks' order.
		candidate := cloneInput(*input)
		candidateHidden := hidden
		var warnings []Warning
		hookIgnored := hideIgnored{}
		if err == nil {
			if d.Settings != nil {
				candidate.Settings = *d.Settings
			}
			if !finalAnswerOnly {
				candidateHidden = hideTools(
					&candidate,
					hidden,
					d.Hide,
					bound,
					&hookIgnored,
				)
				warnings = keepToolChoiceOffered(&candidate, h.Name, candidateHidden)
			}
			err = validModelDecision(d, candidate)
		}
		if err != nil {
			if r.ctx.Err() != nil {
				r.dropHooks(mark)
				o := r.contextStop()
				return nil, nil, nil, &o
			}
			r.hookFailed(HookBeforeModel, h.Name, err, true)
			continue
		}
		if d.Stop != nil {
			r.addHookResult(HookResult{
				Point:   HookBeforeModel,
				Name:    h.Name,
				Verdict: VerdictStop,
				Reason:  d.Stop.Reason,
			})
			stop := *d.Stop
			if stop.Limit != nil {
				limit := *stop.Limit
				stop.Limit = &limit
			}
			return nil, nil, &stop, nil
		}
		ignored.merge(hookIgnored)
		r.pendingWarnings = append(r.pendingWarnings, warnings...)
		hid := len(candidateHidden) > len(hidden)
		if d.Settings == nil && len(d.Deliver) == 0 && !hid {
			continue
		}
		input.Settings = candidate.Settings
		input.Tools = candidate.Tools
		hidden = candidateHidden
		for _, item := range d.Deliver {
			deliver = append(deliver, cloneContextItem(item))
		}
		r.addHookResult(HookResult{
			Point:   HookBeforeModel,
			Name:    h.Name,
			Verdict: VerdictChange,
		})
	}
	if w, ok := ignored.warning(); ok {
		r.pendingWarnings = append(r.pendingWarnings, w)
	}
	return deliver, hidden, nil, nil
}

// hideTools removes names from in.Tools and returns the hidden names so
// far, earlier hides first. dashi_submit_output is never hidden, and a name
// no tool of this call has is noted in ignored; a name an earlier hook
// already hid is not.
func hideTools(
	in *llm.Input,
	hidden []string,
	names []string,
	bound map[string]bool,
	ignored *hideIgnored,
) []string {
	out := slices.Clone(hidden)
	hide := map[string]bool{}
	for _, name := range names {
		switch {
		case name == SubmitOutputTool:
			ignored.submit = true
		case !bound[name]:
			ignored.add(name)
		case !slices.Contains(out, name):
			out = append(out, name)
			hide[name] = true
		}
	}
	if len(hide) == 0 {
		return out
	}
	kept := make([]tool.Definition, 0, len(in.Tools))
	for _, def := range in.Tools {
		if !hide[def.Name] {
			kept = append(kept, def)
		}
	}
	in.Tools = kept
	return out
}

// keepToolChoiceOffered clears a forced tool choice that names a hidden
// tool, whichever hook hid it: the hide wins. When no tools remain, the tool
// choice and ParallelTools are cleared, as on the reserved final-answer
// call, since providers reject them without tools. Only losing a forced
// choice warns.
func keepToolChoiceOffered(
	in *llm.Input,
	hook string,
	hidden []string,
) []Warning {
	choice := in.Settings.ToolChoice
	forced := false
	if choice != nil {
		named := choice.Mode == llm.ToolChoiceTool &&
			slices.Contains(hidden, choice.Name)
		forced = named ||
			len(in.Tools) == 0 && len(hidden) > 0 &&
				(choice.Mode == llm.ToolChoiceAny || choice.Mode == llm.ToolChoiceTool)
		if forced {
			in.Settings.ToolChoice = nil
		}
	}
	if len(in.Tools) == 0 && len(hidden) > 0 {
		in.Settings.ToolChoice = nil
		in.Settings.ParallelTools = nil
	}
	if !forced {
		return nil
	}
	return []Warning{{
		Code: WarningToolChoiceCleared,
		Message: fmt.Sprintf(
			"after before_model hook %q, the forced tool choice named a "+
				"hidden tool or no tools remained; it was cleared for this call",
			hook,
		),
	}}
}

// hideIgnored collects the hide names a call could not apply, for one
// warning per call.
type hideIgnored struct {
	submit  bool
	unknown []string
}

func (h *hideIgnored) add(name string) {
	if !slices.Contains(h.unknown, name) {
		h.unknown = append(h.unknown, name)
	}
}

func (h *hideIgnored) merge(o hideIgnored) {
	h.submit = h.submit || o.submit
	for _, name := range o.unknown {
		h.add(name)
	}
}

func (h hideIgnored) warning() (Warning, bool) {
	var parts []string
	if h.submit {
		parts = append(parts, SubmitOutputTool+" cannot be hidden")
	}
	if len(h.unknown) > 0 {
		parts = append(parts, fmt.Sprintf(
			"no tool is named %s",
			strings.Join(h.unknown, ", "),
		))
	}
	if len(parts) == 0 {
		return Warning{}, false
	}
	return Warning{
		Code: WarningHideIgnored,
		Message: "before_model hide names were ignored: " +
			strings.Join(parts, "; "),
	}, true
}

// validModelDecision checks a BeforeModel answer before any of it applies,
// including its settings against the rest of the input, so an invalid
// answer skips the hook instead of failing request preparation.
func validModelDecision(d ModelDecision, input llm.Input) error {
	if d.Settings != nil || len(d.Hide) > 0 {
		if err := input.Validate(); err != nil {
			return fmt.Errorf("invalid settings or hide: %w", err)
		}
	}
	if d.Stop != nil && !validHookStop(*d.Stop) {
		return errors.New(
			"invalid stop: a hook may refuse, fail, or report a host, cost, " +
				"or token limit, and may not use a reserved stop reason",
		)
	}
	for _, item := range d.Deliver {
		if err := validateContext(item); err != nil {
			return fmt.Errorf("invalid context item: %w", err)
		}
	}
	return nil
}

// beforeSend runs the BeforeSend chain for a prepared request. It returns
// the admissions by hook name, or the stop that ends the run: a hook's
// denial, or StopReasonHookFailed when a hook fails, since BeforeSend guards
// spending.
func (r *runtime) beforeSend(
	s *replayState,
	req llm.Request,
	atRisk *llm.Cost,
	remaining *Money,
) (map[string]json.RawMessage, *Stop, *Outcome) {
	var admission map[string]json.RawMessage
	total := 0
	mark := r.hookMark()
	for _, h := range r.hooks.BeforeSend {
		plan := SendPlan{Request: req, Turn: snapshotView(s.TurnView)}
		if atRisk != nil {
			plan.AtRisk = new(*atRisk)
		}
		if remaining != nil {
			plan.Remaining = new(*remaining)
		}
		d, err := callHook(r.ctx, h, plan)
		if err == nil {
			err = validSendDecision(d, total)
		}
		if err != nil {
			if r.ctx.Err() != nil {
				r.dropHooks(mark)
				o := r.contextStop()
				return nil, nil, &o
			}
			r.hookFailed(HookBeforeSend, h.Name, err, false)
			return nil, hookFailedStop(), nil
		}
		if d.Deny || d.Limit != nil {
			result := HookResult{
				Point:   HookBeforeSend,
				Name:    h.Name,
				Verdict: VerdictDeny,
				Reason:  d.Reason,
			}
			stop := &Stop{Kind: StopRefused, Reason: d.Reason}
			if d.Limit != nil {
				result.Verdict = VerdictLimit
				limit := *d.Limit
				stop = &Stop{Kind: StopLimited, Reason: d.Reason, Limit: &limit}
			}
			r.addHookResult(result)
			return nil, stop, nil
		}
		if len(d.Admission) > 0 {
			if admission == nil {
				admission = map[string]json.RawMessage{}
			}
			admission[h.Name] = slices.Clone(d.Admission)
			total += len(d.Admission)
			r.addHookResult(HookResult{
				Point:   HookBeforeSend,
				Name:    h.Name,
				Verdict: VerdictAdmit,
			})
		}
	}
	return admission, nil, nil
}

func validSendDecision(d SendDecision, used int) error {
	if d.Limit != nil && !validHookLimit(*d.Limit) {
		return errors.New(
			"invalid limit: a hook may report a host, cost, or token limit",
		)
	}
	if len(d.Admission) > 0 && !json.Valid(d.Admission) {
		return errors.New("admission is not valid JSON")
	}
	if used+len(d.Admission) > maxAdmission {
		return errors.New("admissions exceed 16 KiB")
	}
	return nil
}

// authorizeTool runs the BeforeTool chain for a call whose arguments match
// its schema, and returns the call as rewritten and the combined verdict. A
// failing hook denies the call, since BeforeTool guards execution, and the
// model sees only the hook's name. The returned Outcome is set when the
// run's context ended during a hook.
//
// Steering's check for a pending urgent steer belongs just before this call
// in the tools loop, so no hook runs for a call that will not start.
func (r *runtime) authorizeTool(
	ctx context.Context,
	def tool.Definition,
	call tool.Call,
	ex tool.Executor,
) (tool.Call, ToolVerdict, *Outcome) {
	pass := 0
	mark := r.hookMark()
	// A mid-run view: its Stop stays empty, unlike Replay's. Nothing is
	// committed during the chain, so one replay serves every hook.
	view := TurnView{}
	if current, err := replay(r.turn); err == nil {
		view = current.TurnView
	}
restart:
	var allow, deferred *ToolVerdict
	for _, h := range r.hooks.BeforeTool {
		d, err := callHook(ctx, h, ToolPlan{
			Call:       call.Clone(),
			Definition: def.Clone(),
			Turn:       snapshotView(view),
		})
		if err == nil {
			err = validToolDecision(d, def, call, ex)
		}
		if err != nil {
			if ctx.Err() != nil {
				r.dropHooks(mark)
				o := r.contextStop()
				return call, ToolVerdict{}, &o
			}
			r.hookFailed(HookBeforeTool, h.Name, err, true)
			return call, ToolVerdict{
				Kind:   DecisionDeny,
				Reason: fmt.Sprintf("Denied: check %s failed.", h.Name),
				Hook:   h.Name,
			}, nil
		}
		if len(d.Rewrite) > 0 {
			if ArgsDigest(d.Rewrite) == ArgsDigest(call.Args) {
				continue
			}
			r.addHookResult(HookResult{
				Point:    HookBeforeTool,
				Name:     h.Name,
				Verdict:  VerdictRewrite,
				Pass:     pass,
				Revision: d.Revision,
				Reason:   d.Reason,
			})
			if pass == maxRewrites {
				r.pendingWarnings = append(r.pendingWarnings, Warning{
					Code: WarningHookFailed,
					Message: fmt.Sprintf(
						"call %s denied: BeforeTool rewrites did not converge after %d restarts",
						call.ID,
						maxRewrites,
					),
				})
				return call, ToolVerdict{
					Kind:   DecisionDeny,
					Reason: "Not run: a check kept changing the arguments.",
					Hook:   h.Name,
				}, nil
			}
			if len(call.RequestedArgs) == 0 {
				call.RequestedArgs = call.Args
			}
			call.Args = slices.Clone(d.Rewrite)
			pass++
			goto restart
		}
		if d.Kind == "" {
			continue
		}
		r.addHookResult(HookResult{
			Point:    HookBeforeTool,
			Name:     h.Name,
			Verdict:  HookVerdict(d.Kind),
			Pass:     pass,
			Revision: d.Revision,
			Reason:   d.Reason,
		})
		verdict := ToolVerdict{
			Kind:     d.Kind,
			Reason:   d.Reason,
			Hook:     h.Name,
			Revision: d.Revision,
		}
		switch d.Kind {
		case DecisionDeny:
			return call, verdict, nil
		case DecisionDefer:
			if deferred == nil {
				deferred = &verdict
			}
		case DecisionAllow:
			if allow == nil {
				allow = &verdict
			}
		}
	}
	switch {
	case deferred != nil:
		return call, *deferred, nil
	case allow != nil:
		return call, *allow, nil
	}
	return call, ToolVerdict{Kind: DecisionAllow, Reason: "no hook opinion"}, nil
}

func validToolDecision(
	d ToolDecision,
	def tool.Definition,
	call tool.Call,
	ex tool.Executor,
) error {
	if len(d.Rewrite) > 0 {
		if !json.Valid(d.Rewrite) {
			return errors.New("rewrite is not valid JSON")
		}
		call.Args = d.Rewrite
		if err := validateToolCall(def, call); err != nil {
			return fmt.Errorf("rewrite does not match the tool schema: %w", err)
		}
		if err := validateBoundArguments(ex, call.Args); err != nil {
			return fmt.Errorf("rewrite does not match tool arguments: %w", err)
		}
		return nil
	}
	switch d.Kind {
	case "", DecisionAllow, DecisionDeny, DecisionDefer:
		return nil
	}
	return fmt.Errorf("invalid decision kind %q", d.Kind)
}

// finishResult is the combined BeforeFinish answer. A reason is a veto: the
// answer is unacceptable. Items alone are a nudge: try once more.
type finishResult struct {
	// steers supersede the candidate before the hooks see it.
	steers      []ContextItem
	deliver     []ContextItem
	acceptEmpty bool
	// reason is the first nonempty Reason, bounded; hook is the first hook
	// that turned the answer down.
	reason string
	hook   string
	// noCall means the hooks vetoed the answer and no further model call can
	// happen: the caller records the rejection and stops the turn with
	// StopReasonOutputRejected.
	noCall bool
}

// beforeFinish runs the BeforeFinish chain for a candidate answer. A failing
// hook stops the turn, since BeforeFinish guards what the turn accepts; the
// candidate survives the stop, so Continue asks the hooks again.
func (r *runtime) beforeFinish(plan FinishPlan) (finishResult, *Outcome) {
	var out finishResult
	mark := r.hookMark()
	for _, h := range r.hooks.BeforeFinish {
		p := plan
		p.Turn = snapshotView(plan.Turn)
		p.Output = slices.Clone(plan.Output)
		d, err := callHook(r.ctx, h, p)
		if err == nil {
			for _, item := range d.Deliver {
				if e := validateContext(item); e != nil {
					err = fmt.Errorf("invalid context item: %w", e)
					break
				}
			}
		}
		if err != nil {
			if r.ctx.Err() != nil {
				r.dropHooks(mark)
				o := r.contextStop()
				return finishResult{}, &o
			}
			if h.FailOpen {
				r.hookFailed(HookBeforeFinish, h.Name, err, true)
				continue
			}
			r.hookFailed(HookBeforeFinish, h.Name, err, false)
			o := r.stopWith(hookFailedStop())
			return finishResult{}, &o
		}
		if len(d.Deliver) > 0 || d.Reason != "" {
			reason := boundedReason(d.Reason)
			for _, item := range d.Deliver {
				out.deliver = append(out.deliver, cloneContextItem(item))
			}
			if out.hook == "" {
				out.hook = h.Name
			}
			if out.reason == "" {
				out.reason = reason
			}
			verdict := VerdictDeliver
			if reason != "" {
				verdict = VerdictReject
			}
			r.addHookResult(HookResult{
				Point:   HookBeforeFinish,
				Name:    h.Name,
				Verdict: verdict,
				Reason:  reason,
			})
		}
		if d.AcceptEmpty {
			out.acceptEmpty = true
			r.addHookResult(HookResult{
				Point:   HookBeforeFinish,
				Name:    h.Name,
				Verdict: VerdictAcceptEmpty,
			})
		}
	}
	return out, nil
}

// finishPoint is the one place a candidate final answer is decided, on both
// the final-text path and the dashi_submit_output path. Steering's check for
// waiting steers belongs first here, before the BeforeFinish hooks: a steer
// supersedes a candidate without asking the hooks about it, recorded with
// source sourceSteering.
//
// When the hooks turn the candidate down, the caller records the rejection
// and the model is asked again. When no further normal model call can
// happen, a nudge (items alone) is dropped with a warning and the candidate
// stands, since a finish hook never spends the last answer; a veto (a
// reason) is still recorded, and the turn stops with
// StopReasonOutputRejected instead of accepting the answer.
func (r *runtime) finishPoint(
	s *replayState,
	plan FinishPlan,
) (finishResult, *Outcome) {
	// A steer is a nudge: it supersedes the candidate only when a further
	// model call can happen, and otherwise waits with its source.
	if steers := r.waitingSteers(s); len(steers) > 0 {
		if r.nextCallPossible(s, estimatedItemTokens(steers)) {
			return finishResult{steers: steers}, nil
		}
		r.steersWait(s, len(steers))
	}
	mark := r.hookMark()
	finish, o := r.beforeFinish(plan)
	if o != nil {
		return finishResult{}, o
	}
	if r.ctx.Err() != nil {
		r.dropHooks(mark)
		o := r.contextStop()
		return finishResult{}, &o
	}
	// Items the turn already records are not new: a nudge made only of
	// them is no nudge, and costs no model call.
	finish.deliver = r.skipRecordedItems(finish.deliver)
	if !finish.rejects() {
		return finish, nil
	}
	extra := estimatedItemTokens(finish.deliver) + len(finish.reason)
	if finish.reason != "" {
		// Once the answer is vetoed, the reserve protects nothing, so the
		// reserved final-answer call may answer again.
		finish.noCall = !r.callFits(s, extra, true)
		return finish, nil
	}
	if r.nextCallFits(s, extra) {
		return finish, nil
	}
	r.pendingWarnings = append(r.pendingWarnings, Warning{
		Code: WarningDeliveryDropped,
		Message: fmt.Sprintf(
			"BeforeFinish hook %s asked for another model call, but none "+
				"can happen before the reserved final answer or a limit; "+
				"the answer stands",
			finish.hook,
		),
	})
	return finishResult{acceptEmpty: finish.acceptEmpty}, nil
}

// rejects reports whether the hooks turned the candidate down.
func (f finishResult) rejects() bool {
	return len(f.deliver) > 0 || f.reason != ""
}

// notAccepted is the model-visible text of a turned-down answer.
func notAccepted(reason string) string {
	return "Not accepted: " + reason
}

// sourceSteering is the source a steer records when it turns a submission
// down through rejectSubmission.
const sourceSteering = "steering"

// deliveryDropped warns that context delivered while an answer was kept
// cannot reach the model, because no further model call can happen.
func (r *runtime) deliveryDropped() {
	r.pendingWarnings = append(r.pendingWarnings, Warning{
		Code: WarningDeliveryDropped,
		Message: "context delivered after the answer was kept cannot " +
			"reach the model: no further model call can happen, so the " +
			"answer is checked again",
	})
}

// rejectSubmission settles a started dashi_submit_output call that a hook
// or a steer turned down, as failed with the model-visible output
// "Not accepted: <reason>". The items to deliver ride on the same step, so
// a crash cannot record the rejection without them; projection places them
// after the call's result. source names who rejected it for the record,
// such as "hook <name>" or "steering". It is the same started-to-settled
// transition a schema rejection uses, so the next successful submission is
// the one recorded as output.
//
// vetoed records a BeforeFinish veto, which keeps the text of the model
// message that made the call out of TurnView.Answer until a new model result
// arrives.
func (r *runtime) rejectSubmission(
	call tool.Call,
	reason string,
	source string,
	items []ContextItem,
	vetoed bool,
) *Outcome {
	out := tool.Outcome{
		State:  tool.StateFailed,
		Output: new(tool.TextOutput(notAccepted(reason))),
	}
	step := Step{
		Kind: StepToolSettled,
		Tool: &ToolFact{
			Call:    call,
			Outcome: &out,
			Reason:  "not accepted: " + source,
			Vetoed:  vetoed,
		},
	}
	if len(items) > 0 {
		step.Context = &ContextFact{
			Items:    items,
			Steering: source == sourceSteering,
		}
	}
	return r.commit(step)
}

// nextCallPossible is nextCallFits with a pause also refusing: steering
// uses it, since a steer not delivered stays with its source.
func (r *runtime) nextCallPossible(s *replayState, extraInputTokens int) bool {
	return !r.pauseRequested() && r.nextCallFits(s, extraInputTokens)
}

// nextCallFits reports whether a further normal model call can happen: one
// that still leaves the reserved final-answer call available (or fits in
// ModelAttempts when there is no reserve), before the deadline, with the
// context live, and within every Budget cap for its worst case.
// extraInputTokens is the size of input the caller would add, such as
// delivered items, estimated as Dashi estimates request input. A pause is
// not a reason to refuse: the run pauses before the next model call anyway,
// so a pause must never let a rejected answer through.
//
// A model call in flight already counts as used, so the check holds while a
// call runs. The cost term estimates the next call from the latest recorded
// request, grown by what the model and tools returned since and by
// extraInputTokens, priced like BeforeSend's check; a cost cap that cannot
// be priced fails it, since that call would stop anyway.
func (r *runtime) nextCallFits(s *replayState, extraInputTokens int) bool {
	return r.callFits(s, extraInputTokens, false)
}

// callFits is nextCallFits, with the reserved final-answer call counted as
// available when reserved is true: any call that fits ModelAttempts.
func (r *runtime) callFits(
	s *replayState,
	extraInputTokens int,
	reserved bool,
) bool {
	if reserved {
		if !s.roomForAnyCall() {
			return false
		}
	} else if !s.roomForNormalCall() {
		return false
	}
	return r.callBlocked(s, extraInputTokens) == ""
}

// callBlocked is callFits without the ModelAttempts term: it reports why no
// further model call fits the deadline, the run's context, and every Budget
// cap, or "" when one does. A model call in flight counts at its worst
// case against a cost cap, and a cap it cannot price blocks the call. It
// reads only r's immutable fields and s, so the steering listener can call
// it on a state captured for it.
func (r *runtime) callBlocked(s *replayState, extraInputTokens int) string {
	if d := r.agent.Limits.Deadline; !d.IsZero() && !time.Now().Before(d) {
		return "the turn deadline passed"
	}
	if r.ctx.Err() != nil {
		return "the run is ending"
	}
	if budgetSpentStop(s) != nil {
		return "the turn's budget is spent"
	}
	if s.Budget == nil || s.Budget.MaxCost == nil {
		return ""
	}
	req, ok := estimatedNextRequest(s, extraInputTokens)
	if !ok {
		return "the next call cannot be priced under the cost cap"
	}
	spent, ok := spendWithCallInFlight(s)
	if !ok {
		return "the call's worst-case cost is unknown under the cost cap"
	}
	if _, _, stop := checkSendBudget(spent, req, r.agent.Model); stop != nil {
		return "a replacement call would not fit the cost cap"
	}
	return ""
}

// spendWithCallInFlight returns s with a model call in flight counted at its
// worst case. Recorded spend counts an unsettled call at its estimated input
// only, and the next call must fit beside everything the running one could
// still cost. It fails when that worst case is unknown.
func spendWithCallInFlight(s *replayState) (*replayState, bool) {
	if s.modelIntent == "" {
		return s, true
	}
	intent := s.modelIntents[s.modelIntent]
	limit := s.Budget.MaxCost
	if intent == nil || intent.AtRisk == nil ||
		intent.AtRisk.Currency != limit.Currency {
		return nil, false
	}
	counted := money.Amount(0)
	if _, cost, _, _ := attemptSpend(intent, nil); cost != nil {
		if cost.Currency != limit.Currency {
			return nil, false
		}
		counted = cost.Total
	}
	used, known := s.Spend.costIn(limit.Currency)
	if !known {
		return nil, false
	}
	total, ok := used.Add(intent.AtRisk.Total - counted)
	if !ok {
		return nil, false
	}
	adjusted := *s
	adjusted.Spend = s.Spend.Clone()
	if adjusted.Spend.Cost == nil {
		adjusted.Spend.Cost = map[string]money.Amount{}
	}
	adjusted.Spend.Cost[limit.Currency] = total
	return &adjusted, true
}

// estimatedNextRequest stands in for the next model request when pricing it
// before it exists. Its base is the latest real request: the call in flight,
// if any, or else the one that produced the latest result. What the next
// request adds is counted at twice its JSON size: the model's output since,
// bounded by the in-flight call's max_tokens or measured from the recorded
// result and tool outcomes, plus extraInputTokens. The doubling covers
// providers that encode such content once more, such as tool arguments sent
// as a JSON string, so the estimate stays an upper bound; the base is exact.
func estimatedNextRequest(s *replayState, extraInputTokens int) (llm.Request, bool) {
	grown := max(extraInputTokens, 0)
	var intent *ModelFact
	if s.modelIntent != "" {
		intent = s.modelIntents[s.modelIntent]
		if intent != nil {
			grown += max(intent.Effective.MaxTokens, 0)
		}
	} else {
		intent = s.modelIntents[s.latestAttempt]
		if s.latestResult != nil {
			if b, err := json.Marshal(s.latestResult.Message); err == nil {
				grown += len(b)
			}
		}
		for _, id := range s.callOrder {
			if s.callAttempts[id] != s.latestAttempt {
				continue
			}
			if o := s.Tools[id].Outcome; o != nil {
				if b, err := json.Marshal(o); err == nil {
					grown += len(b)
				}
			}
		}
	}
	if intent == nil {
		return llm.Request{}, false
	}
	size := intent.EstimatedInputTokens + 2*grown
	// EstimatedInputTokens is the body size plus 1024, so the body carries
	// the rest; a JSON string keeps it valid.
	bodySize := max(size-1024, 2)
	body := make([]byte, bodySize)
	body[0] = '"'
	for i := 1; i < bodySize-1; i++ {
		body[i] = 'x'
	}
	body[bodySize-1] = '"'
	req, err := llm.NewRequest(llm.RequestSnapshot{
		Origin: intent.Origin,
		Input: llm.Input{
			Model:    intent.Model,
			Messages: []llm.Message{llm.UserMessage(llm.Text("estimate"))},
			Tools:    intent.Tools,
			Settings: intent.Effective,
		},
		Effective: intent.Effective,
		Body:      body,
		ModelInfo: intent.ModelInfo,
	})
	return req, err == nil
}

// estimatedItemTokens sizes context items the way Dashi estimates request
// input: one token per byte of their encoding, an upper bound.
func estimatedItemTokens(items []ContextItem) int {
	n := 0
	for _, item := range items {
		if b, err := json.Marshal(item.Message); err == nil {
			n += len(b)
		}
	}
	return n
}

// afterTool settles what the model sees of a tool or host result about to be
// recorded. It runs the AfterTool chain, then applies the result limit, and
// sets fact.Result when either changed the view. The limit runs last, so a
// hook sees the full text and cannot bring the flood back.
func (r *runtime) afterTool(ctx context.Context, fact *ToolFact) {
	if fact.Outcome == nil {
		return
	}
	base := modelToolResult(fact.Call.ID, fact.Outcome)
	current := base
	hooked := len(r.hooks.AfterTool) > 0
	if hooked {
		current = r.afterToolHooks(ctx, fact, base)
	}
	limited, facts, short := shortenResult(
		current,
		r.agent.Limits.ToolResultBytes,
	)
	if !hooked && !short {
		// Nothing can differ from base, so skip the copy and the compare.
		return
	}
	if short {
		current = limited
		fact.Shortened = &facts
	}
	if !sameJSONValue(current, base) {
		current.CallID = fact.Call.ID
		fact.Result = &current
	}
}

// afterToolHooks runs the AfterTool chain and returns the view it leaves.
// It fails closed: a failing hook, or a run context that ends during the
// chain while a hook still to run fails closed, withholds the result from
// the model. The outcome and its cost are recorded either way, since the
// call already finished.
func (r *runtime) afterToolHooks(
	ctx context.Context,
	fact *ToolFact,
	base llm.ToolResult,
) llm.ToolResult {
	current := cloneToolResult(base)
	view := TurnView{}
	if s, err := replay(r.turn); err == nil {
		view = s.TurnView
	}
	def := view.Tools[fact.Call.ID].Definition
	mark := r.hookMark()
	for i, h := range r.hooks.AfterTool {
		d, err := callHook(ctx, h, ResultPlan{
			Call:       fact.Call.Clone(),
			Definition: def.Clone(),
			Outcome:    fact.Outcome.Clone(),
			Result:     cloneToolResult(current),
			Turn:       snapshotView(view),
		})
		var replace llm.ToolResult
		if err == nil && d.Replace != nil {
			replace = cloneToolResult(*d.Replace)
			replace.CallID = fact.Call.ID
			err = validToolResult(replace)
		}
		if err != nil {
			if ctx.Err() != nil {
				// The rest of the chain cannot run. Withhold only when a
				// hook that has not answered would fail closed.
				withhold := slices.ContainsFunc(
					r.hooks.AfterTool[i:],
					func(h ResultHook) bool { return !h.FailOpen },
				)
				what := "the result so far was kept"
				if withhold {
					r.dropHooks(mark)
					current = withheldResult(*fact.Outcome, fmt.Sprintf(
						"check %q did not finish",
						h.Name,
					))
					what = "the result was withheld"
				}
				r.pendingWarnings = append(r.pendingWarnings, Warning{
					Code: WarningHookFailed,
					Message: fmt.Sprintf(
						"after_tool hook %q did not finish before the run "+
							"ended; for call %s, %s",
						h.Name,
						fact.Call.ID,
						what,
					),
				})
				break
			}
			r.hookFailed(HookAfterTool, h.Name, err, true)
			if h.FailOpen {
				continue
			}
			current = withheldResult(*fact.Outcome, fmt.Sprintf(
				"check %q failed",
				h.Name,
			))
			break
		}
		if d.Replace == nil || sameJSONValue(replace, current) {
			continue
		}
		current = replace
		r.addHookResult(HookResult{
			Point:   HookAfterTool,
			Name:    h.Name,
			Verdict: VerdictReplace,
		})
	}
	return current
}

// withheldResult is what the model sees when a result hook failed. It is not
// marked as an error, and for a call that ran it says so, so the model does
// not retry a call that may have taken effect.
func withheldResult(o tool.Outcome, why string) llm.ToolResult {
	lead := "The tool ran, but its result was withheld: "
	if o.State == tool.StateNotExecuted {
		lead = "The call finished without running, and its result was withheld: "
	}
	return llm.ToolResult{Output: tool.TextOutput(lead + why + ".")}
}

func validToolResult(result llm.ToolResult) error {
	part := llm.Content{Kind: llm.ContentToolResult, ToolResult: &result}
	if err := part.Validate(); err != nil {
		return fmt.Errorf("invalid replacement: %w", err)
	}
	return nil
}

// hookMark and dropHooks bracket one hook chain. When the run's context ends
// during a chain, no decision was made, so the answers already given are
// dropped: the chain runs again on resume and records them then.
type chainMark struct{ hooks, warnings int }

func (r *runtime) hookMark() chainMark {
	return chainMark{hooks: len(r.pendingHooks), warnings: len(r.pendingWarnings)}
}

func (r *runtime) dropHooks(m chainMark) {
	r.pendingHooks = r.pendingHooks[:m.hooks]
	r.pendingWarnings = r.pendingWarnings[:m.warnings]
}

func (r *runtime) addHookResult(result HookResult) {
	r.pendingHooks = append(r.pendingHooks, result)
}

// hookFailed records a failed hook, and a warning when the turn works around
// the failure instead of stopping.
func (r *runtime) hookFailed(
	point HookPoint,
	name string,
	err error,
	warn bool,
) {
	text := boundedHookError(err)
	r.addHookResult(HookResult{
		Point:   point,
		Name:    name,
		Verdict: VerdictFailed,
		Error:   text,
	})
	if warn {
		r.pendingWarnings = append(r.pendingWarnings, Warning{
			Code:    WarningHookFailed,
			Message: fmt.Sprintf("%s hook %s failed: %s", point, name, text),
		})
	}
}

func hookFailedStop() *Stop {
	return &Stop{Kind: StopFailed, Reason: StopReasonHookFailed}
}

// boundedReason makes a reason valid UTF-8, then cuts it at a rune boundary
// so it never exceeds maxHookReason bytes.
func boundedReason(reason string) string {
	reason = strings.ToValidUTF8(reason, "\uFFFD")
	if len(reason) <= maxHookReason {
		return reason
	}
	cut := maxHookReason
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

func boundedHookError(err error) string {
	text := err.Error()
	if text == "" {
		text = "error"
	}
	if len(text) > maxHookError {
		text = text[:maxHookError]
	}
	return strings.ToValidUTF8(text, "\uFFFD")
}
