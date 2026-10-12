# Live delivery: committed steps, previews, and cursors

**Status:** Decided-pending-review · **Date:** 2026-09-28 · **Brief:** decision
3 in [decisions.md](decisions.md)

## Problem

Decision 3 names three delivery classes. A **step** is a committed fact with a
gap-free, per-turn `Seq`; it must never be dropped. A **preview** (model delta,
tool progress) is live-only and superseded by a later step; it may be dropped,
but the consumer must learn that it was. A **cursor** is the last `Seq` a reader
applied; with it, any number of readers replay and follow a turn independently,
and a disconnect never cancels work.

Today `Runner.Observer` drops any event when its 64-slot queue is full, steps
included, with no signal. There is no read-after-`Seq` path, so a UI host
rebuilds paging, waking, and "is it over" itself. `Runner.Stream` is one
backpressured reader whose range break ends the context (decision 17): right
for the owner of the work, wrong for a browser tab.

Our production gateway meets the same properties with a database and a lossy
bus: a connection pages committed rows after its cursor and pages again on a
data-free wake or a poll; previews travel on a bounded bus whose overflow
becomes one resync frame; committed data never travels on the bus. Dashi
adopts that shape and ships only the parts every host would otherwise write.

## Decisions

**1. Three readers, one per need.** `Runner.Stream` stays the owner's
backpressured, single-use view of one invocation; breaking it means "stop this
work". `Runner.Observer` is the in-process, never-blocking view of an
invocation. `Follow` serves everyone else: it reads the record by cursor, from
any process, and yields committed steps only. Previews exist only in the
runner's process, while a follower may be anywhere, so a host merges them
itself, through a fan-out helper ([live-previews.md](live-previews.md)).

**2. The Observer carries every acknowledged step; previews are lossy with a
gap.** The emitter keeps one ordered queue. Steps always enter it; only
previews count against the 64-slot bound. A preview that does not fit, after
evicting the oldest queued progress as today, is dropped and sets a flag. The
next event *enqueued*, or `close`, first enqueues one `Event{TurnID,
ModelCallID, MessageID, Gap: true}` naming the message that lost deltas;
markers do not count against the bound, and consecutive drops make one marker.
Evicted progress sets no gap: each snapshot replaces the last and the finish
step follows (tool-progress.md). Consumer rules: after a gap, discard that
message's preview and ignore its later deltas until its `model_call_finished`
step arrives; a gap with no such step after it (only when a commit failed and
the run ended) means load the turn. A step whose commit has an unknown result
is only in the record. `Stream` never drops, so it never yields a gap.

*Order.* Events from one invocation arrive in order. Each `Run` or `Stream`
has its own delivery goroutine that keeps draining after the call returns, so
`Run` returning does not mean the Observer has everything, and invocations
that overlap (a `Continue` issued before the previous backlog drains) may
deliver concurrently. Order steps by `Seq`.

*Memory.* The Observer must return promptly. A blocked Observer holds one
goroutine plus a copy of the steps of each invocation it has not received,
tool outputs and raw model results included, until it returns; across many
invocations that is unbounded. Dropping steps breaks decision 3 and blocking
the commit path breaks "never blocks work", so there is no cap.

*Why not previews-only, with the cursor as the only lossless route?* A runner
with no `Recorder` uses a private in-memory one the host cannot read, so the
simplest program would lose its only step feed, and an in-process logger would
need a second reader and a poll loop for data the runner already holds.

**3. Readers follow the record through a small optional interface.** The host
implements `StepReader` over the table its recorder writes. Paging is exact
because the owner commits one step at a time, the recorder enforces
`ExpectedSeq`, and `(turn, seq)` is unique: no reader sees `Seq` 6 before 5. A
cursor is owner-independent: a `Claim` or a new worker continues the same
`Seq`, and claims never appear as steps. `Follow`:

- treats a negative cursor as 0;
- checks each step has the requested `TurnID` and `Seq == cursor+1`, and
  otherwise yields an error matching `ErrRecordInvalid` (a lagging replica, a
  table without the unique key);
