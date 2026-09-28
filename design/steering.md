# PRD: Steering a running turn

**Status:** Approach, revised after design review · **Date:** 2026-09-28 ·
**Owner:** Curtis · **Decision:** 32 · **Builds on:** [hooks](hooks.md)

## Problem

People type while an agent works: "use the staging database", "stop, wrong
file". `Deliver` works only between runs, and a `BeforeModel` hook leaves each
host to build an inbox that loses messages on failed writes and after the last
model call. Other agents' trackers show the failure modes: queues matched by
text, messages lost on abort, messages placed between a tool call and its
result. What works: IDs, safe delivery points, and one fate per message.

## Goals

1. One run option plus a ready-made inbox; no hook code or ID scheme.
2. A precise contract for durable hosts, and a stronger one from the inbox.
3. Steers land only where the transcript is valid for every provider.
4. An urgent steer can cut a model response short, never a running tool, and
   never knowingly costs the turn its last possible answer.
5. Steering never gets around limits, budgets, deadlines, pause, or cancel.

## The contract

- **The turn record is the authority.** A steer ID is recorded in a turn at
  most once; whether a steer was delivered is answered by the record.
- **Sources present at least once.** A source keeps returning a steer until
  Dashi acknowledges it with `Delivered`. Dashi skips IDs already recorded.
- **Visibility follows lifetime rules.** A recorded steer is seen on the next
  model call, if one happens, subject to `Snapshot` and `NextCallOnly`.
- **Claims (Inbox).** A steer the inbox has handed to a run is *claimed*: it
  cannot be cancelled. When the run ends, claims it did not record are
  released and can be cancelled or handed out again.
- **In doubt (Inbox).** A steer in a write whose outcome is unknown is *in
  doubt*. It stays claimed and is offered only to runs of the same turn, where
  the record removes duplicates, until `Leftovers` resolves it on the reloaded
  turn.
- **Leftovers.** When a turn ends, its *leftovers* are the steers the model
  never saw: those still in the inbox, plus those recorded with no later
  `model_requested` step (a dispatched call counts as seen, even if it was
  interrupted). `Leftovers` returns them once so the app can start the next turn.
- **Reconcile.** To *reconcile* is to compare steers against the saved record;
  after an unknown write, reload the turn first.

Non-goals: queueing follow-up turns; cancelling running tools; waking a turn
that waits on a tool or approval; rewriting steer text.

## Developer experience

An in-process CLI, with one active run per inbox:

```go
inbox := dashi.NewInbox()
go func() {
    for line := range input {
        inbox.SendText(line)
    }
}()
out, err := runner.Run(ctx, turn, start, dashi.WithSteering(inbox))
if err != nil {
    return err
}
if out.Stop.Terminal { // the turn can never run again (live delivery)
    next, _ := inbox.Leftovers(out.Turn) // input for the next turn, if any
}
// otherwise: Continue with the inbox, or call Leftovers to abandon the turn
```

A durable host implements `Steering` over its own table keyed by turn and
consumes rows in `Delivered` or in the recorder transaction that saves the step
(`Step.Items()` carries the IDs). After a `*CommitError` whose persistence is
unknown, it reloads the turn before reconciling.

## Functional requirements

**API (PR 1)**

- FR-1: `WithSteering(Steering) RunOption`, per run like `WithPause`.
- FR-2: The interfaces below. `Delivered` is idempotent. It runs inline after
  the commit, under `context.WithoutCancel` with a 5 s timeout. Its error
  becomes a warning on the next committed step; after the last step it is
  dropped, and the next run re-acknowledges the IDs the record already holds.
- FR-3: `NewInbox() *Inbox`, safe for concurrent use and reusable across
  turns, one active run at a time (documented, not checked):
  `Send(Steer) (string, error)` validates and fills a missing ID;
  `SendText(string) (string, error)`; `Cancel(id) bool`, false once claimed or
  in doubt; `Leftovers(Turn) ([]Steer, error)` resolves that turn's in-doubt
  steers and returns its leftovers. Its doc comment says to pass a freshly
  loaded turn: a stale one makes recorded steers look unrecorded. An invalid
  turn changes nothing.
