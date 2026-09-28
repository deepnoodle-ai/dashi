# Running turns

This guide shows how to run an agent turn, recover its effects, and consume its
results; [agent turns and recovery](../agent-turns.md) is the detailed
contract.

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
  step's `CommandID` show them; see [IDs and recovery](#ids-and-recovery).
- **Tools.** Every bound local and external tool runs: binding a tool is the
  app's statement that the model may use it. To check or ask first, add a
  [hook](#hooks).
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
`commitErr.Step` was not recorded; recover as after a crash). Any other error
means the command was rejected and nothing was recorded, except
`ErrRecordInvalid`: steps this run recorded fail replay, a bug to report. A
failed or limited turn is not an error; see `out.Stop`. `out.Turn.Citations()` and
`out.Turn.HostedUses()` collect sources and provider-run tool calls from every
model call in the turn.

## Hooks

A hook is a named function that Dashi calls at one point of a turn. Set them
on `Runner.Hooks`; any number run at each point, in order:

| Point | Runs | Can | If it fails |
|---|---|---|---|
| `BeforeModel` | before each model request is prepared | change settings, deliver context, stop | skipped, with a warning |
| `BeforeSend` | on each prepared request, with its cost at risk | deny, report a limit, record admission terms | the turn stops failed |
| `BeforeTool` | on each client tool call with valid arguments | allow, deny, defer, rewrite the arguments | the call is denied, with a warning |
| `BeforeFinish` | on each final answer: final text, or output submitted with `dashi_submit_output` | ask the model to continue, turn a submission down, accept an empty answer | the turn stops failed |

A zero answer means no opinion. Each hook's answer is saved in the turn record
with the step it leads to (`Step.HookResults`), and replay never calls hooks.
A failed stop has the reason `dashi.StopReasonHookFailed`: fix the cause, then
`Continue` resumes where the turn stopped; for `BeforeFinish` it asks the hooks
again about the same answer, without another model call, unless you delivered
context in between, which the model then sees. If the run's context ends while
a hook runs, the turn stops as it would for any ended context.

A tool hook that returns `dashi.Allow(reason)` leaves an audit record naming
it; one with no opinion leaves none. When a `BeforeTool` hook fails, the model
sees its name, so keep private details out of hook names. A hook that stops
the turn with a cost or token limit resumes like the turn's own caps, with a
`Continue` that raises the `Budget`; a limit the host owns, such as prepaid
credits, uses `dashi.LimitHost`.

Supply the same `Runner.Hooks` on every command for a turn: each command runs
only the hooks it is given, and a change is recorded as a warning.
`dashi.WithHooks` adds hooks for one call, for extras such as a terminal
prompt. Hook names must be unique within a point; reusable packages should
prefix theirs. `dashi.JoinHooks` combines hook sets.

A hook that defers every tool not marked read-only:

```go
gate := dashi.ToolHook{Name: "read-only-gate", Func: func(
	_ context.Context, p dashi.ToolPlan) (dashi.ToolDecision, error) {
	if ro := p.Definition.Annotations.ReadOnly; ro != nil && *ro {
		return dashi.ToolDecision{}, nil
	}
	return dashi.Defer("changes something"), nil
}}
runner.Hooks = gate.Hooks()
```

`BeforeModel` runs before every model call in the turn, so a hook that
delivers context should check `p.Turn.Context(origin, name)` first and skip an
item that is already current. `BeforeFinish` vetoes an answer with a
`Reason`, which the model sees as "Not accepted: …" before it answers again,
or nudges with `Deliver` items alone. A nudge never spends the reserved
final-answer call: when no normal model call is possible (on the reserved
call or the one before it, past the deadline, or when the next call would
exceed a cost cap), it is dropped with a warning and the answer stands. A
veto may use the reserved call. When no call fits at all, or the reserved
answer is vetoed too, the turn stops failed with
`dashi.StopReasonOutputRejected` and no answer; `Continue` after raising the
limit lets the model try again. A vetoed answer never shows in `out.Answer`.

`out.Answer` is the latest assistant text, even when the turn did not
complete. Check `out.Stop` before showing it.

