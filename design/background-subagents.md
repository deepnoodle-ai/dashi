# PRD: background subagents

| Field | Content |
| --- | --- |
| Status | Revised after review, 2026-09-28 |
| Author | Background-tasks track lead |
| Roadmap | Item 13, second slice. Replaces the blocking `task` in the [13 PRD](roadmap-research/13-child-turns-prd.md) |
| Demo | Wednesday 2026-09-30. Ship by Tuesday night |

## Problem

Today the `task` tool blocks. The parent agent waits while the child works,
so a long search stops all other work. The user cannot ask for two things at
once. Ctrl-C on the parent also cancels the child. The user said: "The agent
should not block on the agent he is delegating to. He should be told later,
or call a function to check status and get a result."

Without this work, subagents save context but never save the user time.

## Goals and measures

1. Starting a subagent returns at once with a task ID. The parent Turn goes on.
2. Each result reaches the conversation once: as a message in the running
   Turn, or in a follow-up Turn when no Turn runs.
3. The model can check status, wait for a result, and stop a task.
4. A crash, a quit, or a reopen loses no task and no result.
5. Ctrl-C on the running Turn does not stop tasks.

Measures (all are tests): `task` returns before the child ends; a result
reaches a running Turn once; a result with no running Turn reaches the host
once; a kill between the end and the notice still gives one notice after
reopen; `task_stop` ends the child; spend counts once.

Guardrails: a Turn that never calls `task` records as before. Zero `Config`
works. Users: the `dashi` CLI user, and any Dashi host with long delegated work.

## What the user meets

```text
> refactor Parse, and find where config is parsed
  ▸ started t_k3f2  explore  find where config is parsed
  edit_file  parse.go                                   ok  0.1s
      · t_k3f2  grep "func Parse"
  ◂ t_k3f2 explore completed (5 tool calls, $0.02)
  (the running Turn now reads the result and uses it)
```

No Turn running:

```text
  ◂ t_k3f2 explore completed (5 tool calls, $0.02)
  ◂ t_m9a1 explore completed (3 tool calls, $0.01)
The parser is in config/load.go, function Parse. ...
[turn 4 | follow-up for 2 tasks | $0.006]  total $0.41 (tasks $0.12)
```

`/tasks` shows one line per task: ID, subagent, state, tool calls, cost,
label. State words are the Dashi stop words: running, completed, failed,
limited, refused, canceled, interrupted. `/tasks ID` prints the full result.
`/tasks stop ID` stops one task. The status line shows "2 tasks running".

## Model-facing tools

Three tools. Status and result are one tool: a model that checks status also
wants the result when it exists.

| Tool | Arguments | Result |
| --- | --- | --- |
| `task` | `subagent` (enum), `prompt`, `description` | `Started task t_k3f2 (explore): <description>. It runs in the background. A message "[task t_k3f2 completed]" arrives when it ends. Keep working. Do not poll.` |
| `task_status` | `task_id` (optional), `wait_seconds` (0 to 60, default 0) | Running: state, tool calls, cost so far. Ended: state and answer, cut at `MaxAnswer`. No `task_id`: one line per task. `wait_seconds` without `task_id` waits for the next task to end, then lists all. |
| `task_stop` | `task_id` | `Stopped task t_k3f2.` For an ended task: `Task t_k3f2 already completed.` |

`wait_seconds` holds the tool call open. The context of the call ends it
early. An unknown ID is a tool error that lists the known IDs.

The task ID is `t_` plus the first six base-32 characters of the SHA-256 of
the child ID. It is a fixed function of the child ID, so it is the same
after reopen and needs no counter. `Tasks` never changes an ID. If two
children in one conversation gave the same ID (about 1 in 10^9), the second
`task` call returns a tool error and the model retries.

## Public API

```go
// package delegate. New(Config) tool.Toolset is removed.
// Tasks belongs to one conversation, which the host owns.
type Tasks struct{ /* host-owned */ }

func NewTasks(cfg Config) *Tasks
func (t *Tasks) Tools() tool.Toolset // task, task_status, task_stop
func (t *Tasks) List() []TaskView
func (t *Tasks) Stop(id string) error // cancels the child; final
func (t *Tasks) Close(ctx context.Context) error // interrupts children, waits
// Recover rebuilds tasks from the Turns the host holds for the conversation
// and resumes unfinished ones. It calls OnTask for each ended task whose
// notice the record does not hold.
func (t *Tasks) Recover(ctx context.Context, r dashi.Runner, turns []dashi.Turn) error

type Config struct { // existing fields kept
    OnTask func(TaskEvent) // the host decides how a result reaches the agent
}
type TaskEvent struct{ Kind TaskEventKind; Task TaskView } // started, ended
// TaskState is "running", or the child's dashi.StopKind: completed, failed,
// limited, refused, canceled, interrupted. No other names exist.
type TaskState string
type TaskView struct {
    ID, Subagent, Description string
    Parent     dashi.CallRef // the parent Turn's task call that started it
    State      TaskState
    ModelCalls, ToolCalls int
    Spend      dashi.Spend  // live for a running task
    Answer, Reason string
}

func (v TaskView) Notice() Notice
func (n Notice) Text() string       // "[task t_k3f2 completed] explore: ..."
func (n Notice) Steer() dashi.Steer // origin ContextTool, ID NoticeID(child)
func NoticeID(childID string) string // "task-done:" + childID
func IsNotice(s dashi.Steer) bool    // origin ContextTool and the ID prefix
func FollowUpTurnID(ns []Notice) string // stable for a set of notices
```

