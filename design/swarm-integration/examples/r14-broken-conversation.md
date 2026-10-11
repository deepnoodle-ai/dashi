# R14: Refuse a conversation with unreadable history

[All examples](README.md) · [Review](../README.md)

Conversation support-7 contains a Turn with invalid saved sequence numbers.
Continuing without that Turn could lose an earlier decision. Swarm deliberately
stops instead of sending the remaining messages.

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

## After: Proposed API sketch

These Dashi APIs do not exist yet. Application variables and storage methods
show the intended caller responsibilities, not a complete runnable program.

```go
// Proposed strict builder returns errors with the affected Turn/summary identity.
history, err := dashi.BuildHistoryStrict(records)
if err != nil { return showHistoryRepairRequired(err) }
start.History = history
out, err := runner.Run(ctx, newTurn, start, dashi.WithOwner(owner.OwnerID))
```

**Observable result:** Today BuildHistory returns remaining messages and a warning; Swarm turns warnings into an error. Strict construction would directly report the integrity failure and send nothing.

**Developer benefit:** Make Swarm's required history policy explicit and discoverable. It saves only a small wrapper, but prevents an easy adoption mistake.

**Application responsibility:** Decide whether tolerant history is appropriate for the product. Preserve original records for inspection; harmless notices should not become integrity errors. Strict mode validates supplied records; it cannot independently check omitted history before an applied summary.

**Runnable evidence:** `Example_brokenHistory` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
