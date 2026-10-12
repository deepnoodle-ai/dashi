# Recovering turns

This guide covers turn and command IDs, recorders, recovery after a crash or
interruption, the three turn states, and limits.
[Running turns](running-turns.md) covers the basics;
[agent turns and recovery](../agent-turns.md) is the detailed contract.

## IDs and commands

A turn ID lets any worker find the turn again, and a `CommandID` lets Dashi
recognize a repeated command: the same ID with the same content returns the
recorded result instead of running again. Generated IDs are fine in one
process. For retries across restarts, choose the turn ID and each
`CommandID` yourself (`dashi.NewID()` makes one), save them before the call,
and reuse them on retry; nobody can reuse an ID generated inside a call that
crashed. Without that, a retry from an old turn snapshot is still refused by
the recorder, because its step numbers are taken, so no model call or tool
runs twice. What can happen is a second turn from a `Start` retried with no
turn ID, or an `AddMessages` recorded twice when its items carry no IDs.

A host that stores commands, such as an [inbox](#advancing-saved-turns)
table, encodes them with `dashi.MarshalCommand` and decodes them with
`dashi.UnmarshalCommand`. The decoded command equals the original and has the
same hash, so applying it again is a retry, not a conflict.
`dashi.CommandIDOf` and `dashi.CommandKindOf` give the identity to index saved
commands by. Commands also encode with plain
`encoding/json`; a JSON `null` output schema reads as none.

Values a turn records travel on its commands: `Start.Agent` carries the agent,
and the turn records it. `Continue`, `Resolve`, and `Cancel` take no agent;
they run with the one recorded in the turn you pass in, so load that turn
from your own store. Process wiring for one call, such as steering, goes in
run options, and long-lived services go on the `Runner`. A retried `Start`
whose agent resolves differently from the recorded one fails with
`ErrAgentChanged`, whose message names the fields. When the recorder
implements `Loader`, Dashi also checks the snapshot's agent against the
stored record and returns `ErrAgentChanged` if they differ.

## Resuming after a stop or crash

A turn can span workers; its saved record holds the steps needed to resume its
own work. When the running Go context ends, Dashi normally records
`StopInterrupted` with the context cause. Reload the record and call `Continue`
with the earlier conversation on any worker. `Continue` also recovers a crash
that happened before the stop was written; such a turn's `Next()` is
`NextRecover`, and the next model call gets a reminder that the previous worker
stopped mid-turn, so the model checks the current state before repeating any
action. Repeated crashes before that call still add one reminder. Close the
channel passed with
`dashi.WithPause` for a graceful stop before the next model or tool call. Work
already running finishes.

Only `Start` and `Continue` call the model, so only `Run` needs
`Runner.Client`. `Runner.Record` (below) does not when the Start's Agent names
its provider, API, and model. `Resolve`, `AddMessages`, and `Cancel` record
facts and run with a `Runner` that has just a `Recorder`. A turn with no steps
has not started, so `Cancel` refuses it; record its `Start` first.

To save a turn without running it, so it can run later, before any paid
request, call `runner.Record(ctx, turn, start)`. Dashi commits the `Start`
step alone: no model request, tool, steering read, or content load. A turn
whose only step is its `Start` began no work, so `Next()` gives
`NextContinue`, and a later `Continue` runs it without counting toward
`Limits.Resumes` or adding a crash reminder. A `Cancel` ends it. A retry
of `Record` with the same `CommandID` returns the recorded result.

```go
saved, err := runner.Record(ctx, dashi.NewTurn(id), start, dashi.WithOwner(owner))
// later, to cancel work that never ran:
out, err := runner.Run(ctx, saved.Turn, dashi.Cancel{Reason: "withdrawn"})
```

A command is applied once per recorder. Running the same command again, with
the same `CommandID` and content against the same recorder, returns the
recorded result without calling the model or running tools again; this is how
a restarted process safely repeats its `Start`. A new `CommandID` is a new
command, and a new recorder (such as the default in-memory one) knows nothing
of earlier runs.

## Advancing saved turns

A host that runs turns as durable background work needs no driver of its
own. Admission saves the `Start` with `Record`. Everything that arrives later
(tool results, approvals, raises, steers, a `Cancel`) goes into the host's
[`Inbox`](steering.md#durable-inboxes), keyed by turn ID. The worker that
holds the turn's lease calls `Drive`:

```go
runner := dashi.Runner{Client: client, Tools: tools, Recorder: rec, Inbox: inbox}
out, err := runner.Drive(ctx, turnID, dashi.WithOwner(owner), dashi.WithHistory(history))
// err is nil once the turn has ended; ctx ends when the lease is lost.
```

`Advance` is one pass: it loads the turn (the recorder must be a `Loader`),
applies the waiting commands in order, and runs the turn until it must wait.
When `Next()` is `NextContinue`, or `NextRecover` with `ReadyToContinue`, it
runs a `Continue` with command ID `continue:<LastSeq>`. That ID comes from
the record alone, so a crash between deciding and recording derives the same
command again and the recorder treats it as a retry; no outbox is needed.
The `continue:` prefix is reserved: `Advance` refuses an inbox command whose
ID starts with it. A command the record already holds, as after a crash
before its acknowledgment, is acknowledged without running again.
`Advance` never continues from a stop it recorded itself without new input,
so a turn that cannot progress returns instead of spinning. A turn held by a
host limit without `Ends`, such as a balance too low, is the exception the
host does not have to act on: each pass prepares the held call and asks
`BeforeModel` and `BeforeSend` about it again, without sending or recording
anything. While the hook refuses with the same host limit code, nothing is
written; once it allows the call, the turn runs. A hold the recorder made
(`LimitError`) is retried for real at most once per `Advance` call and per
inbox signal, since only the recorder's transaction can answer for it. Pass
`WithHistoryMismatch(dashi.HistoryMismatchReject)` to make the `Continue`s
that `Advance` runs refuse a history that differs from the turn's start.
`Drive` repeats `Advance`, waiting between passes for the inbox's signal
(`InboxNotifier`), a 15-second poll, or the turn's deadline, and unwatches
the signal when it returns. It does not
retry a failed model call: the turn rests at `NextFix` with the failure's
category on its `ModelFact`, and the host puts a `Continue` in the inbox when
it wants another try.

A command the turn cannot take, such as a result for an unknown call, is
acknowledged with a `WarningInbox` on the record; it never fails the turn.
Any other error, such as a failed write or `ErrStaleTurn`, stops `Advance`
and leaves the command in the inbox for the next pass.
Once a turn has ended, every waiting command except a late model result is
refused with `ErrTurnEnded`, records nothing, and is acknowledged. An inbox
that implements `InboxRefuser` hears each refusal, so the host can tell its
caller the command came too late.

## Recorders

`Runner.Recorder` may be omitted. Dashi uses a fresh in-memory recorder
for that call, and you can pass the returned `RunResult.Turn` to a later call
in the same process. Bind a durable recorder when turns must resume across
processes. `filerecorder` keeps each turn in an append-only file on local
disk and is safe for several processes sharing the directory:

```go
recorder, err := filerecorder.New(filerecorder.Config{Dir: "var/turns"})
runner := dashi.Runner{Client: client, Recorder: recorder}
```

To keep turns in your database instead, implement `dashi.Recorder` and
`dashi.Loader`; the `Recorder` doc comment states the rules, and
`dashitest.Run` checks them. In a SQL store, commit each step in one
transaction with your own rows:

- Lock the turn's row. Reject a commit from an owner other than the latest
  claim. A known command ID succeeds only with the same kind and hash.
  At an occupied sequence, succeed only if the stored step `Equal`s the new
  one; otherwise require `seq = ExpectedSeq`, insert, and advance.
- Keep the step's own JSON as the record. Do not rebuild `At` from a
  timestamp column: steps carry nanoseconds, a microsecond column drops
  them, and a genuine retry would then be rejected.
- Postgres `jsonb` rejects `\u0000` in strings; store such steps as `json`
  or text, or reject them explicitly.
- Return a `*dashi.CommitError` with `NothingWritten` only when the
  transaction certainly wrote nothing. A timeout or lost connection after
  `COMMIT` was sent must be a plain error, which Dashi treats as unknown.
- `Load` returns `dashi.ErrTurnNotFound` when the turn has no steps.
- Use `CommitRequest.ModelCall` for accounting: reserve its `WorstCaseCost`
  on a `model_call_started` step and settle its `Cost` on the finish, without
  loading the turn. To refuse a start that would break a balance, return
  `&dashi.CommitError{NothingWritten: true, Err: &dashi.LimitError{...}}`;
  Dashi records the limited stop. `CommitRequest.Delivered` names the inbox
  commands the step records, each with its `CommandKind`, to mark them in
  the same transaction; steers have `CommandSteer`.

## Unfinished work

An interrupted model call is sent again; its partial output is display-only,
while its usage still counts toward spend. The `ModelFact.Interrupted` of its
finish step names the cause: `InterruptRun` when the run stopped the call,
`InterruptWorker` when recovery finishes a call left open by a worker that
stopped. A started tool without a result is rerun only if declared `ReadOnly`
or `Idempotent` (see
[read-only and idempotent tools](tools.md#read-only-and-idempotent-tools)).
Otherwise its result is unknown, since it may have run: check the outside
system, report the result with `Resolve`, and call `Continue`. `Resolve` only
records, including for [approvals](approvals.md) and external tool results.
`RunResult.ReadyToContinue` and the `ReadyToContinue` of `turn.View()` show
when no unresolved call blocks the next `Continue`. The host's sweeper should
pick up ready turns with no active worker.

## Turn states and limits

A turn is in one of three states between runs: **ready**
(`ReadyToContinue`), **waiting on the host** ([approvals](approvals.md),
results, a [budget raise](budgets.md)), or **ended** (`Stop.Ended`). A stop
ends the turn when nothing can run the turn again: completed, canceled,
limited by its deadline, its resumes, or its context window, a host limit
with `LimitDetail.Ends` (such as a routine's spending cap), any model-call,
cost, or token limit when the agent sets `Limits.NoRaise`, or a cost cap that
can no longer be checked, including a token cap with incomplete usage. A host
limit without `Ends` waits (`NextFix`) until the host puts a `Continue` in the
inbox. Before that stop is
recorded, `Continue` can recover existing work or record the stop after a
crash, but checks before each call stop any new model or tool call. Raising a
budget does not revive such a turn. After that, the turn's conversation is
closed: only `Resolve` reporting the result of a model call that had already
started is accepted, because it corrects spend. Every other command, a tool
result included, returns `dashi.ErrTurnEnded`. Unstarted calls are recorded as finished without
running, so no approval stays pending on an ended turn.

An ended turn always builds into messages, so it stays in the history of
later turns. `dashi.TurnMessagesFull` reads it only up to the stop that ended
it: a call still started, waiting, or with an unknown result there becomes an
error result ("Result unknown; the turn ended"), and a model call with no
recorded result adds nothing. A late model result is recorded and counts
toward spend, but never changes the history later turns see.

The zero `Limits` value gives the defaults: 8 model calls, 10 resumes after
interruption, 3 provider pauses, one tool call at a time, a reserved final
reply, and no deadline. Set only the fields you want to change; a negative
`Resumes` or `ProviderPauses` allows none. Dashi records the resolved
numbers at `Start`, so a saved turn keeps its limits if the defaults change.
A turn that stopped at its model-call limit is not ended. Call
`Continue{ModelCalls: n}` with `n` above the limit now in force, and the
same turn goes on with the new limit. The record keeps the raise, and the
agent's own `Limits` stay as they were. A `Continue` with no raise, or a raise
that leaves no room, returns an error and changes nothing. A stop after the
last allowed model call for another reason, such as a budget, needs the
budget raise and the model-call raise together.
Resumes after interruption do not consume `ModelCalls`. The turn's
`Limits.Deadline` stops it as limited; a host context deadline interrupts it.
