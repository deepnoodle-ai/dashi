# Steering

People type while an agent works: "use the staging database", "stop, wrong
file". Pass a steering source to the run, and each message reaches the turn
at a safe point: before the next model call, or, when the model has just
answered, as a reason to ask it again. A steer never lands between a tool
call and its result, and never stops a running tool.
[Running turns](running-turns.md) covers the basics. To add context while
no run is active, use [`Deliver`](#delivering-context).

```go
inbox := dashi.NewInbox()
go func() {
	for line := range input {
		inbox.SendText(line)
	}
}()
out, err := runner.Run(ctx, turn, start, dashi.WithSteering(inbox))
if err != nil {
	return err
}
if out.Stop.Terminal {
	next, _ := inbox.Leftovers(out.Turn) // input for the next turn, if any
}
// otherwise: Continue with the inbox, or call Leftovers to abandon the turn
```

[`examples/steering`](../../examples/steering) is a complete chat CLI.

- **One active run per inbox.** An inbox can serve many turns, one at a time.
- **Claims.** A steer the inbox hands to a run is claimed: `Cancel` returns
  false from then on. Claims the run did not record are released when it ends.
- **Leftovers.** `Leftovers(turn)` returns, once, the steers not recorded in
  that turn: those still queued or never written. Recorded steers are part of
  the turn's history, so they reach the next turn through `Project(turn)`,
  even when no model call saw them. Pass a freshly loaded turn.
- **A steer may cost a model call.** A steer that supersedes an answer needs
  another call; several steers share one, and a steer that arrives before a
  planned call adds none. Steering never spends the
  [reserved final-answer call](recovering-turns.md#turn-states-and-limits) or goes past a limit, the deadline, or a pause: the steer then waits
  for the next turn, and a `WarningSteering` on the record says why.
  Interactive agents should raise `Limits.ModelAttempts` from its
  [default of 8](recovering-turns.md#turn-states-and-limits).
- **Urgent steers.** `inbox.SendUrgent(text)` also cuts short the model call
  in flight, when a replacement call fits the turn's limits. The next call
  sees the partial text, a reminder that the user interrupted it, and the
  steer. The cut call is recorded with `Interrupted: "steering"`; it does not
  count toward `ModelAttempts`, but its cost counts, and under a `MaxCost`
  cap it is charged its worst case. A call that offers a provider-hosted
  tool unsafe to repeat is never cut once sent. A cut that does not happen
  leaves one `WarningSteering` saying why, and the steer waits for the next
  call. The cut call's settlement closes its live preview; a UI should keep
  its partial text visible, marked as interrupted, since `out.Answer` and
  the transcript helpers skip interrupted results. A durable source signals
  urgent steers by implementing `dashi.SteeringNotifier`.

A durable host implements `dashi.Steering` over its own table, keyed by turn:
`Steers` returns the rows not yet acknowledged, and `Delivered` consumes them.
The record is the authority: each steer carries an ID, a turn records an ID at
most once, and Dashi skips a steer it finds already recorded, so a steer
presented twice after a crash is delivered once. A host may instead consume
rows in the recorder transaction that saves the step, reading the IDs from
`Step.Items()` of a step whose `Context.Steering` is set. After a
`*CommitError` whose persistence is unknown, reload the turn before deciding
what happened to its steers.

## Delivering context

`Deliver` adds context to a turn that is not running. Its items may carry IDs
too; a repeat with the same content is skipped. Context items, from steers,
`Deliver`, or hooks, may hold only text, media, documents, and reminders.
