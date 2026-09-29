# Subagent messages: tech spec

**Status:** Proposed, 2026-09-29. **Workflow:** spec, then review, then build.
**PRD:** [subagent messages](roadmap-research/13-subagent-messages-prd.md).
**Decisions:** [41 to 43](decisions.md). **Extends:**
[background subagents](background-subagents.md),
[session input](session-input.md). Terms are in the
[glossary](ubiquitous-language.md).

## Shape

A task becomes a **child session** that `delegate.Tasks` drives. Mail in both
directions is a context item put on the recipient's session with the calls
that exist: `SteerItem` for a steer, `EnqueueItem` for a queued message.
Nothing new carries mail; the session's inbox, queue, and dispatch do.

```text
parent Session ──task_send──▶ Tasks ──SteerItem/EnqueueItem──▶ child Session
      ▲                         │                                  │
      └──SteerItem/EnqueueItem──┘◀──message_parent, turn ends──────┘
```

## Child sessions (track 2)

- **IDs.** `ChildID(parentTurn, call)` now names the child session. Its turns
  are `TurnID(child, n)`: `<child>-1`, `<child>-2`. `TaskID` hashes the
  session ID, so it is unchanged in form and stable across reopen.
- **The handle.** Per task, `Tasks` holds `dashi.Session{ID: child, Runner:
  childRunner, Exclusive: true, Unattended: true}`. Turn 1 is
  `Start{Parent: &ref, Agent: childAgent, Input: prompt}` with `New`.
  `Exclusive` holds because one `Tasks` is the only driver of its children.
- **`Session.Unattended`** (new, package `dashi`): nobody answers this
  session's stops. The queue then also runs after a stop whose next action is
  not `NextNone`, closing that turn (`turn_closed`), unless the turn is busy
  or interrupted. A child with a waiting approval or an uncertain call keeps
  its mail queued until that clears; an interrupted child is resumed by
  `Recover`. Without the field, a limited or failed child could never take a
  follow-up, since the parent model cannot raise its limit.
- **Running.** A goroutine that `Tasks` owns ranges over `child.Stream`. Each
  turn's `Outcome` event ends that turn: `Tasks` updates the view and sends
  its notice. The goroutine exits when the call returns. A message to a child
  with no goroutine starts one that runs `child.RunQueue`.
- **Views.** `TaskView.ChildID` becomes `SessionID`; `TurnID` names the
  current or last child turn, and `Turns` counts them. `State` is the last
  turn's state, `running` while a turn runs. `Spend` and `Report` sum every
  child turn (`child.Turns`).
- **Notices** are per child turn: ID `task-done:<child turn ID>`. `OnTask`
  gets `TaskStarted` for each child turn and `TaskEnded` for each end.
- **`MaxTaskTurns`** (default 10; negative means none) bounds the turns of one
  child session. `task_send` past it is a tool error: "task t_k3f2 has run 10
  turns; start a new task".

## Agent messages

```go
// package delegate
type Delivery string

const (
    DeliverSteer Delivery = "steer" // before the recipient's next model call
    DeliverQueue Delivery = "queue" // when the recipient's turn ends
)

type Message struct {
    ID       string   // MessageID of the sending call
    From, To string   // "parent" or a task ID
    Text     string
    Delivery Delivery
}

func MessageID(turnID, callID string) string // "msg:" + turnID + "/" + callID
func (m Message) Item() dashi.ContextItem    // origin ContextAgent, a reminder
func (m Message) Steer() dashi.Steer         // never Urgent

const TaskMessage TaskEventKind = "message" // TaskEvent.Message is set

type Config struct {
    // existing fields
    Session      *dashi.Session // Tasks delivers to it; nil: route from OnTask
    MaxMessages  int            // per child turn; zero = 8, negative = none
    MaxTaskTurns int            // per task; zero = 10, negative = none
}
```

**Routing.** `Tasks.deliver(to, m)` calls `SteerItem(m.Steer())` for a steer
and `EnqueueItem(m.Item())` for a queued message, on the child session or on
`Config.Session`. `SteerItem` already queues when no turn runs, and a steer
that misses its turn moves to the queue, so no mail is lost.

| Recipient | `steer` | `queue` |
| --- | --- | --- |
| Running | Before its next model call, or at a candidate answer | After the turn ends, as the next turn |
| Idle or ended | Starts a turn (queue turn, `StartScheduled`) | Same |
| Waiting, uncertain | Held until it clears | Same |

Without `Config.Session`, `OnTask` gets `TaskMessage` and `TaskEnded` events,
and the host calls the same two methods. `Config.Session` needs the handle's
own inbox; with a host `Session.Steering`, route from `OnTask`. Pending
accounting is as today: a message is pending until delivered, so
`Tasks.Wait` and `RunUntilQuiet` wake on it.

## Tools (tracks 3 and 4)

| Tool | Who | Arguments | Result |
| --- | --- | --- | --- |
| `task` | parent | as today, plus `wait_seconds` (0 to 60) | as today; a wait that ends early says what follows |
| `task_send` | parent | `task_id`, `message`, `deliver` | `Sent to task t_k3f2 (running). It reads it before its next model call.` |
| `task_status` | parent | as today | a wait also ends on a steer message |
| `task_stop` | parent | as today | as today |
| `message_parent` | child | `message`, `deliver` | `Sent to your parent.` |

