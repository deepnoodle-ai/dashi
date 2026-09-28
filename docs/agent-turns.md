# Agent turns and recovery

A turn is one input processed until the agent settles; it can contain many
model calls. Some SDKs call a single model call a "turn"; Dashi calls that a
model attempt.

An `Agent` pins reusable behavior and limits as plain JSON data. A `Runner`
holds the agent and its long-lived services: the model client, tool source,
hooks, and optional recorder and observer. An empty `Agent.Provider` or
`Agent.API` is filled from the client, an empty `Model` is the provider's
`catalog.Default`, and an empty `Revision` is derived from the agent's
behavior fields. On an existing turn, a zero `Agent` is the recorded one, and a
partly set agent takes its empty `ID`, `Model`, and `Revision` from the record.
The turn ID (`dashi.NewTurn(id)`) and each command's `CommandID` are generated
when empty (`dashi.NewID`), but a durable host should choose and save them
before the call. The command ID is an idempotency key: repeating it with the
same payload returns the recorded result without repeating an effect, which a
generated ID cannot do for a retry, and reusing it with different content
fails. Pass the returned `Outcome.Turn` to the next call, or reload it from a
durable recorder. When the recorder implements `Loader`, Dashi also rejects a
snapshot whose recorded agent differs from the stored one.

`Start` records its origin, its own input, and a fingerprint of
`Start.History`. Pass the earlier conversation again in `Continue.History` on
every resume; the saved turn holds only its own input and output. A mismatch
warns and proceeds, unless `Continue.OnHistoryMismatch` is
`HistoryMismatchReject`, which returns `ErrHistoryMismatch` and commits
nothing. See [the runnable example](../examples/agent/main.go) for setup.

## Results and errors

`Runner.Run` returns an `Outcome` that is always the latest known state, and an
error that says whether the command applied. `Outcome` holds the turn and
embeds its `TurnView`: `Stop`, `Answer`, `Output`, `Spend`, typed `Pending`
work, and per-call `Tools`. `Replay(turn)` gives the same view for a loaded
turn.

| `Run` error                                   | Caller action                                                                                  |
| --------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `nil`                                         | Every step was recorded.                                                                       |
| `ErrRevisionConflict`, `ErrCommandConflict`   | Another worker owns or advanced the turn, or the command ID was reused. Reload and back off.   |
| `*CommitError`, `PersistenceRejected`         | The run executed, but `CommitError.Step` was not saved. Recover as after a crash.              |
| `*CommitError`, `PersistenceUnknown`          | The write may have committed. Reload the turn before any retry.                                |
| `ErrRecordInvalid`                            | Steps this run recorded fail replay: a runner or recorder bug. Something was recorded.        |
| `ErrTurnEnded`                                | The turn has ended; only late results of started work are accepted. Reload for its final state. |
| any other error                               | The command was rejected and nothing was recorded. The outcome is the caller's turn.          |

Check the conflicts with `errors.Is` first; they may arrive inside a
`*CommitError`. A failed `model_settled` write leaves that call's usage out of
`Outcome.Spend`; it is on `CommitError.Step`.

| Stop              | Caller action                                                                           |
| ----------------- | --------------------------------------------------------------------------------------- |
| `StopWaiting`     | Answer each `Pending` item with `Approve`, `Deny`, or `Result`, then `Continue`.         |
| `StopInterrupted` | Call `Continue` with the saved record and earlier history, on any worker.               |
| `StopUncertain`   | Reconcile the identified model or tool effect with `Settle`, then `Continue`.            |
| `StopCompleted`   | No further execution is allowed.                                                        |
| any, `Terminal`   | The turn has ended. Record late results of started work with `Settle`; nothing else.    |

`StopFailed`, `StopRefused`, and `StopLimited` keep their reason and record.
`Settle` records a result, approval, or reconciliation; it never runs the turn.
`Continue` resumes a stopped turn and returns `ErrNotReady`, naming what the
turn waits on, when it cannot. `Outcome.ReadyToContinue`, and the same flag on
a replayed turn, tells a host's sweeper when to schedule `Continue`. `Deliver`
records context without running. `Cancel` ends the turn. `Stop.Terminal`
marks every stop that ends a turn: completed, canceled, a deadline or
interruptions limit, any limit reached after the last model attempt, or a cost
cap over an unknown cost. At any terminal stop, unstarted calls become known
not executed, while started effects can still receive late results; other
commands return `ErrTurnEnded`.

An ended Go context records `StopInterrupted` with its cause; a crash may leave
no stop, and `Continue` recovers that too. `dashi.WithPause(ch)` requests a
stop before the next model or tool call. `Runner.OnContextDone: ContextCancel`
makes an ended context record `StopCanceled`. `Limits.Deadline` stops the turn
as limited, and `Limits.Interruptions` bounds resumptions (default 10).

By default the last allowed model attempt, after earlier ones, is reserved for
a final answer: it omits tools and ends with `StopLimited` and reason
`model_attempts`, with its text in `Outcome.Answer`. Set
`Limits.NoReservedFinalAnswer` to turn this off.

## Structured final results

