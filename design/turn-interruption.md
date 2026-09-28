# Turn interruption and resumption

**Date:** 2026-09-28 · **Status:** Decided (decisions 8–17 in
[decisions.md](decisions.md)); one question open

## The problem

An application runs a Turn inside a Go context. That context ends for many
reasons that have nothing to do with the user: a deploy restarts the server, the
process crashes, the server loses its claim on the turn (its *lease*, the
database row saying "this server is working on this turn") because a heartbeat
missed, or a request timeout fires. Today Dashi records every one of these as
`StopCanceled`, a final stop. A routine deploy therefore destroys every turn in
progress.

These events are common. The work should simply wait to be picked up again, on
any server, without alarming the user.

## How our production hosts handle this today

Neither of our two production applications on Dive keeps a goroutine alive for
a turn's whole life. A worker runs a turn until it finishes, waits (for an
approval, say), or loses its lease, then exits; the next worker rebuilds the
turn from the database record. Both converge on the same practices:

1. **Any worker can resume a turn from its record.** A crash and a deploy share
   one recovery path: a background sweeper requeues turns whose lease expired.
2. **The context ending is not a user decision.** A user cancel is a separate
   action. Dive treats a context end as a cancel, and both hosts pay for it:
   one never cancels on deploy, "because cancelling would terminalize them as
   `cancelled` instead of leaving them retryable"; the other records lease loss
   as a user cancel.
3. **A model call that was cut off is sent again.** It costs money but changes
   nothing in the outside world. A tool call is rerun only when it is known to
   be safe to repeat.
4. **Stale writes are rejected.** Each pickup increments an attempt number, and
   every write checks both the owner and that attempt number.
5. **Moves are bounded,** by a requeue count or the turn's time budget.

Dashi already meets points 1 and 4 (`Runner.Run` runs from the record;
`Recorder.Commit` carries `Owner` and `ExpectedSeq`). The gap is what happens
when the context ends.

## The rule

**By default, the Go context ending pauses the turn; it does not end it.** The
runner records the stop `StopInterrupted` ("interrupted"), and `Continue`
resumes the turn on any worker. A crash is an interruption whose stop was never
written; `Continue` handles both the same way. The stop's reason is the
context's cause (`context.Cause(ctx).Error()`), so an application that cancels
with `context.WithCancelCause(ctx, ErrDeploying)` sees "deploying" in the
record. Nothing about this status should alarm a user: it means "waiting to be
rescheduled".

A per-call option and a runner field let the caller say more:

```go
// closed: pause at the next safe point
out, err := runner.Run(ctx, turn, cmd, dashi.WithPause(pause))

type Runner struct {
    // ...agent and services...
    OnContextDone ContextDone // ContextInterrupt (default) or ContextCancel
}

const StopInterrupted StopKind = "interrupted" // resumable

// ModelFact.Interrupted: the runner stopped reading this model call before it
// finished. Partial output and usage are kept for display and accounting;
// they are never acted on.
Interrupted bool `json:"interrupted,omitempty"`
```

**Pause** is for graceful shutdown. When the channel closes, the runner stops
at the next safe point, before the next model call or before starting the next
tool call, and records `interrupted` with reason "pause requested". Work
already in flight is not cut off; if it outlasts the host's grace period, the
host cancels the context as before.

**OnContextDone** says what an ended context means. A server going down should
say interrupted, the default. A web handler whose client disconnected knows
the turn will never resume there, so it sets `ContextCancel`: an ended context,
including breaking out of `Runner.Stream`, then records a final `StopCanceled`,
exactly as a `Cancel` command would, with unstarted calls marked not executed.
Detecting this from the cause alone is not enough: `net/http` cancels a request
context with a plain `context.Canceled`, so the handler cannot attach a cause.
The recommended setup for long turns, a worker running the turn while the UI
follows the saved record (decision 3), keeps the default.

## What happens to work in progress

A turn alternates between calling the model and running the tools the model
asked for. What an interruption means depends on where the context ended.

**Between steps, or before a tool starts.** The turn records the stop. Tool
calls that had not started, and approvals still waiting, stay pending and run
after `Continue`.

**During a model call.** The call is recorded as interrupted, keeping whatever
partial text and token usage arrived so the UI can show it and billing counts
it. On resume the model call is simply sent again.

One exception: *provider-hosted tools*, tools the provider runs itself during
the model call, such as web search, code execution, or remote MCP connectors.
If the interrupted call may have run a hosted tool that is not known to be
read-only or idempotent (safe to repeat), resending it could repeat a real
action, so the call is *uncertain* (below) and is not resent. Dashi marks the
built-in hosted tools it knows are read-only, such as web search; a tool
definition's annotations can say so for others.

