# R01: Cancel a request before it starts

[All examples](README.md) · [Review](../README.md)

The user queues “Check order 42,” then cancels before a worker runs. Swarm needs
a saved canceled Turn even if the model credentials have since been removed.

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

## After: Proposed API sketch

The proposed Dashi helper does not exist yet. Application storage methods
show the responsibilities that remain with the caller.

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

**Observable result:** Today, saving without running produces an interrupted stop with “pause requested.” The proposal would save ready work directly. Both paths send zero model requests.

**Developer benefit:** Remove the closed-channel convention and make saving work an obvious supported operation.

**Application responsibility:** Save the original command and delivery receipt, reuse its ID on retry, and protect ownership. Fully resolved provider/API/model identity and compaction-window evidence must be saved before keys disappear; record-only admission must not require a live Client.

**Runnable evidence:** `Example_saveBeforeCancel` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
