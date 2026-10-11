# R20: Connect a browser to an independently running agent

[All examples](README.md) · [Review](../README.md)

Two browser tabs watch an agent. One closes while a model request is in flight.
The worker later saves its final reply and exits. The remaining tab must see
the finished reply even if it receives the worker-exit signal first.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:1302–1323](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1302). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 1302 1323 -->
```go
steps := make(chan followed, 128)
go func() {
	defer close(steps)
	for step, err := range dashi.Follow(followCtx, stepReader{runtime: r}, id, int64(len(turn.Steps))) {
		select {
		case steps <- followed{step, err}:
		case <-followCtx.Done():
			return
		}
	}
}()
defer func() {
	stopFollow()
	for range steps {
	}
}()
// in stops delivering once Follow ends, which it does after the step
// that ended the Turn; the next pass reports that step.
in := (<-chan followed)(steps)
// Status is part of the change check: a worker that exits between
// steps turns "running" into "interrupted" at the same revision.
last, lastCost, lastStatus := int64(-1), int64(-1), runtime.TurnStatus("")
```

## After: Proposed API sketch

These Dashi APIs do not exist yet. Application variables and storage methods
show the intended caller responsibilities, not a complete runnable program.

```go
// Proposed subscription. requestCtx belongs to the browser, not the worker.
for update, err := range feed.WatchViews(requestCtx, feed.ViewSource{
    Loader: store, Steps: store, Activity: workers,
}, turnID) {
    if err != nil { return err }
    // Host ledger changes need their own subscription/overlay; they may have no Step.
    if err := sendTurnViewToBrowser(update); err != nil { return err }
}
return nil // The subscription cleans up its reader; the worker continues.
```

**Observable result:** Closing one reader would leave execution alone. The remaining reader would catch up to the final saved record before reporting worker exit.

**Developer benefit:** Potentially replace Follow setup/joining and lifecycle catch-up for Dashi views. Compare this with a smaller view accumulator plus today's Follow; do not claim the whole watcher disappears.

**Application responsibility:** Check access/existence, retain ledger overlays, worker errors, retirement handling, and worker lifetime. Activity must register notifications before reading state, distinguish worker generations, refresh after notifications, and catch up records after observing stopped activity. Display cannot authorize tools.
