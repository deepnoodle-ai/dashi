# R19: Show a running worker as running

[All examples](README.md) · [Review](../README.md)

The model-call-start record is saved but the reply has not arrived. That same
record prefix could describe a live worker or a crashed process. The UI needs
Swarm's worker status to distinguish them.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:1197–1202](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1197). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 1197 1202 -->
```go
case dashi.StopInterrupted:
	if live && view.Stop.Inferred {
		ev.StopReason = "" // between steps of a live worker: running
	} else {
		ev.Status = runtime.TurnInterrupted
	}
```

## After: Proposed API sketch

These Dashi APIs do not exist yet. Application variables and storage methods
show the intended caller responsibilities, not a complete runnable program.

```go
// Proposed presentation helper; worker activity is supplied by the application.
activity, err := workers.Snapshot(turn.ID) // One trustworthy activity observation.
if err != nil { return err }
shown := feed.Present(view, activity)
render(shown.Status, shown.RecordedStop, shown.Pending)
```

**Observable result:** The saved view currently infers interrupted. With reliable running activity, presentation would say running. Unknown effects and recorded final stops would remain visible.

**Developer benefit:** Remove the bespoke status-repair rule and share one clear record-plus-worker mapping.

**Application responsibility:** Provide trustworthy activity, not just a stored lease token. A live worker is not evidence that an uncertain tool result has become known.

**Runnable evidence:** `Example_workerStatus` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
