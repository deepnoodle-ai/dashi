# Subagent messages: tech spec

**Status:** Proposed, 2026-09-29. **Workflow:** spec, then review, then build.
**PRD:** [subagent messages](roadmap-research/13-subagent-messages-prd.md).
**Decisions:** [42 to 44](decisions.md). **Extends:**
[background subagents](background-subagents.md),
[session input](session-input.md). Terms are in the
[glossary](ubiquitous-language.md).

## Shape

A task becomes a **child session** that `delegate.Tasks` drives. Mail is a
context item put on the recipient's session with calls that exist:
`SteerItem` into a running turn; to the parent otherwise `EnqueueItem`; to
an idle child, a `Start` of its next turn. Nothing new carries mail.

```text
parent Session ──task_send──▶ Tasks ─────SteerItem/Start─────▶ child Session
      ▲                         │                                  │
      └──SteerItem/EnqueueItem──┘◀──message_parent, turn ends──────┘
```

## Child sessions (track 2)

- **IDs.** `ChildID(parentTurn, call)` now names the child session. Its turns
  are `TurnID(child, n)`: `<child>-1`, `<child>-2`. `TaskID` hashes the
  session ID, so it is unchanged in form and stable across reopen.
- **The handle.** Per task, `Tasks` holds `dashi.Session{ID: child, Runner:
  childRunner, Exclusive: true}`. Turn 1 is `Start{Parent: &ref, Agent:
  childAgent, Input: prompt}` with `New`. `Exclusive` holds because one
  `Tasks` is the only driver of its children. No new `Session` field.
- **Later turns.** A message to an idle or ended child starts its next turn
  with `Start{CommandID: m.ID, Origin: StartScheduled, Parent: &<sending
  call>, Input: [m's reminder]}`. A `Start` already closes a stopped turn
  (`closeLast`), so a completed, failed, limited, or canceled child takes it.
  `closeLast` refuses a busy turn (`waiting`, or an `uncertain` call), so
  `task_send` to such a child is a tool error that says why, before any
  send. `Recover` resumes an `interrupted` child. The command ID puts the
  message ID in the child's record.
- **Running.** A goroutine that `Tasks` owns ranges over `child.Stream`. Each
  turn's `Outcome` event ends that turn: `Tasks` updates the view and sends
  its notice. The goroutine exits when the call returns. A message to a child
  with no goroutine starts one with the `Start` above. Steers that miss a
  turn stay in the child session's queue and run as the queue turn after it
  (`queue:["msg:…"]`).
- **Views.** `TaskView.ChildID` becomes `SessionID`; `TurnID` names the
  current or last child turn, and `Turns` counts them. `State` is the last
  turn's state, `running` while a turn runs. `Spend` sums every child turn.
- **Spend by starting call.** A child turn's starting call is its `Parent`
  (the `task` or `task_send` call), or, for a queue turn, the call named by
  the first message ID in its key. `Report(parent)` counts each child turn,
  of the tasks the parent turn's calls name, whose starting call is in that
  parent turn: child turn 2, started from parent turn 7, counts in turn 7.
