# PRD: Steering a running turn

**Status:** Draft, revised after four reviews · **Date:** 2026-09-28 ·
**Owner:** Curtis

## Problem

People type while an agent works: "use the staging database", "stop, wrong
file", "also update the README". Kernel has no natural way to get that message
into a running turn.

- `Deliver` works only between runs. While `Runner.Run` owns the turn, a second
  writer conflicts with the running worker.
- `BeforeModel` can return context, but each host must build its own inbox,
  cannot tell which messages were saved when a write fails, and strands any
  message that arrives after the last model call.
- Pause, then `Deliver`, then `Continue` counts every steer against
  `Limits.Interruptions` (default 10) and makes the host resend history.

Dive never shipped steering, so Nvoken rebuilt it from hooks. Pi's tracker shows
the failure modes: queues matched by text (#9803), messages lost on abort
(#10017), messages inserted between a tool call and its result (#1717), and
races with an "idle" snapshot (#5886). Nvoken shows what works: IDs, safe
delivery points, and an unambiguous fate for each message.

Also in this change: the runner adds its `<system-reminder>` explanation to the
system prompt only when a request carries a reminder. The system prompt changes
when the first reminder arrives and again when a one-call reminder drops out,
and each change discards the provider's prompt cache. Dive always sends it.

## Goals

1. Steering is one run option plus a ready-made inbox. An in-process app needs
   no hook code and no ID scheme.
2. A precise contract that durable hosts can build on (below), with a stronger,
   clearly scoped guarantee from the built-in inbox.
3. Steers land only where the transcript stays valid for every provider.
4. An urgent steer can cut a model response short, but never a running tool,
   and Kernel never knowingly gives up the turn's last possible answer.
5. Steering never gets around limits, budgets, deadlines, pause, or cancel.
6. The system prompt no longer depends on whether reminders are present.

## The contract

- **The turn record is the authority.** A steer ID is recorded in a turn at most
  once. Whether a steer was delivered is answered by the record, never by the
  source.
- **Sources present at least once.** A source keeps returning a steer from
  `Pending` until it is acknowledged as recorded. Kernel filters IDs already in
  the record, so repeats are harmless.
- **Model visibility follows lifetime rules.** A recorded steer is seen on the
  next model call, if one happens, subject to `Snapshot` and `NextCallOnly`.
- **The built-in `Inbox` adds claims.** A steer returned by `Pending` is claimed
  by that run. A claimed steer cannot be cancelled. Claims are released when the
  run ends without acknowledging them.
- **Reconcile before moving input.** Before an app moves leftover steers into
  another turn, it reconciles them against the turn record. After an unknown
  write, it first reloads the turn.

## Non-goals

- **Queueing follow-up turns.** Input for after a turn ends is the app's job.
- Cancelling running tools. A tool that must stop reacts to its context, or the
  host uses `Cancel`.
- Waking a turn that waits on an external tool or approval. The model cannot be
  called while a call is unanswered.
- Rewriting steer text. Kernel records what the app sent and adds no framing.

## Developer experience

A CLI reuses one inbox across turns, one active run at a time:

```go
inbox := kernel.NewInbox()
go func() {
    for line := range input {
        inbox.SendText(line) // returns the generated ID
    }
}()
out, err := runner.Run(ctx, turn, start, kernel.WithSteering(inbox))
switch {
case err != nil:
    // command rejected; nothing new was recorded
case out.RecordingErr() != nil:
    // reload the turn before deciding anything about queued input
case out.Stop.Final():
    next, err := inbox.Leftovers(out.Turn) // drops steers already recorded
    // start the next turn with next, if any
default:
    // stopped but resumable: pass the inbox again on Continue,
    // or give up on the turn and take its Leftovers
}
```

A durable host implements `Steering` over its own table, one value per run
(`store.Steering(turnID)`), and consumes rows either in `Delivered` or in the
recorder transaction that saves the step, since the step carries the IDs.

## Functional requirements

**API**

- FR-1: `WithSteering(Steering) RunOption`, per run like `WithPause`.
- FR-2: The interface and value below. `Delivered` is idempotent; an error is
  recorded as a warning and retried on the next acknowledgment or run.
- FR-3: `kernel.NewInbox()` returns a concurrency-safe `*Inbox`, reusable across
  turns. It has these methods:
  - `Send(Steer) (string, error)` validates and generates a missing ID.
  - `SendText(text string) (string, error)` sends user-origin text.
  - `Cancel(id string) bool` succeeds only while the steer is unclaimed.
  - `Leftovers(Turn) ([]Steer, error)` removes steers already in that turn's
    record, then removes and returns the rest. An invalid record returns an
    error and removes nothing.

  `Pending` from a second concurrent run returns an error.
- FR-4: `ContextItem` gains `ID string`: one delivery, as opposed to `Name`, a
  slot that a later snapshot replaces. It is optional for `Deliver` and hook
  items and required for steers. `Step.Items() []ContextItem` returns the items
  of either step kind that carries them. `Stop` gains `Final() bool`, true only
  for completed or canceled turns; for other stops (a budget stop can resume)
  the app decides.

```go
type Steering interface {
    // Pending returns unacknowledged steers, oldest first.
    Pending(ctx context.Context) ([]Steer, error)
    // Delivered acknowledges steers recorded in the turn. Idempotent.
    Delivered(ctx context.Context, ids []string) error
    // Ready is a coalesced hint that Pending may have changed. Nil disables
    // urgent interruption; Pending remains the authority.
    Ready() <-chan struct{}
}

// Optional: sources that claim steers, such as Inbox, release unacknowledged
// claims when a run ends.
type SteeringReleaser interface {
    Release(ctx context.Context, ids []string)
}

type Steer struct {
    Item   ContextItem // Item.ID is required
    Urgent bool        // may cut short an in-flight model call
}
```

**Allowed content**

- FR-5: Every context item (steer, `Deliver`, or hook) may carry only user text,
  media, and typed reminders. Tool calls, tool results, approval messages,
  reasoning, and provider replay data are rejected before recording. Today a
  user-role tool result with an arbitrary call ID passes validation, and Gemini
  rejects it.

**Delivery points.** Kernel always calls `Pending` at these points (not gated on
`Ready`), and nowhere else:

- FR-6: **Before each model call**, including the first after `Start` or
  `Continue`, after the previous tool batch has fully settled. Steers are
  recorded in one `StepDelivered`, in arrival order, before the `BeforeModel`
  hooks, so the hooks see them.
- FR-7: **At a candidate final answer**, before the `BeforeFinish` hooks and
  before structured output is recorded, on both the final-text path and the
  `kernel_submit_output` path. A pending steer is recorded as a
  `StepLoopContinued` that names the superseded candidate (attempt ID or
  submission call ID). Replay allows this step after a successful submission and
  selects the latest eligible submission, not the first.
- FR-8: **Never between a tool call and its result.** Projection already places
  delivered items after the preceding call's results; tests lock this in.
- FR-9: **Never on the last possible answer.** Before superseding a candidate or
  interrupting a call, Kernel checks that the next call is still possible:
  - attempts remain beyond the reserved final answer;
  - the budget is not spent;
  - the deadline has not passed;
  - no pause is requested.

  Otherwise steers stay unacknowledged. If the next call is refused anyway (by
  `BeforeSend` or an admission failure), the turn stops with that stop kind.
  Steering can therefore leave a turn without an accepted answer: the
  superseded candidate was never recorded as output. Only a completed stop
  means an accepted answer; `AnswerText` alone does not.

**Contract details**

- FR-10: Kernel calls `Delivered` after each acknowledged step that records
  steers, and for filtered IDs already in the record. It uses
  `context.WithoutCancel` with a short timeout, so it runs after the run's
  context ends. At run end it calls `Release` for claimed, unacknowledged IDs.
- FR-11: IDs are unique within a turn, and replay rejects duplicates. A repeated
  ID with the same content (same JSON value) is skipped. With different content,
  `Deliver` returns an error; a steer is skipped with a warning and left
  unacknowledged for the host to repair. `Inbox` IDs cannot conflict.
- FR-12: `Pending` errors and invalid steers never fail the turn. They produce a
  new `WarningSteering` code. Replay accepts warnings on any step kind; a
  warning with no step of its own rides on the next committed step.
- FR-13: Pause and cancel win over steering. Steering never raises a limit, the
  budget, or the deadline.
- FR-14: Delivery is observable reliably through a fully consumed `Stream` or
  the recorder. `Observer` is lossy and suits display only.
- FR-15: `Deliver` stays for stopped turns. Its docs say to use steering while a
  turn runs.

**Urgent steers** (implemented after ordinary steering passes its tests, in the
same PR)

- FR-16: Each model call runs under its own context, derived from the run's.
  Cancelling it abandons that call without ending the turn. It works for
  streamed and non-streamed calls alike, so steering does not force streaming.
- FR-17: While a call is active, a listener waits on `Ready`, then calls
  `Pending`. If an urgent steer is pending and FR-9 allows it, Kernel cancels
  the call, records the attempt as interrupted with a new field
  `ModelFact.InterruptedBy: "steering"`, and delivers at FR-6. The existing
  interruption path settles the attempt: provider ID, usage, cost, and partial
  output are recorded but never acted on. A call that completes before the
  cancel takes effect stands, and the steer waits for FR-6. `InterruptedBy`
  tells `Stream` consumers to discard partial deltas.
- FR-18: Kernel does not interrupt a call whose request included a
  provider-hosted tool that is unsafe to repeat. The steer waits, with a
  warning.
- FR-19: An urgent steer during a tool batch stops new calls from starting.
  The FR-17 listener also runs while a batch drains. When `Pending` confirms
  an urgent steer, it sets a flag that the runner checks before committing
  each `ToolStarted`; once set, no later call starts, at any concurrency.
  Tests cover `Ready` firing between sibling tools at concurrency 1 and
  above. Unstarted allowed calls settle as `StateNotExecuted` with the output
  "Not run: the user sent new input." This is a new replay transition, from
  allowed to not executed, with the reason `steering`. Running calls finish.
  Calls awaiting approval and waiting external calls stay pending.
- FR-20: An attempt actually cancelled for a steer counts toward `ModelAttempts`
  and the budget. This supersedes part of decision 12 (decision 32), so
  replay counts only `InterruptedBy: "steering"`. Update the
  `Limits.ModelAttempts` comment and `docs/agent-turns.md`. It does not count
  toward `Limits.Interruptions`.

**Reminder instruction**

- FR-21: `modelSystem` always includes the reminder instruction. Tests assert
  that the system prompt is identical across calls in a turn whose reminders
  appear and drop out, except on the reserved final-answer call, which adds its
  own text.

**Docs**

- FR-22: A running-turns guide section on steering:
  - the contract above;
  - one active run per inbox;
  - a steer may require an additional model call; several steers share one
    call, and a steer arriving before an already planned call adds none;
  - interactive agents should raise `ModelAttempts` from its default of 8.

## Success criteria

- A fault-injection suite holds the contract in each of these cases:
  - a crash, an unknown write, a rejected write;
  - an ended context, pause, each limit;
  - output recorded, the reserved final answer, a superseded submission with a
    crash on either side;
  - `Cancel` racing a claim, and `Leftovers` after an unknown write.

  In every case a steer is recorded at most once and never lost from the
  source until the record acknowledges it.
- Provider contract tests (Anthropic, OpenAI Responses, Gemini) accept a steer
  after a tool batch and after an interrupted response. Invalid steer content is
  rejected before recording.
- The CLI example works as written; a durable example maps Nvoken's nudges onto
  `Steering` with no hooks or sentinel errors.
- Guardrails: turns without steering make the same model and tool calls as
  today. The always-on reminder sentence is measured on its own (about 30 tokens
  per request), and the prompt-cache prefix stays stable across reminder
  changes.

## Risks

| Risk | Mitigation |
| --- | --- |
| Interrupts waste tokens | Opt in per steer; cost is counted and visible |
| A flood of urgent steers keeps cancelling calls | FR-20 counts each; FR-9 protects the last answer |
| A slow `Pending` delays each call | Document that it must be cheap (in-memory or indexed) |
| Models misread a user message after tool results | Live tests per provider; apps may add a contextual reminder |
