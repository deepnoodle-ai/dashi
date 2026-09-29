# PRD: background subagents

| Field | Content |
| --- | --- |
| Status | Draft, 2026-09-28 |
| Author | Background-tasks track lead |
| Roadmap | Item 13, second slice. Replaces the blocking `task` in the [13 PRD](roadmap-research/13-child-turns-prd.md) |
| Demo | Wednesday 2026-09-30. Ship by Tuesday night |

## Problem

Today the `task` tool blocks. The parent agent waits while the child works,
so a long search stops all other work. The user cannot ask for two things at
once. Ctrl-C on the parent also cancels the child. The user said: "The agent
should not block on the agent he is delegating to. He should be told later,
or call a function to check status and get a result."

If we do nothing, subagents help only with context size. They do not save
the user any time.

## Goals and measures

1. Starting a subagent returns at once with a task ID. The parent turn goes on.
2. The result reaches the parent exactly once: as a message in the running
   turn, or as a follow-up turn when the parent is idle.
3. The model can check status, wait for a result, and stop a task.
4. A crash, a quit, or a reopen loses no task and no result.
5. Ctrl-C on the parent turn does not stop tasks.

Measures (all are tests): `task` returns before the child ends; a finished
task reaches a running turn once; a finished task with no running turn
reaches the host once; a kill between finish and notice still notifies once
after reopen; `task_stop` ends the child; spend counts once.

Guardrails: a turn that never calls `task` records exactly as before. Zero
`Config` works.

Users: the `dashi` CLI user (primary) and any Dashi host with long delegated work.

## What the user meets

```text
> refactor Parse, and find where config is parsed
  ▸ started t_k3f2  explore  find where config is parsed
  edit_file  parse.go                                   ok  0.1s
      · t_k3f2  grep "func Parse"
  ◂ t_k3f2 explore finished (5 steps, $0.02)
  (the running turn now reads the result and uses it)
```

Parent idle:

```text
> _
  ◂ t_k3f2 explore finished (5 steps, $0.02)
The parser is in config/load.go, function Parse.
[turn 4 | follow-up for t_k3f2 | $0.004]  total $0.41 (tasks $0.12)
```

`/tasks` shows one line per task: ID, subagent, state, steps, cost, label.
`/tasks ID` prints the full result. `/tasks stop ID` stops one task.
The status line shows "2 tasks running" while any task runs.

## Model-facing tools

Three tools. Status and result are one tool, because a model that checks
status also wants the result when it exists.

| Tool | Arguments | Result |
| --- | --- | --- |
| `task` | `subagent` (enum), `prompt`, `description` | `Started task t_k3f2 (explore): <description>. It runs in the background. A message "[task t_k3f2 finished]" arrives when it ends. Keep working. Do not poll.` |
| `task_status` | `task_id` (optional), `wait_seconds` (0 to 60, default 0) | Running: state, tool calls, cost so far. Finished: the answer, cut at `MaxAnswer`. No `task_id`: one line per task. |
| `task_stop` | `task_id` | `Stopped task t_k3f2.` A finished task: `Task t_k3f2 already finished.` |

`task_status` with `wait_seconds` holds the parent call open, up to the
limit. The parent's context ends it early. An unknown ID is a tool error that
lists the known IDs. The task ID is `t_` plus four base-32 characters of the
SHA-256 of the child ID. It grows by one character on a clash. No counter is
stored, so the ID is the same after reopen.

## Public API

```go
// package delegate. New(Config) tool.Toolset is removed.
type Tasks struct{ /* host-owned; one per session */ }

func NewTasks(cfg Config) *Tasks
func (t *Tasks) Tools() tool.Toolset // task, task_status, task_stop
func (t *Tasks) List() []TaskView
func (t *Tasks) Stop(id string) error // cancels the child; final
func (t *Tasks) Close(ctx context.Context) error // interrupts children, waits
// Recover rebuilds tasks from the session's parent turns and resumes the
// unfinished ones. It calls OnTask for each finished task not yet notified.
func (t *Tasks) Recover(ctx context.Context, r dashi.Runner, turns []dashi.Turn) error

type Config struct { // existing fields kept
    OnTask func(TaskEvent) // the host decides how a result reaches the parent
}
type TaskEvent struct { Kind TaskEventKind; Task TaskView } // started, finished
type TaskState string // running, completed, failed, stopped, interrupted
type TaskView struct {
    ID, Subagent, Description string
    Parent       dashi.CallRef // the task call
    State        TaskState
    ModelCalls, ToolCalls int
    Spend        dashi.Spend  // live for a running task
    Answer, Reason string
}

// Notice is the message for one finished task.
func (v TaskView) Notice() Notice
func (n Notice) Text() string        // "[task t_k3f2 finished] explore: ..."
func (n Notice) Steer() dashi.Steer  // ContextTool origin, ID "task-done:<child ID>"
func (n Notice) TurnID() string      // for a follow-up turn; stable
```

