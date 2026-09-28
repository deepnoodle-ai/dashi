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

Dashi meets none of these today. `Runner.Observer` has a 64-slot queue that
drops any event when full, committed steps included, with no signal
(observer.go:48-66; progress is evicted first). There is no read-after-`Seq`
path: `Recorder` has `Commit`, the optional `Loader` has `Load`, and a UI host
rebuilds paging, waking, and "is it over" on its own. `Runner.Stream` is one
backpressured reader whose range break ends the context: the turn records an
interruption, or a cancel under `OnContextDone: ContextCancel` (runner.go:104).
That is right for the owner of the work and wrong for a browser tab.

Our production gateway meets the same properties with a database and a lossy
bus. Committed rows carry a gap-free counter; a connection subscribes to the
bus, then pages rows after its cursor, and pages again on each wake and on a
poll. Wakes carry no data: a lost wake costs one poll. Previews travel on a
bounded bus whose overflow becomes one resync frame. Committed data never
travels on the bus. Dashi adopts that shape and ships only the parts every
host would otherwise write.

## Decisions

**1. Three readers, one per need.** `Runner.Stream` stays the owner's
backpressured, single-use view of one invocation; breaking it means "stop this
work". `Runner.Observer` is the in-process, never-blocking view of the same
invocation. `Follow` is the view for everyone else: it reads the record by
cursor, from any process, before, during, or after the run.

**2. The Observer carries steps losslessly; previews are lossy with a gap.**
The emitter keeps one ordered queue. Steps always enter it; only previews count
against the 64-slot bound. A preview that does not fit (after evicting the
oldest queued progress, as today) is dropped and sets a flag; the next event
delivered, or the end of the invocation, is preceded by one `Event{Gap: true}`.
A gap means "previews since the last event are incomplete: discard in-flight
previews and wait for the step that settles them". Evicted progress sets no
gap, since each snapshot replaces the last and the settled step follows
(tool-progress.md). Order is preserved because there is one queue.

Why not previews-only with the cursor as the only lossless route (the
alternative)? A runner with no `Recorder` uses a private in-memory one the host
cannot read, so the simplest program would lose its only step feed; and an
in-process logger or CLI would need a second reader and a poll loop to see what
the runner already holds. The cost of lossless steps: a stuck observer pins its
goroutine and the steps of that invocation after `Run` returns. Steps are
bounded by the invocation; previews stay bounded. Blocking the commit path and
an unbounded preview queue are rejected. `Stream` never drops, so it never
yields a gap.

**3. Readers follow the record through a small optional interface.** The host
implements `StepReader` over the table its recorder already writes. `Follow`
replays after the cursor, then pages again whenever a `Waiter` says a commit
may have landed, or on a one-second poll. Correctness comes from paging, never
from the wake. A plain `WHERE turn_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`
is exact because the owner commits one step at a time and the recorder enforces
`ExpectedSeq`, so no reader can see `Seq` 6 before 5.

**4. A follow ends on a final stop.** A stop is **final** when no `Continue`
can ever run the turn again, whatever the host settles or raises. The runner
records this on the stop, `Stop.Final`, computed from the same predicate that
`Continue` uses to refuse, so it is defined once. A field, not a method: a
follower reads raw steps without replaying the turn, and one final case
depends on spend, not on the stop. The final set, from `canContinueWithBudget`
and the `Cancel` path:

- `completed` and `canceled`;
- `limited` by `deadline`, `interruptions`, or `model_attempts` (agent limits
  that `Continue` cannot change: decision 25);
- `limited` by `cost` when spend includes an unknown cost, since the cap can
  never be checked again.

Not final: `waiting`, `interrupted`, `failed`, `refused`, `uncertain` (the host
settles, then continues), and the other `limited` kinds (a raise, a new
attempt, or a host decision resumes them). Implementation will check each
remaining kind against `Continue` and adjust this list if the code disagrees.
`Follow` ends after yielding a step whose stop is final; a host that wants to
close on any stop (say, while waiting for approval) checks `step.Stop` itself.

**5. Reasoning is never altered in the record; previews are tagged by kind.**
Every delta already carries `Delta.Kind` (`reasoning`, `text`, ...), and
progress is its own field. Recommendation: deliver reasoning previews by
default. The Observer and `Stream` feed trusted application code, the record
already holds the same reasoning for provider replay, so suppressing previews
alone would protect nothing; the one place to redact is the host's projection
to its clients, and the guide's handler shows the filter. Revisit if Dashi
gains a projection helper.

**6. Scope is one turn.** A cursor is a turn's `Seq`. A conversation-wide
cursor belongs to the host, which owns conversations (decision 19); no helper.

