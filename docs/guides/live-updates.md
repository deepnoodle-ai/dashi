# Live updates

This guide covers serving a turn's live updates to browsers and observing a
run in process. [Running turns](running-turns.md) covers `Runner.Stream`;
[tools](tools.md#tool-progress) covers reporting tool progress.

## Serving live updates

`Runner.Stream` suits a caller that owns the turn's lifetime, such as a CLI or
a worker. For a durable turn served to browsers, do not connect an HTTP or SSE
request to `Stream`: leaving the range interrupts the turn, and a closed tab
must not control agent work.

Instead, run the turn in your own background execution (a queue worker or job)
under a context you control, using `Runner.Run` or `Stream`. Each browser
connection then follows the turn's record with `dashi.Follow`, which reads
committed steps after a cursor from any process. Each step carries a gap-free
`Seq` per turn, so a client reconnects with the last `Seq` it applied and
receives everything after it, exactly once. A disconnect ends only that
follow; any number of clients can follow one turn.

`Follow` reads through the recorder's optional `dashi.StepReader`.
`MemoryRecorder` and `filerecorder` implement it; a host recorder implements
`StepsAfter` as one range query on the table it commits to. To tell a
cursor at the last step (an empty page) from one past it
(`dashi.ErrCursorAhead`), a positive cursor also selects its own row:

```sql
-- $2 is after; $3 is limit, plus 1 when after > 0 for the cursor's row
SELECT seq, step FROM turn_steps
WHERE turn_id = $1 AND seq >= $2 ORDER BY seq LIMIT $3
```

When `after > 0`, the first row must be `seq = after`; if it is missing,
return `dashi.ErrCursorAhead`, and otherwise drop it, leaving at most
`limit` steps. At `after = 0` there is no such row, so the query asks for
`limit` rows (`seq >= 0` is every step). Read from the primary and keep
`(turn_id, seq)` unique; `dashitest.Run` checks these rules.
`Follow` pages until it catches up, then
polls every second, or every 30 seconds while the turn rests at a stop that
has not ended it. For faster updates, also implement `dashi.StepWaiter`: for
example, `pg_notify` in the commit transaction and one shared `LISTEN`
connection that wakes waiting readers. With a waiter, `Follow` still reads
every 15 seconds, so a missed wake delays a step by up to that long.

`Follow` ends when the request ends, or after the stop that ends the turn
(see [turn states](recovering-turns.md#turn-states-and-limits)); from a
cursor that is already at that stop, it ends at once. A late result of work
started before the stop can still be recorded after it; a reload, or
`StepsAfter` from the stop's `Seq`, shows it.

Live previews (token streaming and tool progress) never enter the record;
they come from `Runner.Observer` in the runner's process. Package `feed`
merges the two: set `Runner.Observer` to a `feed.Hub`'s `Observe`, and serve
each connection with `feed.Follow`, which yields the record's steps with the
hub's previews in their places. The handler:

```go
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Authorize first: Follow waits forever on a turn that does not exist.
	if !s.canView(r, id) {
		http.NotFound(w, r)
		return
	}
	// EventSource resends the last id it received as Last-Event-ID.
	after, _ := strconv.ParseInt(cmp.Or(
		r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after"),
	), 10, 64)
	// A client reconnecting after the end gets 204, which tells
	// EventSource to stop reconnecting.
	if after > 0 {
		last, err := s.store.StepsAfter(r.Context(), id, after-1, 1)
		if err == nil && len(last) == 1 && last[0].Stop != nil &&
			last[0].Stop.Ended {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flush := w.(http.Flusher).Flush
	flush() // the browser sees the stream open before the first event

	// Steps come from the record; previews from the hub, each in its place.
	for e, err := range feed.Follow(r.Context(), s.hub, s.store, id, after) {
		switch {
		case errors.Is(err, dashi.ErrCursorAhead):
			send(w, "", "reset", nil) // the client drops its state, follows from 0
			flush()
			return
		case err != nil:
			return // the client reconnects from its last id
		case e.Step != nil:
			send(w, strconv.FormatInt(e.Seq, 10), "step", project(*e.Step))
			if e.Step.Stop != nil && e.Step.Stop.Ended {
				send(w, "", "end", nil) // the page calls EventSource.close
			}
		case e.Gap:
			send(w, "", "gap", e.MessageID) // the page discards that preview
		default:
			send(w, "", "preview", projectPreview(e))
		}
		flush()
	}
}
```

Steps go out with an SSE `id`, so the browser resumes from them; previews
and gaps have none, since they cannot be replayed. Each preview comes after
the step it follows (`Event.Seq`) and before the next, and one whose message
or call the client's steps have already finished is never sent, even on a
reconnect. The page still clears a message's preview when it applies the step
whose `Step.Finishes()` names it. A `gap` means the message lost deltas: the
client fell behind, or it joined while the message was streaming. The page
discards that preview and shows the message when its finish step arrives. Tool
progress may drop without a gap; a page that joins while a tool runs shows its
progress from the tool's next report.

At the stop that ends the turn the handler sends `end`, so the page calls
`EventSource.close()`; a browser that reconnects anyway gets `204 No
Content`, which stops `EventSource` from reconnecting. This handler runs in
Dashi's tests (`run_follow_public_test.go`). The hub serves readers in the
runner's process; a multi-process host relays the Observer's events, with
`Seq` and gaps unchanged, into a hub in each web process.

**Never send a `Step` to a browser as it is.** A step is the internal record:
it holds the provider's raw response (`ModelFact.Result.Raw`), encrypted or
signed reasoning, the agent's instructions and tool definitions (the
`turn_started` step), command hashes, and lease owners. `project` builds the
client's view from the fields it needs. Reasoning reaches the client two
ways, in steps and as reasoning deltas, so if your product does not show it,
hide it in `project` and have `projectPreview` skip deltas whose
`Delta.Kind` is `llm.DeltaReasoning`. `examples/live_follow` follows a
turn in process and prints the model's text as it streams.

## Observing a run in process

`Runner.Observer` sees a run without ever slowing it: it is called on its own
goroutine and must return promptly.

- **Steps are never dropped.** Every step the recorder acknowledges reaches
  the Observer, in order within one `Run` or `Stream` call. A blocked Observer
  holds its goroutine and a copy of every step it has not received until it
  returns.
- **Previews may be dropped.** Deltas and tool progress wait in a queue of 64
  previews. Progress is dropped first; dropped deltas are reported by one
  event with `Gap` set, naming the message in `MessageID`. That message's
  preview now has a hole: discard it and ignore its later deltas until its
  `model_call_finished` step arrives with the whole message. A gap with no
  such step after it means the run ended early; load the turn.
- **Delivery runs behind the call.** `Run` can return before the Observer has
  every event, and a `Continue` issued before the previous call's events
  drain may deliver concurrently with them. Order steps by `Seq`.

```go
// Overlapping calls may deliver concurrently, so shared state takes a lock.
var mu sync.Mutex
holed := map[string]bool{} // messages that lost deltas
runner.Observer = func(e dashi.Event) {
	mu.Lock()
	defer mu.Unlock()
	switch {
	case e.Gap:
		holed[e.MessageID] = true
		ui.DiscardPreview(e.MessageID) // or show a placeholder
	case e.Step != nil:
		if id := e.Step.Finishes(); id != "" {
			delete(holed, id)
		}
		ui.ApplyStep(*e.Step) // supersedes previews for its message or call
	case e.Delta != nil && !holed[e.MessageID]:
		ui.Preview(e.MessageID, *e.Delta) // filter e.Delta.Kind to hide reasoning
	case e.Progress != nil:
		ui.Progress(*e.Progress)
	}
}
```
