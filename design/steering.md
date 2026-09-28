# PRD: Steering a running turn

**Status:** Approach, pending review · **Date:** 2026-09-28 · **Owner:**
Curtis · **Decision:** 32 · **Builds on:** [hooks](hooks.md) (decision 31)

## Problem

People type while an agent works: "use the staging database", "stop, wrong
file". Dashi has no natural way to get that message into a running turn.
`Deliver` works only between runs. A `BeforeModel` hook can add context, but
each host must build its own inbox, cannot tell which messages were saved when
a write fails, and strands a message that arrives after the last model call.
Pause, `Deliver`, `Continue` counts every steer against `Limits.Interruptions`.

Dive never shipped steering, so our production gateway rebuilt it from hooks.
Other agents' trackers show the failure modes: queues matched by text, messages
lost on abort, messages inserted between a tool call and its result, and races
with an "idle" snapshot. What works: IDs, safe delivery points, and one
unambiguous fate for each message.

Also in this change: the runner sends its `<system-reminder>` instruction only
when a request carries a reminder, so the system prompt changes as reminders
come and go, and each change discards the provider's prompt cache.

## Goals

1. Steering is one run option plus a ready-made inbox; an in-process app writes
   no hook code and no ID scheme.
2. A precise contract for durable hosts, and a stronger one from the inbox.
3. Steers land only where the transcript is valid for every provider.
4. An urgent steer can cut a model response short, never a running tool, and
   Dashi never knowingly gives up the turn's last possible answer.
5. Steering never gets around limits, budgets, deadlines, pause, or cancel.
6. The system prompt no longer depends on whether reminders are present.

## The contract

- **The turn record is the authority.** A steer ID is recorded in a turn at
  most once; whether a steer was delivered is answered by the record.
- **Sources present at least once.** A source returns a steer from `Steers`
  until it is acknowledged. Dashi skips IDs already recorded.
- **Visibility follows lifetime rules.** A recorded steer is seen on the next
  model call, if one happens, subject to `Snapshot` and `NextCallOnly`.
- **The built-in `Inbox` adds claims.** A steer returned by `Steers` is claimed
  by that run and can no longer be cancelled; claims are released when the run
  ends without acknowledging them.
- **Reconcile before moving input.** Before moving leftover steers to another
  turn, an app reconciles them against the record, reloading it after an
  unknown write.

