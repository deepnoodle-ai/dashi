# R04: Resume a command saved before a crash

[All examples](README.md) · [Review](../README.md)

Swarm saves a Continue command, then crashes before it runs. The replacement
worker must apply that exact command before choosing new work. Once the agent
waits for an outside tool, repeated advancement must stop making busy work.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:713–732](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L713). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 713 732 -->
```go
command, err := r.unapplied(ctx, turn)
if err != nil {
	return false, false, err
}
if command == nil {
	if len(turn.Steps) == 0 {
		command, err = r.startCommand(saved, req, history)
	} else {
		var view dashi.TurnView
		if view, err = turn.View(); err == nil {
			command, err = r.nextCommand(ctx, saved, view, history)
		}
	}
	if command == nil || err != nil {
		return false, false, err
	}
	if err = r.plan(ctx, id, owner, command); err != nil {
		return false, false, err
	}
}
```

From [swarm/internal/runtime/dashirt/dashirt.go:753–763](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L753). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 753 763 -->
```go
func runAgain(stepsBefore int, out dashi.RunResult) bool {
	switch out.Next() {
	case dashi.NextContinue:
		return len(out.Turn.Steps) > stepsBefore
	case dashi.NextRaiseModelCalls:
		return true
	case dashi.NextFix:
		return hostLimit(out.Stop) == routineCapCode || (out.Stop.Kind == dashi.StopLimited && contextWindowStop(out.Stop.Reason))
	}
	return false
}
```

## After: Proposed API sketch

The proposed Dashi helper does not exist yet. Application methods are illustrative.

```go
// Deferred experiment: all these application adapters still need implementation.
progress, err := driver.Advance(ctx, driver.AdvanceRequest{
    TurnID: turnID, Owner: owner.OwnerID,
    Runner: runner, Journal: appCommandJournal,
    AdmittedStart: savedStart, AdmittedHistory: originalHistory,
    Planner: appNextCommand, // Account availability, caps, recovery policy.
})
if err != nil { return err } // Unknown writes reload through the journal adapter.
if progress.Changed { return wakeAgain(turnID) }
return nil // Waiting/finished/attention: no immediate rerun without progress.
```

**Observable result:** The old command would retain its ID, and saved-state checks would prevent duplicate command records. This is not an exactly-once remote-execution guarantee. A waiting/no-progress result would return control without spinning.

**Developer benefit:** Defer this helper until a complete journal/planner implementation removes more code than it adds. The offline demo proves an already-saved Start retry, not this driver or a crash before an unapplied Continue.

**Application responsibility:** Acquire ownership first, save commands durably through the adapter, schedule workers, and reload on uncertain writes.

**Runnable evidence:** `Example_restartAndRetry` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