`Config.Runner`, `MaxAnswer`, `OnChild`, `Report`, `ChildID`, `Subagent`,
and the child link stay as merged in PR 77. The child ID, `CurrentCall`,
`CurrentRunner`, `Spend.Add`, and the `Loader` rule do not change.

## Main flows

**Start.** `task` reads `CurrentCall` and `CurrentRunner`. It computes the
child ID and asks `Tasks` to start it. If `Tasks` already holds that child,
it returns the same task ID. That keeps `task` idempotent after a crash. If
not, `Tasks` copies the running runner (client, recorder, hooks, model),
starts a goroutine under its own context, and runs `Runner.Run` with
`Start.Parent`. The tool call settles at once. The child is not tied to the
tool call or to the parent turn.

**Running parent.** When the child ends, `Tasks` calls `OnTask`. The CLI
calls `inbox.Send(notice.Steer())`. The steer has origin `ContextTool` and
the ID `task-done:<child ID>`. It is never operator or user text. Dashi
records an ID once per turn, so a repeat cannot double. The next model call
sees the message. If the turn ends first, `Inbox.Leftovers` returns it and
the idle rule below applies.

**Idle parent.** The CLI shows "◂ t_k3f2 explore finished". It starts a
follow-up turn at once with turn ID `notice.TurnID()` and `Start{Origin: StartScheduled}`
and the notice text as input. A scheduled start claims no user authority. The
turn uses the session history. The user's half-typed line stays in the
input box. Two finished tasks give two follow-up turns, in order. A user
prompt that arrives during a follow-up turn is a steer, as for any turn.

**Crash or reopen.** Each `task` call in the parent record links to its
child by `ChildID`. On reopen the CLI loads the session turns and calls
`Recover`. For each `task` call whose child exists, `Recover` registers the
task. It resumes a child that has no stop or was interrupted. For a finished
child it checks if the notice was delivered: a turn in the session holds the
`task-done` ID, or the turn `notice.TurnID()` exists. If not, `Recover` calls
`OnTask`, and the flows above run. A `task` call that crashed before it
settled runs again (idempotent) and finds the same child.

**Stop.** Ctrl-C ends the parent turn. It does not touch tasks. `task_stop`,
`Tasks.Stop`, and `/tasks stop` cancel one child. It records `canceled`. The
notice says "stopped". A stopped task is final and `Recover` does not resume
it. `/quit` and Ctrl-D call `Close`. `Close` interrupts running children, so
each stays resumable, waits for the records, and the screen says "2 tasks
paused. They resume when you reopen this session."

## Requirements

- FR-1: `task` returns within one tool call, before the child ends, with the
  task ID and the text above.
- FR-2: Background is the only mode. The blocking path and its tests are removed.
- FR-3: `Tasks` owns the child goroutines and their context. The parent
  tool call's context and the parent turn's end do not cancel a child.
- FR-4: `Tasks` calls `OnTask` once per finish in one process. Without
  `OnTask`, results reach the model only through `task_status`.
- FR-5: A finish notice has origin `ContextTool` and a stable ID. Delivery
  is once in the record, by the ID rules of Dashi.
- FR-6: `task_status` returns the answer of a finished task at any time. If
  the model reads it in the same process, the notice is not sent.
- FR-7: `Recover` needs a `Loader` recorder. Without one, `Recover` returns
  an error that says so, and `task` warns once at first use. Tasks then run
  but do not survive a restart.
- FR-8: `Close` interrupts children with `ContextInterrupt`. It never marks
  a child failed.
- FR-9: A child stopped by `task_stop` or `Tasks.Stop` records
  `StopCanceled` and is not resumed.
- FR-10: A child keeps its own `Limits` and `Budget`. A parent `Budget` does
  not cap a child, and the end of the parent does not stop it.
- FR-11: A background child tool call that needs approval uses the host
  hook. The prompt names the subagent and task ID. The CLI serializes
  prompts, including while the parent is idle.

## User stories

- **US-1 No blocking.** As a user, I ask for a search and a fix together.
  - [ ] A test holds a child at a gate. `task` returns, and the parent
    finishes another tool call while the gate is closed.
