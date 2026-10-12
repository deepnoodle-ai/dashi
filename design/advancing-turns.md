# Advancing saved turns

**Status:** Proposed · **Date:** 2026-10-11 · **Builds on:** [turn](turn.md),
[durable recorders](durable-recorders.md), [steering](steering.md),
[live delivery](live-delivery.md). Assumes the in-flight PR that adds
`Start.Items`, saving a Start without running it, a Start-only turn that is
ready to continue, and error facts on `ModelFact`.

## Problem

A host that runs turns as durable background work writes the same driver
around Dashi each time. Our first such host has a 1,500-line adapter, most of
it in five places where Dashi stops one level too low:

1. **Driving.** It maps `View().Next()` to a command, saves each planned
   command in an outbox so a crash replays the same one, loops with kicks and
   backoff, and guards against spinning. Tool results arrive from other
   processes and become planned `Resolve` commands, because only the lease
   holder may write steps.
2. **Limits recorded as `Cancel`.** A model-call limit, a context-window
   stop, and a host spending cap are closed with `Cancel` and recognized later
   by reason strings.
3. **Accounting.** Its recorder reloads the whole turn on every commit to
   find the started call and price it with `ModelCallCostOf`.
4. **Watching.** A live turn reads as "interrupted" between steps
   (`Stop.Inferred`), so it tracks worker liveness itself; and `Follow`
   yields one step at a time, so it coalesces bursts on a 20 ms timer to keep
   a reader from acting on an intermediate state and running a tool twice.
5. **Durable summarization** cannot be expressed with `compaction`.

The host keeps storage and transactions, worker placement and leases,
account and business policy, tool execution, and approvals. Dashi takes the
rest.

## The application developer's view

**Before:** admission saves a request; a worker loads the turn, checks an
outbox, asks `Next()`, maps it to `Continue` or `Cancel` with a made-up ID,
saves that command, runs it, decides whether to run again, and sleeps with
backoff. Submitting a tool result plans a `Resolve` in the outbox. Cancelling
stops the worker, waits for it, then records `Cancel`.

**After:** admission saves the `Start` through Dashi. Everything that arrives
later goes into the host's inbox table. The worker holds the lease and calls
`Drive`.

```go
// Admission: save the Start without running it (name from the in-flight PR).
runner.Save(ctx, start, dashi.WithOwner(owner))

// Anywhere, any process: tool results, approvals, raises, cancels, steers.
inbox.Put(ctx, turnID, pending.Result(outcome)) // Resolve{CommandID: "result:"+callID}
inbox.Put(ctx, turnID, dashi.Cancel{CommandID: "cancel"})

// The worker that holds the turn's lease.
func (w *worker) run(ctx context.Context, turnID string) error {
	runner := dashi.Runner{Client: client, Tools: tools, Recorder: rec,
		Inbox: inbox, Hooks: budgetHooks}
	_, err := runner.Drive(ctx, turnID,
		dashi.WithOwner(w.owner), dashi.WithHistory(w.history))
	return err // nil once the turn has ended; ctx ends when the lease is lost
}
```

## Decisions

**1. Dashi owns "what next".** `Runner.Advance(ctx, turnID, opts...)
(RunResult, error)` loads the turn (the `Recorder` must also be a `Loader`),
applies the commands waiting in the inbox, and runs until the turn must wait.
`RunResult.Next()` says why it returned. Each pass:

- Ended turn: return. Waiting inbox commands are refused (below).
- Apply inbox commands in order. `Cancel` wins and ends the pass.
  `Resolve`, `AddMessages`, and a raising `Continue` apply as today.
- If `Next()` is `NextContinue`, or `NextRecover` with `ReadyToContinue`,
  run a `Continue`. Otherwise record a stop if the last step is not one
  (decision 5) and return.
- Repeat while the inbox had something new. Advance never continues from a
  stop it recorded itself in the same call unless an input arrived after it,
  so a turn that cannot progress returns instead of spinning.

`Runner.Drive` is Advance in a loop: between passes it waits for the inbox's
signal, a 15 s backstop poll, or the turn's `Limits.Deadline`, and it returns
when the turn ends or `ctx` ends. Both live in the root package, in
`run_advance.go`, as `Follow` does: Advance needs the run's internals to let
a waiting `Cancel` interrupt it.

