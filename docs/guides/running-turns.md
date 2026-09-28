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
- **Tools.** A nil `Policy` allows every bound local and external tool. To
  ask a person first, see [approving tool calls](#approving-tool-calls).
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

## Approving tool calls

A command-line program can ask inside the policy: `AuthorizeTool` prompts, then
allows or denies, all within one `Run`; a denial's `Reason` is what the model
sees ([example](../../examples/approval/main.go)). A server that cannot wait
defers instead. `dashi.RequireApproval("deploy")` allows local tools and defers
the named ones; the turn stops with `StopWaiting`, and each pending item builds
its own answer ([example](../../examples/durable_approval/main.go)):

```go
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

`Approve` and `Deny` bind the answer to the exact arguments shown and the
policy revision that deferred the call. `Result(outcome)` reports the result of
a waiting external call the same way. Each builder derives its command ID from
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
turn ID, or a `Deliver` recorded twice.

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
`dashi.ErrNoOutput`, naming the stop, when the turn has no output. The output fact includes tool-call or final-text provenance;
see [structured final results](../agent-turns.md#structured-final-results).

## Streaming a turn

`Runner.Stream` is the iterator form of `Runner.Run`. It enables provider
streaming, yields acknowledged steps and provisional deltas in order, then one
final `Outcome`. Breaking the range interrupts the turn by default; reload
its record and call `Continue` when ready. Set
`Runner.OnContextDone: ContextCancel` when a disconnect should instead end
the turn as canceled. The iterator is backpressured; `Runner.Observer`
remains a separate, lossy display hook. Ranging the same turn sequence again
returns `dashi.ErrStreamConsumed`.

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
its queue, and steps and deltas evict queued progress when the queue is full.
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
under a context you control, using `Runner.Run` or `Stream`. Your
`Recorder.Commit` is the natural place to write what clients read: in the same
transaction that appends the step, write your projection or outbox row. Clients
then follow your committed record, not the runner. Each step carries a gap-free
`Seq` per turn, so a client reconnects with the last `Seq` it applied and
receives everything after it. A disconnect ends only that follow.

```go
func (r *pgRecorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	return r.db.InTx(ctx, func(tx Tx) error {
		// Check ExpectedSeq and Owner, append req.Step, then in the same tx:
		return tx.InsertEvent(req.Step.TurnID, req.Step.Seq, project(req.Step))
	})
}

// GET /turns/{id}/events?after=N: replay rows with seq > N, then poll
// (optionally woken by a notify) until the request context ends.
```

`Runner.Observer` is lossy: it drops events, including steps, when its queue
of 64 events fills; tool progress is dropped first. Use it for in-process display hints such as text deltas, never as a
delivery channel. Treat deltas as previews that the committed model step
supersedes.

## Runtime reminders

For typed recorded context, see
[runtime reminders](../../design/runtime-reminders.md).

## Open gates

The [Dive comparison and open gates](../agent-loop-review.md) record what
remains to prove before stabilizing the loop API.
