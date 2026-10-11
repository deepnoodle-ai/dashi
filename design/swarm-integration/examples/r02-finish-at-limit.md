# R02: Display a limit as a limit

[All examples](README.md) · [Review](../README.md)

The agent uses its four allowed model calls without finishing the task. Swarm
wants to finish this work and display “Incomplete: model-call limit reached.”

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:802–803](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L802). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 802 803 -->
```go
case dashi.NextRaiseModelCalls:
	return dashi.Cancel{CommandID: fmt.Sprintf("model-call-limit:%d", view.LastSeq), Reason: modelCallLimitReason}, nil
```

From [swarm/internal/runtime/dashirt/dashirt.go:1192–1196](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1192). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 1192 1196 -->
```go
case dashi.StopCanceled:
	ev.Status = runtime.TurnCancelled
	if view.Stop.Reason == modelCallLimitReason || contextWindowStop(view.Stop.Reason) {
		ev.Status = runtime.TurnIncomplete
	}
```

## After: Proposed API sketch

The proposed Dashi helper does not exist yet. Application storage methods
show the responsibilities that remain with the caller.

```go
// Proposed: end the current saved stop; do not accept an invented Stop value.
if out.Stop.Kind == dashi.StopLimited && out.Stop.Limit != nil && out.Stop.Limit.Kind == dashi.LimitModelCalls {
    out, err = runner.Run(ctx, out.Turn, dashi.End{
        CommandID: "end-at-call-limit", ExpectedStopSeq: out.LastSeq,
    }, dashi.WithOwner(owner.OwnerID))
    if err != nil { return err }
}
showLimit(out.Stop.Limit, out.Stop.Ended)
```

**Observable result:** Today the final saved stop says canceled, and Swarm translates its reason back to incomplete. Afterward it would remain a model-call limit with Ended=true.

**Developer benefit:** Remove special cancellation reasons and the reverse translation in the UI adapter.

**Application responsibility:** Decide which application limits finish work and which wait for a top-up. Dashi must reject a stale stop reference, recognize an exact retry, preserve late-result handling, and never fabricate completion.
