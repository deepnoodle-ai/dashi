# R06: Supply a reminder once without an initialization hook

[All examples](README.md) · [Review](../README.md)

The user expects an agent to keep working in the same project after a restart.
Swarm already saves the initial project and operator reminders. This change
removes the callback used to deliver those saved reminders to Dashi.

## Sequence where it matters

1. The user connects a project workspace and asks the agent to fix a bug there.
2. Swarm prepares the request, including the workspace and operator reminders,
   and saves those reminders with the request.
3. The process exits before the first model call.
4. On restart, Swarm reloads that request. The first model call must get the
   original reminders, rather than newly assembled instructions for different work.
5. The model requests a tool. After its result arrives, the agent continues.
   The application must not append another copy of the initial reminders.
6. Today a BeforeModel callback adds them only when LastSeq is 1. Dashi records
   them and replay retains them. With Start.Context, Dashi saves them with Start
   and applies its existing context rules without that first-call callback.

The user-facing expectation already holds in Swarm. The developer no longer
has to tie initialization to a particular saved sequence number.
[Swarm constructs workspace/operator reminders](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/conductor/conductor.go#L924)
and [delivers them through this callback](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L658).
This sequence illustrates supported mechanics, not an observed loss of reminders.

**Judgment:** A useful ergonomic change when initial context is common. Keep the
reminder construction; remove only callback and first-call bookkeeping. A local
initialization helper is the smaller alternative. The record-version upgrade is
a real adoption cost, so this is not free simplification.

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

## After: implemented in [#252](https://github.com/deepnoodle-ai/dashi/pull/252), awaiting merge

Application variables show caller responsibilities. The complete restart example
is [ExampleStart_context](https://github.com/deepnoodle-ai/dashi/blob/76caabc7afc902599f6b9434ba03235dcceb3850/run_admit_example_test.go).

```go
// ContextItem already describes authority and lifetime.
start.Context = []dashi.ContextItem{{
    ID: "reminder:checkout", Name: "checkout", Origin: dashi.ContextRuntime,
    Snapshot: true,
    Message: llm.ReminderMessage(llm.Reminder{
        Name: "checkout", Content: "Use /workspace/orders", Tier: llm.ReminderContextual,
    }),
}}
out, err := runner.Run(ctx, turn, start, dashi.WithOwner(owner.OwnerID))
```

**Observable result:** The same initial reminder reaches the model after restart and is not appended again on Continue. It is recorded with Start.

**Developer benefit:** Delete the first-call hook and LastSeq guard. The code still constructs the meaningful reminder.

**Application responsibility:** Choose the reminder authority and lifetime. Keep its original content pinned to the accepted work.
