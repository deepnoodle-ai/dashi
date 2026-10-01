package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

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
		d, err := callHook(hookContext(ctx, view), h, ToolPlan{
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

// afterTool settles what the model sees of a tool or host result about to be
// recorded. It runs the AfterTool chain, then applies the result limit, and
// sets fact.Result when either changed the view. The limit runs last, so a
// hook sees the full text and cannot bring the flood back.
func (r *runtime) afterTool(ctx context.Context, fact *ToolFact) {
	if fact.Outcome == nil {
		return
	}
	hooked := len(r.hooks.AfterTool) > 0
	if !hooked && r.agent.Limits.ToolResultBytes <= 0 {
		return
	}
	// base shares its blocks with the outcome and is only read. Hooks get
	// their own copies, and the shortened view copies only what it keeps.
	base := sharedToolResult(fact.Call.ID, fact.Outcome)
	current := base
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
		if facts.Limit > 0 {
			fact.Shortened = &facts
		}
	}
	if !canonjson.SameValue(current, base) {
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
	current := llmclone.ToolResult(base)
	view := TurnView{}
	if s, err := replay(r.turn); err == nil {
		view = s.TurnView
	}
	def := view.Tools[fact.Call.ID].Definition
	mark := r.hookMark()
	for i, h := range r.hooks.AfterTool {
		d, err := callHook(hookContext(ctx, view), h, ResultPlan{
			Call:       fact.Call.Clone(),
			Definition: def.Clone(),
			Outcome:    fact.Outcome.Clone(),
			Result:     llmclone.ToolResult(current),
			Turn:       snapshotView(view),
		})
		var replace llm.ToolResult
		if err == nil && d.Replace != nil {
			replace = llmclone.ToolResult(*d.Replace)
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
		if d.Replace == nil || canonjson.SameValue(replace, current) {
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
