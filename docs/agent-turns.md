# Agent turns and recovery

A turn is one input processed until the agent settles; it can contain many
model calls. Some SDKs call a single model call a "turn"; Dashi calls that a
model attempt.

The root `dashi` package runs one identified turn at a time. An `Agent` pins
reusable behavior and limits as plain JSON data. A `Runner` holds the agent and
its long-lived services: the model client, tool source, policy, and optional
recorder and observer. An empty `Agent.Provider` or `Agent.API` is filled from
the runner's client; a value that differs from the client's is an error. The
caller supplies a stable turn ID (`dashi.NewTurn(id)`) and a unique `CommandID`
for each `Start`, `Deliver`, `Continue`, `Settle`, or `Cancel` command. The
command ID is an idempotency key: repeating it with the same payload
acknowledges the recorded step without repeating an effect. Pass the returned
`Outcome.Turn` to the next call, or reload it from a durable recorder. When the
recorder implements `Loader`, Dashi can refresh a stale turn snapshot on a
retried command. Reusing a command ID with different content fails. `Start`
records its origin and own input, plus a fingerprint of `Start.History`. Pass
the earlier conversation again in `Continue.History` on every resume, including
after a crash. Dashi uses it for that call's model requests; the saved turn
holds its own input and output, not those earlier messages. A mismatch warns and
proceeds. A host that must never resume with different history sets
`Continue.OnHistoryMismatch: HistoryMismatchReject`; `Continue` then returns
`ErrHistoryMismatch` and commits nothing, so the turn resumes once the host
passes the history it started with. If `Start` had no history, omit `History` on
`Continue` or pass an empty slice.

See [the runnable example](../examples/agent/main.go) for setup.
`Runner.Run(ctx, turn, cmd, opts...)` returns a Go error for a command or
record rejected before a new fact is acknowledged. After work begins, inspect
`Outcome`: it retains the turn and stop, including partial work;
`Outcome.RecordingErr()` reports a write that was not acknowledged.
`Outcome.Turn.AnswerText()` extracts text from the last settled assistant
result; a valid tool-only turn may have no answer text.

| Outcome field | Caller action |
| --- | --- |
| `PersistenceAcknowledged` | Use the returned turn or load it from the recorder. |
| `PersistenceRejected` | No new step was accepted. Fix the write or command before trying again. |
| `PersistenceUnknown` | Reload the turn by ID before any retry. The write may have committed. |
| `StopWaiting` | Inspect `Pending`; use `Settle` for external results or approvals. |
| `StopInterrupted` | Reschedule the turn and call `Continue` with its saved record and earlier history. |
| `StopUncertain` | Reconcile the identified model or tool effect before advancing. |
| `StopCompleted` | No further execution is allowed for this turn. |

`StopFailed`, `StopRefused`, and `StopLimited` retain their reason and record.
`Settle` records an external result, approval, or reconciliation; it never runs
the turn. `Continue` is the command that resumes a stopped turn. After `Settle`,
check `Outcome.ReadyToContinue` or replay the saved turn and check its
`TurnView.ReadyToContinue` flag. Schedule `Continue` when true. `Deliver`
records context without advancing execution. `Cancel` ends the turn: unstarted
calls become known not executed, while started effects can still receive late
results for reconciliation and usage accounting.

By default, an ended Go context records `StopInterrupted`, a resumable stop
whose reason is the context cause. A crash may leave no stop; `Continue` also
recovers that record on another worker. Closing the channel passed with
`dashi.WithPause(ch)` requests a stop before the next model or tool call,
without cutting off work already running. A request handler that treats a
disconnect as final can set `Runner.OnContextDone: ContextCancel`; an ended
context then records `StopCanceled`. `Limits.Deadline` ends the turn with
`StopLimited`; a host's context deadline interrupts it. `Limits.Interruptions`
bounds resumptions after interruptions (default 10; a negative value allows
none).

## Runtime reminders

Use `llm.ReminderMessage(llm.Reminder{Name: "workspace", Tier:
llm.ReminderContextual, Content: "..."})` in `Start.Input` or a `ContextItem`
delivered with `Deliver` for recorded context. A `ContextItem` can use
`Snapshot` for latest-value replacement or `NextCallOnly` for one model call.
Only `StartOperator` or `ContextItem{Origin: "operator"}` may carry an
operator-tier recorded reminder. The host authenticates those origins.

