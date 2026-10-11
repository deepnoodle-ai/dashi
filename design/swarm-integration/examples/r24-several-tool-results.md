# R24: Submit results without repeating command construction

[All examples](README.md) · [Review](../README.md)

Two outside tools finish together. Swarm needs to save commands for both,
then wake the agent. If only one command saves, retry must preserve that result
and safely save the other.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:945–950](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L945). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 945 950 -->
```go
	outcome := toolResultOutcome(res)
	command := dashi.Resolve{CommandID: "result:" + native, CallID: native, Outcome: &outcome}
	if err = r.plan(ctx, id, owner, command); err != nil {
		return err
	}
}
```

## After: Existing API

This uses current Dashi APIs. Application storage methods are illustrative;
the snippet is not a complete program.

```go
// Existing API. Correlation/duplicate checks precede this loop; see R23.
for _, result := range results {
    pending := pendingByRef[result.Call]
    command := pending.Result(result.Outcome)
    if err := repo.PlanCommand(ctx, turn.ID, command); err != nil { return err }
}
return wakeWorker(turn.ID) // Apply saved commands; Continue remains explicit.
```

**Observable result:** Pending.Result already supplies stable Resolve IDs. The application still must recover partial saves, acknowledge exact retries, and reject different outcomes for a reused result ID.

**Developer benefit:** Use a smaller existing API now. Defer a batch planner/submitter: replacing these few lines does not justify a new set of public types.

**Application responsibility:** Own storage and wake/Continue decisions. Preserve concurrency so a blocking inbox tool does not hold up a sibling post.
