# Live delivery: committed steps, previews, and cursors

**Status:** Proposal · **Date:** 2026-09-27 · **Brief:** decision 3 in
[decisions.md](decisions.md)

## Problem

A UI host needs several readers on one turn, each replaying from its own cursor,
reconnecting without loss or duplication, and never cancelling work by
disconnecting. Dashi offers two things. `Runner.Stream` is one backpressured
reader for the caller that runs the turn; it is single-use, and breaking the
range cancels the turn (engine.go:30-34, 46-53). `Runner.Observer` is a
64-slot channel that drops any event when full, committed steps included, with
no signal (observer.go:11, 19-26). That contradicts agent-loop.md:99-100 and
leaves every host to rebuild the reader, gap handling, and preview bookkeeping
(gap-analysis themes 1 and 6; gaps/D top gaps 1-3).

## How the gateway does it

nvoken-cloud meets the same properties with a database and a lossy bus.

- **Storage and cursor.** Messages and lifecycle changes are Postgres rows; each
  commit bumps two gap-free counters in the same transaction
  (queries/execution_commit.sql:46-60). A cursor is an opaque, scope-bound
  encoding of that position (services/target_streams.go:221-233).
- **Replay then live.** A connection subscribes to the bus first, then reads
  pages after its cursor until caught up, and drains again on every wake and on
  a 2 s poll (httpapi/target_streams.go:232-254, 274, 349-392; spec §6.1-6.3).
  Readers are independent and none affects the turn (R45).
- **Wakes.** After a commit the store publishes a payload-free wake
  (postgres/stream_commits.go:10-33). Wakes are a latency hint and the poll is
  the correctness path (R38); a dropped wake is not a gap
  (liveevents/inprocess.go:46-54).
- **Previews.** The executor numbers each fragment's byte offset and publishes
  it (services/generation.go:881-931) to an in-process or Redis bus behind one
  port (ports/streaming.go:22-50). Overflow becomes one `stream.resync` and a
  drain (target_streams.go:399-404; R39). The bus exists because previews are
  produced in one process and served from another; committed data never travels
  on it.

## Proposal

**(a) Owner and readers.** `Runner.Stream` stays what it is: the owner's
backpressured view of one invocation, where breaking means "stop this work".
Everyone else reads the record. A UI host runs the turn detached (a worker calls
`Run` or `Stream` under its own context) and each HTTP connection follows the
record by `Seq` under the request context, so a disconnect ends only the follow.
Guides should steer hosts away from ranging `Stream` in a request handler.