The full typed reminder is saved in the turn and rebuilt for later model
requests after `Settle` or `Continue`. `BeforeModel` can also deliver one
through `ModelDecision.Deliver`; that delivery is recorded before the model
call. Provider preparation renders the typed content and records the resulting
role in `ContentChanges`. Operator-tier reminders use developer authority on
first-party OpenAI Responses; other routes currently receive user-role text. See
[runtime reminders](../design/runtime-reminders.md) for the tradeoffs.

By default, Dashi reserves the last model attempt for a final answer after at
least one earlier attempt. That request omits application and provider-hosted
tools and clears forced tool choice. It is prepared, authorized, recorded, and
counted like any other attempt; there is no extra call beyond `ModelAttempts`.
The final response ends with `StopLimited` and reason `model_attempts`, even
when it contains useful text. Read that text with `Turn.AnswerText()`. A
structured-output turn may still call the internal `dashi_submit_output` tool;
its validated value is available in `Outcome.Output`. Set
`Limits.NoReservedFinalAnswer` to turn this behavior off.
With only one allowed attempt, there is no earlier call from which to reserve
a final answer.

## Structured final results

Set `Agent.OutputSchema` for a reusable default, or set
`Start.OutputSchema` to request a different shape for one turn. The schema must
be a JSON Schema object with root `"type":"object"`; it is recorded at start
and cannot change during recovery. Schema and value are bounded at 32 KiB and
256 KiB. `Outcome.Output` contains the validated JSON object, its source, model
attempt ID, and optional tool call ID. `Turn.StructuredOutput()` retrieves the
same recorded fact. A completed turn with an output schema always has this
value; `AnswerText()` may be empty. Once the output is recorded the result is
final: if the final stop was not acknowledged, `Continue` finishes the
record, while `Deliver` and `Cancel` are rejected rather than silently ignored.

The runner advertises `dashi_submit_output` as a reserved model tool. The
model may use application tools first, then call it with the final object in a
separate response. Dashi validates and records the submission; the host does
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
optional `Owner` fencing token, passed with `dashi.WithOwner(token)`. A host
recorder should atomically check the current revision and owner, append the
step, and commit any related budget, accounting, or outbox rows. `BeforeSend`
may supply bounded, non-secret admission JSON derived from the prepared request
for that transaction. Return `CommitError` with `PersistenceRejected` only when
the write certainly did not commit. Return `PersistenceUnknown` for an ambiguous
acknowledgement; plain errors are also treated as unknown. Provide a way to load
the authoritative turn by ID after an unknown result. With nil
`Runner.Recorder`, Dashi creates a fresh `MemoryRecorder` for that call; keep
`Outcome.Turn` for same-process continuation. Bind a durable recorder for
cross-process recovery, and pass the earlier history on `Continue`.
`MemoryRecorder` and its `Claim` method are local fixtures, not a production
lease service. A recorder that also implements `Loader` lets `Runner.Run`
recognize a command retried with an old turn snapshot and answer from the stored
record; other hosts should reload before retrying after an ambiguous write.
`Load` reports a turn with no steps as `ErrTurnNotFound` or an empty turn with
the requested ID. Any other `Load` error is returned from `Run` without
committing, because the caller's snapshot may be stale.

A recorder must preserve each step's JSON meaning, not its bytes. Storage that
reorders keys, drops whitespace, or respells numbers and strings, such as a
Postgres `jsonb` column, is fine: Dashi compares recorded values with live
ones by their canonical JSON (RFC 8785 layout, numbers compared by exact
decimal value), and history passed on `Continue` may round-trip the same way.

Package `dashitest` checks a host recorder against this contract with
`dashitest.Run`, and its `FaultRecorder` fails one chosen commit (rejected,
lost write, or lost acknowledgement) so a host can test recovery end to end.

