# R16: Recover a conversation summary after a restart

[All examples](README.md) · [Review](../README.md)

The user wants a long-running conversation to continue after a restart without
paying for the same summary again. Swarm already provides that behavior.
This helper supplies extra library-owned validation; it does not create recovery.

## Sequence where it matters

1. The user has worked with the agent long enough that older messages need
   summarization before the next request fits.
2. The user sends another request. Swarm saves that pending input and a summary
   job containing the source history, policy, account, and bound summary Turn.
3. The summary model finishes. Swarm saves its response and charge.
4. The process exits before summary activation, or activation commits but its
   acknowledgment is lost.
5. After restart, Swarm finds the saved job and the same summary Turn. Its
   existing code reuses the result rather than sending another summary request.
6. It checks completion and usable replacement history, then activates the
   summary through its database operation. Only then can the pending work proceed.

ValidateReplacement can check the finished record against the exact saved
summary Start and source. That centralizes Dashi's source/input/content checks.
Swarm still finds the job, preserves its account, checks the current revision,
and commits activation. The user gets the same restart behavior.
[Swarm already tests lost activation acknowledgment without repeating the summary](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt_test.go#L698).

**Judgment:** Not yet a strong Swarm simplification case. Do not attribute the
331-line job adapter to validation or imply this helper removes that adapter.
The smaller alternative is existing View/ReplacementHistory checks. Defer adoption
until a real port shows useful checks removed while preserving job and transaction rules.

## Before: current code

From [swarm/internal/runtime/dashirt/compaction.go:228–240](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/compaction.go#L228). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/compaction.go 228 240 -->
```go
record := activationRecord{Version: 2, TurnID: summary.ID, Summary: summary.Reply}
record.Covers.Messages, record.Covers.Digest = len(history), start.Compaction.Hash
if _, known := policy.Window(); !known {
	record.Warnings = append(record.Warnings, "model context window unknown; compaction used the 128000-token fallback")
}
encoded, err := json.Marshal(record)
if err != nil {
	return nil, err
}
if err = r.repo.ActivateCompaction(ctx, operation.ID, encoded); err != nil {
	return nil, compactionPersistenceError(err)
}
return replacement, nil
```

## After: implemented candidate in [#254](https://github.com/deepnoodle-ai/dashi/pull/254)

The helper is implemented and awaiting merge. Application job/transaction methods
below remain pseudocode; they do most of the recovery work.

```go
// The app finds its existing job; it still owns scheduling and recorded execution.
job, summaryTurn, err := repo.LoadSummaryJob(ctx, "summary:request-42")
if err != nil { return err }
// The helper checks the frozen source hash and the finished summary
// against its recorded compaction metadata; the transaction checks current revision.
activation, err := compaction.ValidateReplacement(job.FrozenStart, job.FrozenHistory, summaryTurn)
if err != nil { return keepOldHistoryAndReportSummaryFailure(job, err) }
return repo.Transaction(ctx, func(tx Tx) error {
    // Compare source revision, preserve account/deadline, reject conflicting retry.
    return tx.ActivateSummaryOnce(job.ID, job.SourceRevision, activation)
})
```

**Observable result:** Swarm already reuses the finished summary. The helper adds validation; old history remains active until the application's activation transaction commits.

**Potential developer benefit:** Consolidate validation and replacement-value assembly. The amount of Swarm code removed has not been demonstrated.

**Application responsibility:** Load/reuse the saved job, run its summary Turn, choose required vs optional summarization, preserve the original account/deadline, and own the activation transaction. Required failures must not fall back to oversized input.
