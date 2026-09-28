# PRD: Steering a running turn

**Status:** Part 1 merged (#52); part 2 (urgent steers) implemented, except
skipping tools not yet started ·
**Date:** 2026-09-28 · **Owner:** Curtis · **Decision:** 32 · **Builds on:**
[hooks](hooks.md)

## Problem

People type while an agent works: "stop, wrong file". Part 1 delivers such a
message, a *steer*, at safe points. But a model call can stream a wrong answer
for a minute. Part 2 lets an *urgent* steer cut that call short.

## Part 1 (merged, unchanged)

The turn record is the authority: a steer ID is recorded at most once, and
sources present a steer until `Delivered`. `Inbox` claims steers per run and
keeps writes of unknown outcome in doubt; `Leftovers(turn)` returns unrecorded
steers. Steers are delivered before each model call (FR-6, one
`StepDelivered` before `BeforeModel`) and at a candidate answer when
`nextCallPossible` holds, never between a tool call and its result. A steer is
a nudge, never a veto. Non-goals: queueing follow-up turns, cancelling running
tools, waking a waiting turn, rewriting steer text.

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
buffer of one, created lazily under the mutex so `Inbox{}` works. `SendText`
does not signal. A source without a notifier gets no interrupts; the first
urgent steer FR-6 delivers from it records one `WarningSteering` per run. The
example sends lines starting with `!` as urgent.

### The listener

- **U-1. One listener per run.** When the source is a notifier with a non-nil
  `Ready`, the run starts one goroutine, the only reader of `Ready`. A source
  lock serializes all calls to the source. The listener's `Steers` runs
  under a context cancelled when the model call settles, when the run needs
  the source or the listener's decision, or when the run ends; a fetch the
  run cut short is retried. A slow source can delay a cut, never stall the
  run. On every exit path the run cancels the listener's context, joins it,
  and merges the IDs it saw into its claims before `Release`.
- **U-2. One definition of "waiting".** A pure
  `classifySteers(record, steers) (fresh, again, invalid)` serves FR-6 and the
  listener. The listener holds a snapshot of recorded IDs, refreshed after
  each commit. A separate state lock, never held while calling the source,
  guards the snapshot, the urgent set, the registered call, and the reason
  the listener last declined to interrupt; commits take only it. The
  listener writes no warnings; the run attaches that reason to the call's
  `StepModelSettled`, or at the latest to the stop step.
- **U-3. What a signal does.** The listener calls `Steers`. For a fresh
  urgent steer, it adds the ID to the run's *urgent set* and cancels the
  registered model call if eligible (U-4). Each ID causes at most one
  interrupt. FR-6 removes an ID from the urgent set when it records the steer
  or when the source no longer returns it.
- **U-4. Eligibility.** The run registers the call after preparation,
  before `BeforeSend`. Until the hand-off nothing was sent, so a cut is free
  and always allowed. After it, a call offering a provider-hosted tool
  unsafe to repeat (decision 10) is never cut, and otherwise a replacement
  must fit: the run hands the listener a private replay taken at the
  hand-off, and `replacementFits` is `nextCallPossible` without the attempt
  term (U-8). Token caps use recorded spend (the output cap already clamps
  `max_tokens`). The cost cap also counts the call in flight at its worst
  case, so a call with unknown worst case (`AtRisk` nil) is ineligible. At
  signal time the listener adds the size of the steers and the reminder, and
  rechecks pause, deadline, and context; the partial is covered by the
  call's `max_tokens`. An ineligible call is not cut; the steer waits for
  FR-6, with one `WarningSteering` per run naming the reason, unless the run
  is ending.
- **U-5. Before dispatch.** After `BeforeModel` and request preparation, but
  before `BeforeSend` admits the call, the run reads the urgent set. The run
  also keeps a *loop-back set*: IDs that already caused a loop-back. If the
  urgent set holds an ID not in the loop-back set, the run adds it there,
  drops the pass's hook results (`dropHooks(mark)`), and loops back to FR-6.

### Interrupting a model call

- **U-6. Its own context.** Each model call, streamed or collected, runs under
  a context derived from the run's, cancelled with a private cause
  `errSteered`. The stream loop emits and accumulates partial text only while
  that context is live; later frames still count toward usage and the
  estimate. The run marks the attempt *handed off* just before it calls
  `llm.Stream`.
- **U-7. Settlement order.** After the call returns:
  1. The run's context ended: cause `run`, as today.
  2. The result is complete (no error, a stop kind, `validateModelResult`
     passes): the call stands and the steer waits for FR-6. If that result
     stops the turn (an `output` or `context` limit), the steer goes to
     `Leftovers`.
  3. The call context's cause is `errSteered`: cause `steering`. It is
     `NoDispatch` only if the cancel came before the hand-off mark or the
     client reports `FailureNotDispatched`; a missing response proves
     nothing. Every other cut call counts as sent. The settlement never sets
     `Failure: FailureNotDispatched`, which replay rejects when interrupted.
  4. Otherwise, today's error path.
- **U-8. Counting.** A call cut off by a steer does **not** count toward
  `ModelAttempts` (replay already skips interrupted calls; no change). It is
  recorded as an attempt and counts toward spend and every cost cap. Each
  interrupt needs a new urgent steer, so user input bounds them, and even the
  reserved final-answer call may be cut and asked again. This amends decision
  32, which counted steering interrupts as attempts. `Interruptions` is
  unchanged.
- **U-9. Billing.** The settlement keeps the response ID, partial output, and
  observed usage. The output estimate counts every streamed delta (text,
  reasoning, tool arguments). Under a `MaxCost` cap, every sent call cut off
  by a steer is charged the worst case reserved at admission, written into
  the settlement's `Charge` (which `attemptSpend` prefers, so replay needs no
  cap logic). Without a cap, decision 24's estimate applies. For every
  interrupt cause, `Result.Usage.OutputTokens` records the larger of the
  reported count and the delta estimate (capped at `max_tokens`), so token
  caps and replay see it with no new field; the settlement is
  `EstimatedUsage`. Crash recovery keeps decision 24's rule.
