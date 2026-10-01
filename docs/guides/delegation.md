# Delegating to subagents

A parent agent can hand a focused task to a child agent. The child does its
own model calls and tool calls in a separate turn. The task runs in the
background: the parent gets a task ID at once and keeps working. The result
comes later, as a message, or when the parent asks for it. A long search does
not stop the parent, and it does not fill the parent's context.

The `delegate` package provides three tools:

| Tool | What it does |
| --- | --- |
| `task` | Starts a subagent. Returns `Started task t_k3f2 ...` at once. |
| `task_status` | Gives the state of a task, or its reply when it ended. `wait_seconds` (0 to 60) waits for the end. Without `task_id`, it lists all tasks. |
| `task_stop` | Stops a running task. |

A `Tasks` value owns the running children. The host makes one for each
conversation, usually a [session](sessions.md). The wiring is short:

```go
s := &session.Session{Runner: runner}
tasks := delegate.NewTasks(delegate.Config{Session: s}) // results reach s
defer tasks.Close(ctx)
s.Runner.Tools = tool.Join(runner.Tools, tasks.Tools())
out, err := s.Send(ctx, "Who owns billing, and who is on call for search?")
out, err = delegate.RunUntilQuiet(ctx, s, tasks) // results that came later
```

[`examples/delegate`](../../examples/delegate) is a complete program; it
runs with no API key, on scripted clients.

A child inherits from the runner that runs the `task` call, at the time of the
call: the client, recorder, and hooks. It also takes the parent turn's current
model, which a tool reads with `dashi.CurrentTurn(ctx)`, unless the `Subagent`
sets its own. `Config.Runner` overrides the client, recorder, or observer for
the rare host that wants a child to differ. A parent agent that lists
`Agent.Tools` must name the task tools in that list to use them.

## Subagents

`Config.Subagents` is the list the model chooses from. If it is empty, the
tool offers two:

- `Explore` reads and searches. It uses only tools whose `ReadOnly` annotation
  is true. A tool with no annotation is left out; mark your own tools with
  [`tool.ReadOnly()`](tools.md#read-only-and-idempotent-tools).
- `General` can use every tool the parent agent may use.

A `Subagent` holds a name, a description for the model, and a `dashi.Agent`
(instructions, model, limits, and tools) that becomes the child's
`Start.Agent`. Its `Agent.Tools` names the tools of the runner's `Tools` that
the child may use, even ones the parent agent did not select, so a parent that
only delegates can give a worker `write`. A subagent with no `Agent.Tools`
draws only from the tools the parent agent may use, so the parent's
`Agent.Tools` or `Agent.NoTools` limits it too. `ReadOnly` applies in both
cases. When a rule restricts the child, `delegate` resolves it at child start
into explicit names in the child's recorded `Agent.Tools` (`NoTools` if none
are left). A resumed child uses that record, not the subagent's current config,
and a tool a dynamic toolset adds after the child starts is not offered to it.
A child with no rule (no named tools, a parent that may use every tool, and
not `ReadOnly`) records empty `Agent.Tools`, so, like the parent, it may use
every tool, including one a toolset adds later. A child never receives the
three task tools, so a child cannot start a child. It sees only its prompt, so
the model must put every fact in the prompt.

## The link between parent and child

Each task runs in a child session, a `session.Session` that `Tasks` drives. Its
ID comes from `delegate.ChildID(parentTurnID, callID)`, and its turns are
`<child>-1`, `<child>-2`, and so on, each with its own record. A child turn's
`dashi.Start.Parent` names the parent turn and tool call that started it, and
`TurnView.Parent` shows it. The task ID (`delegate.TaskID`) is a fixed
function of the session ID, so it is the same after a reopen.

## When a result arrives

Each child turn that ends calls `Config.OnTask` once with `TaskEnded`. With
`Config.Session` set, `Tasks` first puts the turn's notice on that session
itself; without it, the host decides what to do. `TaskView.Notice()` gives
the message: `Text()` for display, and `Item()` and `Steer()` to send it to
the conversation.
The item is a `task-ended` reminder from the task
(`from="task t_k3f2 (explore)"`) with origin `ContextAgent`, so it is never
user or operator text.

There is one rule: **a result has reached the parent when the conversation's
record holds the notice** (the context item `task-done:<child turn ID>`), or
holds a `task_status` result that gave the model the same text. A
`task_status` call on an ended task returns the notice's text, so the model
that reads a result there gets no notice afterwards, and the `TaskEnded`
event has `Delivered` set; a host that routes notices from `OnTask` skips
those, and a host that holds notices until a turn ends checks
`Notice.DeliveredIn(turns)` first. A read of a running task, or of the task
list, delivers nothing. Two cases:

