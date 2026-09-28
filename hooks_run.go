package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// maxHookError bounds the hook error text kept on the record and in
// warnings, which are persisted with every turn.
const maxHookError = 1 << 10

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
	for _, h := range r.hooks.BeforeModel {
		d, err := callHook(r.ctx, h, ModelPlan{
			Input: cloneInput(*input),
			Turn:  snapshotView(s.TurnView),
		})
		if err == nil {
			err = validModelDecision(d)
		}
		if err != nil {
			if r.ctx.Err() != nil {
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
				Verdict: "stop",
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
			Verdict: "change",
		})
	}
	return deliver, nil, nil
}

func validModelDecision(d ModelDecision) error {
	if d.Stop != nil && !validHookStop(*d.Stop) {
		return errors.New("invalid stop: a hook may refuse, fail, or report a host, cost, or token limit")
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
				Verdict: "deny",
				Reason:  d.Reason,
			}
			stop := &Stop{Kind: StopRefused, Reason: d.Reason}
			if d.Limit != nil {
				result.Verdict = "limit"
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
				Verdict: "admit",
			})
		}
	}
	return admission, nil, nil
}

func validSendDecision(d SendDecision, used int) error {
	if d.Limit != nil && !validHookLimit(*d.Limit) {
		return errors.New("invalid limit: a hook may report a host, cost, or token limit")
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
restart:
	var allow, deferred *ToolVerdict
	for _, h := range r.hooks.BeforeTool {
		// A mid-run view: its Stop stays empty, unlike Replay's.
		view := TurnView{}
		if current, err := replay(r.turn); err == nil {
			view = snapshotView(current.TurnView)
		}
		d, err := callHook(ctx, h, ToolPlan{
			Call:       call.Clone(),
			Definition: def.Clone(),
			Turn:       view,
		})
		if err == nil {
			err = validToolDecision(d, def, call)
		}
		if err != nil {
			if ctx.Err() != nil {
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
				Verdict:  "rewrite",
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
					Reason: "rewrite limit",
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
			Verdict:  string(d.Kind),
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

func validToolDecision(d ToolDecision, def tool.Definition, call tool.Call) error {
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

// finishResult is the combined BeforeFinish answer.
type finishResult struct {
	deliver     []ContextItem
	acceptEmpty bool
}

// beforeFinish runs the BeforeFinish chain for a candidate answer. A failing
// hook stops the turn, since BeforeFinish guards what the turn accepts; the
// candidate survives the stop, so Continue asks the hooks again.
func (r *runtime) beforeFinish(plan FinishPlan) (finishResult, *Outcome) {
	var out finishResult
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
				o := r.contextStop()
				return finishResult{}, &o
			}
			r.hookFailed(HookBeforeFinish, h.Name, err, false)
			o := r.stopWith(hookFailedStop())
			return finishResult{}, &o
		}
		if len(d.Deliver) > 0 {
			for _, item := range d.Deliver {
				out.deliver = append(out.deliver, cloneContextItem(item))
			}
			r.addHookResult(HookResult{
				Point:   HookBeforeFinish,
				Name:    h.Name,
				Verdict: "deliver",
			})
		}
		if d.AcceptEmpty {
			out.acceptEmpty = true
			r.addHookResult(HookResult{
				Point:   HookBeforeFinish,
				Name:    h.Name,
				Verdict: "accept_empty",
			})
		}
	}
	return out, nil
}

func (r *runtime) addHookResult(result HookResult) {
	r.pendingHooks = append(r.pendingHooks, result)
}

// hookFailed records a failed hook, and a warning when the turn works around
// the failure instead of stopping.
func (r *runtime) hookFailed(point HookPoint, name string, err error, warn bool) {
	text := boundedHookError(err)
	r.addHookResult(HookResult{
		Point:   point,
		Name:    name,
		Verdict: "failed",
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
	return text
}