## Approving tool calls

A command-line program asks inside a `BeforeTool` hook, all within one `Run`.
`dashi.OnTool` runs only for one tool and decodes its arguments; a denial's
reason is what the model sees
([example](../../examples/approval/main.go)):

```go
ask := dashi.OnTool("deploy", func(
	_ context.Context, _ dashi.ToolPlan, in deployArgs,
) (dashi.ToolDecision, error) {
	if confirm(in) {
		return dashi.Allow("operator approved"), nil
	}
	return dashi.Deny("The operator declined this deploy."), nil
})
runner.Hooks = ask.Hooks()
```

A server that cannot wait defers instead. `dashi.RequireApproval("deploy")`
defers the named tools (every tool when none are named); the turn stops with
`StopWaiting`, and each pending item builds its own answer
([example](../../examples/durable_approval/main.go)):

```go
runner.Hooks = dashi.RequireApproval("deploy")
// ... Run, then:
for _, p := range out.Pending {
	if p.Kind != dashi.PendingApproval {
		continue
	}
	answer := p.Deny("The operator declined.")
	if approved(p.Name, p.Args) {
		answer = p.Approve()
	}
	if out, err = runner.Run(ctx, out.Turn, answer); err != nil {
		return err
	}
}
out, err = runner.Run(ctx, out.Turn, dashi.Continue{})
```

`Approve` and `Deny` bind the answer to the call and the exact arguments
shown; hooks do not run again. `Result(outcome)` reports the result of a
waiting external call the same way. Each builder derives its command ID from
the call, so a retried answer is a duplicate and a different second answer
fails with `ErrCommandConflict`. `Settle` only records; `Continue` runs the
approved calls and returns `ErrNotReady` while any call is unanswered. Once
every call is answered, `out.Pending` is empty and `out.ReadyToContinue` is
true, but `out.Stop` still says waiting until `Continue` runs.

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

## IDs and recovery

A turn ID lets any worker find the turn again, and a `CommandID` lets Dashi
recognize a repeated command: the same ID with the same content returns the
recorded result instead of running again. Generated IDs are fine in one
process. For retries across restarts, choose the turn ID and each
`CommandID` yourself (`dashi.NewID()` makes one), save them before the call,
and reuse them on retry; nobody can reuse an ID generated inside a call that
crashed. Without that, a retry from an old turn snapshot is still refused by
the recorder, because its step numbers are taken, so no model call or tool
runs twice. What can happen is a second turn from a `Start` retried with no
turn ID, or a `Deliver` recorded twice when its items carry no IDs.

On an existing turn, a zero `Agent` runs with the turn's recorded agent, which
suits commands such as `Settle` and `Cancel`. Setting any field, even only
`Limits.Deadline`, makes the agent partly set: it takes its empty `ID`,
`Model`, and `Revision` from the record, and any other difference fails with
`ErrAgentChanged`, whose message names the fields. A zero `Agent` trusts the
agent recorded in the turn you pass in, so load that turn from your own store.
When the recorder implements `Loader`, Dashi checks the snapshot's agent
against the stored record and returns `ErrAgentChanged` if they differ.

A turn can span workers; its saved record holds the steps needed to resume its
own work. When the running Go context ends, Dashi normally records
`StopInterrupted` with the context cause. Reload the record and call `Continue`
with the earlier conversation on any worker. `Continue` also recovers a crash
that happened before the stop was written. Close the channel passed with
`dashi.WithPause` for a graceful stop before the next model or tool call. Work
already running finishes.

A command is applied once per recorder. Running the same command again, with
the same `CommandID` and content against the same recorder, returns the
recorded result without calling the model or running tools again; this is how
a restarted process safely repeats its `Start`. A new `CommandID` is a new
command, and a new recorder (such as the default in-memory one) knows nothing
of earlier runs.

`Runner.Recorder` may be omitted. Dashi uses a fresh in-memory recorder
for that call, and you can pass the returned `Outcome.Turn` to a later call in
the same process. Bind a durable recorder when turns must resume across
processes. `filerecorder` keeps each turn in an append-only file on local
disk and is safe for several processes sharing the directory:

