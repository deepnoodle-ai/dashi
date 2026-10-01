package dashi

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// SeedContext returns a BeforeModel hook, named after item, that delivers
// item unless the latest visible message with its Origin and Name has the
// same value. It reads the proposed model input, so unchanged context is not
// repeated across Session turns or reopened handles. Changed values append
// without replacing earlier messages. Compaction that removes the item causes
// it to be delivered again. Summary text does not establish its presence.
//
// It panics if item has no Name and clears NextCallOnly. Empty Origin means
// ContextUser. Delivery IDs do not affect value comparison. If a configured
// ID already occurs in this turn, a fresh derived ID identifies the delivery.
func SeedContext(item ContextItem) ModelHook {
	if item.Name == "" {
		panic("dashi: SeedContext item has no name")
	}
	item = cloneContextItem(item)
	item.NextCallOnly = false
	origin := item.Origin
	if origin == "" {
		origin = ContextUser
	}
	return ModelHook{
		Name: item.Name,
		Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
			for i := len(p.Input.Messages) - 1; i >= 0; i-- {
				message := p.Input.Messages[i]
				if message.Context == nil || message.Context.Origin != string(origin) ||
					message.Context.Name != item.Name {
					continue
				}
				message.Context = nil
				configured := item.Message
				configured.Context = nil
				if sameJSONValue(message, configured) {
					return ModelDecision{}, nil
				}
				break
			}
			delivery := cloneContextItem(item)
			if delivery.ID != "" && p.Turn.usedContextIDs[delivery.ID] {
				base := fmt.Sprintf("%s:%s:%d", delivery.ID, p.Turn.ID, p.Turn.Revision)
				delivery.ID = base
				for suffix := 1; p.Turn.usedContextIDs[delivery.ID]; suffix++ {
					delivery.ID = fmt.Sprintf("%s:%d", base, suffix)
				}
			}
			return ModelDecision{Deliver: []ContextItem{delivery}}, nil
		},
	}
}

// OnTool returns a BeforeTool hook, named after the tool, that runs f only
// for calls to that tool, with the arguments decoded into T. Other calls get
// no opinion. A decode failure is a hook error, so the call is denied; it
// means T does not match the tool's schema. Set Name on the result to add a
// second hook for the same tool.
func OnTool[T any](
	toolName string,
	f func(context.Context, ToolPlan, T) (ToolDecision, error),
) ToolHook {
	return ToolHook{
		Name: toolName,
		Func: func(ctx context.Context, p ToolPlan) (ToolDecision, error) {
			if p.Call.Name != toolName {
				return ToolDecision{}, nil
			}
			var in T
			if err := json.Unmarshal(p.Call.Args, &in); err != nil {
				return ToolDecision{}, fmt.Errorf(
					"decode %s arguments: %w",
					toolName,
					err,
				)
			}
			return f(ctx, p, in)
		},
	}
}

// Allow records that a hook approved the call, for example after asking a
// person. It combines as no opinion: a later hook may still deny or defer.
func Allow(reason string) ToolDecision {
	return ToolDecision{Kind: DecisionAllow, Reason: reason}
}

// Deny refuses the call. The model sees reason as the call's result, so say
// what happened and what to do instead; an empty reason gives "The call was
// denied."
func Deny(reason string) ToolDecision {
	return ToolDecision{Kind: DecisionDeny, Reason: reason}
}

// Defer makes the call wait for approval: the turn stops waiting, and the
// call is listed in Outcome.Pending. The host answers with Pending.Approve or
// Pending.Deny, then Continue runs or refuses the call.
func Defer(reason string) ToolDecision {
	return ToolDecision{Kind: DecisionDefer, Reason: reason}
}

// Rewrite replaces the call's arguments with args encoded as JSON. The new
// arguments must match the tool's schema, and every hook is asked again
// about them, so rewrites must converge: a rewrite that leaves the
// arguments' JSON meaning unchanged is no opinion, and the call is denied
// when a fourth rewrite would restart the chain a fourth time.
func Rewrite(args any) (ToolDecision, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return ToolDecision{}, err
	}
	return ToolDecision{Rewrite: b}, nil
}

// RequireApproval returns a BeforeTool hook that defers each call to one of
// the named tools, or to any application tool when none are named, until a
// host answers it with Pending.Approve or Pending.Deny. Other calls get no
// opinion. The hook's name and the deferral's revision are both
// "require_approval:" followed by the sorted, comma-separated names, or "*"
// for every tool, so two calls with different tools do not collide.
func RequireApproval(tools ...string) Hooks {
	names := slices.Clone(tools)
	slices.Sort(names)
	names = slices.Compact(names)
	id := "require_approval:*"
	if len(names) > 0 {
		id = "require_approval:" + strings.Join(names, ",")
	}
	return ToolHook{
		Name: id,
		Func: func(_ context.Context, p ToolPlan) (ToolDecision, error) {
			if len(names) > 0 {
				if _, found := slices.BinarySearch(names, p.Call.Name); !found {
					return ToolDecision{}, nil
				}
			}
			return ToolDecision{
				Kind:     DecisionDefer,
				Reason:   "requires approval",
				Revision: id,
			}, nil
		},
	}.Hooks()
}
