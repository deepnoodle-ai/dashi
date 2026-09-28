# Running turns

This guide shows how to run an agent turn, recover its effects, and consume its
results; [agent turns and recovery](../agent-turns.md) is the detailed
contract.

## Try it

With the same environment as the [model call guide](model-calls.md) (Go 1.26,
`OPENAI_API_KEY`, and `OPENAI_MODEL`), run:

```sh
go run ./examples/agent
```

Its [source](../../examples/agent/main.go) shows the agent, runner, command,
and outcome. The recorder in that example is optional for a single process.

A `Runner` holds the agent and its services; each call passes a turn and one
command. Provider and API default to the client's:

```go
runner := kernel.Runner{
	Agent:  kernel.Agent{ID: "support", Revision: "1", Model: model},
	Client: client,
}
out, err := runner.Run(ctx, kernel.NewTurn("turn-1"), kernel.Start{
	CommandID: "start-1",
	Input:     input,
})
```

## Prior conversation

A turn is one input processed until the agent settles, possibly across many
model calls. If it follows earlier conversation, pass
those messages in `Start.History` and again in `Continue.History` on every
resume. Kernel uses that history to build model requests during the current
call. The saved turn records its own input and output plus a fingerprint of
the earlier history; it does not save the earlier messages. Keep them in your
application so a worker can supply them after a crash. If the history on
`Continue` differs, Kernel proceeds and records a warning in
`Outcome.Warnings` and `Replay(turn).Warnings`. If `Start` had no history,
omit `History` on `Continue` (or pass an empty slice).

For example, reuse the same `prior` messages on both commands:

```go
start := kernel.Start{
	CommandID: "start-1",
	History:   prior,
	Input: []llm.Message{
		llm.UserMessage(llm.Text("How do I resume it?")),
	},
}
resume := kernel.Continue{
	CommandID: "resume-1",
	History:   prior,
}
```

## IDs and recovery

Each turn requires a stable ID, and each command requires a `CommandID` for
duplicate delivery handling. A turn can span workers; its saved record holds the
steps needed to resume its own work. When the running Go context ends, Kernel
normally records `StopInterrupted` with the context cause. Reload the record and
call `Continue` with the earlier conversation on any worker. `Continue` also
recovers a crash that happened before the stop was written. Close the channel
passed with `kernel.WithPause` for a graceful stop before the next model or tool
call. Work already running finishes.

`Runner.Recorder` may be omitted. Kernel uses a fresh in-memory recorder
for that call, and you can pass the returned `Outcome.Turn` to a later call in
the same process. Bind a durable recorder when turns must resume across
processes.

An interrupted model call is sent again; its partial output is display-only,
while its usage still counts toward spend. A started tool without a result is
rerun only if declared `ReadOnly` or `Idempotent`. Otherwise its outcome is
uncertain: check the outside system, report the result with `Settle`, and call
`Continue`. `Settle` only records, including for approvals and external tool
results. To answer an approval, replay the turn and echo the pending call's
`ToolView.ArgsDigest` and `Decision.Revision` in an `ApprovalFact`;
`kernel.ArgsDigest(args)` gives the same digest for arguments shown earlier.
`Outcome.ReadyToContinue` and the replayed turn's `TurnView.ReadyToContinue`
show when no unresolved effect blocks the next `Continue`. The host's sweeper
should pick up ready turns with no active worker.

The zero `Limits` value gives the defaults: 8 model attempts, 10 resumes after
interruption, 3 provider pauses, one tool call at a time, a reserved final
answer, and no deadline. Set only the fields you want to change; a negative
`Interruptions` or `ProviderPauses` allows none. Kernel records the resolved
numbers at `Start`, so a saved turn keeps its limits if the defaults change.
Resumes after interruption do not consume `ModelAttempts`. The turn's
`Limits.Deadline` stops it as limited; a host context deadline interrupts it. See
[agent turns and recovery](../agent-turns.md) for recording and stop details.

## Structured output

For a validated object result, set `Agent.OutputSchema` or pass a
per-turn `Start.OutputSchema`, then read `Outcome.Output.Value` after
`StopCompleted`. The output fact includes tool-call or final-text provenance;
see [structured final results](../agent-turns.md#structured-final-results).

## Streaming a turn

`Runner.Stream` is the iterator form of `Runner.Run`. It enables provider
streaming, yields acknowledged steps and provisional deltas in order, then one
final `Outcome`. Breaking the range interrupts the turn by default; reload
its record and call `Continue` when ready. Set
`Runner.OnContextDone: ContextCancel` when a disconnect should instead end
the turn as canceled. The iterator is backpressured; `Runner.Observer`
remains a separate, lossy display hook. Ranging the same turn sequence again
returns `kernel.ErrStreamConsumed`.

## Serving live updates

`Runner.Stream` suits a caller that owns the turn's lifetime, such as a CLI or
a worker. For a durable turn served to browsers, do not connect an HTTP or SSE
request to `Stream`: leaving the range interrupts the turn, and a closed tab
must not control agent work.

Instead, run the turn in your own background execution (a queue worker or job)
under a context you control, using `Runner.Run` or `Stream`. Your
`Recorder.Commit` is the natural place to write what clients read: in the same
transaction that appends the step, write your projection or outbox row. Clients
then follow your committed record, not the runner. Each step carries a gap-free
`Seq` per turn, so a client reconnects with the last `Seq` it applied and
receives everything after it. A disconnect ends only that follow.

```go
func (r *pgRecorder) Commit(ctx context.Context, req kernel.CommitRequest) error {
	return r.db.InTx(ctx, func(tx Tx) error {
		// Check ExpectedSeq and Owner, append req.Step, then in the same tx:
		return tx.InsertEvent(req.Step.TurnID, req.Step.Seq, project(req.Step))
	})
}

// GET /turns/{id}/events?after=N: replay rows with seq > N, then poll
// (optionally woken by a notify) until the request context ends.
```

`Runner.Observer` is lossy: it drops events, including steps, when its buffer
fills. Use it for in-process display hints such as text deltas, never as a
delivery channel. Treat deltas as previews that the committed model step
supersedes.

## Runtime reminders

For typed recorded context, see
[runtime reminders](../../design/runtime-reminders.md).

## Open gates

The [Dive comparison and open gates](../agent-loop-review.md) record what
remains to prove before stabilizing the loop API.
