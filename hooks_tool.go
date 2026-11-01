package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// authorizeTool runs the BeforeTool chain for a call whose arguments match
// its schema, and returns the call as rewritten and the combined verdict. A
// failing hook denies the call, since BeforeTool guards execution, and the
// model sees only the hook's name. A failing hook with FailOpen counts as no
// opinion: the warning is recorded and the chain goes on. The returned RunResult is set when the
// run's context ended during a hook.
//
// The inbox check for a pending urgent steer belongs just before this call
// in the tools loop, so no hook runs for a call that will not start.
func (r *runtime) authorizeTool(
	ctx context.Context,
	def tool.Definition,
	call tool.Call,
	ex tool.Executor,
) (tool.Call, ToolVerdict, *RunResult) {
	pass := 0
	mark := r.hookMark()
	// A mid-run view: its Stop stays empty, unlike turn.View()'s. Nothing
	// is committed during the chain, so one view serves every hook.
	view := TurnView{}
	if current, err := r.replay(); err == nil {
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
			if h.FailOpen {
				continue
			}
			return call, ToolVerdict{
				Kind:   DecisionDeny,
				Reason: fmt.Sprintf("Denied: check %s failed.", h.Name),
				Hook:   h.Name,
			}, nil
		}
		if len(d.Rewrite) > 0 {
			if ArgsHash(d.Rewrite) == ArgsHash(call.Args) {
				continue
			}
			r.addHookResult(HookResult{
				Point:        HookBeforeTool,
				Name:         h.Name,
				Verdict:      VerdictRewrite,
				Pass:         pass,
				RulesVersion: d.RulesVersion,
				Reason:       d.Reason,
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
			Point:        HookBeforeTool,
			Name:         h.Name,
			Verdict:      HookVerdict(d.Kind),
			Pass:         pass,
			RulesVersion: d.RulesVersion,
			Reason:       d.Reason,
		})
		verdict := ToolVerdict{
			Kind:         d.Kind,
			Reason:       d.Reason,
			Hook:         h.Name,
			RulesVersion: d.RulesVersion,
			RulesFinding: d.RulesFinding,
		}
		switch d.Kind {
		case DecisionDeny:
			return call, verdict, nil
		case DecisionNeedsApproval:
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
	case "", DecisionAllow, DecisionDeny, DecisionNeedsApproval:
		return nil
	}
	return fmt.Errorf("invalid decision kind %q", d.Kind)
}

// afterTool decides what the model sees of a tool or host result about to be
// recorded. It runs the AfterTool chain, then applies the result limit, and
// sets fact.Result when either changed the view. The limit runs last, so a
// hook sees the full text and cannot bring the flood back. It returns the
// items the hooks added, which the caller records on the same
// tool_finished step as the result.
func (r *runtime) afterTool(ctx context.Context, fact *ToolFact) []ContextItem {
	if fact.Outcome == nil {
		return nil
	}
	hooked := len(r.hooks.AfterTool) > 0
	if !hooked && r.agent.Limits.ToolResultBytes <= 0 {
		return nil
	}
	// base shares its blocks with the outcome and is only read. Hooks get
	// their own copies, and the shortened view copies only what it keeps.
	base := sharedToolResult(fact.Call.ID, fact.Outcome)
	current := base
	var items []ContextItem
	if hooked {
		current, items = r.afterToolHooks(ctx, fact, base)
	}
	limited, facts, short := shortenResult(
		current,
		r.agent.Limits.ToolResultBytes,
	)
	if !hooked && !short {
		// Nothing can differ from base, so skip the copy and the compare.
		return nil
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
		if !sameForModel(current, base) {
			r.dropChangedData(fact)
		}
	}
	return items
}

// afterToolContext is the ContextFact for the items AfterTool hooks added
// for one call, or nil when they added none.
func afterToolContext(items []ContextItem) *ContextFact {
	if len(items) == 0 {
		return nil
	}
	return &ContextFact{Items: items}
}

// sameForModel reports whether two results send the model the same thing.
// Providers build the request from the content blocks and IsError only:
// Output.Display is for presentation and never reaches the model, and
// empty text blocks carry no text. CallID is the same call either way.
func sameForModel(a, b llm.ToolResult) bool {
	return a.IsError == b.IsError &&
		canonjson.SameValue(modelBlocks(a), modelBlocks(b))
}

// modelBlocks returns the content blocks of r that carry something.
func modelBlocks(r llm.ToolResult) []content.Block {
	out := make([]content.Block, 0, len(r.Output.Content))
	for _, b := range r.Output.Content {
		if b.Text != nil && b.Text.Text == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

// dropChangedData removes Outcome.Data from a fact whose model-visible
// result differs from the tool's output (see sameForModel). Data describes the output the tool
// returned, so a host must never read it as a statement about what the
// model saw after a hook replaced, withheld, or the limit shortened it.
func (r *runtime) dropChangedData(fact *ToolFact) {
	if len(fact.Outcome.Data) == 0 {
		return
	}
	out := *fact.Outcome
	out.Data = nil
	fact.Outcome = &out
	r.pendingWarnings = append(r.pendingWarnings, Warning{
		Code: WarningToolDataDropped,
		Message: fmt.Sprintf(
			"tool call %s: the model saw a changed result, so the outcome "+
				"is recorded without its Data",
			fact.Call.ID,
		),
	})
}

// afterToolHooks runs the AfterTool chain and returns the view it leaves
// and the items its hooks added. It fails closed: a failing hook, or a run
// context that ends during the chain while a hook still to run fails
// closed, withholds the result from the model and drops every item of the
// chain, since an item may describe the withheld result. The outcome and
// its cost are recorded either way, since the call already finished.
func (r *runtime) afterToolHooks(
	ctx context.Context,
	fact *ToolFact,
	base llm.ToolResult,
) (llm.ToolResult, []ContextItem) {
	current := llmclone.ToolResult(base)
	view := TurnView{}
	if s, err := r.replay(); err == nil {
		view = s.TurnView
	}
	def := view.Tools[fact.Call.ID].Definition
	mark := r.hookMark()
	var items []ContextItem
	// recorded holds the items the turn and earlier hooks of this chain
	// add, by ID, so the record shows add_messages only for a hook whose
	// items are recorded.
	var recorded map[string]ContextItem
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
					Hook: h.Name,
				})
				if withhold {
					items = r.dropAfterToolItems(mark, fact.Call.ID, items)
				}
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
			items = r.dropAfterToolItems(mark, fact.Call.ID, items)
			break
		}
		added := r.afterToolItems(fact.Call.ID, h.Name, d.AddMessages, &recorded)
		items = append(items, added...)
		verdict := VerdictReplace
		switch {
		case d.Replace != nil && !canonjson.SameValue(replace, current):
			current = replace
		case len(added) > 0:
			verdict = VerdictAddMessages
		case d.Reason != "" || d.RulesVersion != "":
			// A hook that looked and kept the result can still leave a
			// record, such as a check that passed.
			verdict = VerdictNoChange
		default:
			continue
		}
		r.addHookResult(HookResult{
			Point:        HookAfterTool,
			Name:         h.Name,
			Verdict:      verdict,
			RulesVersion: d.RulesVersion,
			Reason:       d.Reason,
		})
	}
	return current, items
}

