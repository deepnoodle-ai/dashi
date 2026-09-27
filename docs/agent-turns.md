# Agent turns and recovery

The root `kernel` package runs one identified turn at a time. A `Definition`
pins reusable behavior and limits; `Bindings` supplies the model client, tool
source, recorder, policy, and optional observer. The caller supplies a stable
`Turn.ID` and a unique ID for each `Start`, `Deliver`, `Continue`, `Accept`, or
`Cancel` command. Repeating the same command ID and payload returns the current
recorded turn without repeating an effect. Reusing an ID with different content
fails.

See [the runnable example](../examples/agent/main.go) for setup. `Engine.Apply`
returns a Go error for a command or record rejected before a new fact is
acknowledged. After work begins, inspect `Outcome`: it retains the turn and stop,
including partial work. `Outcome.Turn.AnswerText()` extracts text from the last
settled assistant result; a valid tool-only turn may have no answer text.

| Outcome field | Caller action |
| --- | --- |
| `PersistenceAcknowledged` | Use the returned turn or load it from the recorder. |
| `PersistenceRejected` | No new step was accepted. Fix the write or command before trying again. |
| `PersistenceUnknown` | Reload the turn by ID before any retry. The write may have committed. |
| `StopWaiting` | Inspect `Pending`; use `Accept` for external results or approvals. |
| `StopUncertain` | Reconcile the identified model or tool effect before advancing. |
| `StopCompleted` | No further execution is allowed for this turn. |

`StopFailed`, `StopRefused`, and `StopLimited` retain their reason and record.
`Continue` can advance a continuable stopped turn; it does not reset attempt
counts, deadlines, or a pending effect. `Deliver` records context without
advancing execution. `Cancel` prevents further execution; a late model or tool
result can still be accepted for reconciliation and usage accounting.

## Recorder contract

`Recorder.Commit` acknowledges one versioned `Step` under `ExpectedSeq` and an
optional `Owner` fencing token. A host recorder should atomically check the
current revision and owner, append the step, and commit any related budget,
accounting, or outbox rows. Return `CommitError` with
`PersistenceRejected` only when the write certainly did not commit. Return
`PersistenceUnknown` for an ambiguous acknowledgement; plain errors are also
treated as unknown. Provide a way to load the authoritative turn by ID after
an unknown result. `MemoryRecorder` and its `Claim` method are local fixtures,
not a production lease service.

The engine records a model intent before `Send` and a tool start before a local
executor call. It records their outcomes before acting on them. After a crash,
an intent without a result means the effect may have occurred. Reload, call
`Replay` to validate the record and inspect `Pending`, then reconcile by the
recorded attempt or call ID. `Accept{ModelResult: ...}` or
`Accept{Outcome: ...}` records a verified result; an external result must be a
final `tool.Outcome`. Never infer nonexecution from a transport error or send
the same unresolved intent again. A durable host also needs downstream
idempotency or reconciliation for effects that cannot be queried.

## Tool and policy boundaries

Tools resolve before each model request; the accepted intent records their
definitions. A local executor runs only after valid arguments and an allowed
policy decision. A policy rewrite is checked and authorized again with its
effective arguments; the original arguments remain on the recorded call.
`tool.KindExternal` produces pending work for a host to settle with `Accept`.
Deferred decisions require an `ApprovalFact` bound to the call ID, argument
digest, and policy revision. Provider-hosted approval is a separate response;
it never becomes a locally executable tool call. The default policy denies
client tools. Use `AllowLocalPolicy` only when that permission is intended.

`Observer` receives bounded, lossy display events. Stream deltas are
provisional and carry turn, attempt, and message IDs; the recorder's settled
result is authoritative. The engine does not own session storage, durable
leases, budget enforcement, child turns, or compaction. Its Go API and record
shape are experimental.
