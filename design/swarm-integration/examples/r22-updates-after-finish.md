# R22: Observe a result after the agent stops

[All examples](README.md) · [Review](../README.md)

An external tool may finish after the user stops an agent. Tail can follow those
later records, but the review has not identified an existing Swarm feature that
needs this iterator.

## Sequence where it could matter

1. The user asks the agent to run work through an external tool.
2. That tool starts remote work. Cancellation cannot be assumed to undo work
   the remote service has already accepted.
3. The user stops the agent before the result returns. Dashi records cancellation,
   and normal Follow finishes at that ending record.
4. The remote service finishes. The host retains its result and, where authorized,
   records it as a late tool result in the canceled Turn.
5. A product might offer an activity panel that stays subscribed to such late
   completions. Tail would deliver this record to that separate panel reader.

Step 5 is a possible feature, not an existing consumer found in Swarm. Swarm's
current WatchTurn intentionally returns at a final state. Replacing it with Tail
does not make an activity panel, and must not restart the canceled agent.
[Current watcher lifetime](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1361).

The previous accounting justification was too broad. Swarm saves model charges
in the record-commit transaction; it does not need a Tail subscriber to post them.
[Current accounting commit](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/recording.go#L40).
Tool journals and host-only billing updates also have their own storage paths.

**Judgment:** Defer. First identify a real post-stop consumer and its reader lifetime.
For one-off inspection, read StepsAfter or reload the record. The iterator is
tested, but its necessity and net code saving in Swarm remain unproven.

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

## After: implemented candidate in [#253](https://github.com/deepnoodle-ai/dashi/pull/253)

Tail is implemented and awaiting merge. The separate consumer below is an
application design example, not current Swarm integration.

```go
// This reader observes records. It must not drive canceled work.
for step, err := range feed.Tail(ctx, store, turnID, cursor) {
    if err != nil { return err }
    if err := mirror.Apply(step); err != nil { return err }
    cursor = step.Seq
}
```

**Observable result:** A separate subscriber receives later committed records until its context ends. No existing Swarm user experience changes without that subscriber.

**Potential developer benefit:** Reuse cursor, paging, and waiting rules for a proven post-stop consumer. There is no demonstrated Swarm caller to simplify yet.

**Application responsibility:** Persist the reader cursor and handle duplicates. Money updates outside Dashi records still require a separate notification source.

**Runnable evidence:** `Example_lateResultAfterCancel` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