Set `Agent.OutputSchema`, or `Start.OutputSchema` for one turn: a JSON Schema
object with root `"type":"object"`, at most 32 KiB, recorded at start.
`dashi.MustOutputSchema[T]()` generates one from a Go struct with `tool.Func`'s
rules. The runner advertises a reserved `dashi_submit_output` tool; the model
may use application tools first, then submit the object. Dashi validates and
records it; an invalid submission is returned to the model as a tool error. A
final response holding exactly one JSON object is accepted as a fallback.
Without a valid value the turn stops with `output_schema_unsatisfied`.
`Outcome.Output` holds the value (at most 256 KiB), its source, and its attempt
and call IDs; `Answer` may be empty. `dashi.DecodeOutput[T](outcome)` decodes
the value, or returns an error wrapping `ErrNoOutput` that names the stop when
there is none. Once recorded the result is final: `Continue` can finish an
unacknowledged stop, while `Deliver` and `Cancel` are rejected. This is a
result contract; the provider may not constrain generation.

## Recorder contract

`Recorder.Commit` acknowledges one versioned `Step` under `ExpectedSeq` and an
optional `Owner` token (`dashi.WithOwner`). A host recorder should atomically
check revision and owner, append the step, and commit related budget,
accounting, or outbox rows; each `BeforeSend` hook may supply non-secret
admission JSON for that transaction, recorded under the hook's name. Return
`CommitError` with `PersistenceRejected` only when the write certainly did not
commit, and `PersistenceUnknown` (or a plain error) when it may have. A recorder
that also implements `Loader` lets `Run` answer a command retried with a stale
snapshot from the stored record; `Load` reports a missing turn as
`ErrTurnNotFound`, and any other `Load` error is returned without committing.
With a nil `Runner.Recorder`, Dashi uses a fresh `MemoryRecorder` per call, so
keep `Outcome.Turn` for continuation in the same process. `MemoryRecorder` is a
local fixture, not a lease service.

A recorder must preserve each step's JSON meaning, not its bytes: a Postgres
`jsonb` column is fine, because Dashi compares recorded values by canonical
JSON. Package `dashitest` checks a recorder against this contract, and its
`FaultRecorder` fails a chosen commit so a host can test recovery.

The runner records a model intent before `Send` and a tool start before a local
executor call, and records their outcomes before acting on them. An interrupted
model call keeps its partial result and usage for display and accounting, and
`Continue` resends it under a new attempt ID unless a provider-hosted tool may
have had an effect. `ModelFact.Interrupted` records why a call was cut short
(`run`, `worker`, or `steering` for an urgent steer, which the run replaces
at once). Interrupted calls do not use `ModelAttempts`. A started
tool with no result is rerun only when declared `ReadOnly` or `Idempotent`;
otherwise the effect is uncertain until the host reconciles it with
`Settle{Outcome}` or `Settle{ModelResult}`. `Settle{ModelFailure}` requires
verified nonexecution evidence; never infer nonexecution from an unclassified
transport error. `llm.Call.Attempt` carries the recorded attempt ID, sent at
most once, so a client can key durable jobs by it.

## Tool and hook boundaries

Tools resolve before each model request, and the intent records their
definitions. A local executor runs only after valid arguments and an allowed
`BeforeTool` verdict; a hook's rewrite is checked against the schema and every
hook is asked again about the new arguments. `tool.KindExternal` calls wait for
the host's `Settle`. A deferred call needs an answer bound to its call ID and
arguments: `Pending.Approve` and `Pending.Deny(reason)` build it, and
`dashi.ArgsDigest` computes the digest from arguments shown earlier. The digest
compares JSON meaning, so it survives a store that respells the arguments. A
denial's reason becomes the call's result for the model. With no hook opinion,
every bound local and external tool runs, recorded with the reason
`no hook opinion`; binding a tool is the app's statement that the model may use
it.
`dashi.RequireApproval(names...)` defers the named tools. Provider-hosted
approval is a separate response that never becomes a local call.

Context from the user (`ContextUser`, also the meaning of an empty origin),
the application's runtime (`ContextRuntime`), or a tool (`ContextTool`) uses
the user role; only operator context (`ContextOperator`) and operator-origin
starts may use a system or developer role, and the host authenticates that
origin. A local executor's Go error settles the call as
`tool.StateFailed` with the error text, and the turn continues; wrap it with
`tool.Fatal` to stop the turn with reason `tool_fatal`. An outcome that fails
`Validate` stops with `tool_outcome_invalid`, and a recovered panic stops
uncertain. [Runtime reminders](../design/runtime-reminders.md) describes
recorded, typed context.

`Observer` receives every acknowledged step in order, plus model deltas and
tool progress that it may drop when it falls behind, reporting dropped deltas
with an `Event.Gap` that names the message. `Runner.Stream` is a
backpressured iterator over acknowledged steps, provisional deltas, and tool
progress, ending with one `Event.Outcome`. Leaving the range interrupts the
turn by default; do not range it in an HTTP handler for a durable turn (see
[serving live updates](guides/running-turns.md#serving-live-updates)).

An optional per-turn `Budget` caps output tokens, total tokens, and model cost
with a resumable `limited` stop; see [turn budgets](guides/budgets.md). The
runner does not own session storage, durable leases, child turns, or
compaction. Its Go API and record shape are experimental.
