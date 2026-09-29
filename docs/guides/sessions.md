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
	out, err = s.Answer(ctx, out.Pending[0].Approve())
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
  `filerecorder` probes. `dashitest.Run` checks both.
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

- the same key and input returns that turn's outcome once it has ended, and
  `*TurnBusyError{Duplicate: true}` before;
- the same key with other input fails with `ErrCommandConflict`.

Two workers that race to start the same turn collide on its ID; the loser
reloads and applies the rules once more.

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
`memory_only`, and `turn_unreadable`, for a turn left out of the history
because it could not be loaded or projected. An unreadable last turn is an
error.

Steering, queued input, streams across turns, and compaction turns come next
([design](../../design/session-input.md)); until then, pass
[steering](steering.md) with `WithSteering`.
