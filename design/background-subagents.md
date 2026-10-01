# PRD: background subagents

| Field | Content |
| --- | --- |
| Status | Implemented on `turns/background-subagents`, 2026-09-28 |
| Author | Background-tasks track lead |
| Roadmap | Item 13, second slice. Replaces the blocking `task` in the [13 PRD](roadmap-research/13-child-turns-prd.md) |
| Demo | Wednesday 2026-09-30 |

## Problem

The old `task` tool blocked. The parent agent waited while the child worked,
so a long search stopped all other work, and Ctrl-C on the parent also
cancelled the child. The user said: "The agent should not block on the agent
he is delegating to. He should be told later, or call a function to check
status and get a result."

## Goals and measures

1. `task` returns at once with a task ID. The parent Turn goes on.
2. Each result reaches the conversation once: in the running Turn, or in a
   follow-up Turn when no Turn runs.
3. The model can check status, wait for a result, and stop a task.
4. A crash, a quit, or a reopen loses no task and no result.
5. Ctrl-C on the running Turn does not stop tasks.

Each is a test (see "Test map"). A Turn that never calls `task` records as
before. A zero `Config` works.

## What the user meets

```text
> refactor Parse, and find where config is parsed
  ▸ started t_k3f2  explore  find where config is parsed
  edit_file  parse.go                                   ok  0.1s
      · t_k3f2  grep "func Parse"
  ◂ t_k3f2 explore completed (5 tool calls, $0.02)
  (the running Turn now reads the result and uses it)
```

With no Turn running, the CLI shows "◂ t_k3f2 explore completed" and starts one
follow-up Turn with all waiting results. `/tasks` lists one line per task: ID,
subagent, state, tool calls, cost, label. `/tasks ID` prints the result.
`/tasks stop ID` stops one. The CLI wiring is a later PR.

## Model-facing tools

| Tool | Arguments | Result |
| --- | --- | --- |
| `task` | `subagent` (enum), `prompt`, `description` | `Started task t_k3f2 (explore): <description>. It runs in the background. A message that starts with "[task t_k3f2" arrives when it ends. Keep working. Do not poll.` |
| `task_status` | `task_id` (optional), `wait_seconds` (0 to 60, default 0) | Running: state, tool calls, cost so far. Ended: state and answer. No `task_id`: one line per task; with `wait_seconds`, it waits for the next end. |
| `task_stop` | `task_id` | `Stopped task t_k3f2.` or `Task t_k3f2 already completed.` |

Status and result are one tool. The context of the call ends a wait early. An
unknown ID is a tool error that lists the known IDs. The task ID is `t_` plus
six base-32 characters of the SHA-256 of the child ID (`delegate.TaskID`). It
is the same after reopen and needs no counter. A clash (about 1 in 10^9) is a
tool error, and the model retries.

## Public API

```go
// package delegate. New(Config) tool.Toolset is removed.
func NewTasks(cfg Config) *Tasks            // one per conversation
func (t *Tasks) Tools() tool.Toolset        // task, task_status, task_stop
func (t *Tasks) List() []TaskView
func (t *Tasks) Stop(ctx context.Context, id string) error // ErrUnknownTask, ErrTaskEnded
func (t *Tasks) Wait(ctx context.Context) (idle bool, err error)
func (t *Tasks) Close(ctx context.Context) error
func (t *Tasks) Recover(ctx context.Context, r dashi.Runner, turns []dashi.Turn) error

type Config struct { /* existing fields kept */ OnTask func(TaskEvent) }
type TaskEvent struct{ Kind TaskEventKind; Task TaskView } // started, ended
type TaskState string // "running" or a dashi.StopKind word
type TaskView struct { ID, ChildID, Subagent, Description string; Parent dashi.CallRef
    State TaskState; ModelCalls, ToolCalls int; Spend dashi.Spend; Answer, Reason string }

func (v TaskView) Notice() Notice
func (n Notice) Text() string; func (n Notice) Steer() dashi.Steer
func NoticeID(childID string) string        // "task-done:" + childID
func RunUntilQuiet(ctx context.Context, s *session.Session, t *Tasks,
    opts ...dashi.RunOption) (dashi.RunResult, error)
```

