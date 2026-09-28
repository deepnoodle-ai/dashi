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
itself (see the handler below).

**2. The Observer carries every acknowledged step; previews are lossy with a
gap.** The emitter keeps one ordered queue. Steps always enter it; only
previews count against the 64-slot bound. A preview that does not fit, after
evicting the oldest queued progress as today, is dropped and sets a flag. The
next event *enqueued*, or `close`, first enqueues one `Event{TurnID, Gap:
true}`; markers do not count against the bound, and consecutive drops make one
marker. Evicted progress sets no gap: each snapshot replaces the last and the
settled step follows (tool-progress.md). Consumer rules: a gap followed by a
step means discard in-flight previews, since the step settles them; a gap at
the end with no step after it (only when a commit failed and the run ended)
means load the turn. A step whose commit ended with unknown persistence is
only in the record. `Stream` never drops, so it never yields a gap.

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
- polls every 30 s while the last step is a non-terminal stop, so a turn
  waiting days for approval does not cost a query per second per tab;
- ends when ctx ends, after a terminal stop (decision 4), or after yielding a
  read error. Ending never affects the turn.

Correctness comes from paging, never from the wake. The intervals are
constants.

**4. A terminal stop means the turn is over.** `Stop.Terminal` (recorded as
`"terminal"`, omitted when false) is set in one place, `rt.commit` of a
`StepStopped` step, from `replayState.terminal()` on the state after that step.
That covers `stop`, `stopWith`, `Cancel`, the interruptions stop inside
`Continue`, and stops the hooks work adds. Replay recomputes it after each stop
and rejects a recorded value that differs (`ErrRecordInvalid`). It is not
`canContinue`, which asks whether `Continue` can run *now* and is false while
waiting on approvals, reconciliation, or a raise. The terminal cases, checked
against the code:

- `completed` and `canceled`;
- `limited` with `Limit.Kind` `deadline` or `interruptions` (keyed on the
  kind, not the reason string);
- any `limited` stop once `ModelAttempts` reaches the agent's limit;
- `limited` by `cost` when spend includes an unknown cost.

Not terminal: `waiting`, `interrupted` (including `hook_failed`, when hooks
lands), `failed`, `refused`, `uncertain`, and `limited` by `provider_pauses`,
`output_tokens`, `total_tokens`, `host`, or cost with an unpriced model or no
raise yet. Invariant, tested: `terminal()` implies `Continue` is refused with
any raise. Every input only grows (attempts, unknown cost) or is fixed at
Start (agent limits, deadline), so a later command cannot make a recorded
value untrue. Cost of a field over a method: if decision 25 later lets
`Continue` raise agent limits, old records carry a stale value; acceptable
before release.

*After a terminal stop the record accepts only late results of work that had
already started* (`StepModelSettled`, `StepModelReconciled`,
`StepToolSettled`), as a canceled turn does today. Replay applies the cancel
rules to every terminal stop: unstarted calls settle as not executed and
pending hosted approvals clear, so no UI offers "Approve" on a dead turn. The
runner rejects `Deliver`, approvals, `Cancel`, raises, and new work with
`ErrTurnOver` ("dashi: turn is over"). Late results may therefore follow a
terminal stop: `Follow` ending there is a display convenience; a reload or a
later read from the same cursor shows them. A reader that must see every step
(accounting, mirroring) keeps its cursor.

**5. Reasoning is never altered in the record; previews are tagged by kind.**
Every delta carries `Delta.Kind`, and progress is its own field. Reasoning
previews stay on by default: the Observer and `Stream` feed trusted
application code, and the record holds the same reasoning for provider
replay, so hiding previews alone protects nothing. A `Step` is the internal
record: project it, never send it verbatim. The projection removes
`ModelFact.Result.Raw`, encrypted or signed reasoning, the `StartFact` agent
(instructions, tool definitions), command digests, and owners.

**6. Scope is one turn.** A cursor is a turn's `Seq`. A conversation-wide
cursor belongs to the host (decision 19); no helper.

**7. Deferred.** A byte `Offset` on deltas and a call ID on every
`tool_arguments` delta only help resume previews, which may be lossy.

## API