**During a tool call.** The runner stops waiting. Any result that arrives after
the context ended is dropped, because an error caused by the cancellation is not
a real outcome. The call stays marked as started with no result.

## What `Continue` does after an interruption or crash

`Continue` first resolves anything left in flight, then carries on:

- An interrupted model call is sent again, unless a hosted tool makes it
  uncertain.
- A local tool call that started but has no saved result is rerun if its
  definition is annotated `ReadOnly` or `Idempotent` (`tool/tool.go:33-43`).
- Any other such call is **uncertain**: the tool started but no result was
  saved, so Dashi cannot tell whether it ran. Running it again might send a
  second email or charge a card twice. The turn stops `uncertain`, and the
  application must report what happened.

The application reports with `Settle`: the tool's result if it can find it (for
example by looking up an idempotency key in the downstream system), or a
failure; for an uncertain model call, the result or evidence that it never ran.
Or it can `Cancel` the turn. `Settle` only records the report; `Continue`
resumes the turn. The same holds everywhere `Settle` is used, including an
approval or a tool result delivered to a waiting turn, which today carries on
immediately: one rule, and the request that records an approval does not run
the model inline. The report is committed before `Settle` returns, so a
forgotten `Continue` loses nothing; the turn stays stopped. To make that
visible, `Outcome` and the saved turn say when nothing blocks the turn and it
is ready to continue, so an application calls `Continue` and a host's sweeper
can find ready turns that no worker is running.

This refines [turn.md](turn.md)'s rule that work is "never blindly repeated": it
still holds for actions with uncertain effects, but repeating a model call, or
a tool declared safe to repeat, is the normal recovery.

## Limits and deadlines

**Interruptions have their own limit.** A new `Limits.Interruptions` counts
resumes after an interruption or crash, with a default of 10. Interruptions are
usually the hosting's fault, not the model's, so they do not consume
`ModelAttempts`: a model call recorded as interrupted is not counted as an
attempt, so its resend on resume uses the attempt the cut-off call would have
used. Its token usage and cost still count toward spend and budgets. A day of
deploys therefore cannot exhaust a turn's model budget. When a
`Continue` would exceed the limit, the turn stops as `limited` instead of
resuming, so a turn that crashes its worker every time cannot loop forever.

**The turn's deadline is a limit, not an interruption.** `Limits.Deadline` is
the turn's own time budget. The runner derives its context with a distinct
cause, so reaching the deadline stops the turn as `limited`. Any other context
deadline (a request timeout, a per-run time cap) belongs to the host and
interrupts the turn.

**If writing the stop fails,** the record looks like a crash, and `Continue`
recovers it the same way.

## What an application does

- **Deploy.** Stop picking up new turns, close each running turn's `Pause`
  channel, and after a grace period cancel the contexts of turns still busy.
- **Crash.** Nothing special: the sweeper requeues it; the next worker calls
  `Continue`.
- **Lease loss.** Cancel the context with a cause such as `ErrLeaseLost`. The
  runner still tries its final writes; the application's recorder rejects them
  if another worker already holds the turn. The store must check owner and
  attempt, not the turn's status.
- **Client disconnect in a request handler** that runs the turn itself: set
  `OnContextDone: ContextCancel`.

## Alternatives considered

**A `Pause` command.** A crashed worker cannot send one, so the crash path is
needed anyway. **Draining through `Policy.BeforeModel`**, which can already
return an interrupted stop: `Policy` is a four-method interface, so every app
would wrap whatever policy it has, and the hook fires only before model calls,
so a turn in a long tool loop would not pause until the grace period expired.
It still works but is not the documented path. **Keeping cancel final** forces
hosts never to cancel the context and records lease loss as a user cancel.
**Accepting late tool results for a short window** adds timing-dependent
behavior for little gain; rerunning safe tools covers the common case.

## Open questions

None for decision 6; naming of the ready flag is left to implementation.

## Migration

**Tests.** Context-cancel tests in `cancellation_test.go` and
`engine_stream_test.go` move from `StopCanceled` to `StopInterrupted`, with
unstarted calls left pending and a cut-off model call recorded as interrupted.
Explicit `Cancel` tests are unchanged. New tests: `Continue` after an
interruption and after a crash, with an interrupted model call, a started safe
tool, and a started unsafe tool; a hosted tool of unknown safety; the `Pause`
channel during a model call and during a tool loop; `ContextCancel` with a
broken `Stream`; `Settle` on an interrupted turn; repeated interruptions
during model calls, which reach the interruption limit while `ModelAttempts`
is unchanged and usage still accumulates; a
deadline cutting off a running call; and a lease loss rejected by the store.

**Docs.** Update the `Runner.Stream` comment, `docs/agent-turns.md`,
`docs/guides/running-turns.md`, `design/agent-loop.md`, and the cancellation
and repetition rules in `design/turn.md`.
