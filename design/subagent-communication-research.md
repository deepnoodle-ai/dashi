# Research: parent and subagent communication

**Status:** Research · **Date:** 2026-09-29 · **Informs:**
[background subagents](background-subagents.md), [steering](steering.md),
[session input](session-input.md)

## Question

After a parent agent starts a subagent, how do the two talk while both work,
inside the user's original session? The hypothesis: products inject "system
reminders" into a running turn, even while the recipient is busy with tool calls
or reasoning, and a reminder wakes an idle recipient into a new turn.

## Verdict

**Half right.** Three things hold across products; one does not.

1. **Right: injected, tagged text is the mechanism.** Products deliver a
   message to a busy agent as a model-visible note, often
   `<system-reminder>` or a similar tag, added to the transcript. Nothing else
   crosses between two model loops.
2. **Right: an idle recipient is woken** by a message in most products that
   support background children (Claude Code, grok-build, openclaw, Codex for
   some message kinds, OpenHands with a flag).
3. **Wrong: nothing lands mid-call.** No product inserts text while a model
   call streams or a tool runs. Every one delivers at a boundary: after the
   running tool calls return, before the next model request. Only Codex ever
   cuts a model stream short for mail, and it later made that opt-in.
4. **Wrong: two-way channels are rare.** Most products have only "prompt in,
   final answer out". Only Claude Code and Codex let a parent message a running
   child (openclaw does too, but its children cannot message back). Pi, opencode, Gemini CLI, zed, ADK-Go, Eino,
   MAF-Go and OpenHands (delegate tools) block the parent until the child ends.

## What each product does

| Product | Children | Parent → running child | Child → parent | Delivery to a busy recipient | Wakes an idle recipient |
| --- | --- | --- | --- | --- | --- |
| Claude Code | Background or blocking | `SendMessage`; queued for the child's next tool round. A stopped child resumes from its transcript | Completion notice, queued at `later` priority | `<system-reminder>` after tool results | Yes: an idle parent starts a follow-up turn |
| Codex V2 (flag) | Background, path-addressed | `send_message` (queue only), `followup_task` (wakes) | Final answer, automatic, queue only | Own `agent_message` item, before the next model request | Only `trigger_turn` mail; a parent in `wait_agent` wakes |
| Codex V1 (default) | Background, thread-ID addressed | `send_input`, optional `interrupt` | Hidden `<subagent_notification>` user-role block | Steers the running turn | Yes |
| grok-build | Background | Cancel only, or resume a finished child | `<system-reminder>` completion text | After each tool call and at turn start | Yes, as a queued prompt |
| openclaw | Always background | `sessions_send` | Completion report | Steering queue, next model call | Yes, new run |
| Pi | Blocking (example extension) | None | Final text | n/a (`steer` and `followUp` queues exist for the user) | `triggerTurn: true` |
| opencode | Blocking `task` | Another `task` call with `session_id` | Final text; child permissions show in the parent view | Queued user text wrapped in `<system-reminder>` | No |
| Gemini CLI, zed | Blocking | None (zed: resume after the end) | Final text | User hints only | No |
| OpenHands, ADK-Go, Eino, MAF-Go | Blocking agent-as-tool | None | Final text | Step boundary (OpenHands, MAF-Go) | OpenHands `run=True` only |

Evidence and file:line citations are in
`tmp/research/subagent-comms/` (`codex.md`, `pi-opencode.md`,
`claude-gemini-others.md`, `frameworks.md`). The Claude Code source there is an
unofficial extraction (v2.1.88), so treat its details as dated.

## Patterns worth knowing

**One queue, priority classes (Claude Code).** User input, task notices and
permission requests share a queue ordered `now > next > later`. A tool boundary
takes only `next`. A child's completion is `later`, so by default it waits until
the parent is idle; the user's typed message is `next` and enters the running
turn. Priority, not the transport, decides who interrupts whom.

**Trigger versus queue-only (Codex V2).** Each message carries a
`trigger_turn` flag. A "do this" message wakes the recipient. An FYI or a final
answer only queues. This is the cleanest wake control found.

**Sender tags.** Codex frames mail with author, recipient and a type
(`MESSAGE`, `NEW_TASK`, `FINAL_ANSWER`). Claude Code words it as "The coordinator
sent a message while you were working". A model that can tell a parent message
from user text from a tool result follows each correctly.

**Preemption is a separate, explicit act (Eino, Codex).** Eino's plain `Push`
waits for the next turn; `WithPreempt` cancels at a safe point. Codex's
mid-stream cut dropped tool calls the model had not yet emitted; it was added,
reverted, and put behind `defer_mailbox_preemption`.

**Acknowledge receipt, not completion (ACP v2).** A send returns a message ID
once the message is in the conversation. Run state (`running`, `requires_action`,
`idle`) is reported on a separate channel. The ACP prompt RFD leaves queueing
and steering open and names background subagents as its reason.