**(b) Dashi owns the contract; the host owns the store.** The read path is an
optional interface the host implements over the table its `Recorder` already
writes. Dashi ships `MemoryRecorder`'s implementation, a small `Follow` loop,
and a conformance test (`dashitest.StepReader`): exact paging ("repeats
nothing, skips nothing"), ascending `Seq`, the limit honored, and a cursor past
the head rejected. Dashi's case is simpler than nvoken's. `Seq` is assigned by
the single owner (engine.go:306) and the recorder enforces `ExpectedSeq`
(recorder.go:85), so a turn's commits are serial and a reader can never see
`Seq` 6 before 5. A plain `WHERE turn_id = $1 AND seq > $2 ORDER BY seq LIMIT
$3` is exact.

**(c) Wake by poll, optionally hinted.** `Follow` drains, then waits for either
a poll tick (default 1 s) or an optional `Waiter` hint, then drains again.
Correctness comes from the drain, as in nvoken R38. `MemoryRecorder` wakes
waiters by closing a per-turn channel on commit. A Postgres host can call
`pg_notify` inside the commit transaction and `LISTEN` in the reader, or reuse
whatever bus carries its previews. No broker is needed: the only state a reader
needs is its cursor, and the database already serializes and stores every step.

**(d) Committed steps are lossless to the Observer.** Stop copying steps into
the lossy channel. The observer goroutine instead walks the runtime's own
`turn.Steps` from its last delivered `Seq`; the runner already holds that slice
for the invocation (engine.go:334). Previews stay in the bounded channel, each
tagged with the `Seq` current at emission, so steps through that `Seq` are
delivered first and the original order survives. This never blocks and adds no
queue. Tradeoff: a stuck observer pins a goroutine and the turn's steps after
`Run` returns. Rejected: blocking the commit path (design-pitfalls item 10);
an unbounded channel (a second copy of the same data); dropping steps from the
observer (breaks agent-loop.md:99-100 and pushes in-process loggers to the
store).

**(e) Previews: a gap signal and enough identity to resync.** When the preview
channel overflows, the goroutine delivers one `Event{Gap: true}` before the next
preview. The consumer discards previews for the turn and relies on the record,
where `model_settled` carries the whole result. Two additions are needed.
`Offset` is the UTF-8 byte count already emitted for the same `(MessageID,
Index)`; the runner sees every delta before the lossy buffer, so it can number
them, as nvoken does. `ToolCallID` and `Name` on every `tool_arguments` delta
come from a normalized block start in each provider adapter; only Anthropic
emits one today (providers/anthropic/messages/stream.go:151). An execution epoch
is not needed. `MessageID` derives from the attempt counter (engine.go:564-565),
a recovering owner settles the old attempt before starting a new one, and a
client drops previews for any `MessageID` whose `model_settled` it has seen.

**(f) A reconnecting client, end to end.**

1. The client posts a message. The host commits `turn_started` in a worker and
   returns the turn ID.
2. The client opens `GET /turns/{id}/events?after=N`, with `N` the last `Seq` it
   applied (0 at first).
3. The handler subscribes to its preview fan-out, then calls `Follow` from `N`.
   Subscribing first means a racing wake is either buffered or already readable
   (nvoken spec §6.1 step 2).
4. Steps go out as durable frames with `id: Seq`; previews go out without an id;
   a `Gap` goes out as a resync frame.
5. The connection drops. The request context ends, `Follow` returns, and the
   worker keeps going.
6. The client reconnects with its last `Seq`. Replay delivers everything it
   missed, including the settled messages that supersede lost previews, then
   follows live. Once stops say whether they are final, the handler closes on
   one.

```go
// StepReader is optional; a host implements it over its recorder's table.
type StepReader interface {
	// StepsAfter returns up to limit steps with Seq > after, ascending; an
	// after past the last committed Seq returns ErrCursorAhead.
	StepsAfter(ctx context.Context, turnID string, after int64,
		limit int) ([]Step, error)
}
// Waiter is an optional latency hint for Follow; a lost wake costs one poll.
type Waiter interface {
	Wait(ctx context.Context, turnID string, after int64) error
}
// Follow replays steps after the cursor, then follows until ctx ends.
func Follow(ctx context.Context, r StepReader, turnID string, after int64,
	poll time.Duration) iter.Seq2[Step, error]

type Event struct {
	// ...existing fields...
	Gap bool // previews were dropped here; discard them, trust the record
}
// llm.Delta gains Offset int64 (bytes already emitted for this MessageID and
// Index), and ToolCallID and Name on tool_arguments.
```

## What not to build

- **An in-process broadcast bus.** Any multi-instance host already needs a
  cross-process one for previews (Redis, `NOTIFY`, its queue), and a Dashi bus
  would be the wrong one there. A single-process host fans out with a map of
  bounded channels (nvoken's is 131 lines, liveevents/inprocess.go). Reconsider
  if two hosts write the same one.
- **A multi-reader `Runner.Stream`.** It would buffer per reader or block on the
  slowest, serve only readers in the owning process, and end when the invocation
  ends, so a late or reconnecting reader gets nothing. The record already serves
  any number of readers across processes and restarts.
- **A Dashi-owned event store.** The `Recorder` already writes each step to the
  host's database, atomically with budget and outbox writes (recorder.go:17-18).
  A second store duplicates that data and can diverge from it. Dashi's job is
  the read contract and its conformance test.

## Open questions

- **Reasoning.** The record keeps reasoning blocks for provider replay (gaps/D,
  redaction row). Should Dashi's per-step projection helper redact by default,
  and should reasoning previews be suppressed when reasoning is not meant for
  display, as nvoken does (generation.go:888-891)?
- **Tool progress.** Decided in [tool-progress.md](tool-progress.md):
  `tool.Report` snapshots are previews delivered as `Event.Progress`. A
  snapshot replaces the last, so it needs neither `(CallID, Offset)` keys nor
  the gap signal; those remain for appended tool output, added later.
- **Ending a follow.** `Follow` ends only with its context until stops carry a
  final flag (`Stop.Final`, gap-analysis theme 1). Should it end on one?
- **Conversation scope.** `Seq` is per turn; a cursor across a conversation's
  turns is the host's, like nvoken's execution stream. Is a helper warranted?
- **Observer role.** With `Follow` available, does any in-process consumer still
  need committed steps on the observer, or should it become previews-only later?