The runner records a model intent before `Send` and a tool start before a local
executor call. It records their outcomes before acting on them. After a crash,
reload and call `Replay` to inspect `Pending`. An interrupted model call keeps
its partial result and usage for display and accounting, but its output is not
acted on. `Continue` resends it unless a provider-hosted tool may have caused
an effect. Interrupted calls do not use `ModelAttempts`; their usage still
counts toward spend. A started tool with no result is rerun only when its
definition says `ReadOnly` or `Idempotent`. Otherwise the effect is uncertain:
reconcile by the recorded attempt or call ID. `Settle{ModelResult: ...}` or
`Settle{Outcome: ...}` records a verified result; an external result must be a
final `tool.Outcome`. A host can submit `Settle{ModelFailure: ...}` only with
verified nonexecution evidence. An explicit HTTP rejection or pre-dispatch
failure is recorded as known; ambiguous transport and protocol failures remain
uncertain. Never infer nonexecution from an unclassified transport error.
Provider-hosted tools follow the same safety rule as local tools. A durable
host also needs downstream idempotency or reconciliation for effects that
cannot be queried. `Send` receives the recorded attempt ID in
`llm.Call.Attempt`; Dashi sends each ID at most once and resends an
interrupted call under a new ID, so a client can key durable jobs by it.

## Tool and policy boundaries

Tools resolve before each model request; the accepted intent records their
definitions. A local executor runs only after valid arguments and an allowed
policy decision. A policy rewrite is checked and authorized again with its
effective arguments; the original arguments remain on the recorded call.
`tool.KindExternal` produces pending work for a host to settle with `Settle`.
Deferred decisions require an `ApprovalFact` bound to the call ID, argument
digest, and policy revision. Copy the digest from the pending call's
`ToolView.ArgsDigest` (from `Replay`), or compute it with `dashi.ArgsDigest`
from the arguments shown; it binds the answer to the arguments' JSON meaning,
so it survives a store that respells them. Provider-hosted approval is a
separate response; it never becomes a locally executable tool call. The
default policy denies client tools. Use `AllowLocalPolicy` only when that
permission is intended.
Context from a user, library, or tool uses the user message role; only
operator-sourced context may use a system or developer role. The host must
authorize who can submit operator context.
Only operator-origin starts may introduce system or developer role input; a
scheduled start retains its separate origin with user-role input. The host
authenticates that origin.

A local executor's Go error is a failure the model sees. The call settles as
`tool.StateFailed` with the error text as its content and in
`ToolFact.Reason`, and the turn continues. Wrap an error with `tool.Fatal`
when continuing would be wrong, such as lost credentials or an outage: the
call settles the same way and the turn stops with `StopFailed` and reason
`tool_fatal`. An outcome that fails `Validate` is an executor bug and stops
with `tool_outcome_invalid`. Fix the cause, then `Continue`; the model sees
the failure as the call's result. Uncertainty is Dashi's finding, never a
tool's: a recovered panic (reason `panic: ...`) stops with `StopUncertain`.
After a crash, a started tool without a result stops uncertain if it was not
declared safe to repeat. Reconcile it with `Settle`, then call `Continue`. A
tool result that arrives after the running context ends is dropped.

`Observer` receives bounded, lossy display events. Stream deltas are provisional
and carry turn, attempt, and message IDs; the recorder's settled result is
authoritative. `Runner.Stream` offers a backpressured iterator over acknowledged
steps and provisional deltas. A fully consumed range ends with one
`Event.Outcome`; command validation errors use the iterator's error slot.
Stopping the range interrupts the turn by default and suppresses the terminal
event. Set `Runner.OnContextDone: ContextCancel` when leaving the range should
end the turn instead. Reload the record by turn ID to inspect what was
committed, including partial model results or pending tools. The sequence is
single-use; another range returns `ErrStreamConsumed`. `Runner.Observer` remains
an independent, lossy hook, while `Runner.Run` remains the collected call. Do
not range `Stream` in an HTTP handler for a durable turn: a client disconnect
would end it. See [serving live
updates](guides/running-turns.md#serving-live-updates).

Dashi enforces an optional per-turn `Budget`: output-token, total-token, and
model-cost caps, with a resumable `limited` stop. `BeforeSend` admission lets
the host hold credits and deny a call with the same resumable stop. The host
owns credit balances, plan ceilings, and any ceiling on tool charges; see
[turn budgets](guides/budgets.md). The runner does not own session storage,
durable leases, child turns, or compaction. Its Go API and record shape are
experimental.