**2. One inbox for everything that comes from outside the run.** Steering
already is the durable-inbox pattern: the host stores items, Dashi fetches
them, the record removes duplicates by ID, and `Delivered` acknowledges. We
generalize it instead of adding a second one. The interface `Steering` is
renamed `Inbox` and returns commands; a `Steer` becomes a command whose ID is
its item ID (a steer is the mid-run form of `AddMessages`). The in-memory
`Inbox` becomes `MemoryInbox`, matching `MemoryRecorder`.

```go
type Inbox interface {
	// Waiting returns the commands waiting for turnID, oldest first.
	Waiting(ctx context.Context, turnID string) ([]Command, error)
	// Delivered acknowledges commands the record now holds, by command ID.
	Delivered(ctx context.Context, turnID string, ids []string) error
}
// InboxNotifier (was SteeringNotifier) now takes the turn ID, so one host
// inbox serves many turns. A signal means "check again".
type InboxNotifier interface{ Ready(turnID string) <-chan struct{} }
```

`Runner.Inbox` holds the host's inbox; `WithSteering` becomes `WithInbox`
for a per-run source such as `session`. During a run, a signal fetches the
inbox: an urgent steer cuts the model call as today, and a `Cancel` ends the
run at once with the cancel rules. `Resolve` and raises wait for the end of
the run. A command the turn cannot take (an unknown call ID, a raise on a
turn not stopped at a limit) gets one `WarningInbox` and is acknowledged:
work by default, never fail the turn on bad input.

Tool results and approvals reach a turn only while it waits for them. Once a
turn has ended, its conversation is closed: Advance refuses every waiting
command with `ErrTurnEnded`, records nothing, and acknowledges it, so the
host can tell its caller the result came too late. No delivery after the end
is designed.

**3. Command IDs come from the record, so no outbox is needed.** Every
command Advance runs is either already durable in the inbox under the
host's ID, or a `Continue` it derives from the record alone, with ID
`continue:<LastSeq>`. A crash between deciding and recording replays the
same derivation from the same record and gets the same ID and content, which
the recorder acknowledges as a retry. Two workers with a stale lease derive
identical commands and the owner fence rejects one. `WithHistory` supplies
the Continue's `History`; the host passes the history pinned at admission.

**4. Host limits say whether they wait or end.** No more `Cancel` plus a
reason string:

- `LimitDetail.Ends` (`json:"ends,omitempty"`), allowed with `LimitHost`:
  this limit ends the turn. Replay's ending rule adds "limited by host with
  `Ends`", so `Stop.Ended` is set and `Next()` is `NextNone`. A routine cap is
  `Limit{Kind: LimitHost, Code: "routine_cap", Ends: true}`.
- A host limit without `Ends` waits: `Next()` is `NextFix` and the host puts
  a `Continue` in the inbox when the cause clears (a budget top-up).
- `Limits.NoRaise` on the agent: limits cannot be raised, so a model-call,
  cost, or token limit ends the turn. It is recorded with the agent.
- A context-window stop always records `Limit{Kind: LimitContextWindow}`
  (the provider path records only a reason today) and ends the turn, as
  Dashi's own warning already advises. A successor turn reads the typed kind.
- A recorder may refuse `model_call_started` with
  `&CommitError{NothingWritten: true, Err: &LimitError{Reason, Limit}}`
  when the money check races. The runner records that limited stop as if
  `BeforeSend` had returned it. This is the one new type: the reservation is
  only atomic in the recorder's transaction, and without it the race leaves
  a turn with no stop.

**5. Watchers act only at stops.** A turn is *stopped* when its last step is
a stop step, and *working* otherwise. `Stop.Inferred` is tightened to mean
"the last step is not a stop", so a `Resolve` or steer after a stop makes the
old stop no longer current. Advance always leaves the turn stopped when it
returns, recording a `waiting` stop after inputs that do not let it go on.
A working turn's view is for display ("running"), never for action; the
stop's `Pending` is exact because Advance drains the inbox before it records
the stop. Whether a working turn has lost its worker is a lease question: the
next lease holder's Advance recovers it.

