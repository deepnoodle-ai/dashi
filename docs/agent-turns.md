# Agent turns and recovery

The root `kernel` package runs one identified turn at a time. A Turn is one
piece of agent work. A `Definition` pins reusable behavior and limits;
`Bindings` supplies the model client, tool source, policy, and optional
recorder and observer. The caller supplies a stable `Turn.ID` and a unique ID
for each `Start`, `Deliver`, `Continue`, `Accept`, or `Cancel` command.
Repeating the same command ID and payload acknowledges its
recorded step without repeating an effect. Pass the returned `Outcome.Turn` to
the next call, or reload it from a durable recorder. When the recorder exposes
`Load`, Kernel can refresh a stale turn snapshot on a retried command.
Reusing an ID with different content fails. `Start` records its origin and own
input, plus a fingerprint of `Start.History`. Pass the earlier conversation
again in `Continue.History` on every resume, including after a crash. Kernel
uses it for that call's model requests; the saved turn holds its own input and
output, not those earlier messages. A mismatch warns and proceeds. A host that
must never resume with different history sets `Bindings.OnHistoryMismatch:
HistoryMismatchReject`; `Continue` then returns `ErrHistoryMismatch` and commits
nothing, so the turn resumes once the host passes the history it started with.
Turns recorded without a fingerprint continue unchecked. If `Start`
had no history, omit `History` on `Continue` or pass an empty slice.

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
| `StopInterrupted` | Reschedule the turn and call `Continue` with its saved record and earlier history. |
| `StopUncertain` | Reconcile the identified model or tool effect before advancing. |
| `StopCompleted` | No further execution is allowed for this turn. |

`StopFailed`, `StopRefused`, and `StopLimited` retain their reason and record.
`Accept` records an external result, approval, or reconciliation; it never runs
the turn. `Continue` is the command that resumes a stopped turn. After `Accept`,
check `Outcome.ReadyToContinue` or replay the saved turn and check its
`TurnView.ReadyToContinue` flag. Schedule `Continue` when true. `Deliver`
records context without advancing execution. `Cancel` ends the turn: unstarted
calls become known not executed, while started effects can still receive late
results for reconciliation and usage accounting.

By default, an ended Go context records `StopInterrupted`, a resumable stop
whose reason is the context cause. A crash may leave no stop; `Continue` also
recovers that record on another worker. `Bindings.Pause` closes to request a
stop before the next model or tool call, without cutting off work already
running. A request handler that treats a disconnect as final can set
`Bindings.OnContextDone: ContextCancel`; an ended context then records
`StopCanceled`. `Limits.Deadline` ends the turn with `StopLimited`; a host's
context deadline interrupts it. `Limits.Interruptions` bounds resumptions
after interruptions (10 in `DefaultLimits()`).

## Runtime reminders

Use `llm.ReminderMessage(llm.Reminder{Name: "workspace", Tier:
llm.ReminderContextual, Content: "..."})` in `Start.Input` or a `ContextItem`
delivered with `Deliver` for recorded context. A `ContextItem` can use
`Snapshot` for latest-value replacement or `NextCallOnly` for one model call.
Only `StartOperator` or `ContextItem{Origin: "operator"}` may carry an
operator-tier recorded reminder. The host authenticates those origins.

The full typed reminder is saved in the turn and rebuilt for later model
requests after `Accept` or `Continue`. `BeforeModel` can also deliver one through
`ModelDecision.Deliver`; that delivery is recorded before the model call.
Provider preparation renders the typed content and records the resulting role in
`ContentChanges`. Operator-tier reminders use developer authority on
first-party OpenAI Responses; other routes currently receive user-role text.
See [runtime reminders](../design/runtime-reminders.md) for the tradeoffs.

`DefaultLimits()` reserves the last model attempt for a final answer after at
least one earlier attempt. That request omits application and provider-hosted
tools and clears forced tool choice. It is prepared, authorized, recorded, and
counted like any other attempt; there is no extra call beyond `ModelAttempts`.
The final response ends with `StopLimited` and reason `model_attempts`, even
when it contains useful text. Read that text with `Turn.AnswerText()`. A
structured-output turn may still call the internal `kernel_submit_output` tool;
its validated value is available in `Outcome.Output`. Set
`Limits.ReserveFinalAnswer` on a custom `Limits` value to use this behavior.
With only one allowed attempt, there is no earlier call from which to reserve
a final answer.

## Structured final results