`Config.Runner`, `MaxAnswer`, `OnChild`, `Report`, `ChildID`, `Subagent`,
`CurrentCall`, `CurrentRunner`, `Spend.Add`, and the `Loader` rule stay as in
PR 77. "Parent" always means the parent Turn. A notice goes to the Turn that
runs now, or to a new follow-up Turn, not to the parent Turn as such.

## Main flows

**Start.** `task` reads `CurrentCall` and `CurrentRunner`, computes the child
ID, and asks `Tasks` to start it. If `Tasks` holds that child, it returns the
same task ID, so `task` stays idempotent after a crash. If not, `Tasks`
copies the running runner (client, recorder, hooks, model), starts a
goroutine under its own context, and runs `Runner.Run` with `Start.Parent`.
The tool call settles at once. The child does not depend on the tool call or
on the parent Turn.

**Delivery rule.** A result is delivered when the record of the
conversation holds the notice with ID `task-done:<child ID>`, in a Turn's
context or in the input of a follow-up Turn. Nothing else counts. A
`task_status` read does not suppress a notice. `Recover` and the live path
use this one check.

**Running Turn.** When a child ends, `Tasks` calls `OnTask`. The CLI calls
`inbox.Send(notice.Steer())`. The steer has origin `ContextTool` and is never
operator or user text. The next model call sees it. Dashi records an ID once
per Turn, so a repeat cannot double. A steer that a run claimed but did not
record is released and delivered again (test).

**Leftovers.** `IsNotice` splits `Inbox.Leftovers`. User steers follow the
existing steering rule and become the next user Turn. Notices never become
user text. They go to the next Turn that runs, or to a follow-up Turn.

**No Turn running.** The CLI shows "◂ t_k3f2 explore completed". It starts
ONE follow-up Turn at once, with all waiting notices as input. The Turn uses
`Start{Origin: StartScheduled}` and `FollowUpTurnID`. It claims no user
authority and carries the history. The user's half-typed line stays in the
input box. A task that ends during this Turn is a steer into it. The CLI
raises `Limits.ModelAttempts` for follow-up Turns, as the steering guide
advises for interactive hosts.

**Crash or reopen.** Each `task` call in a parent Turn links to its child by
`ChildID`. On reopen the host loads its Turns and calls `Recover`. For each
`task` call whose child exists, `Recover` registers the task and resumes a
child with no stop or an `interrupted` stop. For an ended child it applies
the delivery rule and calls `OnTask` if the record lacks the notice. A
`task` call that crashed before it settled runs again and finds the same
child. Before the first prompt, the CLI lists resumed tasks and their cost.

**Stop.** Ctrl-C ends the running Turn and leaves tasks alone. `task_stop`,
`Tasks.Stop`, and `/tasks stop` cancel one child. It records `canceled`,
and `Recover` does not resume it. `/quit` and Ctrl-D call `Close`, which
interrupts running children so each stays resumable, and waits for their
records. The screen says "2 tasks interrupted. They resume when you reopen
this session."

## Requirements

- FR-1: `task` returns before the child ends, with the task ID and the text above.
- FR-2: Background is the only mode. The blocking path and its tests are removed.
- FR-3: `Tasks` owns the child goroutines and context. The end of the tool
  call or the parent Turn does not cancel a child.
- FR-4: `Tasks` calls `OnTask` once per end in one process. Without `OnTask`,
  results reach the model only through `task_status`.
- FR-5: A notice has origin `ContextTool` and the ID `task-done:<child ID>`.
  The delivery rule is the only test of delivery.
- FR-6: `task_status` returns state and answer of an ended task at any time.
  It does not affect delivery.
- FR-7: `Recover` needs a `Loader` recorder. Without one it returns an error
  that says so, and `task` warns once. Tasks then run but do not survive a restart.
- FR-8: `Close` interrupts children with `ContextInterrupt`. It never
  records a child as failed.
- FR-9: `task_stop` and `Tasks.Stop` record `canceled`. A canceled child is not resumed.
- FR-10: A child keeps its own `Limits` and `Budget`. A parent `Budget` does
  not cap a child. The end of the parent Turn does not stop it.
- FR-11: A child tool call uses the host hooks. The CLI approval prompt
  names the subagent and task ID, also while no Turn runs. This is a CLI
  task with its own test. The live run uses `explore`, which needs no approval.
- FR-12: `Report` counts each child once. The CLI total includes tasks.
- FR-13: `dashi -p` runs follow-up Turns until no task runs, then exits (CLI task).

## User stories

- **US-1 No blocking.**
  - [ ] A test holds a child at a gate. `task` returns and the parent
    finishes another tool call while the gate is closed.