- FR-4: `ContextItem` gains `ID`: one delivery, unlike `Name`, a slot a later
  snapshot replaces. Optional for `Deliver` and hooks, required for steers.
  `Step.Items() []ContextItem` returns a step's context items.

```go
type Steering interface {
    Steers(ctx context.Context, turnID string) ([]Steer, error) // oldest first
    Delivered(ctx context.Context, turnID string, ids []string) error
}
// Optional, for sources that claim, such as Inbox. Called once at run end.
type SteeringReleaser interface {
    // unrecorded were not written; inDoubt were in a write of unknown outcome.
    Release(ctx context.Context, turnID string, unrecorded, inDoubt []string)
}
type Steer struct {
    Item ContextItem // Item.ID is required
}
```

**Allowed content**

- FR-5: Every context item (steer, `Deliver`, or hook) carries only
  `llm.ContentBlock` (text, media, documents) and `ContentReminder`; anything
  else is rejected before recording. Today a user-role tool result passes, and
  Gemini rejects it.

**Delivery points.** Dashi calls `Steers` here, and nowhere else:

- FR-6: **Before each model call**, after the limit, budget, deadline, and
  pause checks and after the previous tool batch settled. Steers are recorded
  in arrival order in one `StepDelivered`, before `BeforeModel`, so the hooks'
  input includes them.
- FR-7: **At a candidate final answer**, at the finish point (hooks.md, "The
  finish point (PR 1b)"), before
  `BeforeFinish`. On the text path, before output is recorded, steers are
  recorded in a `StepLoopContinued`; replay knows from its position which
  candidate it replaced. On the submission path, steering calls hooks'
  `rejectSubmission(call, reason)`, which settles the started submission as
  `StateFailed` with "Not accepted: <reason>"; the reason is "the user sent
  new input. Read it, then submit again." FR-6 then delivers the steer. The model never reads "Output
  accepted" for a superseded answer.
- FR-8: **Never between a tool call and its result.** Projection emits results
  with their call, so a later item follows them; tests lock this in.
- FR-9: **Never on the last possible answer.** Dashi supersedes or interrupts
  only when the runtime method `nextCallPossible(extraInputTokens)` from hooks
  holds. It covers attempts with the reserve, deadline, pause and context,
  and every cap, with a worked example in hooks.md; steering passes the
  waiting steers' estimated input tokens and adds no checks of its own. An
  in-flight call already counts as used: replay's `ModelAttempts` counts
  settled calls that were not interrupted, and `nextCallPossible` also counts
  a `model_requested` step not yet settled (hooks.md).
  Otherwise steers wait, and Dashi records `WarningSteering`
  naming the limit, once per candidate. Only hooks and unpredictable failures
  can still refuse the next call; then the turn stops with that stop kind and
  no accepted answer.

**Contract details**

- FR-10: Dashi calls `Delivered` after each acknowledged step that records
  steers, and for IDs skipped as already recorded. At run end, `Release` gets
  the claimed IDs it never wrote, and, as in doubt, those in a write that was
  neither acknowledged nor confirmed rejected.
- FR-11: IDs are unique within a turn; replay rejects duplicates. A repeat
  with the same JSON value is skipped. With different content, `Deliver`
  returns an error and a steer is skipped with a warning, left unacknowledged.
- FR-12: `Steers` errors and invalid steers never fail the turn; they record
  `WarningSteering` on the next committed step (replay accepts warnings on any
  step kind).
- FR-13: Pause and cancel win. Delivery is observable reliably through a
  fully read `Stream` or the recorder; previews are lossy; committed steps are not.
- FR-14: `Deliver` stays for stopped turns; its docs point to steering.

**Urgent steers (PR 2)**

- FR-15: `Steer` gains `Urgent`. An optional `SteeringNotifier` interface adds
  `Ready() <-chan struct{}`, a coalesced hint; `Steers` stays the authority.
  Without a notifier, `Urgent` has no effect and the steer waits for the next
  delivery point.
- FR-16: Each model call runs under its own context derived from the run's.
  While a call is active, a listener waits on `Ready`, then calls `Steers`. If
  an urgent steer is present and FR-9 allows, Dashi cancels the call and
  settles it with `Interrupted: steering` (FR-20), keeping provider ID, usage,
  cost, and partial output. A call that completes first stands. How the cause
  reaches preview consumers is agreed with live delivery; discarding partial
  deltas applies to every interruption.
- FR-17: Partial text of a steering-interrupted call is projected into later
  calls: its text blocks only (no tool calls, reasoning, or replay data),
  followed by a projected runtime reminder that the user interrupted the
  response. If no text was streamed, nothing is added. This refines decision
  9 ("never acted on") for steering only; nothing in it runs. Other
  interrupted calls stay out of projection.
- FR-18: A call offering a provider-hosted tool unsafe to repeat is not
  interrupted; the steer waits, with a warning.
- FR-19: During a tool batch the listener also runs, and `Ready` is checked
  before each call's `BeforeTool` and start. Once an urgent steer is confirmed,
  no later call starts at any concurrency; unstarted calls settle not executed,
  reason `steering`, output "Not run: the user sent new input." FR-9 does not
  gate this, since a model call follows anyway. Running calls finish;
  approvals and external waits stay pending. First to cut if PR 2 grows.
- FR-20 (decision 32): an attempt interrupted by steering counts toward
  `ModelAttempts` and spend, not `Interruptions`; other interruptions keep
  decision 12. The `Limits.ModelAttempts` comment says so.

**Also in PR 1**

- FR-21: `modelSystem` always includes the `<system-reminder>` instruction, so
  the system prompt stays identical across a turn's calls (except the reserved
  final answer) and the prompt cache survives reminders coming and going. Its
  own commit.
