# Agent turns and recovery

The root `kernel` package runs one identified turn at a time. A `Definition`
pins reusable behavior and limits; `Bindings` supplies the model client, tool
source, recorder, policy, and optional observer. The caller supplies a stable
`Turn.ID` and a unique ID for each `Start`, `Deliver`, `Continue`, `Accept`, or
`Cancel` command. Repeating the same command ID and payload acknowledges its
recorded step without repeating an effect. When the recorder exposes `Load`,
the outcome contains the latest turn; otherwise callers should reload it.
Reusing an ID with different content fails. `Start` records its origin and a
copy of prior history, which later invocations project without substitution.

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
advancing execution. `Cancel` prevents further execution: unstarted calls
become known not executed, while started effects can still receive late results
for reconciliation and usage accounting.

## Structured final results

Set `Definition.OutputSchema` for a reusable default, or set
`Start.OutputSchema` to request a different shape for one turn. The schema must
be a JSON Schema object with root `"type":"object"`; it is recorded at start
and cannot change during recovery. Schema and value are bounded at 32 KiB and
256 KiB. `Outcome.Output` contains the validated JSON object, its source, model
attempt ID, and optional tool call ID. `Turn.StructuredOutput()` retrieves the
same recorded fact. A completed turn with an output schema always has this
value; `AnswerText()` may be empty. Once the output is recorded the result is
final: if the completion stop was not acknowledged, `Continue` finishes the
record, while `Deliver` and `Cancel` are rejected rather than silently ignored.

The engine advertises `kernel_submit_output` as a reserved model tool. The
model may use application tools first, then call it with the final object in a
separate response. Kernel validates and records the submission; the host does
not bind, authorize, or settle this internal tool. An invalid submission
returns a tool error so the model can correct it within the attempt limit. A
final response containing only one JSON object (or one `json` fenced object)
is accepted as a fallback after the same validation. If neither path provides
a valid value, the turn stops with `output_schema_unsatisfied`. Provider
refusal and output limits keep their own stop kinds. This is a result contract;
generation is not guaranteed to be constrained at the provider. Application
tools cannot use the reserved name.

## Recorder contract

`Recorder.Commit` acknowledges one versioned `Step` under `ExpectedSeq` and an
optional `Owner` fencing token. A host recorder should atomically check the
current revision and owner, append the step, and commit any related budget,
accounting, or outbox rows. `BeforeSend` may supply bounded, non-secret admission
JSON derived from the prepared request for that transaction. Return `CommitError` with
`PersistenceRejected` only when the write certainly did not commit. Return
`PersistenceUnknown` for an ambiguous acknowledgement; plain errors are also
treated as unknown. Provide a way to load the authoritative turn by ID after
an unknown result. `MemoryRecorder` and its `Claim` method are local fixtures,
not a production lease service. `Engine.Apply` uses an optional recorder
`Load(string)` method to recognize a command retried with an old turn snapshot;
other hosts should reload before retrying after an ambiguous write.

The engine records a model intent before `Send` and a tool start before a local
executor call. It records their outcomes before acting on them. After a crash,
an intent without a result means the effect may have occurred. Reload, call
`Replay` to validate the record and inspect `Pending`, then reconcile by the
recorded attempt or call ID. `Accept{ModelResult: ...}` or
`Accept{Outcome: ...}` records a verified result; an external result must be a
final `tool.Outcome`. A host can submit `Accept{ModelFailure: ...}` only with
verified nonexecution evidence. An explicit HTTP rejection or pre-dispatch
failure is recorded as known; ambiguous transport and protocol failures remain
uncertain. Never infer nonexecution from an unclassified transport error or send
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
Context from a user, library, or tool uses the user message role; only
operator-sourced context may use a system or developer role. The host must
authorize who can submit operator context.
Only operator-origin starts may introduce system or developer role input; a
scheduled start retains its separate origin with user-role input. The host
authenticates that origin.

`Observer` receives bounded, lossy display events. Stream deltas are
provisional and carry turn, attempt, and message IDs; the recorder's settled
result is authoritative. The engine does not own session storage, durable
leases, budget enforcement, child turns, or compaction. Its Go API and record
shape are experimental.