```go
recorder, err := filerecorder.New(filerecorder.Config{Dir: "var/turns"})
runner := dashi.Runner{Agent: agent, Client: client, Recorder: recorder}
```

To keep turns in your database instead, implement `dashi.Recorder` and
`dashi.Loader`; the `Recorder` doc comment states the rules, and
`dashitest.Run` checks them. In a SQL store, commit each step in one
transaction with your own rows:

- Lock the turn's row. Reject a commit from an owner other than the latest
  claim. A known command ID succeeds only with the same kind and digest.
  At an occupied sequence, succeed only if the stored step `Equal`s the new
  one; otherwise require `seq = ExpectedSeq`, insert, and advance.
- Keep the step's own JSON as the record. Do not rebuild `At` from a
  timestamp column: steps carry nanoseconds, a microsecond column drops
  them, and a genuine retry would then be rejected.
- Postgres `jsonb` rejects `\u0000` in strings; store such steps as `json`
  or text, or reject them explicitly.
- Return a `*dashi.CommitError` with `PersistenceRejected` only when the
  transaction certainly wrote nothing. A timeout or lost connection after
  `COMMIT` was sent must be a plain error, which Dashi treats as unknown.
- `Load` returns `dashi.ErrTurnNotFound` when the turn has no steps.

An interrupted model call is sent again; its partial output is display-only,
while its usage still counts toward spend. A started tool without a result is
rerun only if declared `ReadOnly` or `Idempotent`. Otherwise its outcome is
uncertain: check the outside system, report the result with `Settle`, and call
`Continue`. `Settle` only records, including for approvals and external tool
results. `Outcome.ReadyToContinue` and the replayed turn's
`TurnView.ReadyToContinue` show when no unresolved effect blocks the next `Continue`. The host's sweeper
should pick up ready turns with no active worker.

A turn is in one of three states between runs: **ready** (`ReadyToContinue`),
**waiting on the host** (approvals, results, a budget raise), or **ended**
(`Stop.Terminal`). A stop is terminal when nothing can run the turn again:
completed, canceled, limited by its deadline or its interruptions, any limit
reached after its last model attempt, or a cost cap that can no longer be
checked. Raising a budget does not revive such a turn. After that, only
`Settle` reporting the result of work that had already started is accepted;
every other command returns `dashi.ErrTurnEnded`. Unstarted calls are settled
as not executed, so no approval stays pending on an ended turn.

The zero `Limits` value gives the defaults: 8 model attempts, 10 resumes after
interruption, 3 provider pauses, one tool call at a time, a reserved final
answer, and no deadline. Set only the fields you want to change; a negative
`Interruptions` or `ProviderPauses` allows none. Dashi records the resolved
numbers at `Start`, so a saved turn keeps its limits if the defaults change.
Resumes after interruption do not consume `ModelAttempts`. The turn's
`Limits.Deadline` stops it as limited; a host context deadline interrupts it. See
[agent turns and recovery](../agent-turns.md) for recording and stop details.

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

## Tool progress

A long-running tool can report a status snapshot with `tool.Report`. Each
report replaces the previous one; `Total` of zero means unknown, and `Data`
holds optional JSON for custom displays.

```go
tool.Report(ctx, tool.Progress{
	Message: fmt.Sprintf("processed %d/%d records", i, total),
	Done:    float64(i),
	Total:   float64(total),
})
```

Snapshots arrive as `Event.Progress` on `Runner.Stream` and on
`Runner.Observer`, labeled with the call's `CallID` and tool `Name`. They are
previews: never recorded, never replayed, and never shown to the model, so the
tool's `Outcome` must still carry its full result. `Report` never blocks and
does nothing when no one listens. When the consumer is slower than the tool,
snapshots merge and the latest wins. On `Stream`, the last report before
`Execute` returns arrives before the call's settled step. `Observer` gets
progress at the lowest priority and may drop it: progress fills at most half
its preview queue, and deltas evict queued progress when the queue is full.
Nothing for a call follows its settled step, and no progress is delivered once
the turn's context ends, such as after breaking a `Stream` range. A snapshot
may wait while the runner authorizes or commits another call.
Report at human rates, a few times a second at most, and throttle rendering
if needed. To test a tool on its own, install a reporter with
`tool.WithReporter`. See `examples/tool_progress`.

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
is not terminal. For faster updates, also implement `dashi.StepWaiter`: for
example, `pg_notify` in the commit transaction and one shared `LISTEN`
connection that wakes waiting readers. With a waiter, `Follow` still reads
every 15 seconds, so a missed wake delays a step by up to that long.

