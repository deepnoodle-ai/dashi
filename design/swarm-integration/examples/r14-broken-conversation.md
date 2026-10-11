# R14: Refuse a conversation with unreadable history

[All examples](README.md) · [Review](../README.md)

The user expects “continue” to retain the conversation's earlier decisions.
Swarm already refuses unreadable history. This helper makes that policy an
explicit library call for application developers.

## Sequence where it matters

1. The user tells the agent, “Do not change the migration files.” The instruction
   becomes part of the saved conversation.
2. The user closes the application.
3. A faulty restore or migration leaves a supplied saved Turn unreadable.
   This is a failure scenario, not an identified production incident.
4. The user reopens the conversation and asks the agent to continue.
5. The application rebuilds the model's history. A tolerant builder can omit
   an unreadable Turn and return a warning alongside the remaining messages.
6. The application must stop before sending that reduced history. Otherwise
   the next request might lack a prior instruction. Swarm already stops during
   record loading/history construction; its wrapper turns warnings into errors.

BuildHistoryStrict returns an error and no history. It checks every supplied
record, including records covered by later summaries. It cannot detect a record
the application did not supply, or prove that a summary retained every instruction.

**Judgment:** A small safety API worth keeping for applications that require
complete supplied history. The current Swarm benefit is a consistent error with
the affected Turn ID, not a newly fixed continuation bug. Keeping Swarm's existing
wrapper is a reasonable smaller alternative; the code saving is modest.

## Before: current code

From [swarm/internal/runtime/dashirt/compaction.go:167–173](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/compaction.go#L167). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/compaction.go 167 173 -->
```go
func buildHistory(records []dashi.Turn) ([]llm.Message, error) {
	history, warnings := dashi.BuildHistory(records)
	for _, warning := range warnings {
		return nil, &runtime.RecoveryError{Code: "compaction_history_mismatch", Permanent: true, Err: fmt.Errorf("%s: %s", warning.Code, warning.Message)}
	}
	return history, nil
}
```

## After: implemented in [#254](https://github.com/deepnoodle-ai/dashi/pull/254), awaiting merge

Application UI functions below remain pseudocode. The strict builder is implemented.

```go
// An error identifies the supplied Turn that could not be used.
history, err := dashi.BuildHistoryStrict(records)
if err != nil { return showHistoryRepairRequired(err) }
start.History = history
out, err := runner.Run(ctx, newTurn, start, dashi.WithOwner(owner.OwnerID))
```

**Observable result:** Swarm already refuses this continuation. A direct strict call makes refusal explicit for new library users and checks all supplied records.

**Developer benefit:** Make Swarm's required history policy explicit and discoverable. It saves only a small wrapper, but prevents an easy adoption mistake.

**Application responsibility:** Decide whether tolerant history is appropriate for the product. Preserve original records for inspection; harmless notices should not become integrity errors. Strict mode validates supplied records; it cannot independently check omitted history before an applied summary.

**Runnable evidence:** `Example_brokenHistory` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
