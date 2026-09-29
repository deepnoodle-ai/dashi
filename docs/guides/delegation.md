# Delegating to subagents

A parent agent can hand a focused task to a child agent. The child does its
own model calls and tool calls in a separate turn. The parent gets only the
child's final answer, so a long search does not fill the parent's context.

The `delegate` package provides one ready tool, `task`. The model calls it
with a subagent name and a prompt. The tool runs a child turn to its end and
returns the child's answer as the tool result.

```go
tools = tool.Join(tools, delegate.New(delegate.Config{OnChild: draw}))
runner := dashi.Runner{Client: client, Recorder: recorder, Hooks: hooks, Tools: tools}
```

A child inherits from the runner that runs the `task` call, at the time of the
call: the client, recorder, hooks, and `OnContextDone` value. The host can
change any of them for the parent, for example a new recorder when a user
reopens a session, and the children follow. `Config.Runner` is an optional
override for the rare host that wants a child to differ.

A child also uses the parent's current model, unless its `Subagent` sets a
model. If the user changes the model, the next child uses the new one.

[`examples/delegate`](../../examples/delegate) is a complete program.

## Subagents

`Config.Subagents` is the list the model chooses from. If it is empty, the
tool offers two:

- `Explore` reads and searches. It uses only tools whose `ReadOnly` annotation
  is true. A tool with no annotation is left out, so an MCP tool is available
  only when its server declares it read-only.
- `General` can use every parent tool.

A `Subagent` holds a name, a description for the model, a `dashi.Agent` (its
instructions, model, and limits), and an optional list of allowed tool names.
Set `Agent.Limits` to bound the child; the package has no timeout setting of
its own. The child never receives the `task` tool, so a child cannot start a
child. It sees only the prompt and its own instructions, not the parent's
conversation, so the model must put every fact in the prompt.

## The link between parent and child

Each child has its own record. `dashi.Start.Parent` names the parent turn and
tool call, and `TurnView.Parent` shows it on every read. The link cannot
change after `Start`: a second `Start` with another parent is a command
conflict. Dashi does not check that the parent call exists.

The child's turn ID comes from `delegate.ChildID(parentTurnID, callID)`. The
host stores nothing. To find a child, load it by that ID.

## Retries and crashes

If the recorder implements `dashi.Loader` (`MemoryRecorder` and `filerecorder`
do), `task` is marked idempotent. After a crash, the runner runs the call
again, and the tool finds the child by its ID:

| Child record | The retry |
| --- | --- |
| Not found | Starts the child. |
| Started or interrupted | Continues the same child. Settled model calls do not run again. |
| Completed | Returns the saved answer. No model call runs. |
| Failed, limited, refused, or canceled | Returns the saved error. The child does not run again. |

Without a `Loader` recorder, `task` still runs the child on the first call.
This includes a nil host recorder. `task` is not marked idempotent. When the
parent resumes, the runner reports the `task` call as uncertain. No second
child starts. The host must then reconcile the call with `Settle`.

The tool learns the recorder only when it runs. Without a `Loader`, it logs
one warning at its first call. If `Config.Runner.Recorder` is set and is not a
`Loader`, `New` warns at once. Use a `Loader` recorder that the host keeps
between runs, so that a crash resumes the same child.

## Structured output

If a subagent's agent has an `OutputSchema`, `task` returns the child's
structured value as JSON text.

A child tool with side effects can still run twice after a crash, under the
runner's usual rule for a tool that has started and has no result.

## Cancel and interrupt

The child runs under the tool's context. When the parent's context ends, the
child stops too. What its record shows follows the `OnContextDone` value of
the parent runner, which the child inherits:

- `ContextCancel`: the child records `canceled`.
- `ContextInterrupt` (default): the child records an interruption, and the
  `task` call stays open. The parent stops as interrupted. When the host
  continues the parent, the same child continues only when the recorder is a
  `dashi.Loader`. Without a `Loader`, the resumed `task` call is uncertain and
  no child restarts (see "Retries and crashes").

For an interactive program where Ctrl-C ends the session, set `ContextCancel`
on the parent runner. The child follows.

## Answers and errors

`MaxAnswer` caps the answer in bytes (default 16 KiB; negative means no cap).
A cut answer ends with a note that says so. The call settles as an error, with
the stop reason in its text, when the child stops `failed`, `limited`,
`refused`, or `canceled`. The model sees the error and can decide what to do.

## Approvals for child tool calls

Child tool calls pass through the hooks of the parent runner, so one approval
prompt covers both. The hook sees the child's `TurnView`: `Parent` is set, and
`Agent.ID` is the subagent name, so a prompt can say "explore wants to run: go
test". Two children can run at once, so the prompt must handle parallel calls.

The prompt must answer inline, inside the hook. If a hook defers a child's
call, the child stops `waiting`. This host cannot answer it, so the `task`
call settles as an error. With a `Loader` recorder, the child stays
resumable by its ID.

## Showing the child's work

`Config.OnChild` receives every child event: committed steps, model deltas,
and tool progress. Each `ChildEvent` carries the parent link and the subagent
name. The parent's `Observer` does not receive child events. An `Observer` on
`Config.Runner` also gets each event, after `OnChild`. The callback runs on
the child's observer goroutine, so it must return quickly.

## Spend

Each turn's `Spend` covers its own model calls. A parent's `TurnView.Spend`
does not include its children. `delegate.Report` adds them:

```go
report, err := delegate.Report(ctx, recorder, out.TurnView)
fmt.Println(report.Inclusive.Cost, report.Children.Cost) // maps by currency
```

`SpendReport` has `Parent`, `Children`, and `Inclusive`. `Spend.Add` keeps
`Cost` and `ToolCost` separate, and keeps the counts of estimated, unknown,
and partly unpriced calls. A resumed child counts once. `Report` finds
children through the parent's `task` calls. A child that a host started under
its own ID is not counted.

`Budget` still applies to one turn. A cap over parent plus children needs a
host ledger; see [Turn budgets](budgets.md).
