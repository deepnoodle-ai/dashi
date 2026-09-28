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
delivery handling. An acknowledged model or tool intent may already have caused
an external effect; recover it by identity before deciding whether to retry.
See [agent turns and recovery](../agent-turns.md) for the recorder,
pending-result, approval, and stop contracts.

## Structured output

For a validated object result, set `Definition.OutputSchema` or pass a
per-turn `Start.OutputSchema`, then read `Outcome.Output.Value` after
`StopCompleted`. The output fact includes tool-call or final-text provenance;
see [structured final results](../agent-turns.md#structured-final-results).

## Streaming a turn

`Engine.Stream` is the iterator form of `Engine.Apply`. It enables provider
streaming, yields acknowledged steps and provisional deltas in order, then one
final `Outcome`. Breaking the range cancels the turn; reload its record to
inspect the committed result or pending uncertainty. The iterator is
backpressured; `Bindings.Observer` remains a separate, lossy display hook.
Ranging the same turn sequence again returns `kernel.ErrStreamConsumed`.

## Runtime reminders

For typed recorded context, see
[runtime reminders](../../design/runtime-reminders.md).

## Open gates

The [Dive comparison and open gates](../agent-loop-review.md) record what
remains to prove before stabilizing the loop API.
