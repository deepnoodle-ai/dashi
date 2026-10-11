# R22: Keep following later results for accounting

[All examples](README.md) · [Review](../README.md)

An outside tool starts, the user cancels the agent, and the tool result arrives
later. Dashi can save that late result, but normal Follow finishes at the cancel
record. An accounting reader wants to keep watching.

## Before: current code

From [dashi/run_follow.go:172–179](https://github.com/deepnoodle-ai/dashi/blob/ea65bb432ae12893cfe79e2d9bdcbbe1ea8724ed/run_follow.go#L172). Surrounding definitions are omitted.

<!-- source: dashi run_follow.go 172 179 -->
```go
if !yield(step, nil) {
	return
}
cursor = step.Seq
if step.Stop != nil && step.Stop.Ended {
	return
}
resting = step.Kind == StepTurnStopped
```

## After: Proposed API sketch

These Dashi APIs do not exist yet. Application variables and storage methods
show the intended caller responsibilities, not a complete runnable program.

```go
// Proposed feed helper; ordinary Follow still ends with execution.
for step, err := range feed.Tail(ctx, store, turnID, cursor) {
    if err != nil { return err }
    if err := mirror.Apply(step); err != nil { return err }
    cursor = step.Seq
}
```

**Observable result:** Ordinary UI following would still finish at cancellation. The accounting tail would receive later committed results until its context ends.

**Developer benefit:** Remove the manual switch from Follow to StepsAfter. It would also observe proposed billing-correction records from R11.

**Application responsibility:** Persist the reader cursor and handle duplicates. Money updates outside Dashi records still require a separate notification source.

**Runnable evidence:** `Example_lateResultAfterCancel` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
