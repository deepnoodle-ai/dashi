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
// Optional: sources that can wake a run, such as Inbox. Ready is a
// coalesced hint that an urgent steer may be waiting, for the one run the
// source serves. Signal only after the steer is visible to Steers.
type SteeringNotifier interface {
    Ready() <-chan struct{} // the same channel on every call
}

type Steer struct {
    Item   ContextItem
    Urgent bool // may cut short the model call in flight
}

func (in *Inbox) Ready() <-chan struct{}
func (in *Inbox) SendUrgent(text string) (string, error)
```

`SendUrgent`, and `Send` with `Urgent` set, signal `Ready`: a channel with a
buffer of one, created lazily under the mutex so `Inbox{}` works, so signals
coalesce and a send never blocks. `SendText` does not signal. A source
without a notifier gets no interrupts; the first urgent steer FR-6 delivers
from it records one `WarningSteering` per run saying so.

```go
if rest, ok := strings.CutPrefix(line, "!"); ok { // "!stop, wrong file"
    inbox.SendUrgent(rest)
} else {
    inbox.SendText(line)
}
```

### The listener

- **U-1. One listener per run.** When the source is a notifier with a non-nil
  `Ready`, the run starts one goroutine, the only reader of `Ready`. The run
  serializes all its calls to the source with a mutex, so FR-6 and the
  listener never call `Steers` at once. On every exit path the run cancels
  the listener's context, then joins it, so a stuck `Steers` cannot hold up a
  finish; then it merges the IDs the listener saw into its claims, before
  `Release`.
- **U-2. One definition of "waiting".** A pure function
  `classifySteers(record, steers) (fresh, again, invalid)` serves FR-6 and the
  listener, so both agree on "valid and not in the record". The listener
  holds a snapshot of the record's IDs, refreshed by the run after each
  commit. It never writes run state or warnings; it returns what it saw, and
  the run records warnings after joining.
- **U-3. What a signal does.** On a signal, the listener calls `Steers`. For
  a fresh urgent steer, it adds the ID to the run's *urgent set* and cancels
  the registered model call if it is eligible (U-4). Each ID causes at most
  one interrupt. FR-6 removes an ID from the set when it records the steer or
  when the source no longer returns it.
- **U-4. Eligibility.** A call offering a provider-hosted tool unsafe to
  repeat (decision 10) is never cut. Otherwise, before the call starts, the
  run registers its cancel function and captures from the record the inputs
  of `replacementFits`: spend so far, each cap, the call's worst case, and
  prices. `replacementFits` is hooks' `nextCallPossible` without the attempt
  term (U-8), with the call in flight at its worst case against every cap.
  At signal time the listener does plain arithmetic on those inputs plus the
  actual size of the steers, the reminder, and the partial so far, and
  rechecks pause, deadline, and context; it never reads replay state. An
  ineligible call is not cut; the steer waits for FR-6, and the run records
  one `WarningSteering` per run naming the reason.
- **U-5. Before dispatch.** After `BeforeModel` and request preparation, but
  before `BeforeSend` admits the call (an admission may hold outside budget),
  the run reads the urgent set. If it holds an ID that has not yet caused a
  loop-back, the run drops the pass's hook results (`dropHooks(mark)`) and
  loops back to FR-6. Each ID loops back at most once.

### Interrupting a model call

- **U-6. Its own context.** Each model call runs under a context derived from
  the run's, cancelled with a private cause `errSteered`. It works for
  streamed and collected calls alike. Deltas stop at the cancel: the stream
  loop emits and accumulates partial text only while the call context is
  live, so the partial is what the user saw; later frames still count toward
  usage and the output estimate.
- **U-7. Settlement order.** After the call returns:
  1. The run's context ended: cause `run`, as today; the run's stop wins.
  2. The result is complete (no error, a stop kind, `validateModelResult`
     passes): the call stands and the steer waits for FR-6. A complete result
     that stops the turn (an `output` or `context` limit) ends it `limited`,
     and the steer goes to `Leftovers`.
  3. The call context's cause is `errSteered`: cause `steering`, whatever the
     error. It is `NoDispatch` only if the client reports
     `FailureNotDispatched` or the cancel preceded the hand-off to the client;
     a missing response proves nothing (a collected call has none until it
     ends). Any other cut call counts as sent (U-9). The settlement never sets
     `Failure: FailureNotDispatched`: replay rejects that when interrupted.
  4. Otherwise, today's error path.
- **U-8. Counting.** A call cut off by a steer does **not** count toward the
  `ModelAttempts` cap. It counts toward spend and every cost cap, and it is
  recorded as an attempt (request and settlement steps). Replay needs no
  change: it already leaves interrupted calls out of `ModelAttempts`. Each
  interrupt needs a new urgent steer, so the user's input bounds them. With
  the defaults (8 attempts, the last reserved), three interrupts still leave
  the agent all six tool rounds. Since the call uses no attempt, even the
  reserved final-answer call may be cut and asked again. This amends decision
  32, which counted steering interrupts as attempts. `Interruptions` is
  unchanged.
- **U-9. Billing.** The settlement keeps what the provider returned: response
  ID, partial output, observed usage. The output estimate counts the
  characters of every streamed delta (text, reasoning, tool arguments), not
  only text; this helps every interrupted call. Under a `MaxCost` cap, a
  sent steering interrupt with no *final* usage is charged the worst case
  reserved at admission, written into the settlement's `Charge` (which
  `attemptSpend` prefers, so replay needs no cap logic). Early frames are not
  final (Anthropic reports about one output token up front), and a cut stream
  never has final usage, so the cap stays hard however often the user
  interrupts. Without a cap, decision 24's estimate applies. Crash recovery
  keeps decision 24's rule; this exemption is for steering interrupts only.
- **U-10. Clients must stop remote work.** `llm.Client` documents that ending
  the call's context must stop generation at the provider. A client that
  cannot, such as one running background jobs, must not report usage as
  final for a cancelled call.

### What the model sees next

- **U-11. Partial text.** Projection includes the partial text of a
  steering-interrupted call as one assistant text block: its text parts
  joined, trailing whitespace trimmed, with no citations, tool calls,
  reasoning, or replay data. A partial that is empty or only whitespace adds
  nothing (Anthropic rejects whitespace-only text). A contextual reminder
  named `steering_interrupted` follows: "The user interrupted your previous
  response." It is projected, not recorded, like "Tool was not executed.";
  changing its text changes how older turns project, which matters only for
  prompt caching. The steer follows as a user message. This refines decision
  9 for steering only: nothing in the partial runs. Other interrupted calls
  stay out of projection.
- **U-12. `NextCallOnly`.** A steering-interrupted call does not expire
  `NextCallOnly` items: its answer was discarded, so its replacement is their
  next call. `modelDispatched` excludes it. A hook that delivers a per-call
  item without `Snapshot` may then show it twice; the hooks guide says to use
  `Snapshot` for per-call values.
- **U-13. Answers and vetoes.** An interrupted call is never an answer: it is
  not actionable, so `Answer` and the finish point ignore it, and a veto that
  hides an earlier answer stays in force. Hooks run as usual for the next
  call; none runs for the interrupt itself.

### Recovery and replay

A crash after the cancel but before the settlement leaves an open request.
Recovery settles it with cause `worker` (decision 24's rule, resent on resume);
with a durable source the steer is still waiting and FR-6 delivers it before
the resend. A steering interrupt is an ordinary settlement: replay needs only
the cause, and `canContinue` and `Interruptions` are unchanged. Live
consumers: the settlement closes the attempt's preview (`Step.Settles`), and
its cause tells a UI why the text stopped.

### Recorded-format changes

- `ModelFact.Interrupted` becomes `InterruptCause`
  (`json:"interrupted,omitempty"`), a string type with constants
  `InterruptNone` (`""`), `InterruptWorker` (recovered after a crash),
  `InterruptRun` (the run stopped the call: its context ended, the turn
  deadline passed, or a pause arrived before dispatch), and, with the
  listener, `InterruptSteering`. Replay rejects an unknown cause.
- `Steer.Urgent` is not recorded; the record shows the effect.
- At merge, `decisions.md` 32 says steering interrupts count toward spend and
  cost caps but not `ModelAttempts`, are charged the reserved worst case under
  a cost cap (an exemption from decision 24), and that U-11 refines decision 9.

### Next PR: tools not yet started

Running tools are never interrupted. In a PR right after this one, the tool
loop reads the run's urgent set (never `Ready` or `Steers`) before each
call's `BeforeTool` and start. Once it is non-empty, no later call in the
batch starts: requested calls settle not executed (an existing transition),
and allowed calls gain a new transition from `allowed` to `not_executed`, both
with reason `steering` and output "Not run: the user sent new input." Running
calls finish; approvals and external waits stay pending. `nextCallPossible`
does not gate this, since a model call follows the batch anyway.

## Plan and tests

One PR, in commits: the cause type (no behavior change); the listener,
eligibility, and the settlement; billing; projection; docs, the guide, and
the example (`!` for urgent).

- A client that blocks until cancelled, streamed and collected: the steer
  cancels it, the cause is `steering`, and the next call sees the partial
  text, the reminder, and the steer, in order. Whitespace-only and
  trailing-newline partials; citations stripped.
- A result that completes as the cancel arrives stands. A cancel before
  dispatch settles `NoDispatch`. A buffered signal skips the request before
  `BeforeSend` runs (U-5); a steer the source drops loops back only once.
- Counting: interrupts leave `ModelAttempts` and `Interruptions` unchanged and
  add to spend; the reserved final-answer call can be cut and asked again;
  one ID interrupts at most once.
- Billing: reasoning deltas raise the estimate; under a `MaxCost` cap with no
  final usage (also after an Anthropic-style early usage frame), `Charge` is
  the worst case and the cap holds across many interrupts; a cut collected
  call is charged, not `NoDispatch`.
- No interrupt from unsafe hosted tools, a non-urgent steer, or a source
  without a notifier; each warning appears once.
- `NextCallOnly` items survive an interrupt; vetoes stay in force.
- Faults: a crash after the cancel (recovered as `worker`, steer delivered
  once); rejected and unknown settlement writes; a durable source whose
  `Steers` blocks until its context ends (the run still ends and releases).
- `-race` on the listener, the source mutex, and `Inbox.Ready`, including
  `var in Inbox; in.Ready()`; `Send(Steer{Urgent: true})` signals and
  `SendText` does not.
- Live (Anthropic, OpenAI Responses, Gemini, and one Chat Completions route):
  interrupt a long streamed answer, including after an Anthropic
  `pause_turn`, and check that the provider accepts the partial text, the
  reminder, and the steer, and that the answer follows the steer. On
  Anthropic, also cut a call whose partial ends in whitespace.

## Risks

| Risk | Mitigation |
| --- | --- |
| Interrupts waste tokens | Opt in per steer; charged, and capped by cost caps |
| A client keeps generating after the cancel | U-10's client contract; worst-case charge under a cap |
| A provider rejects partial assistant text | U-11's trimming; live tests per provider |
| A slow `Steers` delays each call | Documented: must be cheap; cancelled when the run ends |
| A flood of urgent steers | One interrupt per ID; without a cost cap, only user input and the turn deadline limit them |