- **US-2 Notice in a running Turn.**
  - [ ] A child ends during a parent model call. The next call holds one
    `ContextTool` item with the answer.
  - [ ] A steer that a run claimed but did not record is delivered once later.
- **US-3 Notice with no Turn running.**
  - [ ] `OnTask` fires once. Two tasks that end while idle give one
    follow-up Turn with both notices (`StartScheduled`).
  - [ ] `Leftovers` with one user steer and one notice: the steer becomes
    the next user Turn, and the notice never becomes user text.
- **US-4 Once.**
  - [ ] Kill the process after the child ends and before the notice.
    `Recover` sends one notice. A second `Recover` sends none.
  - [ ] A `task_status` read of the answer does not stop the notice.
- **US-5 Reopen.**
  - [ ] `Close` then `Recover` continues the same child. No settled model
    call repeats, and no second child exists.
  - [ ] A parent killed inside the `task` call, then resumed, holds one child.
  - [ ] The task ID is the same after reopen.
- **US-6 Stop.**
  - [ ] `task_stop` and `/tasks stop` end the child as `canceled`. Ctrl-C
    on the running Turn leaves tasks running.
- **US-7 Status.**
  - [ ] `task_status` shows counts and cost for a running task, the answer
    for an ended task, and honors `wait_seconds` and context end.
- **US-8 Spend.**
  - [ ] `Report` equals parent plus tasks, once each.

## Out of scope

- A task that outlives the process. A child runs in the `dashi` process. A
  separate worker is the external-settle slice in the 13 PRD.
- A message from the parent to a running child.
- Grandchildren. A child never gets `task`, `task_status`, or `task_stop`.
- A cap over parent and children together. `Report` only, as in the 13 PRD.
- A task list across conversations. `/tasks` shows the open one.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Hooks of the start command run after that Turn ends | A stale closure could misbehave | The copy holds the runner's hooks, not per-run state. A test runs approval while no Turn runs. |
| Model polls `task_status` | Wasted calls | The `task` result says "Do not poll". `wait_seconds` is bounded. |
| Follow-up Turns and resumed tasks cost money unasked | Surprise spend | The screen shows each end, resumed tasks, and cost. `/tasks stop` ends a task. Limits bound each child. |
| The CLI track is not finished | Approval and `-p` wiring slip | Ship the library slice first. FR-11 and FR-13 are CLI follow-ups. |

## Decisions

1. **Background is the only mode.** The user asked for it, and there are no users.
2. **A host-owned `Tasks` value owns children.** Their life must not depend
   on one tool call. "Task" is the word the user sees. The library does not
   use "session"; the host owns the conversation.
3. **Status and result are one tool.** `wait_seconds` covers "get result"
   with a 60 second cap. With no `task_id` it waits for the next end.
4. **The notice uses the existing steer path with origin `ContextTool`.** No
   new delivery code. The library gives `OnTask`, and the host decides.
5. **One delivery rule, from the record only** (coordinator direction; review
   finding 1 accepted). The notice ID in the record decides. A `task_status`
   read does not suppress a notice. A repeat costs little, and one rule is
   testable. Review open decision on a short form: rejected, for the same reason.
6. **Leftovers split by origin and ID prefix** (finding 1b accepted). User
   steers keep the steering rule. Notices go to the next or follow-up Turn.
7. **One follow-up Turn for all waiting notices** (finding 5, direction 3).
   A notice that arrives during that Turn is a steer into it. `ModelAttempts`
   is raised for such Turns.
8. **Idle rule: show it and start a follow-up Turn at once.** Waiting for the
   next prompt hides a result the user asked for. `StartScheduled` claims no
   user authority.
9. **Task ID is a fixed function of the child ID** (finding 4 accepted, by
   the coordinator's simpler rule). No growth on a clash; a clash is a tool error.
10. **Vocabulary** (findings 2 and 3 accepted). States are `running` plus
    Dashi's `StopKind` words, on screen and in the API. "Tool calls" replaces
    "steps". "Parent" means the parent Turn. No "session" in library text.
11. **No child index.** `Recover` walks the Turns the host holds.
12. **Close interrupts, reopen resumes.** The CLI shows resumed tasks and cost
    first (open decision accepted). `/tasks stop` ends a task.
13. **Ctrl-C leaves tasks running.** `/tasks stop` is the explicit stop.
14. **Full scope kept** (finding 6 partly accepted). Library first; live run
    with `explore`; FR-11 and FR-13 are CLI follow-ups with own tests.
15. **A child bounds itself with its own limits.** No cascade from the parent
    budget. A family cap needs a ledger (out of scope).

## Deliverables

`delegate` changes (Tasks, three tools, Recover, Close, notices), tests for
each story, an updated `examples/delegate` and `docs/guides/delegation.md`, a
live run on `gpt-6-luna`, and the `design/remaining-work.md` entry. CLI
wiring (`/tasks`, follow-up rule, status line, `Close`, `Recover`) is a short
adapter in `cmd/dashi` after the CLI PR merges.