- **US-2 Notice in a running turn.** As a user, I see the agent use the
  result when it arrives.
  - [ ] A test finishes a child during a parent model call. The next call
    holds one `ContextTool` item with the answer.
- **US-3 Notice when idle.** As a user, I get the result with no prompt.
  - [ ] A test finishes a child after the parent turn. `OnTask` fires once.
    The CLI test shows a follow-up turn with `StartScheduled`.
- **US-4 Once.** As a host author, no result arrives twice or not at all.
  - [ ] A test kills the process after the child ends and before the notice.
    `Recover` sends one notice. A second `Recover` sends none.
  - [ ] A steer sent twice with the same ID records once.
- **US-5 Reopen.** As a user, my tasks survive quit and crash.
  - [ ] `Close` then `Recover` continues the same child. No settled model
    call repeats, and no second child exists.
  - [ ] A parent killed inside the `task` call, and then resumed, holds one
    child.
- **US-6 Stop.** As a user, I stop a task.
  - [ ] `task_stop` and `/tasks stop` end the child with `canceled`. Ctrl-C
    on the parent leaves tasks running.
- **US-7 Status.** As a model, I ask about a task.
  - [ ] `task_status` shows counts and cost for a running task, the answer
    for a finished task, and honors `wait_seconds` and context end.

## Out of scope

- A task that outlives the process. A child runs in the `dashi` process. A
  separate worker is later work (the external-settle slice in the 13 PRD).
- A message from the parent to a running child. `/tasks` shows the open session only.
- Grandchildren. A child never gets `task`, `task_status`, or `task_stop`.
- A family budget cap. Report only, as in the 13 PRD.
- Batching several notices into one follow-up turn.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Hooks from the start command run after that turn ends | A stale closure could misbehave | The copy holds the runner's hooks, not per-run state. The CLI approval is session-wide. A test runs approval while idle. |
| Model polls `task_status` in a loop | Wasted calls and cost | The `task` result says "Do not poll". `wait_seconds` gives a bounded wait. |
| Follow-up turns cost money unasked | Surprise spend | The screen shows the finish and the cost. `/tasks` lists spend. Limits bound each child. |
| Resume on reopen runs work the user forgot | Surprise spend | The screen lists resumed tasks. `/tasks stop` ends them. |

## Decisions

1. **Background is the only mode.** The user asked for it, and there are no users. One path is easier to test.
2. **A host-owned `Tasks` value owns children.** Their life must not depend
   on one tool call. "Tasks" is the word the user sees. A session holds one.
3. **Status and result are one tool, `task_status`.** Fewer tools, and a
   model that checks status wants the result if it exists. `wait_seconds`
   covers "get result" with a hard 60 second cap.
4. **The notice uses the existing steer path with origin `ContextTool`.** No
   new delivery code. Dashi's ID rule gives once-only delivery. The
   library does not decide the idle case. It gives `OnTask`, and the host
   decides.
5. **CLI rule for an idle parent: show it and start a follow-up turn at
   once.** The user asked to be notified. Waiting for the next prompt hides
   the result. The turn uses `StartScheduled`, so it claims no user
   authority.
6. **Derived task ID, no counter.** A counter needs storage. A hash of the
   child ID is the same after reopen and short enough for a model.
7. **No child index.** `Recover` walks the session's parent turns. No new `Recorder` method.
8. **Close interrupts and reopen resumes.** Killing work the user asked for
   loses money. A user who wants it gone runs `/tasks stop`.
9. **Ctrl-C leaves tasks running.** Ctrl-C ends the turn the user watches.
   `/tasks stop` is the explicit way to stop a task.
10. **`-p` mode waits.** `dashi -p` runs follow-up turns until no task
    runs, then exits. Otherwise the answer would be incomplete.
11. **Budget rule.** A child bounds itself with its own limits. The parent
    budget does not cascade. A family cap needs a ledger (out of scope).

## Deliverables

Changes to `delegate` (Tasks, three tools, Recover, Close, notices), tests
for each story, an update to `examples/delegate`, an update to
`docs/guides/delegation.md`, a live run on `gpt-6-luna` for the PR, and the
`design/remaining-work.md` entry. The CLI wiring (`/tasks`, follow-up rule,
status line, `Close` and `Recover` calls) is a short adapter in `cmd/dashi`
after the CLI PR merges.

## Open questions

None block the slice. Later: batching notices, and a task list across
sessions.