- FR-22: A running-turns guide section: the contract, one active run per
  inbox, that a steer may cost an extra model call (several share one), and
  the `WarningSteering` limit signal.

## What steering needs from hooks

1. Model path order: limit checks, the steering step, `BeforeModel` on a
   fresh projection, then hook deliveries as their own `StepDelivered`.
2. One finish point for both answer paths, before output is recorded, with
   steering's check first. The finish retry after a hook failure stop goes
   through it too.
3. `rejectSubmission(call, reason)`: started to settled as `StateFailed`,
   "Not accepted: <reason>"; steering supplies only the reason.
4. `nextCallPossible(extraInputTokens)` with every check (FR-9), counting a
   call in flight (a `model_requested` step not yet settled) as used.
5. Warnings on any step kind, buffered onto the next committed step.
6. A point in the tools loop, before `BeforeTool`, for steering's check.

Already true in hooks.md: an invalid hook delivery is a skipped hook.
`AfterTool` (hooks.md, "Later: PR 2") must skip steering's not-run outcomes.

## Recorded-format changes

- `ContextItem.id`; warning code `steering`; steering's reason on hooks'
  rejected-submission settlement.
- PR 2: a tool settlement from `allowed` to `not_executed` with reason
  `steering` (skipped batch calls).
- PR 2: `ModelFact.interrupted` changes from a bool to a cause: `worker`
  (recovery after a crash), `context` (the context ended, or a pause
  stopped the call before dispatch), or `steering`. This touches the
  existing interruption code in `runner.go`, `state.go`, `project.go`, and their
  tests; replay counts `steering` as an attempt.

No migration (no users).

## Plan and tests

**PR 1** builds on hooks 1b (the finish point and seams) and on live
delivery's recorded `Stop.Terminal`; FR-21 can land first. It adds `steering.go`, `inbox.go`, delivery points in `runner.go` and
`tools_loop.go`, rules in `state.go` and `validateContext`, fields in
`turn.go`, `examples/steering` (CLI), the guide section. The durable host is a
`dashitest` fault-injection test plus a guide snippet. **PR 2:** FR-15–20.

Tests: fault injection around every steering write (rejected, unknown, crash;
an unknown write then `Cancel` and a run of another turn), each asserting at
most once in the record and no loss from the source; the inbox under `-race`;
each delivery point and FR-9 refusal; content rules; a stable system prompt;
unchanged calls without steering; for PR 2, interrupts streamed and collected,
sibling tools at concurrency 1 and 3, counting, and partial-text projection;
live tests on Anthropic, OpenAI Responses, and Gemini.

## Risks

| Risk | Mitigation |
| --- | --- |
| Steering spends the default 8 attempts | Recorded `WarningSteering`; guide advice |
| A slow `Steers` delays each call | Documented: must be cheap (memory, index) |
| Models misread a user message after tool results | Live tests per provider |