```go
// observer.go (Event and Observer move here from policy.go, unchanged,
// in their own commit). Event gains:
Gap bool // previews were dropped here; discard in-flight previews

// Stop, "a turn's terminal or resumable condition", gains:
// Terminal means the turn is over: no command can run it again.
Terminal bool `json:"terminal,omitempty"`
var ErrTurnOver = errors.New("dashi: turn is over")

// StepReader is an optional Recorder capability for live readers.
type StepReader interface {
	// StepsAfter returns up to limit committed steps with Seq > after, in
	// ascending order; limit <= 0 means 100. after == 0 on a turn with no
	// steps, or after == the last Seq, gives none. An after past the last
	// Seq gives an error matching ErrCursorAhead.
	StepsAfter(ctx context.Context, turnID string, after int64,
		limit int) ([]Step, error)
}
// StepWaiter is optional on a StepReader. Wait returns at once when a step
// after the cursor exists, otherwise when one may have been committed or
// ctx ends. It is a latency hint: a missed wake costs one poll.
type StepWaiter interface {
	Wait(ctx context.Context, turnID string, after int64) error
}
func Follow(ctx context.Context, r StepReader, turnID string,
	after int64) iter.Seq2[Step, error]
var ErrCursorAhead = errors.New("dashi: cursor is past the turn's last step")
```

**The web handler** in the guide is complete, not a sketch. It authorizes the
turn and checks it exists (`Follow` on an unknown turn waits forever); reads
the cursor from `Last-Event-ID`, falling back to `?after=`; subscribes to the
host's preview fan-out (bounded channels fed by `Runner.Observer`) before
following, when the runner is in-process; merges steps and previews with
`iter.Pull2` and a goroutine, dropping any preview whose `MessageID` or
`CallID` already has a settled step sent; sends steps with `id: Seq` and
previews and gaps without one; projects every step; and, on `ErrCursorAhead`
(a wrong turn, a replica read, a filerecorder line lost to a failed fsync),
tells the client to reset and reconnect from 0 rather than retry. `Follow`
needs the store the `Runner` commits to; the default private recorder cannot
be followed.

**Recorders.** `MemoryRecorder` implements both: under its lock, check for a
newer step, else take a per-turn channel closed on commit. `filerecorder`
implements `StepReader` (shared lock, its line index, decode after the cursor)
and a `StepWaiter` that wakes on this process's commits; commits from other
processes arrive by poll. A Postgres recorder needs a unique `(turn_id, seq)`,
selects `seq >= after LIMIT limit+1` so a missing first row reveals
`ErrCursorAhead` in one query (special-casing `after == 0`), reads the primary,
and may wake with `pg_notify` in the commit transaction and one shared
`LISTEN` connection feeding in-process waiters. `dashitest.Run` checks a store
that implements `StepReader`: contiguous ascending `Seq` from `after+1`, each
step's committed JSON meaning, limits, `after == last` and `after == 0` on a
missing turn, turn isolation, claims interleaved with commits, a reader racing
a writer, and `ErrCursorAhead`; and, with `StepWaiter`, an immediate return
when a newer step exists, a wake on commit, and a return when ctx ends.

## Plan and tests

**PR A (small, lands first).** Move `Event`/`Observer` to `observer.go`
(own commit); lossless steps and the gap; `Stop.Terminal`, `terminal()`, the
replay check, and the terminal-stop tightening. `runner.go` changes are the
commit hook and the command guards. Tests: a blocked-then-released Observer
and thousands of deltas lose no step; one gap per run of drops, placed after
earlier events, and one at close; evicted progress alone gives none; a slow
Observer with a back-to-back `Continue` still gets every step; a table over
every stop and limit kind for `Terminal`, with `Continue` refused under any
raise when terminal; replay rejects a mismatched `terminal`; cancel with a
started tool then a late settle is accepted; `Deliver`, approval, `Cancel`,
and a raise after a terminal `limited` stop give `ErrTurnOver`.

**PR B.** `StepReader`, `StepWaiter`, `Follow`, memory and file
implementations, conformance, the guide ("Serving live updates", with ready,
waiting on the host, and terminal side by side) and an example with two
followers on one turn. Tests: replay then live; two cursors; a follower's
cancel leaves the run untouched; ends on a terminal stop; `Follow` from a
cancel's `Seq` yields a late settle; poll path without a waiter; waiter error
falls back; `Seq` discontinuity errors.

## What not to build

- **An in-process broadcast bus.** A multi-instance host needs a cross-process
  one for previews anyway; a single-process host fans out with a map of
  bounded channels fed by the Observer.
- **A multi-reader `Stream`,** which would buffer per reader or block on the
  slowest and end with the invocation.
- **A Dashi-owned event store,** which duplicates the recorder's rows and can
  diverge from them.