Non-goals: queueing follow-up turns (the app's job); cancelling running tools;
waking a turn that waits on a tool or approval; rewriting steer text.

## Developer experience

```go
inbox := dashi.NewInbox()
go func() {
    for line := range input {
        inbox.SendText(line) // returns the generated ID and an error
    }
}()
out, err := runner.Run(ctx, turn, start, dashi.WithSteering(inbox))
switch {
case errors.As(err, &commitErr) || errors.Is(err, dashi.ErrRevisionConflict):
    // the record is uncertain: reload the turn before Leftovers
case err != nil:
    // rejected before running; no steer was claimed
case out.Stop.Final():
    next, err := inbox.Leftovers(out.Turn) // drops steers already recorded
    // start the next turn with next, if any
default:
    // resumable: pass the inbox again on Continue, or take its Leftovers
}
```

A durable host implements `Steering` over its own table, one value per run, and
consumes rows in `Delivered` or in the recorder transaction that saves the step
(`Step.Items()` carries the IDs).

## Functional requirements

**API**

- FR-1: `WithSteering(Steering) RunOption`, per run like `WithPause`.
- FR-2: The interface and value below. `Delivered` is idempotent; its error is
  a warning, retried on the next acknowledgment or run.
- FR-3: `NewInbox() *Inbox`, concurrency-safe and reusable across turns:
  `Send(Steer) (string, error)` validates and fills a missing ID;
  `SendText(string) (string, error)`; `Cancel(id) bool` succeeds only while
  unclaimed; `Leftovers(Turn) ([]Steer, error)` drops steers recorded in that
  turn and removes and returns the rest (an invalid turn removes nothing).
  `Steers` from a second concurrent run returns an error.
- FR-4: `ContextItem` gains `ID`: one delivery, unlike `Name`, a slot a later
  snapshot replaces. Optional for `Deliver` and hooks, required for steers.
  `Step.Items() []ContextItem` returns a step's context items. `Stop.Final()`
  is true only for completed or canceled turns.

```go
type Steering interface {
    Steers(ctx context.Context) ([]Steer, error)          // unacknowledged, oldest first
    Delivered(ctx context.Context, ids []string) error // idempotent
    Ready() <-chan struct{} // coalesced hint; nil disables urgent steers
}
type SteeringReleaser interface { // optional: sources that claim, like Inbox
    Release(ctx context.Context, ids []string)
}
type Steer struct {
    Item   ContextItem // Item.ID is required
    Urgent bool        // may cut short an in-flight model call
}
```

The method was `Pending` in earlier drafts; #45 made `Pending` the name of a
host-actionable work item (`Outcome.Pending`), so it is now `Steers`, matching
`Toolset.Tools`.

**Allowed content**

- FR-5: Every context item (steer, `Deliver`, or hook) carries only
  `llm.ContentBlock` (text, media, documents) and `ContentReminder`. Tool calls
  and results, hosted items, reasoning, refusals, and opaque replay data are
  rejected before recording (today a user-role tool result passes, and Gemini
  rejects it).

**Delivery points.** Dashi calls `Steers` at these points, not gated on
`Ready`, and nowhere else:

- FR-6: **Before each model call**, after the limit, budget, deadline, and
  pause checks and after the previous tool batch settled. Steers are recorded
  in arrival order in one `StepDelivered`, before `BeforeModel` hooks run, so
  their `Input` includes them.
- FR-7: **At a candidate final answer**, before `BeforeFinish` and before
  output is recorded, on the text and `dashi_submit_output` paths. A steer is
  recorded as a `StepLoopContinued` whose `Supersedes` names the candidate
  (attempt ID or submission call ID); `BeforeFinish` does not run for it.
- FR-8: **Never between a tool call and its result.** Projection emits results
  with their call, so a later item follows them; tests lock this in.
- FR-9: **Never on the last possible answer.** Before superseding or
  interrupting, `nextCallPossible` checks that a further non-final model call
  can run (counting the interrupted call, per FR-20), the budget is not spent,
  the deadline has not passed, and no pause or context end is pending.
  Otherwise steers stay unacknowledged. If the next call is refused anyway
  (`BeforeSend`, admission), the turn stops with that stop kind and no accepted
  answer: only a completed stop means an accepted answer, not `Answer`.

**Contract details**

- FR-10: Dashi calls `Delivered` after each acknowledged step that records
  steers, and for IDs skipped as already recorded, under
  `context.WithoutCancel` with a 5 s timeout. At run end it calls `Release` for
  claimed, unacknowledged IDs.
- FR-11: IDs are unique within a turn; replay rejects duplicates. A repeat with
  the same JSON value is skipped. With different content, `Deliver` returns an
  error and a steer is skipped with a warning, left unacknowledged.
- FR-12: `Steers` errors and invalid steers never fail the turn; they record
  `WarningSteering` on the next committed step.
- FR-13: Pause and cancel win; steering never raises a limit, budget, or
  deadline.
- FR-14: Delivery is observable reliably through a fully read `Stream` or the
  recorder; `Observer` is lossy.
- FR-15: `Deliver` stays for stopped turns; its docs point to steering.

**Urgent steers**

- FR-16: Each model call runs under its own context derived from the run's;
  cancelling it abandons that call without ending the turn, streamed or not.
- FR-17: While a call is active, a listener waits on `Ready`, then calls
  `Steers`. If an urgent steer is present and FR-9 allows, Dashi cancels the
  call and settles it as interrupted with `ModelFact.InterruptedBy:
  "steering"`, keeping provider ID, usage, cost, and partial output, never
  acted on; FR-6 then delivers. A call that completes first stands.
  `InterruptedBy` tells `Stream` consumers to discard partial deltas.
- FR-18: A call offering a provider-hosted tool unsafe to repeat is not
  interrupted; the steer waits, with a warning.
- FR-19: During a tool batch the listener also runs, and `Ready` is also
  checked before each call's `BeforeTool` and `ToolStarted`. Once an urgent
  steer is confirmed, no later call starts at any concurrency: requested calls
  and allowed calls settle `StateNotExecuted`, reason `steering`, output "Not
  run: the user sent new input." (a new replay transition from allowed).
  Running calls finish; approvals and external waits stay pending.
- FR-20 (decision 32): an attempt interrupted by steering counts toward
  `ModelAttempts` and spend, not `Interruptions`. Other interruptions keep
  decision 12.

**Reminder instruction**

- FR-21: `modelSystem` always includes the reminder instruction; the system
  prompt is identical across a turn's calls except the reserved final answer.

**Docs**

- FR-22: A running-turns guide section: the contract, one active run per inbox,
  that a steer may cost an extra model call (several share one), and that
  interactive agents should raise `ModelAttempts` from 8.

## What steering needs from hooks

Steering lands after hooks. These must hold in the hooks code, or steering adds
them:

1. **Order in the model path:** `run` checks limits, then steering commits its
   `StepDelivered`, then `BeforeModel` builds from a fresh projection, then
   hook deliveries commit as their own `StepDelivered`.
2. **One finish point** shared by the text and submission paths, reached
   before `StepOutputRecorded`. Steering's check runs first there.
3. **Superseding a submission** (hooks FR-10): replay accepts
   `StepLoopContinued` after a successful submission and selects the latest
   eligible submission. Steering needs the step to carry `Supersedes`.
4. **One `nextCallPossible(state)`** used by both `BeforeFinish.Continue` and
   steering, with the reserve rule; steering adds the budget, deadline, and
   pause terms if hooks checks only attempts.
5. **Warnings on any step kind,** with a runtime buffer that rides on the next
   committed step (hook-skip warnings need it too).
6. **Deliver items validated** by one `validateContext` before commit; an
   invalid hook item is a skipped hook, so FR-5 applies to hooks unchanged.
7. **A pre-decision point** in the tools loop (after the rewrite-restart
   refactor) where steering checks its flag before calling `BeforeTool`.
   `AfterTool` does not run for steering's not-run outcomes (hooks FR-9).

Hooks do not need to tell steers from other messages in v1; a `ModelPlan`
field listing this pass's steers can come later.

## Recorded-format changes

`ContextItem.id`; `ModelFact.interrupted_by`; `Step.supersedes`; warnings on
any step kind and code `steering`; a tool settlement from `allowed` to
`not_executed` with reason `steering`; replay counts steering interruptions as
attempts. No migration (no users).

## Plan: two sequential PRs

**PR 1, ordinary steering** (FR-1–15, 21, 22): new `steering.go`
(`Steering`, `Steer`, `WithSteering`) and `inbox.go`; delivery points in
`runner.go`; duplicate IDs and supersede rules in `state.go`; `ContextItem.ID`,
`Step.Items`, `Stop.Final`, `WarningSteering` in `turn.go`; content rules in
`validateContext`; `examples/steering` (CLI) and `examples/durable_steering`
(a table-backed `Steering` over `filerecorder`); the guide section.

**PR 2, urgent steers** (FR-16–20): per-call context and listener in
`runner.go`, the batch flag in `tools_loop.go`, `InterruptedBy` and counting in
`state.go`, the `Limits.ModelAttempts` and `TurnView.ModelAttempts` comments,
and `docs/agent-turns.md`. Splitting keeps each reviewable; PR 1 alone is
complete because a nil `Ready` disables urgency.

## Tests

- Fault injection (`dashitest.FaultRecorder`): rejected and unknown writes of
  the steering step and of a superseding step; crash on either side of a
  superseded submission; ended context; pause; each limit; the reserved final
  answer. Each asserts a steer recorded at most once and not lost from the
  source until acknowledged.
- `Inbox`: `Cancel` racing a claim, `Leftovers` after an unknown write, a
  second concurrent run, `Release` at run end (`-race`).
- Delivery points: before the first call after `Start` and `Continue`; after
  a tool batch (projection order); at a final text answer; at a submission;
  FR-9 refusals; duplicates with same and different content.
- Urgent: streamed and collected calls; a call that completes before the
  cancel; unsafe hosted tools; `Ready` between sibling tools at concurrency 1
  and 3; attempt and spend counting; `Interruptions` unchanged.
- Content validation for steers, `Deliver`, and hook items.
- System prompt identical across a turn whose reminders appear and drop out.
- Guardrail: a turn without steering makes the same calls as today.
- Provider contract (live tag): Anthropic, OpenAI Responses, and Gemini accept
  a steer after a tool batch and after an interrupted response.

## Risks

| Risk | Mitigation |
| --- | --- |
| Interrupts waste tokens | Opt in per steer; cost is counted and visible |
| A flood of urgent steers | FR-20 counts each; FR-9 protects the last answer |
| A slow `Steers` delays each call | Documented: must be cheap (memory, index) |
| Models misread a user message after tool results | Live tests per provider |
| Merge conflicts with hooks in `runner.go`, `state.go` | Rebase onto hooks before code |
