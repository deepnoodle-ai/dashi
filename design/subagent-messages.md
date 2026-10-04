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
- **The handle.** Per task, `Tasks` holds `session.Session{ID: child, Runner:
  childRunner, Exclusive: true}`. Turn 1 is `Start{Parent: &ref, Agent:
  childAgent, Input: prompt}` with `New`. `Exclusive` holds because one
  `Tasks` is the only driver of its children. No new `Session` field.
- **Later turns.** A message to an idle or ended child starts its next turn
  with `Start{CommandID: m.ID, Origin: StartScheduled, Parent: &<sending
  call>, Input: [m's reminder]}`. A `Start` already closes a stopped turn
  (`closeLast`), so a completed, failed, limited, or canceled child takes it.
  `closeLast` refuses a busy turn (`waiting`, or a `result_unknown` call), so
  `task_send` to such a child is a tool error that says why, before any
  send. `Recover` resumes an `interrupted` child. The command ID puts the
  message ID in the child's record.
- **Running.** A goroutine that `Tasks` owns ranges over `child.Stream`. Each
  turn's final event (`Event.Result`) ends that turn: `Tasks` updates the view
  and sends its notice. The goroutine exits when the call returns. A message
  to a child with no goroutine starts one with the `Start` above. Steers that
  miss a turn stay in the child session's queue and run as the queue turn
  after it (`queue:["msg:…"]`).
- **Views.** `TaskView.ChildID` becomes `SessionID`; `TurnID` names the
  current or last child turn, and `Turns` counts them. `State` is the last
  turn's state, `running` while a turn runs. `Spend`, `ModelCalls`, and
  `ToolCalls` sum every child turn; `Reply` and `Reason` are the `TurnID`
  turn's. A `TaskEnded` view names the turn that ended.
- **Spend by starting call.** A child turn's starting call is its `Parent`
  (the `task` or `task_send` call), or, for a queue turn, the call named by
  the first message ID in its key. `Report(parent)` counts each child turn,
  of the tasks the parent turn's calls name, whose starting call is in that
  parent turn: child turn 2, started from parent turn 7, counts in turn 7.
  Track 2 finds child sessions through `task` calls only; track 3 adds the
  sessions that `task_send` calls name, and the queue-turn rule, since only
  its steers make child queue turns.
- **Stop.** `Tasks.Stop` cancels the running turn only; a stopped child
  takes follow-ups. `Recover` never resumes a canceled turn. The doc
  comments of `Stop`, `Recover`, and `Report` change with track 2.
- **Notices** are per child turn: ID `task-done:<child turn ID>`. `OnTask`
  gets `TaskStarted` for each child turn and `TaskEnded` for each end.
  `Notice.Item()` is the `task-ended` reminder, from `task t_k3f2 (explore)`
  (`taskSender` strips what `From` forbids and cuts it to 64 bytes), cut to
  fit the 64 KiB reminder limit. Compaction keeps reminders with a `From` in
  the summarizer's input, since nobody supplies agent mail again; the
  transcript writes each as an item with its sender and content. It still
  drops the others.
- **`MaxTaskTurns`** (default 10; negative means none) bounds the turns of one
  child session. `Tasks` checks it wherever it starts a child turn.
  `task_send` past it is a tool error: "task t_k3f2 has run 10 turns; start
  a new task".

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
    From     string   // "parent" or "task t_k3f2 (explore)", as the reminder shows it
    To       string   // "parent" or a task ID
    Text     string
    Delivery Delivery
}

func MessageID(turnID, callID string) string // "msg:" + turnID + "/" + callID
func (m Message) Item() dashi.ContextItem    // origin ContextAgent, a reminder
func (m Message) Steer() dashi.Steer         // never Urgent

const TaskMessage TaskEventKind = "message" // track 4; TaskEvent.Message is set

