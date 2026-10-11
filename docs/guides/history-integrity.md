# Validate saved history before continuing

## Refuse broken supplied history

Use `dashi.BuildHistoryStrict(turns)` when an unreadable supplied Turn or
mismatched summary must stop admission. It returns nil history and a `*dashi.HistoryError`
whose `TurnID` identifies the record to inspect. `errors.Is` preserves
`dashi.ErrInvalidTurn` and identifies `dashi.ErrCompactionMismatch`.
It checks every supplied record, including those hidden by a newer summary.
Recorded hook and budget notices do not become construction failures.

The oldest-first suffix may start at an applied summary that the application
already validated. Omitted source records cannot be checked. Valid older
summaries without hashes remain readable, with no source hash proof.
The tolerant `BuildHistory` behavior remains available.
`ExampleBuildHistoryStrict` runs without an API key.

## Validate a saved summary before activation

Save the compaction Start and frozen source before running its bound summary
Turn. After reload, call:

```go
replacement, err := compaction.ValidateReplacement(frozenStart, source, summaryTurn)
if err != nil {
	return err // keep old history; required summarization leaves work waiting
}
// The app checks its job's Turn ID, account, deadline, and source revision.
// Its activation transaction saves replacement and commits once.
```

`Replacement.History` owns its message content. `Clone` makes another independent
copy. The value also carries summary text, Turn ID, source hash, and source
message count. JSON storage preserves it. Validation makes no calls or writes.
The frozen source and prior history must contain valid Dashi messages;
empty messages and invalid named-context authority fail before hashing.
The recorded initial context must match the saved `Start.Context`, including
item order, identity, origin, role, snapshot, and lifetime. Initial user items
belong to the frozen input, including next-call items that later expire.
It rejects later `AddMessages` and steering input of any origin, plus every
recorded continuation-history mismatch before completion. Restoring the original
history later does not erase a mismatch. Same-history recovery and authoritative
model-result resolution remain valid when the summary ultimately completes.

Recorded policy-hook context remains permitted. Hook additions and settings
changes lie outside the frozen-input proof. The application authorizes hooks,
owns durable jobs and required-versus-optional policy, and reloads after an
unknown activation write. Old history stays active until activation commits.
`ExampleValidateReplacement` shows finished-summary reuse without an API key.
