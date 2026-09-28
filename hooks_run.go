package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

// maxHookError bounds the hook error text, including a panic's stack, kept
// on the record and in warnings, which are persisted with every turn.
const maxHookError = 4 << 10

// maxRewrites is how many BeforeTool rewrites one call may take before it is
// denied; rewrites must converge.
const maxRewrites = 3

// beforeModel runs the BeforeModel chain, applying each hook's settings to
// input. It returns the items to deliver or a hook's stop. A hook that fails
// or answers invalidly is skipped with a warning, since BeforeModel only
// shapes the request. The returned Outcome is set when the run's context
// ended during a hook.
func (r *runtime) beforeModel(
	s *replayState,
	input *llm.Input,
) ([]ContextItem, *Stop, *Outcome) {
	var deliver []ContextItem
	mark := r.hookMark()
	for _, h := range r.hooks.BeforeModel {
		d, err := callHook(r.ctx, h, ModelPlan{
			Input: cloneInput(*input),
			Turn:  snapshotView(s.TurnView),
		})
		if err == nil {
			err = validModelDecision(d, *input)
		}
		if err != nil {
			if r.ctx.Err() != nil {
				r.dropHooks(mark)
				o := r.contextStop()
				return nil, nil, &o
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
			return nil, &stop, nil
		}
		if d.Settings == nil && len(d.Deliver) == 0 {
			continue
		}
		if d.Settings != nil {
			input.Settings = *d.Settings
		}
		for _, item := range d.Deliver {
			deliver = append(deliver, cloneContextItem(item))
		}
		r.addHookResult(HookResult{
			Point:   HookBeforeModel,
			Name:    h.Name,
			Verdict: VerdictChange,
		})
	}
	return deliver, nil, nil
}

// validModelDecision checks a BeforeModel answer before any of it applies,
// including its settings against the rest of the input, so an invalid
// answer skips the hook instead of failing request preparation.
func validModelDecision(d ModelDecision, input llm.Input) error {
	if d.Settings != nil {
		input.Settings = *d.Settings
		if err := input.Validate(); err != nil {
			return fmt.Errorf("invalid settings: %w", err)
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
			err = validToolDecision(d, def, call)
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
) error {
	if len(d.Rewrite) > 0 {
		if !json.Valid(d.Rewrite) {
			return errors.New("rewrite is not valid JSON")
		}
		call.Args = d.Rewrite
		if err := validateToolCall(def, call); err != nil {
			return fmt.Errorf("rewrite does not match the tool schema: %w", err)
		}
		return nil
	}
	switch d.Kind {
	case "", DecisionAllow, DecisionDeny, DecisionDefer:
		return nil
	}
	return fmt.Errorf("invalid decision kind %q", d.Kind)
}

// finishResult is the combined BeforeFinish answer. rejected means a hook
// turned the candidate down: it delivered items, or gave a reason, which
// only a submission can act on alone.
type finishResult struct {
	deliver     []ContextItem
	acceptEmpty bool
	reason      string
	hook        string
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
			r.hookFailed(HookBeforeFinish, h.Name, err, false)
			o := r.stopWith(hookFailedStop())
			return finishResult{}, &o
		}
		if len(d.Deliver) > 0 || d.Reason != "" {
			for _, item := range d.Deliver {
				out.deliver = append(out.deliver, cloneContextItem(item))
			}
			if out.hook == "" {
				out.hook = h.Name
				out.reason = d.Reason
			}
			verdict := VerdictDeliver
			if len(d.Deliver) == 0 {
				verdict = VerdictReject
			}
			r.addHookResult(HookResult{
				Point:   HookBeforeFinish,
				Name:    h.Name,
				Verdict: verdict,
				Reason:  d.Reason,
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
// waiting steers belongs first here, before the BeforeFinish hooks, so a
// steer supersedes a candidate without asking the hooks about it. When the
// hooks turn the candidate down but no further normal model call can
// happen, their answer is dropped with a warning and the candidate stands:
// a finish hook never spends the last answer. submission says whether a
// reason alone turns the candidate down, which only a submission can act on.
func (r *runtime) finishPoint(
	s *replayState,
	plan FinishPlan,
	submission bool,
) (finishResult, *Outcome) {
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
	if !finish.rejects(submission) {
		return finish, nil
	}
	if !r.nextCallFits(s, estimatedItemTokens(finish.deliver)) {
		r.pendingWarnings = append(r.pendingWarnings, Warning{
			Code: WarningDeliveryDropped,
			Message: fmt.Sprintf(
				"BeforeFinish hook %s asked for another model call, but none "+
					"can happen before the reserved final answer or a limit; "+
					"the answer stands",
				finish.hook,
			),
		})
		finish.deliver = nil
		finish.reason = ""
		finish.hook = ""
	}
	return finish, nil
}

// rejects reports whether the hooks turned the candidate down.
func (f finishResult) rejects(submission bool) bool {
	return len(f.deliver) > 0 || submission && f.hook != ""
}

// rejectSubmission settles a started dashi_submit_output call that a hook
// or a steer turned down, as failed with the model-visible output
// "Not accepted: <reason>". The items to deliver ride on the same step, so
// a crash cannot record the rejection without them; projection places them
// after the call's result. source names who rejected it for the record,
// such as "hook <name>" or "steering". It is the same started-to-settled
// transition a schema rejection uses, so the next successful submission is
// the one recorded as output.
func (r *runtime) rejectSubmission(
	call tool.Call,
	reason string,
	source string,
	items []ContextItem,
) *Outcome {
	out := tool.Outcome{
		State:  tool.StateFailed,
		Output: new(tool.TextOutput("Not accepted: " + reason)),
	}
	step := Step{
		Kind: StepToolSettled,
		Tool: &ToolFact{
			Call:    call,
			Outcome: &out,
			Reason:  "not accepted: " + source,
		},
	}
	if len(items) > 0 {
		step.Context = &ContextFact{Items: items}
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
	if !s.roomForNormalCall() {
		return false
	}
	if d := r.agent.Limits.Deadline; !d.IsZero() && !time.Now().Before(d) {
		return false
	}
	if r.ctx.Err() != nil {
		return false
	}
	if budgetSpentStop(s) != nil {
		return false
	}
	if s.Budget == nil || s.Budget.MaxCost == nil {
		return true
	}
	req, ok := estimatedNextRequest(s, extraInputTokens)
	if !ok {
		return false
	}
	spent, ok := spendWithCallInFlight(s)
	if !ok {
		return false
	}
	_, _, stop := checkSendBudget(spent, req, r.agent.Model)
	return stop == nil
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
