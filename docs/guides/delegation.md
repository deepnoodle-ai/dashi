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
| `task_status` | Gives the state of a task, or its answer when it ended. `wait_seconds` (0 to 60) waits for the end. Without `task_id`, it lists all tasks. |
| `task_stop` | Stops a running task. |

A `Tasks` value owns the running children. The host makes one for each
conversation. The wiring is short:

```go
inbox := dashi.NewInbox()
tasks := delegate.NewTasks(delegate.Config{
	OnTask: func(e delegate.TaskEvent) {
		if e.Kind == delegate.TaskEnded {
			inbox.Send(e.Task.Notice().Steer()) // the result reaches the agent
		}
	},
})
defer tasks.Close(ctx)
runner.Tools = tool.Join(runner.Tools, tasks.Tools())
out, err := runner.Run(ctx, turn, start, dashi.WithSteering(inbox))
```

[`examples/delegate`](../../examples/delegate) is a complete program. It also
shows the follow-up turn (see "When a result arrives").

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
every tool, including one a toolset adds later. A child never receives the three task tools, so a child cannot start a
child. It sees only its prompt, so the model must put every fact in the
prompt.

## The link between parent and child

Each child has its own record. `dashi.Start.Parent` names the parent turn and
tool call, and `TurnView.Parent` shows it. The child's turn ID comes from
`delegate.ChildID(parentTurnID, callID)`. The task ID (`delegate.TaskID`) is
a fixed function of the child ID, so it is the same after a reopen.

## When a result arrives

A finished task calls `Config.OnTask` once with `TaskEnded`. The host decides
what to do. `TaskView.Notice()` gives the message: `Text()` for display and
`Steer()` for delivery. The steer has origin `ContextTool`, so it is never
user or operator text.

There is one delivery rule: **a result is delivered when the conversation's
record holds the notice** (the context item `task-done:<child ID>`). A
`task_status` read is not delivery, so the model that reads a result still
gets the notice. Two cases:

- **A turn runs.** Send the steer to the turn's `dashi.Inbox`. The next model
  call sees it. A turn records an ID once, so a repeat cannot double.
- **No turn runs.** The steer waits in the inbox. Read it with
  `Inbox.Leftovers(turn)` and split the list with `delegate.IsNotice`: user
  steers keep the [steering](steering.md) rule, and notices go to a follow-up
  turn. `delegate.FollowUp(agent, notices)` returns the `Start` for one turn
  that carries all waiting notices, with the agent you pass, usually the
  conversation's. `delegate.FollowUpTurnID(notices)` gives its turn ID. Set
  `History` on the start. The start is scheduled
  work and claims no user authority. Interactive hosts should raise
  `Limits.ModelAttempts`.

## Waiting for results

`tasks.Wait(ctx)` blocks until a task has been reported through `OnTask`
(it returns false) or nothing is pending (it returns true). A task is pending
until `OnTask` returns for its end, so a notice that `OnTask` sends is in the
inbox when `Wait` returns. Loop: read `Leftovers`; run a follow-up turn if a
notice waits; otherwise call `Wait`; stop when `Wait` returns true and no
notice waits. One goroutine calls `Wait`.

Cost read while tasks run is a lower bound. It grows until each task ends.
Read the cost of each task from its `TaskEnded` event.

## Crash, quit, and reopen

`Tasks.Close(ctx)` interrupts the running children and waits for their
records. It does not call `OnTask`. To reopen, make a new `Tasks` and call
`Recover(ctx, runner, turns)` with the turns of the conversation:

| Child record | `Recover` |
| --- | --- |
| No stop, or interrupted | Resumes the same child. Settled model calls do not run again. `OnTask` gets `TaskStarted`. |
| Ended, and the record holds no notice | `OnTask` gets `TaskEnded`, once. |
| Ended, and the record holds the notice | Nothing. |
| Canceled | Not resumed. |

`Recover` needs a recorder that implements `dashi.Loader` (`MemoryRecorder`
and `filerecorder` do). Without one it returns an error, and the tool logs a
warning: tasks then run but do not survive a restart, and a resumed parent
reports the `task` call as uncertain.

Call `Recover` on reopen. The order against resuming the parent does not
matter: a rerun `task` call that finds an ended child reports it too, once.

The `task` call returns only after the record holds the child. A parent that
crashes inside the call finds the same child when the runner runs the call
again, so a rerun starts no second child.

## Stop and cancel

Ctrl-C on a running turn leaves tasks running. Stop one with the `task_stop`
tool or `Tasks.Stop(ctx, id)`. The child records `canceled`, and `Recover` does not
resume it. The states of a task are `running` and the Dashi stop words:
`completed`, `failed`, `limited`, `refused`, `canceled`, `interrupted`,
`waiting`, and `uncertain`. `Tasks.List()` returns a `TaskView` for each task
with state, tool calls, cost, and the answer.

## Answers, budgets, and spend

`MaxAnswer` caps the answer in bytes (default 16 KiB; negative means no cap).
A child keeps its own `Limits` and `Budget`. A parent `Budget` does not cap a
child, and the end of the parent turn does not stop it.

Each turn's `Spend` covers its own model calls. `delegate.Report` adds the
children of a parent turn:

```go
report, err := delegate.Report(ctx, recorder, out.TurnView)
fmt.Println(report.Inclusive.Cost, report.Children.Cost) // maps by currency
```

`Spend.Add` keeps `Cost` and `ToolCost` separate, and a resumed child counts
once. A follow-up turn is an ordinary turn with its own spend.

## Approvals for child tool calls

Child tool calls pass through the hooks of the parent runner, also while no
turn runs. The hook sees the child's `TurnView`: `Parent` is set, and
`Agent.ID` is the subagent name. Several children can run at once, so the
prompt must handle parallel calls, and it must answer inline. A hook that
defers a child's call ends the task in state `waiting`.

## Showing the child's work

`Config.OnChild` receives every child event: committed steps, model deltas,
and tool progress, with the parent link and the subagent name. It runs on the
child's observer goroutine and must return quickly.
