# Running turns

This guide is the starting point for running an agent turn and consuming its
results. The focused guides cover the rest:

- [Hooks](hooks.md): checking or changing model calls, tool calls, and final
  answers.
- [Approving tool calls](approvals.md): asking a person inline, or deferring
  calls and answering them later.
- [Recovering turns](recovering-turns.md): IDs, recorders, crash recovery,
  turn states, and limits.
- [Live updates](live-updates.md): serving a turn to browsers and observing a
  run in process.
- [Steering](steering.md): messages from people while a turn runs, and
  adding context with `Deliver`.

[Agent turns and recovery](../agent-turns.md) is the detailed contract.

## Try it

With the same environment as the [model call guide](model-calls.md) (Go 1.26
and `OPENAI_API_KEY`), run:

```sh
go run ./examples/agent
```

Its [source](../../examples/agent/main.go) shows the agent, runner, command,
and outcome.

A `Runner` holds the agent and its services; each call passes a turn and one
command. Everything else has a default, so the shortest turn is:

```go
runner := dashi.Runner{Client: client, Tools: tools}
out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{Input: input})
```

- **Agent.** Provider and API come from the client. An empty `Model` is the
  provider's `catalog.Default`; providers without a catalog need `Model`. An
  empty `Revision` is derived from the agent's behavior fields
  (`"auto:…"`), so it changes when they do; tools are not part of it, so set
  `Revision` yourself if you version tools. `ID` may stay empty.
- **IDs.** A `Start` on the zero `Turn` gets a generated turn ID, and a command
  with no `CommandID` gets one. `out.Turn.ID`, every step event, and each
  step's `CommandID` show them; see [IDs and commands](recovering-turns.md#ids-and-commands).
- **Tools.** Every bound local and external tool runs: binding a tool is the
  app's statement that the model may use it. To check or ask first, add a
  [hook](hooks.md).
- **Keys.** A client built without an API key fails on its first call with an
  error matching `llm.ErrNoAPIKey`, which names the environment variable.

## Reading the result

`Run` returns an `Outcome` and an error. The outcome is always the latest known
state of the turn; the error says whether the command applied. `out.Answer` is
the final answer text, `out.Output` the structured output, `out.Spend` the
usage and cost, and `out.Pending` the work the turn waits on. `out.Turn` is the
record to pass to the next call. `dashi.Replay(turn)` gives the same view for a
turn loaded from a recorder.

For most programs, `if err != nil { return err }` is enough. A host that
recovers turns checks, in order: `errors.Is(err, dashi.ErrRevisionConflict)`
(another worker owns the turn; reload and back off), then
`errors.As(err, &commitErr)` for a `*dashi.CommitError` (the run executed, but
`commitErr.Step` was not recorded; recover as after a crash; see
[recovering turns](recovering-turns.md)). Any other error
means the command was rejected and nothing was recorded, except
`ErrRecordInvalid`: steps this run recorded fail replay, a bug to report. A
failed or limited turn is not an error; see `out.Stop`. `out.Turn.Citations()` and
`out.Turn.HostedUses()` collect sources and provider-run tool calls from every
model call in the turn.

`out.Answer` is the latest assistant text, even when the turn did not
complete. Check `out.Stop` before showing it.

`out.Next()` says what to do next, so a host need not decode `out.Stop`:
`NextNone` (completed or ended), `NextAnswer` (settle `out.Pending`, then
`Continue`), `NextContinue`, `NextRaiseBudget` or `NextRaiseAttempts`
(`Continue` with a raise), `NextResolve` (fix a failure, refusal, or uncertain
effect, then `Continue`, or `Cancel`), and `NextResume` (no stop was recorded,
so `out.Stop.Projected` is true: the worker may have crashed; `Continue`
resumes it when `ReadyToContinue`, and otherwise settle `out.Pending` first;
see [recovering turns](recovering-turns.md)). A hook's view of a running turn
gives `NextRunning`.

## Prior conversation

A turn is one input processed until the agent settles, possibly across many
model calls. If it follows earlier conversation, pass
those messages in `Start.History` and again in `Continue.History` on every
resume. Dashi uses that history to build model requests during the current
call. The saved turn records its own input and output plus a fingerprint of
the earlier history; it does not save the earlier messages. Keep them in your
application so a worker can supply them after a crash. If the history on
`Continue` differs, Dashi proceeds and records a warning in
`Outcome.Warnings` and `Replay(turn).Warnings`. If `Start` had no history,
omit `History` on `Continue` (or pass an empty slice).

For example, reuse the same `prior` messages on both commands:

```go
start := dashi.Start{
	CommandID: "start-1",
	History:   prior,
	Input: []llm.Message{
		llm.UserMessage(llm.Text("How do I resume it?")),
	},
}
resume := dashi.Continue{
	CommandID: "resume-1",
	History:   prior,
}
```

## Structured output

For a validated object result, set `Agent.OutputSchema` or pass a
per-turn `Start.OutputSchema`, then decode the value after the turn:

```go
runner.Agent.OutputSchema = dashi.MustOutputSchema[Person]()
// Run the turn, then:
person, err := dashi.DecodeOutput[Person](outcome)
```

`MustOutputSchema` follows `tool.Func`'s schema rules and panics for a type
that cannot be an object schema. `DecodeOutput` returns an error wrapping
`dashi.ErrNoOutput`, naming the stop, when the turn has no output. The output
fact includes tool-call or final-text provenance;
see [structured final results](../agent-turns.md#structured-final-results).

## Streaming a turn

`Runner.Stream` is the iterator form of `Runner.Run`. It enables provider
streaming, yields acknowledged steps and provisional deltas in order, then one
final `Outcome`. Breaking the range interrupts the turn by default; reload
its record and call `Continue` when ready. Set
`Runner.OnContextDone: ContextCancel` when a disconnect should instead end
the turn as canceled. The iterator is backpressured and never drops an
event; `Runner.Observer` is the separate, never-blocking hook. Ranging the
same turn sequence again returns `dashi.ErrStreamConsumed`.

For serving browsers and the `Observer`, see [live updates](live-updates.md);
for tool progress, see [tools](tools.md#tool-progress).

## See also

- [Runtime reminders](../../design/runtime-reminders.md): typed recorded
  context.
- [Dive comparison and open gates](../agent-loop-review.md): what remains to
  prove before stabilizing the loop API.
