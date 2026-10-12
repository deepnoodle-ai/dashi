# Running turns

This guide is the starting point for running an agent turn and consuming its
results. The focused guides cover the rest:

- [Sessions](sessions.md): a conversation of many turns, with the history,
  agent, and open turns handled for you.
- [Hooks](hooks.md): checking or changing model calls, tool calls, and final
  replies.
- [Approving tool calls](approvals.md): asking a person inline, or holding
  calls for approval and resolving them later.
- [Recovering turns](recovering-turns.md): IDs, recorders, crash recovery,
  turn states, and limits.
- [Live updates](live-updates.md): serving a turn to browsers and observing a
  run in process.
- [Steering](steering.md): messages from people while a turn runs, and
  adding context with `AddMessages`.

[Agent turns and recovery](../agent-turns.md) is the detailed contract.

## Try it

With the same environment as the [model call guide](model-calls.md) (Go 1.27
and `OPENAI_API_KEY`), run:

```sh
go run ./examples/agent
```

Its [source](../../examples/agent/main.go) shows the agent, runner, command,
and result.

A `Runner` holds services such as the client and tools; each call passes a
turn and one command, and `Start` carries the agent. Everything else has a
default, so the shortest turn is:

```go
runner := dashi.Runner{Client: client, Tools: tools}
out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{Input: input})
```

- **Agent.** Set it on `Start.Agent`; the turn records it, and later commands
  run with the recorded agent. Provider and API come from the client. An empty
  `Model` is the provider's `catalog.Default`; providers without a catalog
  need `Model`. An empty `Version` is derived from the agent's behavior
  fields (`"auto:…"`), so it changes when they do; the tool selection is part
  of it but tool implementations are not, so set `Version` yourself if you
  version tools. `ID` may stay empty.
- **IDs.** A `Start` on the zero `Turn` gets a generated turn ID, and a command
  with no `CommandID` gets one. `out.Turn.ID`, every step event, and each
  step's `CommandID` show them; see [IDs and commands](recovering-turns.md#ids-and-commands).