- **U-10. Clients must stop remote work.** `llm.Client` documents that ending
  the call's context must stop generation at the provider.

### What the model sees next

- **U-11. Partial text.** Projection includes a steering-interrupted call's
  partial as one assistant text block: text parts joined, trailing whitespace
  trimmed, with no citations, tool calls, reasoning, or replay data. An empty
  or whitespace-only partial adds nothing, reminder included (Anthropic
  rejects a blank message, and the reminder would describe nothing). A
  projected, unrecorded contextual reminder named `steering-interrupted`
  follows ("The user interrupted your previous response."; reminder names
  allow no underscore), then the steer as a user message.
  This refines decision 9 for steering only. Other interrupted calls stay out
  of projection.
- **U-12. `NextCallOnly`.** A steering-interrupted call does not expire
  `NextCallOnly` items; `modelDispatched` excludes it. The hooks guide says
  to use `Snapshot` for per-call values.
- **U-13. Answers and vetoes.** An interrupted call is never an answer:
  `Answer` and the finish point ignore it, and a veto that hides an earlier
  answer stays in force. No hook runs for the interrupt itself.

### Choices made in the code

- U-5's check runs where the call is registered. A cut before the request
  is recorded goes back to FR-6 and records nothing, so `BeforeModel` and
  `BeforeSend` may run again for the same reply; hosts reserve spending in
  the recorder with the intent.
- An answer's provider-pause chain restarts after a steering interrupt, so
  text from before the steer is never joined to the answer after it.
- A provider pause that leaves a hosted call unfinished (an Anthropic
  `pause_turn` during web search) is treated like a tool call awaiting its
  result. FR-6 holds steers and the listener does not cut the continuation,
  since providers reject a request that does not end with the paused
  result. The steer lands after the continuation.
- Under `MaxOutputTokens`, a cut late in a long call can leave the
  replacement a small clamp. Accepted for now; the fix would publish the
  streamed estimate for `replacementFits` to subtract.

### Recovery and record format

A crash after the cancel but before the settlement leaves an open request;
recovery settles it with cause `worker` and resends it, and a durable source
still presents the steer for FR-6. The settlement closes the attempt's preview
(`Step.Settles`), and its cause tells a UI why the text stopped.

- `ModelFact.Interrupted` is an `InterruptCause`
  (`json:"interrupted,omitempty"`): `InterruptNone` (`""`),
  `InterruptWorker` (recovered after a crash), `InterruptRun` (the run's
  context ended, the deadline passed, or a pause arrived before dispatch),
  and, with the listener, `InterruptSteering`.
  Replay rejects an unknown cause.
- `Steer.Urgent` is not recorded; the record shows the effect.
- `decisions.md` 32 records U-8, U-9's exemption from decision 24, and
  U-11's refinement of decision 9.

### Next PR: tools not yet started

Running tools are never interrupted. Next, the tool loop reads the urgent set
before each call's `BeforeTool`. Once it is non-empty, no later call in the
batch starts: requested calls settle not executed, and allowed calls gain a
transition from `allowed` to `not_executed`, both with reason `steering` and
output "Not run: the user sent new input." Approvals and external waits stay
pending.

## Plan and tests

- Cut calls, streamed and collected: cause `steering`; the next call sees
  partial, reminder, and steer, in order; blank partials add nothing;
  citations stripped.
- A result completing as the cancel arrives stands; a cancel before hand-off
  is `NoDispatch`; a buffered signal skips the request before `BeforeSend`;
  a dropped steer loops back once.
- Counting and billing: attempts unchanged, spend grows; the final-answer
  call can be cut; one interrupt per ID; reasoning deltas raise the
  estimate; under `MaxCost`, `Charge` is the worst case and the cap holds.
- No interrupt from unsafe hosted tools, a paused hosted call, a non-urgent
  steer, or a source without a notifier; each warning once. `NextCallOnly`
  items survive; vetoes hold.
- Faults: a crash after the cancel; rejected and unknown settlement writes;
  a `Steers` that blocks. `-race` on the listener, source lock, and `Inbox`.
- Live (Anthropic, OpenAI Responses, Gemini, one Chat Completions route): cut
  a long streamed answer; the provider accepts partial, reminder, and steer.
  After an Anthropic `pause_turn` with an unfinished search, the cut is
  declined with a warning and the steer lands after the continuation.

## Risks

| Risk | Mitigation |
| --- | --- |
| Wasted tokens; generation continues after cancel | Opt-in; U-10; charged, capped |
| A provider rejects partial text | U-11's trimming; live tests |
| Slow `Steers` or a flood of urgent steers | Own lock; one interrupt per ID |
