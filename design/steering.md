# PRD: Steering a running turn

**Status:** Part 1 merged (#52); part 2 (urgent steers) in approach review ·
**Date:** 2026-09-28 · **Owner:** Curtis · **Decision:** 32 · **Builds on:**
[hooks](hooks.md)

## Problem

People type while an agent works: "use the staging database", "stop, wrong
file". Part 1 delivers such a message, a *steer*, at safe points: before each
model call and at a candidate final answer. But a model call can run for a
minute, streaming an answer the user already knows is wrong. Part 2 lets an
*urgent* steer cut that call short, so the model hears the correction now.

## The contract (part 1, unchanged)

- **The turn record is the authority.** A steer ID is recorded in a turn at
  most once. Sources present a steer until Dashi acknowledges it with
  `Delivered`, and Dashi skips IDs already recorded.
- **Claims and doubt (Inbox).** A steer handed to a run is *claimed* and cannot
  be cancelled. One in a write of unknown outcome is *in doubt* and goes only
  to runs of its own turn. `Leftovers(turn)` returns the steers not recorded in
  the turn; recorded ones reach the next turn through `Project`.
- **Delivery points.** Before each model call (one `StepDelivered`, before
  `BeforeModel`), and at a candidate answer, where a steer supersedes the answer
  only when `nextCallPossible` holds (hooks.md). Never between a tool call and
  its result, and never on the last possible answer. A steer is a nudge, never
  a veto.
- **Content.** Context items carry only content blocks and reminders.
  `ContextFact.Steering` marks the steps that record steers.

Non-goals: queueing follow-up turns; cancelling running tools; waking a turn
that waits on a tool or approval; rewriting steer text.

## Part 2: urgent steers

### API

```go
// Optional: sources that can wake a run, such as Inbox.
type SteeringNotifier interface {
    // Ready is a coalesced hint that an urgent steer may be waiting. It
    // returns the same channel on every call. Steers stays the authority.
    Ready() <-chan struct{}
}

type Steer struct {
    Item   ContextItem
    Urgent bool // may cut short the model call in flight
}

func (in *Inbox) Ready() <-chan struct{}
func (in *Inbox) SendUrgent(text string) (string, error)
```

`Inbox.Send` with `Urgent` set, or `SendUrgent`, signals `Ready` (a channel with
a buffer of one, so signals coalesce and a send never blocks). Non-urgent sends
do not signal. Without a notifier, `Urgent` has no effect: the steer waits for
the next delivery point, as in part 1.

```go
inbox := dashi.NewInbox()
go func() {
    for line := range input {
        if strings.HasPrefix(line, "!") { // "!stop, wrong file"
            inbox.SendUrgent(line[1:])
        } else {
            inbox.SendText(line)
        }
    }
}()
```

### Interrupting a model call

- **U-1. Its own context.** Each model call runs under a context derived from
  the run's, cancelled with a private cause `errSteered`. Cancelling it
  abandons that call without ending the run. It works for streamed and
  collected calls alike, so steering does not force streaming.
- **U-2. The listener.** After the model request is recorded, and when the
  source is a notifier with a non-nil `Ready`, a goroutine waits on `Ready`,
  the call's end, and the run's context. On a signal it calls `Steers` and
  cancels the call when all of these hold:
  - a returned steer is urgent, valid, and not in the record;
  - `nextCallPossible(tokens)` holds on the state with the request recorded,
    which counts the call in flight as a used attempt and its worst case as
    at risk (hooks.md). So the reserved final-answer call is never cut short;
  - the call offers no provider-hosted tool unsafe to repeat (U-6).

  The listener only reads; it returns the IDs it saw, and the run merges them
  into its claims after the call, so `Release` covers them. Only one goroutine
  calls `Steers` at a time: the run is blocked in the call.
- **U-3. The race.** A call whose result is complete (a stop kind and no error)
  stands even if the cancel arrived: it is settled normally and the steer waits
  for the next delivery point. If the run's context also ended, the run's stop
  wins, as today.
- **U-4. The settlement.** The call settles with `Interrupted: "steering"`,
  keeping what the provider returned: response ID, partial output, and
  observed usage. Its charge is computed as for any interruption (decision
  24): reported counts where streamed, otherwise the input estimate and the
  visible output, marked estimated. A collected (non-streamed) call has no
  partial output, so it is billed at the input estimate. The run then loops:
  FR-6 records the steer, and the next call runs. No stop is recorded, and
  `Interruptions` is unchanged.
- **U-5. Counting (decision 32).** Replay counts a steering interruption toward
  `ModelAttempts` and spend. Every other interruption keeps decision 12: spend
  only. Each urgent interrupt costs an attempt, which bounds a flood of them,
  and `nextCallPossible` keeps the last answer.
- **U-6. Hosted tools.** A call offering a provider-hosted tool not known to be
  safe to repeat is not interrupted (decision 10): the steer waits for the
  next delivery point, and one `WarningSteering` per run says why.
- **U-7. When no interrupt is allowed.** When `nextCallPossible` refuses, the
  urgent steer waits like any steer, with one `WarningSteering` per run naming
  the limit.

### What the model sees next

- **U-8. Partial text.** Projection includes the text blocks of a
  steering-interrupted call (no tool calls, reasoning, or replay data, which
  may be incomplete), then a projected runtime reminder: "The user interrupted
  your previous response." The steer follows as a user message. With no
  partial text, nothing is added. This refines decision 9 ("never acted on")
  for steering only: nothing in the partial output runs, but the model sees
  what the user reacted to. Other interrupted calls stay out of projection.
- **U-9. `NextCallOnly`.** A steering-interrupted call does not expire
  `NextCallOnly` items, even though it reached the provider: its answer was
  discarded, so the call that replaces it is the "next call" for them.
  `modelDispatched` excludes it.
- **U-10. Answer and vetoes.** An interrupted call is never an answer: it is
  not actionable, so `Answer` and the finish point ignore it, and a veto that
  hides an earlier answer stays in force. Hooks run as usual for the next call
  (`BeforeModel`, `BeforeSend`); no hook runs for the interrupt itself.

### Tools in flight

- **U-11.** Running tools are never interrupted. During a tool batch the
  listener also runs, and the run checks `Ready` before each call's
  `BeforeTool` and start. Once an urgent steer is confirmed, no later call in
  the batch starts: requested and allowed calls settle not executed, reason
  `steering`, output "Not run: the user sent new input." Running calls finish;
  approvals and external waits stay pending. `nextCallPossible` does not gate
  this, since a model call follows the batch anyway. This is the last commit
  and the first to cut if the PR grows.

### Recovery and replay

- A crash after the cancel but before the settlement is recorded leaves an
  open request. Recovery settles it as `Interrupted: "worker"` (spend only,
  resent on resume, decision 9); the steer is still with its source and FR-6
  delivers it before the resend. No steering state needs recovery.
- A steering interruption is an ordinary settlement: replay needs no new step
  kind, only the cause. `canContinue` and the `Interruptions` limit are
  unchanged.
- Live consumers: the settlement step supersedes the attempt's previews
  (`Step.Settles`). Its cause tells a UI why the partial text stopped; whether
  to keep showing it is the UI's choice. Deltas stop at the cancel.

### Recorded-format changes

- `ModelFact.Interrupted` becomes `InterruptCause`, a string: `""` (not
  interrupted), `"worker"` (recovered after a crash), `"run"` (the run's
  context ended, its deadline passed, or a pause stopped the call before
  dispatch, with `NoDispatch`), or `"steering"`. Existing code comparing the
  bool compares against `""`; this touches `runner.go`, `state.go`,
  `project.go`, `budget_accounting.go`, `turn_content.go`, and six test files.
- `Steer.Urgent` is not recorded; the record shows the effect.
- A tool settlement from `allowed` to `not_executed` with reason `steering`
  (U-11).
- `decisions.md` 32 names `InterruptedBy: "steering"`; at merge it becomes
  `Interrupted: "steering"`, and U-8 is noted as refining decision 9.

## Plan and tests

One PR, in commits: the cause type (no behavior change); the notifier, the
listener, and the settlement (U-1 to U-7); projection (U-8, U-9); the tool
batch (U-11); docs, the guide section, and the example (`!` for urgent).

Tests:

- A scripted client that blocks until cancelled, streamed and collected: an
  urgent steer cancels it, the settlement has cause `steering`, the steer is
  delivered, and the next call sees the partial text, the reminder, and the
  steer, in that order.
- A call that completes as the cancel arrives stands.
- Counting: attempts and spend include the interrupt; `Interruptions` does
  not. The reserved final-answer call is never interrupted. A flood of urgent
  steers stops at `ModelAttempts` with an answer.
- Unsafe hosted tools, a non-urgent steer, and a source without a notifier do
  not interrupt; the warnings appear once.
- `NextCallOnly` items survive an interrupt; vetoes stay in force.
- Faults: crash after the cancel (recovered as `worker`, steer delivered once);
  rejected and unknown writes of the settlement.
- Tool batch: `Ready` between sibling tools at concurrency 1 and 3.
- `-race` on the listener and `Inbox.Ready`.
- Live (Anthropic, OpenAI Responses, Gemini): interrupt a long streamed answer
  and check that the provider accepts the partial text, the reminder, and the
  steer, and that the answer follows the steer.

## Open questions

1. `"run"` as the cause name for context end, deadline, and pause (the review
   dropped `pause` as a separate cause). Alternatives: `"context"`.
2. Should U-11 ship in this PR, or wait for demand?
3. `SendUrgent` as a separate method, or only `Send(Steer{Urgent: true})`?

## Risks

| Risk | Mitigation |
| --- | --- |
| Interrupts waste tokens | Opt in per steer; cost is counted and visible |
| A flood of urgent steers | Each costs an attempt; `nextCallPossible` keeps the last answer |
| A provider rejects partial assistant text | Text blocks only; live tests per provider |
| A slow `Steers` delays each call | Documented: must be cheap (memory, index) |
