# Approving tool calls

An approval is a [hook](hooks.md) on `BeforeTool` that allows, denies, or
defers a tool call. Ask inline when the program can wait for a person; defer
when it cannot.

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

`Approve` and `Deny` bind the answer to the call and the exact arguments
shown; hooks do not run again. `Result(outcome)` reports the result of a
waiting external call the same way. Each builder derives its command ID from
the call, so a retried answer is a duplicate and a different second answer
fails with `ErrCommandConflict`. `Settle` only records; `Continue` runs the
approved calls and returns `ErrNotReady` while any call is unanswered. Once
every call is answered, `out.Pending` is empty and `out.ReadyToContinue` is
true, but `out.Stop` still says waiting until `Continue` runs.