`Follow` ends when the request ends, or after a terminal stop (see
[the three turn states](#ids-and-recovery)); from a cursor that is already
the terminal stop, it ends at once. A late result of work started before the
stop can still be recorded after it; a reload, or `StepsAfter` from the
stop's `Seq`, shows it.

The handler serves committed steps:

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
			last[0].Stop.Terminal {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flush := w.(http.Flusher).Flush

	for step, err := range dashi.Follow(r.Context(), s.store, id, after) {
		switch {
		case errors.Is(err, dashi.ErrCursorAhead):
			send(w, "", "reset", nil) // the client drops its state, follows from 0
			flush()
			return
		case err != nil:
			return // the client reconnects from its last id
		}
		send(w, strconv.FormatInt(step.Seq, 10), "step", project(step))
		if step.Stop != nil && step.Stop.Terminal {
			send(w, "", "end", nil) // the page calls EventSource.close
		}
		flush()
	}
}
```

Steps go out with an SSE `id`, so the browser resumes from them. At a
terminal stop the handler sends `end`, so the page calls
`EventSource.close()`; a browser that reconnects anyway gets `204 No
Content`, which stops `EventSource` from reconnecting. This handler runs in
Dashi's tests (`serve_public_test.go`).

Live previews (token streaming and tool progress) never enter the record;
they come from `Runner.Observer` in the runner's process. A tested fan-out
helper that merges them into this stream is coming.

**Never send a `Step` to a browser as it is.** A step is the internal record:
it holds the provider's raw response (`ModelFact.Result.Raw`), encrypted or
signed reasoning, the agent's instructions and tool definitions (the
`turn_started` step), command digests, and lease owners. `project` builds the
client's view from the fields it needs, and is the one place to hide
reasoning if your product does not show it. `examples/live_follow` runs two
followers on one turn.

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
  `model_settled` step arrives with the whole message. A gap with no such step
  after it means the run ended early; load the turn.
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
		if id := e.Step.Settles(); id != "" {
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

## Steering a running turn

People type while an agent works: "use the staging database", "stop, wrong
file". Pass a steering source to the run, and each message reaches the turn
at a safe point: before the next model call, or, when the model has just
answered, as a reason to ask it again. A steer never lands between a tool
call and its result, and never stops a running tool.

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
  planned call adds none. Steering never spends the reserved final-answer
  call or goes past a limit, the deadline, or a pause: the steer then waits
  for the next turn, and a `WarningSteering` on the record says why.
  Interactive agents should raise `Limits.ModelAttempts` from its default
  of 8.

A durable host implements `dashi.Steering` over its own table, keyed by turn:
`Steers` returns the rows not yet acknowledged, and `Delivered` consumes them.
The record is the authority: each steer carries an ID, a turn records an ID at
most once, and Dashi skips a steer it finds already recorded, so a steer
presented twice after a crash is delivered once. A host may instead consume
rows in the recorder transaction that saves the step, reading the IDs from
`Step.Items()` of a step whose `Context.Steering` is set. After a
`*CommitError` whose persistence is unknown, reload the turn before deciding
what happened to its steers.

`Deliver` adds context to a turn that is not running. Its items may carry IDs
too; a repeat with the same content is skipped. Context items, from steers,
`Deliver`, or hooks, may hold only text, media, documents, and reminders.

## Runtime reminders

For typed recorded context, see
[runtime reminders](../../design/runtime-reminders.md).

## Open gates

The [Dive comparison and open gates](../agent-loop-review.md) record what
remains to prove before stabilizing the loop API.