**7. Deferred.** A byte `Offset` on deltas and `ToolCallID`/`Name` on every
`tool_arguments` delta help a host resume previews across processes; they touch
every provider adapter and are not needed for these properties. Later PR.

## API

```go
// Event gains:
Gap bool // previews were dropped here; discard in-flight previews

// Stop gains (recorded as "final"; omitted when false):
// Final means Continue can never run this turn again.
Final bool `json:"final,omitempty"`

// StepReader is an optional Recorder capability for live readers.
type StepReader interface {
	// StepsAfter returns up to limit committed steps with Seq > after, in
	// ascending order; limit <= 0 means a default of 100. A turn with no
	// steps and after == 0 gives none. An after past the last committed Seq
	// gives an error matching ErrCursorAhead.
	StepsAfter(ctx context.Context, turnID string, after int64,
		limit int) ([]Step, error)
}

// Waiter is optional on a StepReader: a latency hint for Follow. Wait
// returns when a step after the cursor may exist, or when ctx ends. A lost
// wake costs one poll.
type Waiter interface {
	Wait(ctx context.Context, turnID string, after int64) error
}

// Follow yields the turn's steps after the cursor, then new ones as they
// commit. It ends when ctx ends, after a step whose stop is final, or after
// yielding a read error. Ending never affects the turn.
func Follow(ctx context.Context, r StepReader, turnID string,
	after int64) iter.Seq2[Step, error]

var ErrCursorAhead = errors.New("dashi: cursor is past the turn's last step")
```

A handler serving `GET /turns/{id}/events?after=N`:

```go
// Subscribe to your preview fan-out first, so a racing preview is buffered.
for step, err := range dashi.Follow(r.Context(), store, id, after) {
	if err != nil {
		return err // the client reconnects with its last id
	}
	sse.Send(step.Seq, project(step)) // id: Seq; previews go out without one
}
```

**Recorders.** `MemoryRecorder` implements both (a per-turn channel closed on
commit). `filerecorder` implements `StepReader` with its existing line index:
take the shared lock, scan new lines, decode entries after the cursor; no
`Waiter`, since commits may come from other processes. A Postgres recorder
selects `seq >= after` with `LIMIT limit+1`, so a missing first row reveals
`ErrCursorAhead` in one query; it may add a `Waiter` with `pg_notify` in the
commit transaction and `LISTEN` in the reader. Read the primary: a lagging
replica breaks the ordering argument in decision 3. `dashitest.Run` checks
`StepReader` whenever the store implements it: exact paging (nothing repeated
or skipped), ascending `Seq`, limits, turn isolation, `ErrCursorAhead`, and a
reader racing a writer.

## Behavior and format changes

- The Observer no longer drops steps and gains `Gap`; `Stream` is unchanged.
- Stopped steps gain `"final": true` when final. No compatibility handling.
- Guides: "Serving live updates" in `docs/guides/running-turns.md` switches
  from hand-rolled polling to `Follow`; the Observer paragraph states the new
  guarantee.

## Tests

Observer: a blocked observer and thousands of deltas lose no step; exactly one
gap precedes the first event after drops, and one arrives at close; evicting
progress alone gives none; order holds. Finality: a table over every stop kind
and limit kind, with `Continue` (and a raise) refused for each final stop and
possible for each other. Follow: replay then live; two followers with different
cursors; a follower's cancel leaves the run untouched; the follow ends on a
final stop; the poll path without a `Waiter`; a read error ends it. Conformance
for `MemoryRecorder` and `filerecorder`.

## Plan

One PR on `dx/live-delivery`, in commits: (1) observer gap and lossless steps;
(2) `Stop.Final`; (3) `StepReader`, `Waiter`, `Follow`, the memory and file
implementations, and conformance; (4) guides and an example with two followers
on one turn. Files: `observer.go`, `policy.go` (Event), `turn.go` (Stop),
`state.go` (the shared predicate), `runner.go` (only the emit path and the
stop/cancel commits), new `follow.go`, `recorder.go`, `filerecorder/`,
`dashitest/`, `docs/guides/running-turns.md`, `examples/`. If review prefers,
commits 1-2 and 3-4 split into two PRs.

## What not to build

- **An in-process broadcast bus.** A multi-instance host needs a cross-process
  one for previews anyway; a single-process host fans out with a map of
  bounded channels fed by the Observer.
- **A multi-reader `Stream`.** It would buffer per reader or block on the
  slowest, and end with the invocation; the record already serves late and
  reconnecting readers across processes.
- **A Dashi-owned event store.** The recorder already writes each step to the
  host's database, atomically with its own rows. A second store can diverge.