Other `task_send` results: queued behind a running turn ("It starts a new turn
with it after its current turn ends"), an ended child ("It starts turn 2 with
it and keeps its memory. A message that starts with "[task t_k3f2" arrives
when it ends."), and held ("It waits for an approval; it reads the message
when it continues"). Tool errors: unknown task (lists known IDs), empty text,
text over 16 KiB, `MaxTaskTurns`, `MaxMessages`.

The `message_parent` description says when to use it: a finding that changes
the parent's plan, a blocker, or a question the child can work around; all
else goes in the answer. A question it cannot work around ends its turn with
the question (PRD decision 7). A child gets `message_parent` and none of the
`task*` tools; `withoutTaskTools` becomes a child toolset that adds it, bound
to that task.

**Waits.** `wait_seconds` on `task` and `task_status` returns when the task
(any task, without `task_id`) ends a turn or sends a steer message, when the
time is up, or when the call's context ends. Delivery happens before the
`changed` channel closes, so the item is in the parent's inbox when the tool
returns, and the next model call holds it. A result that woke on mail omits
the answer and says "The result follows as a message", so the answer does not
arrive twice. A plain read (no wait) still returns the answer.

## Sender tags and authority (track 1)

- **Rendering.** A message is `llm.ReminderMessage(Reminder{Name:
  "agent-message", Tier: contextual, From: from, Content: text})`. A notice
  uses the name `task-ended`, with its current text as content. `From` is
  `parent` or `task t_k3f2 (explore)`.
- **`llm.Reminder.From`** (new, `json:"from,omitempty"`): at most 64 bytes of
  printable UTF-8 with no quote or angle bracket. `FormatReminder` writes it as
  `from="…"` after `name`.
- **Escaping.** `FormatReminder` replaces `<` with `&lt;` where content holds
  `<system-reminder` or `</system-reminder`, ASCII case folded. The record
  keeps the original text; the `render_reminder` change notes the escape.
  This applies to every reminder, so no text can close its tag or open one.
- **Who sets `From`.** The tool executor, from its binding: `task_send` sends
  as `parent` to the task named in the arguments; `message_parent` is built
  per task and sends as that task. `Tasks` checks that `CurrentCall` is a turn
  of the expected session, parent or child, and fails the call otherwise.
- **`ContextAgent`** (new origin, `"agent"`): content another agent in this
  session tree sent. User role only, contextual reminders only, like
  `ContextTool`. Queue turns it starts are `StartScheduled`.

## Delivery once (tracks 3 and 4)

The one rule extends from notices to messages: an item is delivered when the
**recipient's** record holds its ID, in a turn's context or in a `queue:`
start. The sender's record is the source: a settled `task_send` or
`message_parent` call holds the text, the delivery, and, by its turn and call
IDs, the message ID.

- Both tools are idempotent. A rerun after a crash builds the same ID. The
  recipient's inbox or queue refuses a repeat, which the tool treats as sent,
  and a turn records an ID at most once.
- `Recover(ctx, runner, turns)` runs in this order: resume interrupted child
  turns; then for each settled `task_send` in the parent turns, redeliver it
  if the child session lacks its ID; for each settled `message_parent` in the
  child turns, redeliver it if the parent turns lack its ID; then notices, as
  today. Each redelivery goes through `deliver`, so it follows the table.
- A message the in-memory queue held when the process died is rebuilt this
  way, so the child sessions' `memory_only` warnings do not apply to mail.

## CLI

Under a task line, one line per message, first line of text, cut to the
width. `(queued)` marks a queue message. A child turn after the first shows
`▸ t_k3f2 turn 2`.

```text
  ▸ started t_k3f2  explore  find where config is parsed
  → t_k3f2  Skip vendor/; the parser is in config/
  ← t_k3f2  Parse is in config/load.go; env overrides too?
  ◂ t_k3f2 explore completed (5 tool calls, $0.02)
```

`/tasks t_k3f2` prints each turn's answer with the messages between them.
This rides on the pending CLI task wiring (item 13).

## Alternatives considered

- **Tasks holds child `Inbox`es and runs single turns** (no child sessions).
  Smaller for running children, but a finished child could only be answered
  by a new task, and `Tasks` would rebuild history and queue logic that
  `Session` has.
- **A mailbox store keyed by recipient** (Claude Code teams, Codex V2). A
  second store beside the records; both products hit races there. Redelivery
  from the sender's record needs no store.
- **Queue-only messages that do not wake** (Codex `trigger_turn`). See PRD
  decision 3.
- **A new `input_required` stop.** See PRD decision 7.
- **An `Unattended` rule inside `delegate` only.** `Tasks` cannot close a
  stopped turn and run queued items in one step without the session's
  reservation logic, so the switch belongs on `Session`.

## Tradeoffs

- A child's turn IDs change and a task has many turns: every `TaskView` reader
  and `Report` changes. No users yet.
- Every message may start a paid turn. The caps bound it; no FYI mode yet.
- `Recover` reads each child session in full to find sent messages. Fine at
  tens of turns; a child index would help past that.
- Escaping changes rendered text for every reminder. It changes the prompt
  cache key only for content that held a tag.

## Open questions

- Should `Recover` also redeliver a message whose sending call is still
  unsettled (a crash inside `task_send`)? Leaning no: the rerun of the
  idempotent call sends it.
- Does the child need a Dashi-written line such as "sent while you were
  working", as Claude Code adds? Leaning no; test with the live suite.
