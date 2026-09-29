# Sessions

A session is a conversation of many turns. `dashi.Session` drives one: it
numbers the turns, sends each new turn the history of the ones before it,
carries the agent forward, and decides what a new message does while the last
turn is still open. It is built on `Runner.Run` and adds no storage: the turn
records are the only truth. [Running turns](running-turns.md) covers one turn.

```go
s := &dashi.Session{Runner: dashi.Runner{Client: client}}
out, err := s.Send(ctx, "What's the weather in Tokyo?")
out, err = s.Send(ctx, "And in Fahrenheit?")
```

`Send(ctx, text)` is `Run(ctx, dashi.Say(text))`. `Run` takes any command: a
`Start` opens the next turn, and `Continue`, `Settle`, `Deliver`, and `Cancel`
go to the last turn. `Answer` settles each answer and continues, in one call:

```go
if out.Next() == dashi.NextAnswer {
	out, err = s.Answer(ctx, []dashi.Settle{out.Pending[0].Approve()})
}
```

[`examples/chat`](../../examples/chat) runs this flow without an API key, on
the scripted `dashitest.Client`, with a tool call and a denied call.

## IDs and storage

- **Turn IDs.** Turn *n* of session `S` is `dashi.TurnID(S, n)`, `S-n`. An
  empty `Session.ID` is a new session with a generated ID, readable from
  `s.ID` after the first `Start`.
- **Creating.** A handle with an ID reopens that session, and an ID with no
  turns fails with `ErrSessionNotFound`, so a mistyped ID never starts an
  empty session. Set `New` to create it; `New` on a session with turns fails
  with `ErrSessionExists`, except for a retry of the first `Start`.
- **Recorders.** A nil `Runner.Recorder` becomes one in-memory recorder that
  the handle keeps, and the first outcome warns `memory_only`. A durable
  recorder lets any handle, in any process, reopen the session. The session
  finds its last turn by probing turn IDs through `StepReader` or `Loader`,
  about 2·log2(n) reads; a recorder can answer in one with `SessionIndex`.
  `filerecorder` implements it by which turn logs exist, so finding the last
  turn reads none of them. `dashitest.Run` checks both.
- **Open.** `s.Open(ctx)` reports the last turn's view and `Next`; it never
  runs anything.

Keep titles, owners, and other metadata in your application, keyed by the
session ID. A handle holds only settings and a cache, so a server can build
one per request.

## A new message while a turn is open

A new `Start` reads the last turn's `Next`, refined with liveness, and does
one of three things:

- **Starts** when there is no last turn, or it has ended.
- **Refuses** with `*TurnBusyError` (matching `ErrTurnBusy`) when the turn is
  busy: running, possibly running, waiting on the host (`NextAnswer`), or
  holding an unsettled uncertain call. The message names the way out.
- **Closes** any other stopped turn (interrupted, at a limit, refused,
  failed) with `Cancel` and `CloseReason`, then starts. The outcome warns
  `turn_closed`. Closing an interrupted turn keeps a zero-value session
  usable after an Esc or the default model-call limit.

A turn with no recorded stop is running or crashed, and the session cannot
tell which by itself. It is running when this handle runs it, or when the
recorder's `TurnLiveness` reports it live. It counts as stopped, and closes,
only when `TurnLiveness` reports stopped or the handle sets `Exclusive`, the
host's promise that no other driver exists and earlier ones have exited. An
expired lease is unknown, not stopped: the old worker may still be inside a
provider or tool call. An unknown turn refuses.

An **uncertain call** is a started call that is neither `ReadOnly` nor
`Idempotent` and has no result, or a model call that offered an unsafe
provider-hosted tool. It keeps the turn busy whatever its liveness, even under
`Exclusive`, because a stopped worker is not a stopped effect.
`TurnBusyError.CallID` names it. `Settle` it with the real result after
checking the effect, or `Cancel{}` the turn to accept the risk.

To take over a turn whose worker was lost, run `Continue{}` under
`WithOwner`; never close it and start another. Resume reruns no unsafe call.

## Retries

`Start.CommandID` is the idempotency key. A `Start` without one gets a new
key, so each call is a new message. With a key, the session looks back from
the last turn through the newest turn a caller's `Start` began:

- the same key and input returns that turn's current outcome, as the first
  call returned it (ended, or stopped waiting on an answer or at a limit),
  and `*TurnBusyError{Duplicate: true}` while the turn may still be running;
- the same key with other input fails with `ErrCommandConflict`;
- a turn in that range that fails to load fails the `Start` with its error.

Keys beginning with `queue:` or `compact:` are the session's own, and `Run`
refuses them.

Two workers that race to start the same turn collide on its ID; the loser
reloads and applies the rules once more, so it gets the duplicate error
while the winner runs, never a success for an unfinished turn.