- **Tools.** Every bound local and external tool runs: binding a tool is the
  app's statement that the model may use it. `Agent.Tools` narrows that by
  name ([tools](tools.md#choosing-tools-per-agent)). To check or ask first,
  add a [hook](hooks.md).
- **Keys.** A client built without an API key fails on its first call with an
  error matching `llm.ErrNoAPIKey`, which names the environment variable.

## Reading the result

`Run` returns a `RunResult` and an error. The result is always the latest
known state of the turn; the error says whether the command applied.
`out.Reply` is the final reply text, `out.Output` the structured output,
`out.Spend` the usage and cost, and `out.Pending` the work the turn waits on.
`out.Turn` is the record to pass to the next call. `turn.View()` gives the
same view for a turn loaded from a recorder.

For most programs, `if err != nil { return err }` is enough. A host that
recovers turns checks, in order: `errors.Is(err, dashi.ErrStaleTurn)`
(another worker owns the turn; reload and back off), then
`errors.As(err, &commitErr)` for a `*dashi.CommitError` (the run executed, but
`commitErr.Step` was not recorded; recover as after a crash; see
[recovering turns](recovering-turns.md)). Any other error
means the command was rejected and nothing was recorded, except
`ErrRecordInvalid`: steps this run recorded fail replay, a bug to report. A
failed or limited turn is not an error; see `out.Stop`. `out.Turn.Citations()`
and `out.Turn.ProviderToolUses()` collect sources and provider tool calls from
every model call in the turn.

`out.Reply` is the latest assistant text, even when the turn did not
complete. Check `out.Stop` before showing it.

`out.Next()` says what to do next, so a host need not decode `out.Stop`:
`NextNone` (completed or ended), `NextResolvePending` (resolve `out.Pending`,
then `Continue`), `NextContinue`, `NextRaiseBudget` or `NextRaiseModelCalls`
(`Continue` with a raise), `NextFix` (fix a failure, refusal, or a call whose
result is unknown, then `Continue`, or `Cancel`), and `NextRecover` (no stop
was recorded, so `out.Stop.Inferred` is true: the worker may have crashed;
`Continue` resumes it when `ReadyToContinue`, and otherwise resolve
`out.Pending` first;
see [recovering turns](recovering-turns.md)). A hook's view of a running turn
gives `NextRunning`.

## Prior conversation

A turn is one input processed until the agent finishes, possibly across many
model calls. If it follows earlier conversation, pass
those messages in `Start.History` and again in `Continue.History` on every
resume. Dashi uses that history to build model requests during the current
call. The saved turn records its own input and output plus a hash of the
earlier history; it does not save the earlier messages. Keep them in your
application so a worker can supply them after a crash. If the history on
`Continue` differs, Dashi proceeds and records a warning in
`RunResult.Warnings` and the `Warnings` of `turn.View()`. If `Start` had no
history, omit `History` on `Continue` (or pass an empty slice).

A [session](sessions.md) does this for you: it builds the history from the
earlier turns' records. For example, without one, reuse the same `prior`
messages on both commands:

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

## Running turns without a session

A host that creates and stores its own turns needs no loop of its own:

1. **Admit.** Choose the turn ID and save the `Start` with
   `runner.Record(ctx, dashi.NewTurn(id), start, dashi.WithOwner(owner))`.
   It runs nothing, and needs no `Client` when the Agent names its provider,
   API, and model.
2. **Collect.** Put everything that arrives later in the host's
   [`Inbox`](steering.md#durable-inboxes), keyed by turn ID: a pending item's
   `Result`, `Approve`, or `Deny`; a `Continue` that raises a limit; steers;
   a `Cancel`.
3. **Drive.** The worker that holds the turn's lease calls
   `runner.Drive(ctx, id, dashi.WithOwner(owner), dashi.WithHistory(history))`.
   It applies the inbox, runs the turn whenever it can go on, and waits for
   the inbox otherwise, until the turn ends or `ctx` ends.

`Advance` is one pass of `Drive`, for a host that schedules its own wakeups.
Both derive each `Continue`'s ID from the record, so a crash replays the same
command and no outbox is needed. `NextRaiseBudget`, `NextRaiseModelCalls`,
and `NextFix` wait for the host: put a raising `Continue` or a `Cancel` in the
inbox. A host limit without `Ends`, such as a balance hold, needs neither:
each pass asks `BeforeSend` again and runs the turn once it allows the call.
Watch turns with `dashi.FollowNext`, which shows a turn at its stops and once
as working after each, so a watcher never acts on an intermediate state.
`ExampleRunner_Advance` and `ExampleRunner_Drive` run without an API key;
[recovering turns](recovering-turns.md#advancing-saved-turns) has the rules.

The rest of a host's work has its own guide: charging or releasing each model
call's credit hold in the recorder with `CommitRequest.ModelCall`
([budgets](budgets.md)); history across turns and compaction with
`dashi.BuildHistory`
([compaction without a session](compaction.md#without-a-session)); and
leases, crashes, and limits ([recovering turns](recovering-turns.md)).

Package `session` is built only on this public API, so a host can do
everything it does. A few helpers serve such hosts: `Runner.CacheKey` sets a
conversation's prompt cache key, `dashi.Recorded(err)` tells a failure after
recording began from a rejected command, `TurnView.ReplacementHistory` reads
a compaction turn's result, `ContextItem.Validate` checks input before it is
queued, and `MemoryInbox.Has` checks a steer ID. A host that carries a recorded
agent into its next turn clears a `Version` that begins with
`dashi.DerivedVersionPrefix`.

## Structured output

For a validated object result, set `Agent.OutputSchema` or pass a
per-turn `Start.OutputSchema`, then decode the value after the turn:

```go
start.Agent.OutputSchema = dashi.MustOutputSchema[Person]()
// Run the turn, then:
person, err := dashi.DecodeOutput[Person](out)
```

`MustOutputSchema` follows `tool.Func`'s schema rules and panics for a type
that cannot be an object schema. `DecodeOutput` returns an error wrapping
`dashi.ErrNoOutput`, naming the stop, when the turn has no output. The output
fact includes tool-call or final-text provenance;
see [structured final results](../agent-turns.md#structured-final-results).

## Streaming a turn

`Runner.Stream` is the iterator form of `Runner.Run`. It enables provider
streaming, yields acknowledged steps and provisional deltas in order, then one
final `RunResult`. Breaking the range interrupts the turn by default; reload
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
