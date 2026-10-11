# R16: Recover a conversation summary after a restart

[All examples](README.md) · [Review](../README.md)

Swarm generates a summary, then crashes before applying it. On restart it must
find the same summary job and account, validate the completed result, and apply
it once. If summarization was required to fit the next request, failure must
leave that request waiting.

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

## After: Proposed API sketch

The proposed Dashi helper does not exist yet. Application storage methods
show the responsibilities that remain with the caller.

```go
// The app finds its existing job; it still owns scheduling and recorded execution.
job, summaryTurn, err := repo.LoadSummaryJob(ctx, "summary:request-42")
if err != nil { return err }
// Proposed pure helper checks the frozen source hash and the finished summary
// against its recorded compaction metadata; the transaction checks current revision.
activation, err := compaction.ValidateReplacement(job.FrozenStart, job.FrozenHistory, summaryTurn)
if err != nil { return keepOldHistoryAndReportSummaryFailure(job, err) }
return repo.Transaction(ctx, func(tx Tx) error {
    // Compare source revision, preserve account/deadline, reject conflicting retry.
    return tx.ActivateSummaryOnce(job.ID, job.SourceRevision, activation)
})
```

**Observable result:** A restart would reuse the finished summary instead of sending another summary request. Old history would remain active until the activation transaction commits.

**Developer benefit:** Remove repeated summary validation and activation-value assembly. Reuse Compactor.Start and ReplacementHistory; defer a durable coordinator until these smaller helpers prove insufficient.

**Application responsibility:** Load/reuse the saved job, run its summary Turn, choose required vs optional summarization, preserve the original account/deadline, and own the activation transaction. Required failures must not fall back to oversized input.