`FollowUp` and `FollowUpTurnID` were replaced by the session's queue turns
([session-input.md](session-input.md#delegation)).

`ChildID`, `Report`, `OnChild`, `MaxReply`, `Subagent`, `CurrentCall`,
`CurrentRunner`, `Spend.Add`, and the Loader rule stay as in PR 77.
"Parent" means the parent Turn. A notice goes to the Turn that runs now, or to
a follow-up Turn.

## Main flows

**Start.** `task` reads `CurrentCall` and `CurrentRunner`, computes the child
ID, and asks `Tasks` to start it. `Tasks` copies the running runner, starts a
goroutine under its own context, and runs the child with `Start.Parent`. The
call settles once the record holds the child's first step, so a crash after
the call cannot lose a child the model heard about. If `Tasks` holds the
child, `task` returns the same ID, so it is idempotent. The child does not
depend on the tool call or the parent Turn.

**Delivery rule.** A result is delivered when the record of the conversation
holds the notice `task-done:<child ID>`: in a Turn's context, or in the `queue:`
`CommandID` of a session queue Turn's start. Nothing else counts. A
`task_status` read does not suppress a notice. `Recover` and the live path use
this check. `OnTask` fires once per end in one process; a second `Recover` sends
none.

**Running Turn.** On `TaskEnded`, the host sends `notice.Steer()` to the
`Inbox`. The steer has origin `ContextTool`, never user text. The next model
call sees it. A Turn records an ID once. A steer that a run claimed but did
not record comes back from `Leftovers`, once.

**No Turn running.** The notice waits in the session's queue and runs as a
queue Turn, one for all waiting notices, with `Origin: StartScheduled`. It
claims no user authority. A notice that arrives during that Turn is a steer
into it.

**Crash or reopen.** `Recover` walks the Turns the host holds, finds the
`task` calls, and loads each child by `ChildID`. It resumes a child with no
recorded stop or an `interrupted` stop, with the agent the child recorded.
For an ended child it applies the delivery rule and sends `TaskEnded` if the
record lacks the notice. A parent killed inside the `task` call reruns it and
finds the same child. `Recover` skips a child whose subagent the catalog lacks
and returns an error for it.

**Stop and close.** Ctrl-C ends the Turn and leaves tasks alone. `task_stop`,
`Tasks.Stop`, and `/tasks stop` cancel one child and record `canceled`;
`Recover` does not resume it. `Close` interrupts running children, waits for
their records, and sends no `TaskEnded`; the next `Recover` resumes them.

## Requirements

- FR-1: `task` returns before the child ends, with the ID and the text above.
- FR-2: Background is the only mode. The blocking path is removed.
- FR-3: `Tasks` owns the child goroutines. The end of the tool call or the
  parent Turn does not cancel a child.
- FR-4: `Tasks` calls `OnTask` once per end in one process. Without `OnTask`,
  results reach the model only through `task_status`. (Superseded by
  [decision 42](decisions.md): a task has many turns, and each child turn's
  end is reported once.)
- FR-5: A notice has origin `ContextTool` and the ID `task-done:<child ID>`.
  (Superseded by [decision 43](decisions.md): origin `ContextAgent`, a
  `task-ended` reminder, one notice per child turn.)
- FR-6: `task_status` returns state and answer of an ended task at any time.
- FR-7: `Recover` needs a `Loader` recorder and returns an error that says so.
  Without one, `Tasks` warns once, and tasks do not survive a restart.
- FR-8: `Close` records no child as failed.
- FR-9: A canceled child is not resumed. (Still true of the canceled turn;
  since [decision 42](decisions.md), a stopped child takes a follow-up as a
  new turn.)
- FR-10: A child keeps its own `Limits` and `Budget`. A parent `Budget` does
  not cap a child.
- FR-11 (CLI task): the approval prompt names the subagent and task ID, also
  while no Turn runs. The library passes host hooks to children, with a test.
- FR-12: `Report` counts each child once.
- FR-13 (CLI task): `dashi -p` runs follow-up Turns until no task runs.

## Test map

All are in `delegate/delegate_test.go`, run with `-race` and a goroutine-leak
check after each test.

| Story | Test |
| --- | --- |
| No blocking | `TestTaskReturnsBeforeChildEnds` |
| Notice in a running Turn, once | `TestNoticeReachesRunningTurnOnce` |
| Claimed, not recorded | `TestUnrecordedNoticeComesBackOnceFromLeftovers` |
| No Turn running, one follow-up, split | `TestNoticesWithNoRunningTurnMakeOneFollowUp` |
| Killed before the notice, once | `TestResultAfterKillReachesTheAgentOnce` |
| Status read, then notice | `TestStatusReadDoesNotSuppressTheNotice` |
| Close then Recover, same child | `TestCloseInterruptsAndRecoverResumesTheSameChild` |
| Killed inside `task` | `TestKilledInsideTaskCallHoldsOneChild` |
| Stop, Ctrl-C | `TestStopCancelsTheChildForGood`, `TestCtrlCOnTheParentLeavesTasksRunning` |
| Status, wait | `TestStatusShowsProgressAndWaits`, `TestStatusErrors` |
| Spend | `TestSpendCountsOnceAcrossReopen` |

## Out of scope

- A task that outlives the process (external settle, 13 PRD).
- A message from the parent to a running child. (Superseded: now in scope,
  both ways; see the [subagent messages
  PRD](roadmap-research/13-subagent-messages-prd.md) and decision 42.)
- Grandchildren: a child never gets the three task tools.
- A cap over parent and children together.
- A task list across conversations.

## Risks

| Risk | Mitigation |
| --- | --- |
| Hooks run after the start Turn ends | The copy holds the runner's hooks, not per-run state. A test runs approval with no Turn running. |
| Model polls `task_status` | The `task` result says "Do not poll". `wait_seconds` is bounded. |
| Follow-up Turns and resumed tasks cost money unasked | The screen shows each end and cost. Limits bound each child. |
| A record fault ends a task `failed` but leaves the child resumable | The next `Recover` resumes it, and a second notice may follow. Accepted. |

## Decisions

1. **Background is the only mode.** The user asked; there are no users.
2. **A host-owned `Tasks` owns children**, so their life does not depend on one
   tool call. The library does not use "session". (Superseded by
   [decision 42](decisions.md): each task runs in a child `session.Session`
   that `Tasks` drives.)
3. **Status and result are one tool**, with a 60 second wait cap.
4. **The notice uses the existing steer path**, origin `ContextTool`.
   Superseded in part by [decision 43](decisions.md): the steer path stays;
   the origin is `ContextAgent`.
5. **One delivery rule, from the record only.** A repeat costs little; one rule
   is testable.
6. **A notice is told apart** from other steers by origin and its `NoticeID`
   prefix. No core change. Superseded by [decision 43](decisions.md): a
   sender tag that Dashi sets, and origin `ContextAgent`.
7. **One follow-up Turn for all waiting notices**, `StartScheduled`.
   `FollowUp` records the delivered children in the start's `CommandID`.
   AddMessages cannot reach an ended Turn, so a follow-up Turn is the only way.
8. **Idle rule: show it and start a follow-up Turn at once.**
9. **Task ID is a fixed function of the child ID.**
10. **States are `running` plus `StopKind` words**, including `waiting` (a
    deferred approval) and `uncertain`. Both count as an end and send a notice.
11. **No child index.** `Recover` walks the Turns the host holds.
12. **Close interrupts, reopen resumes.** Children always run with
    `OnContextDone: ContextInterrupt`, whatever the parent uses, so Close is
    resumable. `Stop` then records `Cancel` itself.
13. **Ctrl-C leaves tasks running.**
14. **A child bounds itself** with its own limits.
15. **`task` waits for the child's first step** (added in implementation). A
    settled call would otherwise outlive a child that a crash never saved.
16. **`FollowUp` takes notice steers**, not views, so the CLI needs no map of
    ended tasks (changed in implementation).
17. **`Recover` reports an ended child once per process.** It skips the child
    if the record holds the notice, or if `OnTask` already reported it.
    Superseded by [decision 42](decisions.md): one report per child turn,
    keyed by the child turn ID.
18. **Reopen order does not matter** (implementation review). A `task` call
    that reruns and finds an ended child reports the end itself, as `Recover`
    would. The result says the task has ended. `Recover` after that skips the
    child. A rerun may repeat a notice that a follow-up Turn already
    carried, because `startTask` does not see the other Turns. That costs a
    repeat (see 5). A host still calls `Recover` on reopen: a settled `task`
    call is not run again, so only `Recover` finds those children.
19. **`Tasks.Wait(ctx)` is the one idle call.** A task is pending until
    `OnTask` returns for its end, so the gap between "ended" and "notice in
    the inbox" is closed. Wait returns false after each report and true when
    nothing is pending. The host reads its inbox after each return.
20. **`Stop` takes a context**, like `Close`.
21. **`Recover` resumes with the recorded agent**, as the rerun path does.
22. **Cost after a turn (finding 5): accepted, as a guide note.** A total read
    while tasks run is a lower bound. Hosts read `OnTask` for each task.
23. **Host wiring size (finding 6): accepted.** `Wait` shortens the loop. No
    further helper.
