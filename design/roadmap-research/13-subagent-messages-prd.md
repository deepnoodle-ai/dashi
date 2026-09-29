# Messages between a parent agent and its subagents — PRD

**Status:** Draft, 2026-09-29. **Roadmap:** [item
13](../remaining-work.md#13-child-turns-and-delegation), third slice.
**Basis:** [research](../subagent-communication-research.md) and its evidence
in `tmp/research/subagent-comms/`. **Builds on:**
[background subagents](../background-subagents.md),
[session input](../session-input.md), [sessions](../sessions.md). **Tech
spec:** [subagent-messages.md](../subagent-messages.md). **Decisions:**
[41 to 43](../decisions.md). **Size:** L. **Risk:** public API, concepts, UI.

## Problem

A coding or research agent delegates to subagents, and each task runs in the
background. Today the two sides can speak only twice: the parent's prompt at
the start and the child's answer at the end. So:

- The parent learns mid-task that a child searches the wrong place, and cannot
  say so. It can only stop the child and start a new one with no memory.
- A child that is blocked, or has found the key fact, keeps silent until it
  ends, and the parent works on without it.
- A child that finishes cannot take a follow-up. "Now also check the tests"
  needs a new task that rereads every file.
- A child that must ask a question can only guess, or end with a question the
  parent cannot answer back into.

The user asked for this directly (brief, requirements 1 to 7). Claude Code,
Codex, and openclaw let a parent message a running child; Codex also lets a
child message its parent. Without this slice, Dashi's delegation is "prompt
in, answer out", which the research found to be the weak end of the field.

## Context

What exists: `delegate.Tasks` runs each child in the background; the tools
are `task`, `task_status` (with a wait of up to 60 seconds), and `task_stop`.
An ended task sends a **notice** to the parent: a steer into the running turn,
or a queue turn when the parent is idle. The record decides delivery once
(`task-done:<ID>`), so a restart loses and repeats nothing. A child is one
turn, run with `Runner.Run`; it has no inbox and no session. The CLI does not
show tasks yet.

Peers (research table): every product delivers mail at a boundary, after tool
results and before the next model call. None inserts text mid-call; only
Codex cut a model call for mail, and it made that opt-in. Codex tags mail with
its author; Claude Code resumes a stopped child from its transcript.

## Users and job

**Primary:** a Go developer who builds a coding or research agent that
delegates. Job: keep the parent working while children work, redirect a child
mid-task, and hear progress or questions from a child without polling.
**Secondary:** the CLI user who watches the parent and its tasks and must see
who said what.

## Concepts

- **Task:** one subagent started by `task`, named by a task ID (`t_k3f2`). It
  now lives as long as the parent's session: it can run more than one turn.
- **Child session:** the session a task runs in. Its first turn is the
  delegated work; each later turn starts from a message the parent sent.
- **Agent message:** text that a parent sends to one of its tasks, or a task
  sends to its parent, inside the user's session. The user calls it a
  "message"; "agent message" keeps it apart from `llm.Message`.
- **Sender:** who sent an agent message: `parent`, or a task ID with its
  subagent name. Dashi sets it from the sending turn; the text cannot.
- **Steer and queue:** the two ways a message is delivered, as for user input
  ([session input](../session-input.md)). A steer reaches a running recipient
  before its next model call. A queued message waits until the recipient's
  turn ends, then starts its next turn. Either starts a turn when the
  recipient is idle.
- **Notice:** the message Dashi sends for the child when a task's turn ends,
  now one per child turn.

## Use cases

### UC-1: Redirect a running child

The parent calls `task_send{task_id: "t_k3f2", message: "Skip vendor/; the
parser is in config/"}`. The result says "Sent to task t_k3f2 (running). It
reads it before its next model call." The child's next model call shows:

```text
<system-reminder name="agent-message" from="parent">
Skip vendor/; the parser is in config/
</system-reminder>
```

Acceptance:
- [ ] The child sees the message before its next model call, never between a
      tool call and its result, and no model call is cut short.
- [ ] A message that reaches a child as its turn ends starts the child's next
      turn, once.

### UC-2: Give a finished child a follow-up

Task `t_k3f2` completed. The parent calls `task_send{task_id: "t_k3f2",
message: "Now list the tests that cover Parse"}`. The child starts turn 2 with
its own history, so it rereads nothing. Its answer arrives as a second notice.
Acceptance:
- [ ] Turn 2 of the child session holds turn 1 in its history.
- [ ] A completed, failed, limited, or stopped child accepts the message as a
      new turn. A child that waits for an approval or on an uncertain call
      holds it until that clears; the tool result says so.
- [ ] After `MaxTaskTurns` (default 10) turns, `task_send` is a tool error that
      suggests a new task.

### UC-3: A child reports to its parent

The child calls `message_parent{message: "Parse is in config/load.go; env
overrides are in env.go too. Check both?"}`. A busy parent sees it before its
next model call, tagged `from="task t_k3f2 (explore)"`. An idle parent wakes
into a queue turn. With `deliver: "queue"`, a busy parent sees it after its
turn ends.
Acceptance:
- [ ] The tag names the task that ran the call, whatever the text says.
- [ ] After `MaxMessages` (default 8) messages in one child turn, the tool is a
      tool error: "message limit reached; put the rest in your answer".
- [ ] The CLI shows `← t_k3f2  Parse is in config/load.go; …` under the
      task.

### UC-4: The parent chooses to wait

Two ways, both named in the `task` result. End the turn: the next notice or
message wakes the parent (no polling, no limit). Or wait inside the turn:
`task{…, wait_seconds: 30}` or `task_status{task_id, wait_seconds: 30}`
returns when the task's turn ends, when it sends a steer message, or when the
time is up. The content then arrives as a steer right after the tool result,
in the same model call.
Acceptance:
- [ ] A wait that ends on a message says "a message from t_k3f2 follows", and
      the next model call holds that message once.
- [ ] `wait_seconds` above 60 is a tool error, as today.

### UC-5: A child asks a question

The child ends its turn with the question as its answer. The notice reads
"[task t_k3f2 completed] … It can take a follow-up: reply with task_send." The
parent replies; the child resumes in turn 2 with its history (UC-2).
Acceptance:
- [ ] No turn of either side waits on the other: no deadlock is possible.

### UC-6: Restart

The CLI crashes after a child sent a message and before the parent recorded
it. On reopen, `Tasks.Recover` finds the settled `message_parent` call in the
child's record and delivers the message, once. The same holds for `task_send`.
Acceptance:
- [ ] A crash test at each point (before send, after send, after delivery)
      leaves the message in the recipient's record exactly once.

## Requirements

- R-1: `task_send(task_id, message, deliver)` and `message_parent(message,
  deliver)` exist. `deliver` is `steer` (default) or `queue`. A child never
  gets `task`, `task_status`, `task_stop`, or `task_send`.
- R-2: A message is a contextual reminder in a context item with origin
  `ContextAgent`. It never carries operator authority.
- R-3: Dashi sets the sender from the tool's binding and the running turn.
  Reminder rendering escapes reminder tags in content, so text cannot close
  the tag or forge another one.
- R-4: A message ID derives from the sending call (`msg:<turn>/<call>`). A
  message is delivered when the recipient's record holds its ID, in a turn's
  context or a `queue:` start. `Recover` redelivers from the sender's record.
- R-5: Agent messages are never urgent: they cut no model call and skip no
  tool call.
- R-6: With `Config.Session` set, `Tasks` delivers notices and messages to the
  parent session itself. Without it, `OnTask` gets a `TaskMessage` event and
  the host routes it. A zero `Config` still works.
- R-7: `RunUntilQuiet` wakes on messages as on notices.

## Implementation tracks and acceptance

Four stacked tracks; each is usable alone. Tests run with `-race` and the
goroutine-leak check. [Spec](../subagent-messages.md).

**Track 1: sender tags (S).** `ContextAgent`, `llm.Reminder.From`, escaping.
- A body holding `</system-reminder>` or `<system-reminder name="x">` renders
  escaped on every provider route; the prepared text has one open and one
  close tag.
- `ContextAgent` with an operator-tier reminder or a non-user role is refused.

**Track 2: child sessions (L).** Children run as `dashi.Session`s; notices per
child turn, as reminders; `Config.Session`; `MaxTaskTurns`.
- Every existing `delegate` test passes with child turn IDs `<child>-1`.
- `Recover` resumes an interrupted child turn and reports each ended turn
  once; `Report` and `TaskView.Spend` count every child turn once.
- `examples/delegate` drops its `OnTask` routing for `Config.Session`.

**Track 3: parent to child (M).** `task_send`, steer and queue, redelivery.
- UC-1 and UC-2 acceptance; a steer that misses the child's turn runs as its
  next turn; a queue message to a running child runs after its answer.
- UC-6 crash tests for `task_send`.

**Track 4: child to parent and waits (M).** `message_parent`, `MaxMessages`,
`TaskMessage` events, `wait_seconds` on `task`, waits that end on mail.
- UC-3, UC-4, UC-5, and UC-6 acceptance for `message_parent`.
- `RunUntilQuiet` runs a queue turn for a message that reaches an idle parent.

The CLI lines (`→` and `←` under a task, `/tasks ID` with the message log)
join the pending CLI task wiring of item 13.

## Not in scope

- User to child messages (`/tasks send`). The user talks to the parent, who
  relays. Add when the CLI needs it.
- Messages between siblings, and teams. Both peers that have them hit races.
- A `wake: false` (FYI) mode. See decision 3.
- Grandchildren, a family budget cap, and a task that outlives the process.
- A durable queue. Delivery rebuilds from the sender's record instead.

## Decisions

1. **The sender picks steer or queue; steer is the default** (requirements 6
   and 7). Steer means "at the end of your next tool-result loop": before the
   next model call. Queue means "at the end of your turn": the next turn.
   Most mail is a redirect or a finding the recipient should act on now, and
   notices already steer. Rejected: fixed rules per direction, which cannot
   express "finish what you do, then do this".
2. **A child is a child session, resumable by task ID** (requirement 2). A
   message to an idle or ended child starts its next turn with its history.
   This pulls "child sessions" out of the sessions PRD's Later list; it
   reuses `Session`'s inbox, queue, and dispatch, so nothing new carries
   mail. Rejected: steering only running children (Codex V1), which leaves a
   finished child deaf; and a new task per follow-up, which rereads
   everything.
3. **Every message wakes an idle recipient; no FYI mode.** Progress for humans
   already flows through `OnChild` events, so a model-facing message is one
   the recipient should read. Caps (`MaxMessages`, `MaxTaskTurns`) and
   coalescing bound the cost. Rejected: Codex's `trigger_turn` flag, which the
   research recommended: it adds a third state where mail sits unread, and a
   model must guess the flag.
4. **Sender tags are set by Dashi and cannot be forged by text** (requirement
   5). The tool knows which task it serves; content escaping keeps a body
   inside its tag. This guarantees where a tag came from, not that the model
   obeys it. Rejected: a sender field in the tool arguments, which the model
   could set to anything.
5. **Messages are reminders with a new origin, `ContextAgent`** (requirement
   4). A typed reminder is what the model already reads as runtime context;
   the origin lets a UI and the record tell agent mail from user text and tool
   notices. Notices move to it too. Rejected: Codex's separate `agent_message`
   item type, a second rendering path for every provider.
6. **Blocking is the parent's choice, in two forms** (requirement 1). Ending
   the turn waits without limit or polling; `wait_seconds` on `task` and
   `task_status` waits up to 60 seconds inside a turn. A wait that wakes on
   mail returns, and the mail follows as a steer, so there is one delivery
   path (Codex's "wait without read" gap is closed). Rejected: a blocking
   `wait: true` on `task`, the mode background subagents removed.
7. **A question is the end of a child turn, not a new stop.** With child
   sessions, "answer with a question, get a reply as the next turn" is A2A's
   `input-required` with no engine change. Rejected: a new `input_required`
   stop kind, an engine change for what sessions already give.
8. **Delivery is once, from the record** (requirement 7's durability).
   Message IDs come from the sending call, so a rerun send is a duplicate, and
   `Recover` redelivers what the recipient lacks. Rejected: an in-memory
   mailbox, which loses mail on restart as openclaw does.
9. **No model call is ever cut for agent mail.** Codex added, reverted, and
   gated this. Urgent steers stay a user choice.

## Success

The research's two Dashi gaps close: a parent redirects a running or finished
child, and a child reports mid-task. `examples/delegate` shows a follow-up and
a child's message with no host code beyond `Config.Session`. Must not get
worse: a turn that never calls `task` records as before; notices are still
delivered exactly once; no model call is cut by agent mail.

## Risks and open questions

- **Ping-pong.** Two models can thank each other forever. `MaxTaskTurns` and
  `MaxMessages` bound it; the tool texts discourage chatter. Measure on the
  CLI's longest sessions.
- **Wake cost.** Every child message can start a parent turn. If this costs
  too much in practice, add the FYI mode of decision 3. Not blocking.
- **Child turn IDs change** (`<child>-n`). No users, so the format change is
  free.
- **Open (not blocking):** should the CLI user be able to message a task
  directly? Should a child see a short "sent while you were working" line, as
  in Claude Code, or is the tag enough?