Set `Definition.OutputSchema` for a reusable default, or set
`Start.OutputSchema` to request a different shape for one turn. The schema must
be a JSON Schema object with root `"type":"object"`; it is recorded at start
and cannot change during recovery. Schema and value are bounded at 32 KiB and
256 KiB. `Outcome.Output` contains the validated JSON object, its source, model
attempt ID, and optional tool call ID. `Turn.StructuredOutput()` retrieves the
same recorded fact. A completed turn with an output schema always has this
value; `AnswerText()` may be empty. Once the output is recorded the result is
final: if the final stop was not acknowledged, `Continue` finishes the
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
an unknown result. With nil `Bindings.Recorder`, Kernel creates a fresh
`MemoryRecorder` for that call; keep `Outcome.Turn` for same-process
continuation. Bind a durable recorder for cross-process recovery, and pass the
earlier history on `Continue`. `MemoryRecorder` and its `Claim` method are
local fixtures, not a production lease service. A recorder that also
implements `Loader` lets `Engine.Apply` recognize a command retried with an
old turn snapshot and answer from the stored record; other hosts should
reload before retrying after an ambiguous write.

A recorder must preserve each step's JSON meaning, not its bytes. Storage that
reorders keys, drops whitespace, or respells numbers and strings, such as a
Postgres `jsonb` column, is fine: Kernel compares recorded values with live
ones by their canonical JSON (RFC 8785 layout, numbers compared by exact
decimal value), and history passed on `Continue` may round-trip the same way.

Package `kerneltest` checks a host recorder against this contract with
`kerneltest.Run`, and its `FaultRecorder` fails one chosen commit (rejected,
lost write, or lost acknowledgement) so a host can test recovery end to end.

The engine records a model intent before `Send` and a tool start before a local
executor call. It records their outcomes before acting on them. After a crash,
reload and call `Replay` to inspect `Pending`. An interrupted model call keeps
its partial result and usage for display and accounting, but its output is not
acted on. `Continue` resends it unless a provider-hosted tool may have caused
an effect. Interrupted calls do not use `ModelAttempts`; their usage still
counts toward spend. A started tool with no result is rerun only when its
definition says `ReadOnly` or `Idempotent`. Otherwise the effect is uncertain:
reconcile by the recorded attempt or call ID. `Accept{ModelResult: ...}` or
`Accept{Outcome: ...}` records a verified result; an external result must be a
final `tool.Outcome`. A host can submit `Accept{ModelFailure: ...}` only with
verified nonexecution evidence. An explicit HTTP rejection or pre-dispatch
failure is recorded as known; ambiguous transport and protocol failures remain
uncertain. Never infer nonexecution from an unclassified transport error.
Provider-hosted tools follow the same safety rule as local tools. A durable
host also needs downstream idempotency or reconciliation for effects that
cannot be queried. `Send` receives the recorded attempt ID in
`llm.Call.Attempt`; Kernel sends each ID at most once and resends an
interrupted call under a new ID, so a client can key durable jobs by it.

## Tool and policy boundaries

Tools resolve before each model request; the accepted intent records their
definitions. A local executor runs only after valid arguments and an allowed
policy decision. A policy rewrite is checked and authorized again with its
effective arguments; the original arguments remain on the recorded call.
`tool.KindExternal` produces pending work for a host to settle with `Accept`.
Deferred decisions require an `ApprovalFact` bound to the call ID, argument
digest, and policy revision. Copy the digest from the pending call's
`ToolView.ArgsDigest` (from `Replay`), or compute it with `kernel.ArgsDigest`
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
the failure as the call's result. Uncertainty is the engine's finding, never a
tool's: a recovered panic (reason `panic: ...`) stops with `StopUncertain`.
After a crash, a started tool without a result stops uncertain if it was not
declared safe to repeat. Reconcile it with `Accept`, then call `Continue`. A
tool result that arrives after the running context ends is dropped.

`Observer` receives bounded, lossy display events. Stream deltas are
provisional and carry turn, attempt, and message IDs; the recorder's settled
result is authoritative. `Engine.Stream` offers a backpressured iterator over
acknowledged steps and provisional deltas. A fully consumed range ends with
one `Event.Outcome`; command validation errors use the iterator's error slot.
Stopping the range interrupts the turn by default and suppresses the terminal
event. Set `OnContextDone: ContextCancel` when leaving the range should end
the turn instead. Reload the record by turn ID to inspect what was committed,
including partial model results or pending tools. The sequence is single-use;
another range returns `ErrStreamConsumed`. `Bindings.Observer` remains an independent,
lossy hook, while `Engine.Apply` remains the collected call. Do not range
`Stream` in an HTTP handler for a durable turn: a client disconnect would end
it. See [serving live updates](guides/running-turns.md#serving-live-updates).

Kernel enforces an optional per-turn `Budget`: output-token, total-token, and
model-cost caps, with a resumable `limited` stop. `BeforeSend` admission lets
the host hold credits and deny a call with the same resumable stop. The host
owns credit balances, plan ceilings, and any ceiling on tool charges; see
[turn budgets](guides/budgets.md). The engine does not own session storage,
durable leases, child turns, or compaction. Its Go API and record shape are
experimental.