- drains pages until a short page, then waits;
- waits on the reader's `StepWaiter` when present, with a 15 s backstop poll,
  or polls every second without one; a `Wait` error is a hint failure and
  falls back to polling;
- polls every 30 s while the last step is a stop that did not end the turn, so
  a turn waiting days for approval does not cost a query per second per tab;
- rereads the step at a positive cursor first, so a reader resuming at an
  ending stop ends at once and one resuming at a resting stop polls slowly;
- pauses briefly after a wake that found nothing, so a waiter that wakes
  falsely cannot make it spin;
- ends when ctx ends, after an ending stop (decision 4), or after yielding a
  read error. Ending never affects the turn.

Correctness comes from paging, never from the wake. The intervals are
constants.

**4. An ending stop means the turn is over.** `Stop.Ended` (recorded as
`"ended"`, omitted when false) is set in one place, `rt.commit` of a
`StepTurnStopped` step, from `replayState.terminal()` on the state after that
step. That covers `stop`, `stopWith`, `Cancel`, the resumes stop inside
`Continue`, and stops the hooks work adds. Replay recomputes it after each
stop and rejects a recorded value that differs (`ErrRecordInvalid`). It is not
`canContinue`, which asks whether `Continue` can run *now* and is false while
waiting on approvals, a host-resolved model call, or a raise. The ending
cases, checked against the code:

- `completed` and `canceled`;
- `limited` with `Limit.Kind` `deadline` or `resumes` (keyed on the
  kind, not the reason string);
- any `limited` stop once `ModelCalls` reaches the agent's limit;
- `limited` by `cost` when spend includes an unknown cost.

Not ending: `waiting`, `interrupted`, `failed` (including reason `hook_failed`
from hooks, which does not count toward `Limits.Resumes`), `refused`,
`result_unknown`, and `limited` by `provider_pauses`, `output_tokens`,
`total_tokens`, `host`, or cost with an unpriced model or no raise yet.
Invariant, tested: `terminal()` implies `Continue` is refused with any raise.
Every input only grows (model calls, unknown cost) or is fixed at Start (agent
limits, deadline), so a recorded `true` stays true. A recorded `false` stays
accurate because a turn becomes non-continuable only through a recorded stop:
only model results raise those counts, and replay rejects one that would make
an open limited stop end the turn; `Resolve` checks this before saving. Cost
of a field over a method: if decision 25 later lets `Continue` raise agent
limits, old records carry a stale value; acceptable before release.

*After an ending stop the record accepts only late results of model calls
that had already started* (`StepModelCallFinished`,
`StepModelCallFinishedByHost`), which correct spend. The conversation is
closed, so a tool result is refused. Replay applies the cancel
rules to every ending stop: unstarted calls finish as not executed and pending
provider tool approvals clear, so no UI offers "Approve" on a dead turn; a
late model result's tool calls never enter the turn (the two replay checks and
the top of `run()` that test "canceled" today test "ended"). After the
idempotent-retry check, so a retried `Cancel` still gets its recorded result,
the runner rejects `AddMessages`, approvals, `Cancel`, raises, new work, and a
`Resolve` for a call the ending rules finished with `ErrTurnEnded` ("dashi:
turn has ended"). `canContinue` also keys on `Limit.Kind`, so the two checks
cannot disagree. Late results may therefore follow an ending stop: `Follow`
ending there is a display convenience; a reload or `StepsAfter` from the
stop's `Seq` shows them. A reader that must see every step (accounting,
mirroring) keeps its cursor.

**5. Reasoning is never altered in the record; previews are tagged by kind.**
Every delta carries `Delta.Kind`, and progress is its own field. Reasoning
previews stay on by default: the Observer and `Stream` feed trusted
application code, and the record holds the same reasoning for provider
replay, so hiding previews alone protects nothing. A `Step` is the internal
record: filter it, never send it verbatim. The filter removes
`ModelFact.Result.Raw`, encrypted or signed reasoning, the `StartFact` agent
(instructions, tool definitions), command hashes, and owners.