- **Stop.** `Tasks.Stop` cancels the running turn only; a stopped child
  takes follow-ups. `Recover` never resumes a canceled turn. The doc
  comments of `Stop`, `Recover`, and `Report` change with track 2.
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
    MaxMessages  int            // per sending turn and recipient; zero = 8
    MaxTaskTurns int            // per task; zero = 10, negative = none
}
```

**Routing.** `Tasks.deliver(to, m)` first checks the recipient's delivered
IDs (see "Delivery once"). To the parent, it calls `SteerItem(m.Steer())` or
`EnqueueItem(m.Item())` on `Config.Session`, as notices do. To a child, it
calls `SteerItem` on a running turn, and otherwise starts the next turn with
the `Start` above; a queue message to a running child waits in `Tasks` until
that turn ends, then starts the next one. Further messages steer into it.

| Recipient | `steer` | `queue` |
| --- | --- | --- |
| Running | Before its next model call, or at a candidate answer | After the turn ends, as the next turn |
| Idle or ended | Starts a turn (`StartScheduled`) | Same |
| Waiting, uncertain | Tool error; not sent | Same |

Without `Config.Session`, `OnTask` gets `TaskMessage` and `TaskEnded` events,
and the host calls the same two methods. `Config.Session` needs the handle's
own inbox; with a host `Session.Steering`, route from `OnTask`. A message is
pending until delivered, so `Tasks.Wait` and `RunUntilQuiet` wake on it.
Mail is never held for a recipient that cannot read it. A steer accepted
while a child ran, whose turn then stops `waiting` or `uncertain` before it
reads the steer, is **undeliverable**: a final outcome, not pending mail. The
notice for that turn says "your message was not read", and the parent may
send it again once the child can take it.

**Parent wakes.** A child turn's first message that finds the parent idle
starts a parent queue turn. Its later messages steer into a running parent
turn, or wait in `Tasks` and join its notice at the end. So a task starts at
most `MaxTaskTurns` parent turns by message, plus one notice per turn.

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
when it ends."). Tool errors: unknown task (lists known IDs), empty text,
text over 16 KiB, a `waiting` or `uncertain` child (says why, suggests a new
task), `MaxTaskTurns`, and `MaxMessages` (per parent turn and task).

The `message_parent` description says when to use it: a finding that changes
the parent's plan, a blocker, or a question the child can work around; all
else goes in the answer. A question it cannot work around ends its turn with
the question (PRD decision 7). A child gets `message_parent` and none of the
`task*` tools; `withoutTaskTools` becomes a child toolset that adds it, bound
to that task.

**Waits.** `wait_seconds` on `task` and `task_status` returns when the task
(any task, without `task_id`) ends a turn or sends a steer message, when the
time is up, or when the call's context ends. The result omits the content
and says "A message from t_k3f2 follows" only when `Tasks` steered it into
`Config.Session` successfully before the `changed` channel closed; the next
model call then holds it once. Otherwise (no `Config.Session`, a failed
steer) the result holds the text. A plain read still returns the answer.

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
  This applies to every reminder, so no reminder content can close its tag
  or open one.
- **Not escaped: tool results and user text** (PRD decision 4). A coding
  agent must see a file's bytes. A look-alike tag there stays inside its
  block, and no view, event, or record counts it as mail. Track 1 tests a
  tool result with a forged tag.
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

- **Delivered IDs.** `Tasks` keeps, per recipient, the message IDs its
  record holds: IDs in turns' context, child turns' `msg:` command IDs, and
  IDs in `queue:` keys. `deliver` adds an ID when it sends. A rerun send
  builds the same ID, finds it in this set (or in the in-memory inbox or
  queue), and the tool treats it as sent. The set, not the session's
  in-memory queue, is what survives a restart.
- **Target turn.** A settled send records, in its tool result's details, the
  recipient turn it steered into, or none when it started or queued a turn.
  A message whose target turn has ended without its ID, and stopped
  `waiting` or `uncertain`, is undeliverable. Ended turns never change, so
  every reader and every restart reaches the same outcome.
- `Recover(ctx, runner, turns)` runs in this order: build the delivered-ID
  sets from the parent turns and each child session; then resume interrupted
  child turns, whose rerun sends now see those sets; then for each settled
  `task_send` in the parent turns, redeliver it if the child lacks its ID;
  for each settled `message_parent` in the child turns, redeliver it if the
  parent lacks its ID; then notices, as today. Each redelivery goes through
  `deliver`, so it follows the table. `Recover` skips an undeliverable
  message and never sends to a `waiting` or `uncertain` recipient, so it
  neither fails nor retries on one; the message appears as "not read" in
  the notice.
- Crash tests: before send, after send, after delivery, "delivered, call
  not settled, turn resumed" (the resumed turn reruns the call), and
  "steered into a running child that then stops `waiting` before reading
  it" (`Recover` completes, sends nothing, and the notice says "not read").
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
- **A `Session.Unattended` field** that lets the queue run after a stop
  nobody answers. Not needed: `Tasks` starts the next turn with its own
  `Start`, which closes a stopped turn already, and the command ID records
  the message ID for free.

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
