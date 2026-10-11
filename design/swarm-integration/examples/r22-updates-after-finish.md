# R22: Stop displaying results when the user stops the agent

[All examples](README.md) · [Review](../README.md)

**Decision: withdraw Tail from this integration.** Stopping the agent is a
reasonable point to stop displaying tool results. A separate subscriber for
later results adds behavior the user does not want and has no current Swarm consumer.

## Sequence where the earlier proposal fails

1. The user asks the agent to run a tool.
2. The tool starts work, but its result has not returned.
3. The user stops the agent. The request stops contributing results to the
   conversation, and normal Follow finishes at the ending record.
4. If remote work later completes, the user need not receive its result.
   Saving evidence internally does not require displaying it or continuing the agent.

The earlier proposal added a fifth step: keep a separate activity panel subscribed
and deliver the late result. That step was an invented product requirement.
The user's expected behavior is already served by Follow ending with execution.
[Current watcher lifetime](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1361).

Swarm also saves model charges in the record-commit transaction. It does not
need an observation subscriber to post them.
[Current accounting commit](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/recording.go#L40).

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

## After: keep the current completion behavior

No API addition is needed for this sequence. Keep Follow ending when execution
ends. The Tail implementation, generic reader extraction, examples, and guide
changes are removed from [#253](https://github.com/deepnoodle-ai/dashi/pull/253).
That PR retains the independently justified MCP conversion.

**Developer benefit:** No extra iterator, subscriber lifetime, or cursor policy
to learn for unwanted post-stop updates.

The current-API `Example_lateResultAfterCancel` in [the offline demos](demo/)
shows that records can be saved after cancellation and normal Follow has ended.
It proves storage behavior, not a requirement to present those records to the user.