// afterToolItems returns the items of one AfterTool hook's answer that the
// call's step records: each gets an ID when it has none, an invalid item
// is dropped with a warning, and an item whose ID the turn or an earlier
// hook of the chain already adds is skipped, as skipRecordedItems does.
func (r *runtime) afterToolItems(
	callID string,
	hook string,
	add []ContextItem,
	recorded *map[string]ContextItem,
) []ContextItem {
	if len(add) == 0 {
		return nil
	}
	var out []ContextItem
	for i, item := range add {
		if err := item.Validate(); err != nil {
			r.pendingWarnings = append(r.pendingWarnings, Warning{
				Code: WarningMessagesDropped,
				Message: fmt.Sprintf(
					"after_tool hook %q: context item %d for call %s "+
						"dropped: %v",
					hook,
					i+1,
					callID,
					err,
				),
				Hook: hook,
			})
			continue
		}
		item = cloneContextItem(item)
		if item.ID == "" {
			item.ID = afterToolItemID(r.turn.ID, callID, hook, i)
		}
		out = append(out, item)
	}
	if *recorded == nil {
		*recorded = recordedItems(r.turn)
	}
	return r.skipItems(out, *recorded)
}

// afterToolItemID names the i-th item without an ID that AfterTool hook h
// adds for call callID. A rerun after a crash gives the same ID, and the
// ID shows which hook and call added the item.
func afterToolItemID(turnID, callID, h string, i int) string {
	return turnID + ":tool:" + callID + ":hook:" + h + ":" + strconv.Itoa(i+1)
}

// dropAfterToolItems drops every item of a chain whose result was withheld,
// with one warning naming the call when there were any. The chain's
// add_messages results go with them; a hook that also gave a Reason or
// RulesVersion keeps a no_change result, since it did run.
func (r *runtime) dropAfterToolItems(
	mark chainMark,
	callID string,
	items []ContextItem,
) []ContextItem {
	if len(r.pendingHooks) > mark.hooks {
		kept := r.pendingHooks[:mark.hooks]
		for _, result := range r.pendingHooks[mark.hooks:] {
			if result.Point == HookAfterTool &&
				result.Verdict == VerdictAddMessages {
				if result.Reason == "" && result.RulesVersion == "" {
					continue
				}
				result.Verdict = VerdictNoChange
			}
			kept = append(kept, result)
		}
		r.pendingHooks = kept
	}
	if len(items) > 0 {
		r.pendingWarnings = append(r.pendingWarnings, Warning{
			Code: WarningMessagesDropped,
			Message: fmt.Sprintf(
				"call %s: the result was withheld, so the %d context "+
					"item(s) added by after_tool hooks were dropped",
				callID,
				len(items),
			),
		})
	}
	return nil
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