```go
// FollowStops yields the turn's view now, then at each later stop, and ends
// after the stop that ends the turn.
func FollowStops(ctx context.Context, r StepReader, turnID string) iter.Seq2[TurnView, error]
```

It reads like `Follow`, extends one in-memory turn, and builds a view only at
stops, so a burst of steps yields one view and no timer is needed.

**6. Commits carry what accounting needs.** `CommitRequest` gains:

```go
// ModelCall is the call this step starts or finishes, as
// TurnView.ModelCallCosts will show it: WorstCaseCost on a start; Cost,
// Exact, and UsageSource on a finish. Nil on other steps.
ModelCall *ModelCallCost
// Delivered lists the inbox command IDs this step records, so a recorder
// that shares a database with its inbox marks them in the same transaction.
Delivered []string
```

A recorder reserves `ModelCall.WorstCaseCost` on a start and settles
`ModelCall.Cost` on a finish, releasing the hold, without loading the turn.
Since decision 47, `Cost` is already "reported usage only" for a call that is
not `Exact`, so the host's own fallback pricing goes. A late model-call
finish after a turn ended still arrives with `ModelCall`, so spend stays
correct; it is the only step accepted after the end, and it changes nothing a
watcher acts on. A projection row needs nothing more: a stop step carries its
`Stop`, and any other step means working.

**7. Durable summarization is deferred.** With saved Starts and Drive, a
compaction turn is an ordinary saved turn: save `Compactor.Start`, drive it,
and `BuildHistory` uses it once it completes. What remains in the host
(freezing the queued work, choosing when to activate) belongs to how a
conversation owns its turn sequence, which [sessions](sessions.md) has not
settled for durable hosts. Revisit after this lands, measured by what is left.

## Rejected

- **A Dashi worker or lease manager:** makes Dashi a queue framework; hosts
  already own placement.
- **A command inbox beside `Steering`:** two protocols for one pattern.
- **An outbox helper:** keeps two sources of truth; derived IDs need none.
- **Heartbeat steps for liveness:** presence is not a fact; leases answer it.
- **A full `TurnView` on every commit:** O(n) per commit; two fields suffice.
- **A coalescing window in `Follow`:** a guess about timing; stops are exact.
- **A new "hold" stop kind:** `NextFix` plus the host's `Code` covers waiting;
  only ending needed a new field.

## What the host deletes

| Swarm function (dashirt) | Lines now | After |
| --- | --- | --- |
| `advance`, `drive`, `runAgain` | ~120 | ~15 around `Drive` |
| `nextCommand`, `unapplied`, `plan`, `decodeCommand`, `hostLimit`, `contextWindowStop` | ~110 | 0 |
| `CancelTurn` (stop worker, Start-then-Cancel) | ~95 | ~15: put `Cancel`, kick |
| `SubmitToolResults` | ~35 | ~20: put `Resolve` |
| `WatchTurn`, `catchUp`, `extend`, `driving`, watch timing | ~200 | ~40 over `FollowStops` |
| `signals` lifecycle and commit wakes (follow.go) | ~60 | ~15 (inbox signal) |
| `recorder.Commit`, `knownCost` | ~135 | ~60, no load |
| `steering.Delivered` (reloads the turn) | ~25 | 0 |
| `event` (Cancel-reason and liveness mapping) | ~75 | ~45 |

About 650 of ~1,860 lines go, plus the `ModelCommands` outbox table and most
of `next_test`, `worker_poll_test`, and `live_status_test`. `compaction.go`
stays (decision 7).

## Open questions

1. Should Advance re-ask `BeforeSend` on every inbox signal for a waiting
   host limit, so a top-up needs no `Continue`? It costs two steps per signal.
2. Renaming `Steering` to `Inbox` touches `session` and the CLI; needs a
   naming review.
3. Tightening `Stop.Inferred` turns `NextContinue` after a `Resolve` into
   `NextRecover` for hosts that do not use Advance. Acceptable, or keep the
   old meaning and add a field?
4. Should Drive retry a failed model call after the new `ModelFact`
   retry-after, or leave that to hooks?