- **A turn runs.** `Session.SteerItem` sends the steer to the running turn.
  The next model call sees it. A turn records an ID once, so a repeat cannot
  double.
- **No turn runs.** `SteerItem` queues the notice instead, as
  `Session.EnqueueItem` does. A notice is not user input, so it never joins
  the user's next message: it runs as a **queue turn** of its own, with origin
  `StartScheduled`, which claims no user authority. The queue turn's command ID
  is `queue:` and the JSON list of the item IDs it carries, and that also
  counts as the notice reaching the record. A steer that missed its turn moves
  to the queue too. Interactive hosts should raise `Limits.ModelCalls`.

A host that drives turns with `Runner.Run` and its own `dashi.Inbox` can still
send the steer to the inbox; notices left over when no turn runs need a turn
of their own, which the session's queue provides.

## Waiting for results

`delegate.RunUntilQuiet(ctx, s, tasks)` runs the session's queue and waits
while tasks run. It returns when no task is pending and the queue is empty,
with the last turn's result, or when a turn stops with anything but
`NextNone`, which needs the host first. The same `Send` also runs a queue turn
by itself when a notice arrives before its turn ends.

Underneath, `tasks.Wait(ctx)` blocks until a child turn has been reported
through `OnTask` (it returns false) or nothing is pending (it returns true). A
turn is pending until `OnTask` returns for its end, so a notice that `Tasks`
or `OnTask` puts on the session is there when `Wait` returns. One goroutine
calls `Wait`.

Cost read while tasks run is a lower bound. It grows until each task ends.
Read the cost of each task from its `TaskEnded` event.

## Crash, quit, and reopen

`Tasks.Close(ctx)` interrupts the running children and waits for their
records. It does not call `OnTask`. To reopen, make a new `Tasks` and call
`Recover(ctx, runner, turns)` with the turns of the conversation, which a
session's `s.Turns(ctx)` yields:

| Child turn record | `Recover` |
| --- | --- |
| Last turn, no stop or interrupted | Resumes that turn. Finished model calls do not run again. `OnTask` gets `TaskStarted`. |
| Ended, and the record holds no notice | `OnTask` gets `TaskEnded`, once for each such turn. |
| Ended, and the record holds the notice | Nothing. |
| Canceled | Not resumed. |

`Recover` needs a recorder that implements `dashi.Loader` (`MemoryRecorder`
and `filerecorder` do). Without one it returns an error, and the tool logs a
warning: tasks then run but do not survive a restart, and a resumed parent
reports the `task` call's result as unknown.

Call `Recover` on reopen. The order against resuming the parent does not
matter: a rerun `task` call that finds an ended child reports it too, once.

The `task` call returns only after the record holds the child. A parent that
crashes inside the call finds the same child when the runner runs the call
again, so a rerun starts no second child.

## Stop and cancel

Ctrl-C on a running turn leaves tasks running. Stop one with the `task_stop`
tool or `Tasks.Stop(ctx, id)`. The running child turn records `canceled`, and
`Recover` does not resume it; the child session keeps its history. The states
of a task, those of its last turn, are `running` and the Dashi stop words:
`completed`, `failed`, `limited`, `refused`, `canceled`, `interrupted`,
`waiting`, and `result_unknown`. `Tasks.List()` returns a `TaskView` for each
task with state, tool calls, cost, and the reply.

## Replies, budgets, and spend

`MaxReply` caps the reply in bytes (default 16 KiB; negative means no cap).
A child keeps its own `Limits` and `Budget`. A parent `Budget` does not cap a
child, and the end of the parent turn does not stop it.

Each turn's `Spend` covers its own model calls. `delegate.Report` adds the
child turns that a parent turn's calls started:

```go
report, err := delegate.Report(ctx, recorder, out.TurnView)
fmt.Println(report.Inclusive.Cost, report.Children.Cost) // maps by currency
```

`Spend.Add` keeps `Cost` and `ToolCost` separate, and a resumed child turn
counts once. A child turn counts under the parent turn whose call started it
(its `Parent`). `TaskView.Spend` sums every turn of the task. A queue turn is
an ordinary turn with its own spend.

## Approvals for child tool calls

Child tool calls pass through the hooks of the parent runner, also while no
turn runs. The hook sees the child's `TurnView`: `Parent` is set, and
`Agent.ID` is the subagent name. Several children can run at once, so the
prompt must handle parallel calls, and it must decide inline. A hook that
holds a child's call for approval ends the task in state `waiting`.

## Showing the child's work

`Config.OnChild` receives every child event: committed steps, model deltas,
and tool progress, with the parent link and the subagent name. It runs on the
child's observer goroutine and must return quickly.
