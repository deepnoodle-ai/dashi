# Live previews: a tested fan-out for Follow

**Status:** Decided · **Date:** 2026-09-28 · **Relates to:** decision 3,
[live-delivery.md](live-delivery.md) (PR C)

## Problem

A web page wants a turn's committed steps and its live previews (token
deltas, tool progress) on one stream. Steps come from the record through
`dashi.Follow`, from any process. Previews exist only in the runner's
process, through `Runner.Observer`. Three review rounds of a copy-paste hub
each found a new hole: a settled preview sent after a reconnect, a full
channel dropping deltas without a gap, and a "highest `Seq` seen" watermark
fooled by overlapping runs. So the merge is a library package, `feed`, with
one contract and its own tests.

## Contract

For one reader following from a cursor:

1. **Steps are never dropped.** They come from the record, exactly once, in
   `Seq` order, ending as `dashi.Follow` ends.
2. **A preview appears only in its place**, after the step committed just
   before it and before the next. One the reader's steps have settled is
   never sent.
3. **Lost deltas become one gap per message**, and no later delta for it is
   sent. Deltas are lost when the reader falls behind, when the Observer
   drops them, or when the reader joins while the message streams. Its
   settling step usually follows; if the run ended early, reload the turn.
4. **Progress may drop silently.** A reader that joins while a tool runs sees
   no progress for it until the tool reports again; there is no cache.
5. **Observing never blocks**, and a slow reader never slows another.

## Placement by `Seq`

The hub cannot infer a preview's place from the steps it observes: steps
from overlapping runs arrive out of order, the first step a hub sees may be
mid-turn, and a step whose commit ended unknown is never observed. The runner
knows the place, so **`Event.Seq`** records it: on a step it equals
`Step.Seq`; on a preview or gap it is the last step committed before it, and
the preview belongs after that step. `runtime.emit` sets it. This is exact
because commit and emit share the run goroutine; a comment on `emit` and a
test over parallel tools and a recovered model call guard that.

Let `p` be a preview's place and `c` the last step the reader was sent.
`feed.Follow` drops the preview if `p < c` (a later step superseded it),
yields it if `p == c`, and holds it if `p > c` until step `p` is yielded.
Held previews come out right after their step, before the next step of the
same page. A delta sits at its `model_requested` step, since nothing commits
before its `model_settled`, and a progress snapshot sits between its call's
`tool_started` and `tool_settled`. An unplaced preview (`Seq` 0) is dropped.

## The hub

`feed.Hub`, set as `Runner.Observer = hub.Observe`:

- **Steps are hints, never delivery.** They wake `feed.Follow`'s read of the
  record, so previews are not held for a poll interval, and they close the
  turn's open message. Without the reader's own `StepWaiter`, `feed.Follow`
  still reads the record at least every second, for runs in other
  processes.
- **Mid-stream joins.** The hub remembers each turn's streaming message
  (`MessageID`, `Seq`), set by its deltas and cleared by its settling step
  or a terminal stop, and forgotten after ten idle minutes as a fallback. A
  new reader starts with a gap for it, placed at its `Seq`, so a reader
  already past it drops the gap.
- **Queues.** Each reader queues up to 256 previews. At the bound, progress
  is dropped and a delta becomes that message's gap. Gaps, from overflow or
  from the Observer, mark the message as having lost deltas: later deltas
  are dropped, and at most one gap per message is queued. Gaps are placed
  like previews. `feed.Follow`'s held previews have their own bound of 256.
- **Locking.** A hub lock covers the turn map; a per-turn lock covers its
  readers. A turn's entry lives while it has readers or a streaming message.
  `Observe` on a nil hub does nothing; the zero value is ready.
- **Relays.** A multi-process host relays Observer events into a hub in each
  web process, with `Seq` and gaps unchanged, and turns its own losses of
  deltas into gaps before calling `Observe`.

## `feed.Follow`

```go
func Follow(ctx context.Context, h *Hub, r dashi.StepReader,
	turnID string, after int64) iter.Seq2[dashi.Event, error]
```

It registers with the hub before its first read of the record. It reads the
record on one goroutine of its own. Any exit (break, ctx, a terminal stop,
an error) unsubscribes, stops that goroutine, and drops held previews. A nil
hub yields steps only. The guide's handler ranges over it, sends steps with
`id: Seq` and previews and gaps without an id, and returns after `reset`.
The page still clears a preview when it applies the step whose
`Step.Settles` names it.

## Tests

Placement and batch release; the review's reconnect regression; out-of-order
steps; a page load and a reconnect while a message streams; overflow with
one gap per message and an unaffected second reader; Observer gaps
suppressing later deltas; wake latency with no `StepWaiter`; cleanup after
break, cancel, and a terminal stop; a nil hub; readers joining and leaving
under `-race`; a failed settle commit; `Event.Seq` over parallel tools and a
recovered model call; and the guide's handler end to end under the drift
test.
