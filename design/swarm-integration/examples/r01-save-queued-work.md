# R01: Cancel a request before it starts

[All examples](README.md) · [Review](../README.md)

The user wants to stop accepted work without accidentally starting it.
Swarm already supports this. The proposed API makes that behavior easier for
an application developer to express.

## Sequence where it matters

1. The user sends “Check order 42.” Swarm saves the request in its database.
2. The process exits before the worker records Dashi's first Start step.
3. The application starts again without that provider's credentials.
4. The user stops the swarm. The saved request must become canceled, with no
   model request and no need to restore credentials.
5. Swarm loads the request and finds that its Dashi record is still empty.
   Dashi's Cancel command requires a started Turn.
6. Today Swarm records Start by calling Run with an already-closed pause
   channel. It then records Cancel. With Admit, it calls Admit and then Cancel.

The user gets the same result in both versions: the work is canceled without
a model request. The improvement is the developer's explicit record-only call.
It removes the need to know that a closed pause can manufacture an accepted Start.

This is a supported failure scenario, not a reported production incident.
[Swarm tests cancellation before Start and after restart without a provider](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt_test.go#L385).
The proposed helper does not replace Swarm's database admission or worker ownership.

**Judgment:** A modest API convenience worth considering for background workers.
The smaller alternative is to keep a local helper around the existing pause path.
Do not sell this as a new cancellation feature or a large adapter reduction.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:1103–1109](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1103). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 1103 1109 -->
```go
pause := make(chan struct{})
close(pause)
out, err := runner.Run(ctx, turn, start, dashi.WithOwner(owner.OwnerID), dashi.WithPause(pause))
if err != nil {
	return err
}
turn = out.Turn
```

## After: implemented in [#252](https://github.com/deepnoodle-ai/dashi/pull/252), awaiting merge

This uses the implemented API. Surrounding application storage and ownership
code still belong to the caller.

```go
// Preserve the same Recorder owner used by Swarm today.
opts := []dashi.RunOption{dashi.WithOwner(owner.OwnerID)}
out, err := runner.Admit(ctx, dashi.Turn{ID: "order-42"}, savedStart, opts...)
if err != nil { return err }
_, err = runner.Run(ctx, out.Turn, dashi.Cancel{
    CommandID: "cancel-42", Reason: "user canceled queued work",
}, opts...)
return err
```

**Observable result:** Both paths send zero model requests. Internally, Admit records ready work instead of an interruption with “pause requested.”

**Developer benefit:** Remove the closed-channel convention and make saving work an obvious supported operation.

**Application responsibility:** Save the original command and delivery receipt, reuse its ID on retry, and protect ownership. Fully resolved provider/API/model identity and compaction-window evidence must be saved before keys disappear; record-only admission must not require a live Client.

**Runnable evidence:** `Example_saveBeforeCancel` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