**Questions go up, not sideways.** No product routes a child's question to the
parent model. Claude Code subagents cannot ask the user; background children
refuse permission prompts unless the mode is `bubble`. opencode and zed show a
child's permission request in the parent's view. ADK-Go and Eino end the child's
turn and route the user's reply back. A2A models this as `input-required`.

## Pitfalls seen in trackers and code

- **Lost mail across restart.** openclaw drops undelivered completion reports if
  the gateway restarts. Codex had a race where `wait_agent` missed mail queued
  just before it began waiting, and one where eviction of an idle agent raced
  with queued mail.
- **Mail restarting a finished answer.** Codex mail arriving at a candidate final
  answer restarted the turn.
- **Stranded or double-handled queues.** Pi matched queued items by text, cleared
  extension messages with the user queue, and lost follow-ups on abort.
- **Wake without a way to read.** Codex `wait_agent` returns on mailbox activity
  but omits the message text, so the parent waits, wakes, and still has to read.
- **Silent children.** Interrupted Codex children send nothing to the parent.
- **Polling.** Models poll status tools unless told not to; several products add
  a bounded wait tool.

## Where Dashi stands

Dashi already has the hard half. A finished task reaches the parent as a steer
with origin `ContextTool`, delivered before the next model call, or as one
follow-up queue turn when no turn runs. The record decides delivery once
(`task-done:<ID>`), so restarts lose and repeat nothing. That is stronger than
openclaw, Pi, and most of the field. Steers never land between a tool call and
its result, which matches every product studied.

Gaps against the market:

| Gap | Seen in |
| --- | --- |
| Parent cannot message a running child (declared out of scope) | Claude Code, Codex, openclaw |
| Child cannot message its parent, other than at the end | Codex V2 |
| No wake-or-queue choice on a message | Codex `trigger_turn`, Claude Code priorities |
| Progress and questions from a child never reach the parent model | none well; A2A `input-required` is the shape |
| No sender tag on a steer beyond origin and `NoticeID` prefix | Codex, Claude Code |

## Recommendations for Dashi

1. **Reuse the steer path for parent → child.** A running child is a running
   Turn with an `Inbox`. `task_send(task_id, message)` becomes a steer into that
   child, delivered before its next model call. No new transport, and the
   research says none exists that beats it. If the child has ended, resume it
   with the message as its new input (Claude Code's resume) rather than fail.
2. **Give a message a wake flag.** Follow Codex: `Wake bool` on the message. A
   parent-to-child "new instruction" wakes an idle or resumable child; a
   progress FYI queues. Default to queue-only, so a chatty child cannot start
   unpaid turns in the parent.
3. **Never cut a model call for a subagent message.** Codex learned this the
   hard way. Use the existing urgent-steer path only for user steers, where the
   user asked for it.
4. **Tag every steer with its sender** (`parent`, `child t_k3f2`, `user`) in the
   reminder wrapper, not only in origin metadata. Tags survive in the transcript
   the model reads. Keep the authority rule from
   [runtime reminders](runtime-reminders.md): a child's text is never operator
   tier.
5. **Add a child → parent message tool** (`report_to_parent`), queue-only by
   default, that arrives as a steer with origin `ContextTool` and a child tag.
   This also gives the child a way to say "blocked, need a decision" without
   ending its turn.
6. **Model a child's question as a stop, not a message.** Add a resumable
   outcome (`input_required`, A2A's shape; Dashi's `waiting` stop is close). The
   parent sees a notice with the question, answers with `task_send`, and the
   child resumes. This fits the durable record and avoids deadlocks from two
   models each waiting on the other.
7. **Keep the one delivery rule.** Extend `task-done:<ID>` to a per-message ID
   (`msg:<ID>`) so mail is recorded once, survives restart, and returns the ID
   at send time, as ACP does. Do not let a message live only in memory.
8. **Bound the chatter.** Reuse budgets and add a cap on unread messages per
   recipient. Coalesce queued progress notes into one steer, as Dashi already
   does for follow-up notices.
9. **Keep `task_status` with a bounded wait,** and add message text to its
   result, so a wait that wakes also reads (Codex's gap).
10. **Leave agent-to-agent among siblings and teams for later.** Only Claude
    Code teams (file mailboxes) and Codex V2 paths do it, and both hit races.
    A parent-child pair covers the user's stated need.

## Open questions

- Should a parent's `task_send` to a running child be visible to the user in the
  CLI as a line under the task, like the existing progress lines?
- Does a child's `report_to_parent` count against the parent's turn limits, or
  only against its own?
- Should Dashi wrap steers as `<system-reminder>` text on every route (the
  current rendering) or use a distinct typed item like Codex's `agent_message`?
  The first keeps one mechanism; the second is easier to hide in a UI.
