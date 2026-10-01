# Approving tool calls

An approval is a [hook](hooks.md) on `BeforeTool` that allows, denies, or
defers a tool call. Ask inline when the program can wait for a person; defer
when it cannot. [Running turns](running-turns.md) covers `Run` and
`Continue`; [recovering turns](recovering-turns.md) covers `Resolve` and the
[turn states](recovering-turns.md#turn-states-and-limits), including
`StopWaiting`.

## Asking inline

A command-line program asks inside a `BeforeTool` hook, all within one `Run`.
`dashi.OnTool` runs only for one tool and decodes its arguments; a denial's
reason is what the model sees
([example](../../examples/approval/main.go)):

```go
ask := dashi.OnTool("deploy", func(
	_ context.Context, _ dashi.ToolPlan, in deployArgs,
) (dashi.ToolDecision, error) {
	if confirm(in) {
		return dashi.Allow("operator approved"), nil
	}
	return dashi.Deny("The operator declined this deploy."), nil
})
runner.Hooks = ask.Hooks()
```

## Deferring for later

A server that cannot wait defers instead. `dashi.RequireApproval("deploy")`
defers the named tools (every tool when none are named); the turn stops with
`StopWaiting`, and each pending item builds its own answer
([example](../../examples/durable_approval/main.go)):

```go
runner.Hooks = dashi.RequireApproval("deploy")
// ... Run, then:
for _, p := range out.Pending {
	if p.Kind != dashi.PendingApproval {
		continue
	}
	answer := p.Deny("The operator declined.")
	if approved(p.Name, p.Args) {
		answer = p.Approve()
	}
	if out, err = runner.Run(ctx, out.Turn, answer); err != nil {
		return err
	}
}
out, err = runner.Run(ctx, out.Turn, dashi.Continue{})
```

To defer by what a tool does rather than by its name, write the hook
yourself. This one defers every tool not marked read-only:

```go
gate := dashi.ToolHook{Name: "read-only-gate", Func: func(
	_ context.Context, p dashi.ToolPlan) (dashi.ToolDecision, error) {
	if ro := p.Definition.Annotations.ReadOnly; ro != nil && *ro {
		return dashi.ToolDecision{}, nil
	}
	return dashi.Defer("changes something"), nil
}}
runner.Hooks = gate.Hooks()
```

`Approve` and `Deny` bind the answer to the call and the exact arguments
shown; hooks do not run again. `Result(outcome)` reports the result of a
waiting external call the same way. Each builder derives its command ID from
the call, so a retried answer is a duplicate and a different second answer
fails with `ErrCommandConflict`. `Resolve` only records; `Continue` runs the
approved calls and returns `ErrNotReady` while any call is unanswered. Once
every call is answered, `out.Pending` is empty and `out.ReadyToContinue` is
true, but `out.Stop` still says waiting until `Continue` runs.


## Using a model's advice

A model can review whether a proposed tool call fits the user's request.
Its output is advisory: model output never grants permission or replaces
required host approval. Compose a separate judge turn with deferred approval:

1. Use `RequireApproval` for the parent tools that need approval, or a coding
   policy with a nil callback. Wait for `PendingApproval` before reviewing.
2. Persist an association between the parent turn ID, call ID, argument digest
   (`dashi.ArgsDigest`), review-policy revision, and judge turn ID. Save the
   judge's command IDs before dispatch. Reuse these IDs and the recorded judge
   turn on retries. Dashi does not store this association for you.
3. Outside the parent's hook, run a judge `Runner` with a durable
   [recorder](recovering-turns.md#recorders). Give it no application or
   provider-hosted tools. Set its own `MaxTokens`, `Limits.ModelCalls`,
   deadline, and `Budget`. Configure any host credit policy on its
   `BeforeSend` too.
4. Give the judge the proposed call and only the relevant task context.
   Treat arguments and tool text as untrusted evidence. Keep judge instructions
   in host configuration and omit unnecessary credentials. Use structured
   output for a recommendation such as acceptable, concerning, or uncertain,
   plus a bounded reason. Validate these values as well as the schema.
5. Show the authorized host the exact pending call and the recommendation.
   Acceptable still requires approval. A failure, refusal, limit, cancellation,
   invalid output, or uncertain dispatch grants no approval. Keep the call
   pending or deny it under host policy. Reconcile uncertain dispatch before
   requesting another review. Require host authorization for any budget raise.
6. Check current authority and that the advice still matches the call and
   review policy. Only the authorized host answers with `Pending.Approve` or
   `Pending.Deny`. `Resolve` records the answer. `Continue` executes or refuses
   the bound call. Never turn model advice into a remembered approval rule.

On reopening, load the parent and its associated judge record. Inspect
completed advice and `Replay(turn).Spend` without another judge call. The
judge has its own spend and budget. Parent `Spend` and `Budget` exclude this
separate turn. Report both spends. Enforce a combined cap through the host
ledger, reserving each request before dispatch and reconciling its charge.
Separate turn caps do not enforce a shared cap. See [turn budgets](budgets.md)
for recorded admission and charge reconciliation.

A raw `Client.Send` inside `BeforeTool` bypasses the parent runner's model
limits, budget admission, and recorded spend. Dashi provides no reusable judge
gate with durable linkage and shared accounting. Use the existing
[structured-output](../../examples/structured_output/main.go),
[budget](../../examples/budget/main.go), and
[durable-approval](../../examples/durable_approval/main.go) examples as the
building blocks.

## Approving coding tools

[Coding tools](coding-tools.md) includes a ready-made hook. It runs read-only
tools without a prompt and asks your callback for writes, edits, and shell
commands, with a diff or the exact command. With a nil callback it defers.
