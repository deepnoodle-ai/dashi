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

Its [source](../../examples/agent/main.go) shows the definition, command, and
outcome. The recorder in that example is optional for a single process.

## Prior conversation

A Turn is one piece of agent work. If it follows earlier conversation, pass
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
	ID:      "start-1",
	History: prior,
	Input: []llm.Message{
		llm.UserMessage(llm.Text("How do I resume it?")),
	},
}
resume := kernel.Continue{
	ID:      "resume-1",
	History: prior,
}
```

## IDs and recovery

Each turn requires a stable ID and each command requires an ID for duplicate
delivery handling. A Turn can span workers; its saved record holds the steps
needed to resume its own work. When the running Go context ends, Kernel
normally records `StopInterrupted` with the context cause.
Reload the record and call `Continue` with the earlier conversation on any
worker. `Continue` also recovers a crash that happened before the stop was
written. Close `Bindings.Pause` for a graceful stop before the next model or
tool call. Work already running finishes.

`Bindings.Recorder` may be omitted. Kernel uses a fresh in-memory recorder
for that call, and you can pass the returned `Outcome.Turn` to a later call in
the same process. Bind a durable recorder when turns must resume across
processes.

An interrupted model call is sent again; its partial output is display-only,
while its usage still counts toward spend. A started tool without a result is
rerun only if declared `ReadOnly` or `Idempotent`. Otherwise its outcome is
uncertain: check the outside system, report the result with `Accept`, and call
`Continue`. `Accept` only records, including for approvals and external tool
results. `Outcome.ReadyToContinue` and the replayed turn's
`TurnView.ReadyToContinue` show when no unresolved effect blocks the next
`Continue`. The host's sweeper should pick up ready turns with no active
worker.

`Limits.Interruptions` bounds resumes after interruption (default 10); these
resumes do not consume `ModelAttempts`. The turn's `Limits.Deadline` stops it as
limited; a host context deadline interrupts it. See
[agent turns and recovery](../agent-turns.md) for recording and stop details.

## Structured output

For a validated object result, set `Definition.OutputSchema` or pass a
per-turn `Start.OutputSchema`, then read `Outcome.Output.Value` after
`StopCompleted`. The output fact includes tool-call or final-text provenance;
see [structured final results](../agent-turns.md#structured-final-results).

## Streaming a turn

`Engine.Stream` is the iterator form of `Engine.Apply`. It enables provider
streaming, yields acknowledged steps and provisional deltas in order, then one
final `Outcome`. Breaking the range interrupts the turn by default; reload
its record and call `Continue` when ready. Set
`Bindings.OnContextDone: ContextCancel` when a disconnect should instead end
the turn as canceled. The iterator is backpressured; `Bindings.Observer`
remains a separate, lossy display hook. Ranging the same turn sequence again
returns `kernel.ErrStreamConsumed`.

## Runtime reminders

For typed recorded context, see
[runtime reminders](../../design/runtime-reminders.md).

## Open gates

The [Dive comparison and open gates](../agent-loop-review.md) record what
remains to prove before stabilizing the loop API.