```go
start := dashi.Say(message)
start.CommandID = r.Header.Get("Idempotency-Key")
out, err := s.Run(ctx, start, dashi.WithOwner(workerID))
var busy *dashi.TurnBusyError
if errors.As(err, &busy) {
	writeJSON(w, http.StatusConflict, busy)
}
```

## The agent carries forward

Each new turn's agent is merged field by field: `Start.Agent`, then
`Session.Agent`, then the agent the last user turn recorded, then the
engine's defaults. So `Session{Agent: dashi.Agent{Model: m}}` changes the
model and keeps the instructions and tools. A zero field falls through; turn a
setting off with its negative or `No…` form. `Tools` and `NoTools` move
together. `Limits.Deadline` never carries, a derived revision is derived
again, and provider, API, and model carry only while they match the client.
Scheduled and operator turns do not pass their agent on.

## Warnings

`Outcome.SessionWarnings` holds what no turn records: `turn_closed`,
`memory_only`, `queue_blocked` (below), and `turn_unreadable`, for a turn left
out of the history because it could not be loaded or projected. An unreadable
last turn is an error, since its worker may still run it, unless the handle
sets `Exclusive`: then no worker does, so a new turn leaves it out with
`turn_unreadable` and starts after it. A keyed `Start` and `RunQueue` still
fail, since that turn may hold their key.

## Steering and queued input

A `Start` never guesses whether a new message should steer, queue, or replace
the running turn; the caller says which.

- **`s.Steer(ctx, text)`** reaches the turn this handle runs at its next
  model call. `Queued.TurnID` names the turn it reached. With no turn
  running, the text goes to the queue instead and `TurnID` is empty.
  `SteerItem` takes a whole `dashi.Steer`: an urgent one, which may cut the
  model call in flight short, or an item of another origin, such as a
  [task notice](delegation.md).
- **`s.Enqueue(ctx, text)`** holds input for the next turn; `EnqueueItem`
  takes any user-role `ContextItem`. Empty IDs are generated; a repeated ID
  is an error.

The handle owns an `*Inbox` and passes it to every turn it runs. A host with
its own durable steering source sets `Session.Steering`; `Steer` then works
only if that source is an `*Inbox`, and otherwise the host sends through its
own system.

**Dispatch on idle.** When a `Start` or `Continue` ends its turn with
`NextNone` and items are queued, the same `Run` or `Stream` call starts a
**queue turn** from every item, and repeats until the queue is empty or a turn
stops any other way. `Run` returns the last turn's outcome, with the earlier
turns' session warnings. When a turn stops without ending, items wait, and
`SessionState.Queued` counts them. `s.RunQueue(ctx)` runs them on an idle
session with no new input, and reports false when it ran nothing.

A queue turn has the command ID `queue:` and the sorted item IDs, origin
`StartUser` when any item came from the user (else `StartScheduled`), and the
items' messages as its input. A caller's `Start` takes the queued user items
first, ahead of its own input, since the user typed them first; other items
run as a queue turn after it.

**Nothing is lost or doubled.** An item stays queued, in its place, until the
session knows whether the `Start` that carried it recorded its first step. A
`turn_started` event the run already reported proves it did. When the result
is unclear, the session loads the turn to decide. If the load fails, the item
stays reserved: new commands still run, but the queue waits, each outcome
warns `queue_blocked`, and `RunQueue` returns the error until the turn can be
read. A queue turn whose `queue:` key a recent turn already started is a
duplicate, so its items are dropped as delivered. A steer
that misses its turn moves to the queue when the turn ends or a new `Start`
closes it; while the turn can still be continued, the steer waits for its
`Continue`.

**In memory only.** The queue lives in the handle. Under a durable recorder,
every queued item returns a `memory_only` warning in `Queued.Warnings`: a
server that builds a handle per request loses what it queued. Steer a turn
another process runs through your own `Session.Steering` source.

## Streams and transcripts

`s.Stream(ctx, command)` is `Run` with events. It yields every turn the call
runs: the step that closes a stopped last turn, the command's own turn, and
the queue turns after it. Each event carries its `TurnID`, and each turn ends
with its own `Outcome` event; the last one is the result. Breaking the range
interrupts the turn, and no queue turn starts. A retried `Start` whose turn is
running follows that turn instead, when the recorder can read steps.

```go
for ev, err := range s.Stream(ctx, dashi.Say(text)) {
	if err != nil {
		return err
	}
	render(ev.TurnID, ev) // a turn change starts a new block
}
```

`s.Turns(ctx)` yields every turn record in order, closed turns included, for
a transcript; an unreadable turn yields its error and the iteration goes on. A
turn the session closed has a canceled stop with the reason `CloseReason`.
`s.History(ctx)` is the conversation: every message of every turn.
`s.ProjectedHistory(ctx)` is what the next turn sends; the two differ once
compaction turns arrive ([design](../../design/session-input.md)).