type Config struct {
    // existing fields
    Session      *session.Session // Tasks delivers to it; nil: route from OnTask
    MaxMessages  int            // per sending turn and recipient; zero = 8
    MaxTaskTurns int            // per task; zero = 10, negative = none
}
```

**Routing.** `Tasks.deliver(to, m)` first checks the recipient's delivered
IDs (see "Delivery once"). To the parent, it calls `SteerItem(m.Steer())` or
`EnqueueItem(m.Item())` on `Config.Session`, as notices do. To a child, it
calls `SteerItem` on a running turn, and otherwise starts the next turn with
the `Start` above. A queue message to a running child goes to the child
session's own queue (`EnqueueItem`), which runs it after the turn as a queue
turn; further messages steer into that turn. The child session's steering
source is a `*dashi.Inbox` that `Tasks` owns, and its `Delivered` names the
message IDs its record holds and those `Tasks` dropped, so its queue never
runs one twice. When the session's queue does not follow a turn (it failed or
was limited), `Tasks` starts the next turn with the first pending message once
the child's goroutine exits. A message that starts a turn returns once the
child's record holds that turn, as `task` does, and so does a message that
arrives while that turn starts. A turn that fails before the record holds it
is no turn, and each `task_send` that waits on it returns the error. Mail
sent with success earlier, while another turn ran, is never dropped without
a word: when the turn it starts fails, it counts as not read on the turn it
waited on. That turn's notice waits until the next turn's record holds its
first step, or the child's goroutine exits, so it can say so. A child that
`Tasks.Stop` is stopping refuses messages until it has stopped.

| Recipient | `steer` | `queue` |
| --- | --- | --- |
| Running | Before its next model call, or at a candidate final reply | After the turn ends, as the next turn |
| Idle or ended | Starts a turn (`StartScheduled`) | Same |
| Waiting, result unknown | Tool error; not sent | Same |

`MaxTaskTurns` applies to every `task_send`, a steer into a running turn
included, since a steer that misses that turn starts one more.

Without `Config.Session`, `OnTask` gets `TaskMessage` and `TaskEnded` events,
and the host calls the same two methods. A `Config.Session` that refuses a steer
(a host `Session.Steering`, or a compaction turn) gets the item on its queue. A
message is pending until delivered, so `Tasks.Wait` and `RunUntilQuiet` wake on
it. Mail is never held for a recipient that cannot read it. A steer accepted
while a child ran, whose turn then stops `waiting` or `result_unknown` before it
reads the steer, is **undeliverable**: a final state, not pending mail. So
is one whose turn `Tasks.Stop` canceled, since a stop should not restart the
child; `TaskView.UnreadMessages` counts them. The
notice for that turn says "your message was not read", and the parent may send
it again once the child can take it.

**Parent wakes.** A child turn's first message that finds the parent idle
starts a parent queue turn. Its later messages steer into a running parent
turn, or wait in `Tasks` and join its notice at the end. So a task starts at
most `MaxTaskTurns` parent turns by message, plus one notice per turn.

As built (track 4): `Tasks` reads "running" from `Config.Session.Open`
(`Next == NextRunning`); a state it cannot read counts as running, so the
message goes out at once. A message that races the turn's end lands in the
queue, as any steer that misses its turn does. Held messages go out just
before the notice, so one queue turn carries both. The parent is never an
undeliverable recipient: its session queue holds mail behind a `waiting`
turn until the host resolves it, as it holds notices. `Recover`'s
redeliveries do not wait for a notice.

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
when it ends."). Tool errors: unknown task (lists known IDs), empty text, text
over 16 KiB, a `waiting` or `result_unknown` child (says why, suggests a new
task), `MaxTaskTurns`, and `MaxMessages` (per parent turn and task).

The `message_parent` description says when to use it: a finding that changes
the parent's plan, a blocker, or a question the child can work around; all
else goes in the reply. A question it cannot work around ends its turn with
the question (PRD decision 7). A child gets `message_parent` and none of the
`task*` tools; the child toolset (`childTools`) adds it, bound to that task.
A restricted child records it in `Agent.Tools`. A subagent with `NoTools`
goes without, and so does every child when neither `Config.Session` nor
`Config.OnTask` is set: nothing could deliver the message, so `Config{}`
offers no tool that cannot work. A call that arrives with no route anyway is
a tool error, never "Sent to your parent." A message held for the notice returns "Sent to your
parent. It reads it with your final answer." A completed notice ends with
"It can take a follow-up: reply with task_send." (UC-5).

**Waits.** `wait_seconds` on `task` and `task_status` returns when the task
(any task, without `task_id`) ends a turn or sends a steer message, when the
time is up, or when the call's context ends. The result omits the content
and says "A message from t_k3f2 follows" only when `Tasks` steered it into
`Config.Session` successfully before the `changed` channel closed; the next
model call then holds it once. Otherwise (no `Config.Session`, a failed
steer) the result holds the text, and its `Data` (`delivered_message_ids`)
marks the message delivered, so a queue turn drops it. A `task` wait that
returns the final result records `delivered_turn_id`, as `task_status`
does. A plain read still returns the reply.

## Sender tags and authority (track 1)

- **Rendering.** A message is `llm.ReminderMessage(Reminder{Name:
  "agent-message", Tier: contextual, From: from, Content: text})`. A notice
  uses the name `task-ended`, with its current text as content. `From` is
  `parent` or `task t_k3f2 (explore)`; the helper that builds it strips
  forbidden characters from the name and truncates to 64 bytes.
- **`llm.Reminder.From`** (new, `json:"from,omitempty"`): at most 64 bytes of
  printable UTF-8 with no quote or angle bracket. `FormatReminder` writes it as
  `from="…"` after `name`.
- **Escaping.** `FormatReminder` replaces `<` with `&lt;` where content holds
  `<system-reminder` or `</system-reminder`, ASCII case folded. The record
  keeps the original text; the `render_reminder` change notes the escape in
  its reason, and `llm.ReminderEscapes` reports it. This applies to every
  reminder, so no reminder content can close its tag or open one.
- **Not escaped: tool results and user text** (PRD decision 4). A coding
  agent must see a file's bytes. A look-alike tag there stays inside its
  block, and no view, event, or record counts it as mail. Track 1 tests a
  tool result with a forged tag.
- **Who sets `From`.** The tool executor, from its binding: `task_send` sends
  as `parent` to the task named in the arguments; `message_parent` is built
  per task and sends as that task. `Tasks` checks that `CurrentCall` is a turn
  of the expected session, parent or child, and fails the call otherwise.
- **`ContextAgent`** (new origin, `"agent"`): content another agent in this
  session tree sent. User role only, contextual reminders only, with a
  sender: `validateContext` requires exactly one contextual reminder with a
  non-empty `From`, so live input, steering, the inbox, hooks, and replay
  agree. Queue turns it starts are `StartScheduled`.

## Delivery once (tracks 3 and 4)

The one rule extends from notices to messages: an item is delivered when the
**recipient's** record holds its ID, in a turn's context or in a `queue:`
start. The sender's record is the source: a finished `task_send` or
`message_parent` call holds the text, the delivery, and, by its turn and call
IDs, the message ID.

- **Delivered IDs.** `Tasks` keeps, per recipient, the message IDs its
  record holds: IDs in turns' context, child turns' `msg:` command IDs, and
  IDs in `queue:` keys. `deliver` adds an ID when it sends. A rerun send
  builds the same ID, finds it in this set (or in the in-memory inbox or
  queue), and the tool treats it as sent. The set, not the session's
  in-memory queue, is what survives a restart.
- **Target turn.** A finished send records, in its `tool.Outcome.Data`
  (`session_id`, `target_turn_id`, `last_turn_id`), the recipient turn it
  steered into, or none when it started or queued a turn, and the child's
  last turn when it arrived while the child ran. A message whose target turn,
  or a turn from its last turn on, has ended without its ID, and stopped
  `waiting`, `result_unknown`, or `canceled`, is undeliverable. So a queue
  message never restarts a child that `Tasks.Stop` canceled before reading
  it. Ended turns never change, so every reader and every restart reaches the
  same result. A hook that replaces the result drops the `Data`; `Recover`
  then finds the task by `task_id`, and takes as the last turn the last child
  turn that a parent call before the send started (by its `Start.Parent`, or
  the first message in a queue turn's key), by the order of the calls in the
  parent turns. `Report` misses that session unless a `task` call of the same
  parent turn names it.
- `Recover(ctx, runner, turns)` runs in this order (as built, notices come
  before the resume, as they did before track 3, and undeliverable messages
  are sorted out first so the notice can name them): build the delivered-ID
  sets from the parent turns and each child session; then resume interrupted
  child turns, whose rerun sends now see those sets; then for each finished
  `task_send` in the parent turns, redeliver it if the child lacks its ID;
  for each finished `message_parent` in the child turns, redeliver it if the
  parent lacks its ID (as built, before the notices, so both reach one
  queue turn); then notices, as today. The parent's set comes from
  `DeliveredMessages(turns)`, which also backs the parent session's
  `Delivered`. Each redelivery goes through `deliver`, so it follows the
  table. A resumed child gets its messages as its turn resumes, before that
  turn can end, so the turn's end handles them like any mail. `Recover`
  skips an undeliverable message and never sends to a `waiting` or
  `result_unknown` recipient, so it neither fails nor retries on one; the
  message appears as "not read" in the notice. A redelivery that fails for
  another reason, such as a store that refuses the turn, is an error that
  `Recover` returns after the rest; the next `Recover` tries again. A
  redelivery passes `MaxTaskTurns`, as it passes the other caps.
- Crash tests: before send, after send, after delivery, "delivered, call
  not finished, turn resumed" (the resumed turn reruns the call), and
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

`/tasks t_k3f2` prints each turn's reply with the messages between them.
This rides on the pending CLI task wiring (item 13).

As built: the CLI keeps its existing task lines, so the start and end lines
are the `task` tool line and its `⎿` status line, not `▸` and `◂`. A later
turn's status line reads `↳ task t_k3f2 (explore) turn 2 …`. The marker sits
after the task ID: `→ t_k3f2 (queued)  …`, so the terminal edge never cuts
it. Message lines come from the parent's record, so a reopened session shows
the same lines: `→` replaces the tool line of a `task_send` that succeeded,
and `←` shows where the parent got the message, in a steer or a queue turn's
start. A `←` line has no `(queued)`; the parent's record does not say how it
was sent. `/tasks t_k3f2` shows it.

```text
  ✓ task  general · Check the exact effect · t_r4eigy             ok  0.0s
    ⎿  completed · 2 model calls · 1 tool call · /task t_r4eigy to inspect
  → t_r4eigy (queued)  Yes, check env overrides too
  ↳ task t_r4eigy (general) turn 2 completed · 3 model calls · …
  ← t_r4eigy  Parse is in config/load.go; env overrides too?
```

The CLI sets `Config.Session` to its session, like any client, so `Tasks`
delivers notices and messages. Its task driver runs a queue turn only for
queued items that did not come from the user (`session.State.QueuedUser`),
since the user's queued prompts wait for the next prompt.

## Alternatives considered

- **Tasks holds child `Inbox`es and runs single turns** (no child sessions).
  Smaller for running children, but a finished child could only be reached
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
  unfinished (a crash inside `task_send`)? Leaning no: the rerun of the
  idempotent call sends it.
- Does the child need a Dashi-written line such as "sent while you were
  working", as Claude Code adds? Leaning no; test with the live suite.
