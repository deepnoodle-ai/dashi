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

Its [source](../../examples/agent/main.go) shows the definition, recorder,
command, and outcome.

## IDs and recovery

Each turn requires a stable ID and each command requires an ID for duplicate
delivery handling. A Turn is one piece of agent work that can span workers;
its saved record holds the steps needed to resume it. When the running Go
context ends, Kernel normally records `StopInterrupted` with the context cause.
Reload the record and call `Continue` on any worker. `Continue` also recovers a
crash that happened before the stop was written. Close `Bindings.Pause` for a
graceful stop before the next model or tool call. Work already running finishes.

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
