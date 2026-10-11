# R06: Supply a reminder once without an initialization hook

[All examples](README.md) · [Review](../README.md)

“Use the project checkout at /workspace/orders” is known when the work is
accepted. The first model request should see it; later continuations should not
add another copy.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:658–675](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L658). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 658 675 -->
```go
if len(req.Context) > 0 {
	hooks.BeforeModel = append(hooks.BeforeModel, dashi.ModelHook{Name: "swarm-reminders", Func: func(_ context.Context, p dashi.ModelPlan) (dashi.ModelDecision, error) {
		if p.Turn.LastSeq > 1 {
			return dashi.ModelDecision{}, nil
		}
		var items []dashi.ContextItem
		for _, item := range req.Context {
			origin := dashi.ContextRuntime
			tier := llm.ReminderContextual
			if item.Operator {
				origin = dashi.ContextOperator
				tier = llm.ReminderOperator
			}
			items = append(items, dashi.ContextItem{ID: "reminder:" + item.Name, Name: item.Name, Origin: origin, Snapshot: true, Message: llm.ReminderMessage(llm.Reminder{Name: item.Name, Content: item.Content, Tier: tier})})
		}
		return dashi.ModelDecision{AddMessages: items}, nil
	}})
}
```

## After: Proposed API sketch

These Dashi APIs do not exist yet. Application variables and storage methods
show the intended caller responsibilities, not a complete runnable program.

```go
// Proposed Start field. ContextItem itself already exists.
start.Context = []dashi.ContextItem{{
    ID: "reminder:checkout", Name: "checkout", Origin: dashi.ContextRuntime,
    Snapshot: true,
    Message: llm.ReminderMessage(llm.Reminder{
        Name: "checkout", Content: "Use /workspace/orders", Tier: llm.ReminderContextual,
    }),
}}
out, err := runner.Run(ctx, turn, start, dashi.WithOwner(owner.OwnerID))
```

**Observable result:** The reminder would be saved before the first request and remain present once after restart or Continue.

**Developer benefit:** Delete the first-call hook and LastSeq guard. The code still constructs the meaningful reminder.

**Application responsibility:** Choose the reminder authority and lifetime. Keep its original content pinned to the accepted work.