**6. Scope is one turn.** A cursor is a turn's `Seq`. A conversation-wide
cursor belongs to the host (decision 19); no helper.

**7. Deferred.** A byte `Offset` on deltas and a call ID on every
`tool_arguments` delta only help resume previews, which may be lossy.

## API

```go
// observer.go (Event and Observer move here from policy.go, unchanged,
// in their own commit). Event gains:
Gap bool // MessageID lost deltas here; ignore it until its step

// Stop, "a turn's ending or resumable condition", gains:
// Ended means the turn is over: no command can run it again.
Ended bool `json:"ended,omitempty"`
var ErrTurnEnded = errors.New("dashi: turn has ended")

// StepReader is an optional Recorder capability for live readers.
type StepReader interface {
	// StepsAfter returns up to limit (positive) committed steps with Seq >
	// after, in ascending order. after == 0 on a turn with no
	// steps, or after == the last Seq, gives none. An after past the last
	// Seq gives an error matching ErrCursorAhead.
	StepsAfter(ctx context.Context, turnID string, after int64,
		limit int) ([]Step, error)
}
// StepWaiter is optional on a StepReader. Wait returns at once when a step
// after the cursor exists, otherwise when one may have been committed or
// ctx ends. It is a latency hint: a missed wake costs up to the 15 s
// backstop, so a waiter that sees other processes notices them within ~1 s.
type StepWaiter interface {
	Wait(ctx context.Context, turnID string, after int64) error
}
func Follow(ctx context.Context, r StepReader, turnID string,
	after int64) iter.Seq2[Step, error]
// Finishes names the previews a step supersedes: the MessageID of
// model_call_finished or model_call_finished_by_host, or the call ID of tool_finished.
func (s Step) Finishes() string
var ErrCursorAhead = errors.New("dashi: cursor is past the turn's last step")
```

**The web handler** in the guide serves committed steps and runs in the
tests. It authorizes the turn first (`Follow` on an unknown turn waits
forever); reads the cursor from `Last-Event-ID`, falling back to `?after=`;
filters every step and sends it with `id: Seq`; sends `end` at an ending
stop and answers `204` to a reconnect at one, so `EventSource` stops; and, on
`ErrCursorAhead` (a wrong turn, a replica read, a filerecorder line lost to a
failed fsync), tells the client to reset rather than retry. `Follow` needs
the store the `Runner` commits to; the default private recorder cannot be
followed. Merging live previews needs a fan-out with subtle rules, so it
becomes a library helper ([live-previews.md](live-previews.md)).

**Recorders.** `MemoryRecorder` implements both (check under its lock, else
a per-turn channel closed on commit). `filerecorder` implements `StepReader`
over its line index, and a `StepWaiter` that wakes on this process's
commits and notices other processes' by the log's size within 250 ms. A Postgres
recorder needs a unique `(turn_id, seq)`, reads the primary, selects `seq >=
after LIMIT limit+1` to detect `ErrCursorAhead` in one query (special-casing
`after == 0`), and may wake via `pg_notify` and one shared `LISTEN`.
`dashitest.Run` checks any store that implements `StepReader` (and
`StepWaiter`) against these rules.

## Plan

**PR A** (#48, merged): `Event`/`Observer` moved to `observer.go`; lossless
steps and the gap; `Stop.Ended` and the ending-stop tightening.

**PR B**: `StepReader`, `StepWaiter`, `Follow`, `Step.Finishes`, the memory
and file implementations, conformance, the guide's web handler (run by
`run_follow_public_test.go`), and `examples/live_follow`.

## What not to build

- **An in-process broadcast bus:** multi-instance hosts need their own anyway.
- **A multi-reader `Stream`:** it would buffer or block per reader.
- **A Dashi-owned event store:** it would duplicate the recorder's rows.
