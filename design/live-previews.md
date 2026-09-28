# Live previews: a tested fan-out for Follow

**Status:** Proposal · **Date:** 2026-09-28 · **Relates to:** decision 3,
[live-delivery.md](live-delivery.md) (PR C)

## Problem

A web page wants a turn's committed steps and its live previews (token
deltas, tool progress) on one stream. Steps come from the record through
`dashi.Follow`, from any process. Previews exist only in the runner's
process, through `Runner.Observer`. Merging them correctly is subtle, and
three review rounds of a copy-paste hub each found a new hole:

- a preview already settled by a step the client holds was sent after a
  reconnect;
- a full subscriber channel dropped deltas without a gap;
- a "highest `Seq` seen" watermark let a preview through when overlapping
  runs delivered steps out of order.

So the merge becomes a library type with one contract and its own tests.

## Contract

For one subscriber following from cursor `c`:

1. **Steps are never dropped.** They come from the record (`dashi.Follow`),
   exactly once, in `Seq` order, ending as `Follow` ends.
2. **A preview is shown only in its place.** It appears after the step
   committed just before it and before the next step. Anything the
   client's steps have already settled is never shown.
3. **Dropped deltas become a gap.** When a subscriber falls behind, dropped
   deltas for one message coalesce into one `Gap` for that `MessageID`, and
   later deltas for it are suppressed; its settling step follows.
4. **Progress may drop silently.** Each snapshot replaces the last.
5. **Observing never blocks.** A slow subscriber never slows the runner or
   another subscriber.

## Design: position previews by `Seq`, not by a watermark

Rule 2 needs each preview's place in the record. The hub cannot infer it
from the steps it has observed: steps from overlapping calls can arrive out
of order, the first step a hub sees may be the middle of a turn, and a step
whose commit ended with unknown persistence is never observed at all. Each
of these makes an inferred watermark wrong or stuck.

The runner knows the place exactly, so it records it. **`Event` gains
`Seq`**: on a step event it is `Step.Seq`; on a preview or gap it is the
`Seq` of the last step committed when the preview was produced. The runner
sets it in `emit` (the length of the turn's recorded steps; previews are
emitted on the run's own goroutine), and the Observer's gap marker takes the
first dropped delta's `Seq`. This is additive, and it also lets a
multi-process host relay previews over its own bus without losing their
place.

With that, the merge rule is local and deterministic. The iterator tracks
`c`, the `Seq` of the last step it yielded, and for each preview with
position `p`:

- `p < c`: drop it. A step after its place is already out, and a preview
  never outlives the next step (a delta's message settles at the next model
  step; a stale progress snapshot is replaced);
- `p == c`: yield it, unless its message has a gap;
- `p > c`: hold it until the step at `p` is yielded, then apply the rule
  again. The hold is bounded like the subscriber queue.

A delta's position is its `model_requested` step, so deltas stream while
`c` equals that `Seq` and are dropped once `model_settled` is yielded. No
settled-ID bookkeeping and no watermark are needed. Parallel tool progress
observed just before another call's step can be dropped; the next snapshot
takes its place.

## API

```go
package live // github.com/deepnoodle-ai/dashi/live

// Hub fans Runner.Observer events out to Follow subscribers in this
// process. The zero value is ready; set Runner.Observer = hub.Observe.
type Hub struct{ /* mutex, turnID -> subscriptions */ }

// Observe queues each preview for the turn's subscribers. It ignores steps
// (Follow reads those from the record) and never blocks.
func (h *Hub) Observe(e dashi.Event)

// Follow yields the turn's steps from r after the cursor, as dashi.Follow
// does, with h's previews merged in place. A nil h yields steps only.
func Follow(ctx context.Context, h *Hub, r dashi.StepReader,
	turnID string, after int64) iter.Seq2[dashi.Event, error]
```

**Subscription queue.** Each subscription holds up to 256 previews under
the hub's lock. At the bound, progress is dropped; a delta is dropped and
its message is marked holed, with one pending `Gap` that does not count
against the bound and is delivered before that subscriber's next event.
Later deltas for a holed message are dropped at once. Holes are forgotten
when the subscription ends. Observer gaps pass through as gaps.

**The handler**, which the guide shows and the tests run:

```go
for e, err := range live.Follow(r.Context(), s.hub, s.store, id, after) {
	switch {
	case errors.Is(err, dashi.ErrCursorAhead):
		send(w, "", "reset", nil)
	case err != nil:
		return
	case e.Step != nil:
		send(w, strconv.FormatInt(e.Seq, 10), "step", project(*e.Step))
		if e.Step.Stop != nil && e.Step.Stop.Terminal {
			send(w, "", "end", nil)
		}
	case e.Gap:
		send(w, "", "gap", e.MessageID)
	default:
		send(w, "", "preview", projectPreview(e))
	}
	flush()
}
```

The `204`-at-the-end check and authorization stay as they are in the guide.

## What changes

- `observer.go`: `Event.Seq`; the gap marker records the dropped delta's
  `Seq`.
- `runner.go`: `emit` sets `Seq` (one line on the emit path).
- New package `live` (`hub.go`, `follow.go`), about 200 lines.
- Guide: the handler above replaces the steps-only one; `live_follow` shows
  a delta stream next to the two followers.

## Tests

- **Placement:** previews at `p == c` are yielded between steps `c` and
  `c+1`; `p < c` is dropped; `p > c` is held and then yielded or dropped.
- **Reconnect:** the Observer delivers deltas late, after a client resumed
  from their `model_settled` `Seq`; nothing is sent. This is the review's
  regression.
- **Overlap:** two runs deliver out of order; placement still holds.
- **Overflow:** a full subscriber gets one gap per message, and no later
  delta for it before the settling step; progress drops with no gap; other
  subscribers are unaffected.
- **Nil hub:** steps only.
- **Race:** many subscribers joining and leaving during a run (`-race`).
- **Handler:** end to end with deltas, reconnect, `204`, and `reset`, in the
  guide's drift test.

## Not included

- A cross-process bus. `Event.Seq` makes events relayable; a host feeds
  relayed events into a `Hub` in the web process.
- Replaying previews to late subscribers. Previews are live-only.
