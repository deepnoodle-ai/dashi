# Turn interruption and resumption

**Date:** 2026-09-27 · **Status:** Proposal for decision 6 in
[decisions.md](decisions.md)

A host runs a Turn on workers it does not fully control: deploys drain them,
heartbeats miss, leases expire, timeouts fire, processes die. None of these
means "the user wants this work to stop", yet Kernel records each as final
`StopCanceled`, so a deploy destroys the Turn. The owner asked: "Shouldn't the
backend be able to pause and resume a turn later, without undue hardship?" Yes.
This document shows how our production hosts do it and proposes the Kernel
model.

## How the production hosts run a turn today

Neither host keeps a goroutine alive for a turn's life. Each runs one goroutine
per *claim* for one segment, which exits when the turn waits, finishes, or loses
its lease. The durable record, not a live process, carries the turn to whichever
worker claims the next segment.

**nvoken-cloud (Dive v1.34).** The runner starts a goroutine per claim
(`internal/engine/runner.go:163-194`); in Cloud Tasks mode an HTTP request plays
that role (`adapters/executorhttp/server.go:113-115`). Segments are capped at 15
minutes (`runner.go:44-48`). A host-tool wait parks the turn with its lease
cleared, and the goroutine returns (`queries/turn_execution.sql:736-741`).

- *Fence.* A claim increments `attempt` and sets a 30 s lease
  (`turn_execution.sql:322-365`); every write checks owner, attempt, and an
  unexpired lease (`turn_execution.sql:700-703`).
- *Cancel.* A failed heartbeat cancels with a plain `context.Canceled`
  (`engine/claim.go:100`, `361-377`). Drain waits `DrainGrace` (30 s), then
  cancels (`runner.go:215-235`). Only the segment deadline has an inspected
  cause (`claim.go:115-127`). User cancel is a DB write plus a notification
  (`services/controls.go:271-304`); a user interrupt is a flag honored at the
  next checkpoint (`adapters/divegen/generator.go:1131-1139`).
- *Recovery.* The reaper requeues expired running turns, fenced on `attempt`
  (`turn_execution.sql:922-951`). The next worker rebuilds from transcript and
  checkpoints (`services/generation_recovery.go:32-250`), reruns open builtins
  (`generator.go:883-900`) under a newer attempt only (`toolcalls.go:269-276`),
  retries MCP calls only when annotated safe (`mcp_tool.go:240-245`), and issues
  an interrupted model call again. Moves are unlimited; only the time budget
  bounds them (`execution.go:205-210`).

**mobius-cloud (Dive v1.31).** The dispatcher runs a goroutine per claimed turn
(`internal/daemon/session_runtime.go:417-422`); a wait records a durable wait
and the goroutine exits (`session_runtime.go:563-576`). A claim increments
`lease_attempt` (`adapters/postgres/agent_turn_repo.go:1229-1239`); the lease is
2 min, renewed every 30 s (`session_runtime.go:198-199`).

- *Cancel.* Lease loss calls a cause-less `cancel()` (`session_runtime.go:507`,
  `939-942`); the executor maps any parent cancel to `cancelled`
  (`agentruntime/executors/agent.go:414-419`) and writes it through a salvage
  context (`agent.go:4079-4089`). A lost lease becomes a terminal cancel.
- *Drain* never cancels, "because cancelling would terminalize them as
  `cancelled` instead of leaving them retryable" (`session_runtime.go:262-268`,
  `466-469`): the terminal-cancel workaround in plain form.
- *Recovery.* The reaper requeues an expired running turn, fenced on
  `lease_attempt`, up to three times (`session_runtime.go:1339-1343`,
  `domain/agent_turn.go:78`). The next worker rebuilds from committed messages,
  adds a synthetic error for each dangling tool call
  (`services/session_transcript.go:455-463`), and reissues the model call.

**What they have in common.** These are the requirements Kernel must meet:

1. A turn outlives any goroutine; any claimant resumes it from the record, and a
   crash and a drain share one recovery path.
2. The context ends for reasons that are not the user's (drain, lease loss,
   timeouts). A user cancel is a separate action.
3. An interrupted model call is issued again: it costs spend but has no external
   effect. A tool call is rerun only when known to be safe.
4. Stale writes are rejected by an owner-and-attempt fence. (mobius-cloud's
   terminal writes check status only, `agent_turn_repo.go:826-830`, so a stale
   `cancelled` can land on a requeued turn.)
5. Drain lets in-flight work finish for a grace period, then stops it; the host
   bounds how often a turn may move.

## What Kernel does today

`Engine.Apply` already runs one segment: it takes the recorded Turn and a
command, runs to a stop, and any process may call it again. Requirements 1 and 4
are met; `Recorder.Commit` carries `Owner` and `ExpectedSeq`
(`recorder.go:11-15`). The failure is how Kernel treats the context ending:

- *Between steps* it records `StopCanceled` (`engine.go:422-423`;
  `tools_loop.go:47-48`, `168-169`, `213-214`), settling unstarted calls as not
  executed (`state.go:521-533`). The stop is final: `canContinue`, `Deliver`,
  and `Cancel` reject it (`state.go:604`; `engine.go:162`, `282`); only late
  results land (`state.go:129-135`).
- *During a model call*, the attempt settles as uncertain (`engine.go:590-593`),
  which blocks `Continue` (`state.go:297-298`, `604`) until the host proves
  non-dispatch (`engine.go:196-199`), a proof no host has for a stream it
  abandoned. *During a tool call*, started calls block `Continue`
  (`state.go:614-617`) and each needs an `Accept`. A crash leaves the same
  blocks without a stop.
- Breaking `Engine.Stream` cancels its context (`engine.go:42-53`), so a
  disconnected reader ends the turn (`engine_stream_test.go:98`).
  `Limits.Deadline` is checked only before a call (`engine.go:459-460`); cutting
  off a running call needs the host's context, which again cancels.

## Proposal

**Rule.** The `Cancel` command is the only terminal stop a caller causes. When
the Go context ends, the engine records a resting stop, `StopInterrupted`, and
the context's cause becomes its reason. `Continue` resumes the turn on any
owner. A crash is an interruption whose stop was never written.

```go
const StopInterrupted StopKind = "interrupted" // context ended; resumable

// ModelFact: the engine stopped reading this attempt. Its output is never
// acted on; any partial Result and Usage are kept for display and accounting.
Abandoned bool `json:"abandoned,omitempty"`

var ErrStreamStopped = errors.New("kernel: stream reader stopped")

func (s Stop) Final() bool // completed or canceled
```

`Stop.Reason` is `context.Cause(ctx).Error()`; hosts cancel with
`WithCancelCause` (`ErrLeaseLost`, `errDraining`) and already hold the typed
cause. Typed stop codes belong to theme 2.

**What the engine records**, by where the context ends:

- *Between steps or before a tool starts:* `turn_stopped{interrupted}`.
  Unstarted calls and pending approvals stay pending.
- *During a model call:* the attempt settles `Abandoned`, keeping any partial
  result, and counts toward `ModelAttempts` (before send it settles not
  dispatched, as today). Then stop interrupted.
- *During tool execution:* stop waiting, as today; started calls stay `started`,
  and results arriving later in this process are dropped (an error after
  cancellation is not a real outcome). Then stop interrupted.

**Abandoned is not uncertain.** A model call changes nothing outside the turn
but spend, and both hosts reissue it; the admission reservation stays the host's
spend fact. An attempt that advertised an acting provider-hosted tool, such as
an MCP connector, stays uncertain. **If the stop commit fails** under the
existing bounded, detached context (`engine.go:315-320`), `Outcome.Persistence`
says so and the record ends like a crash: correctness never depends on the
interrupted stop, which only explains the pause.

**The next owner** loads the turn; `Replay` shows the stop and pending work.

- *Continue* works on an interrupted turn and on a crashed record with no stop.
  It first adopts in-flight work: an open intent settles `Abandoned` (with the
  hosted-tool exception), and each started local call settles uncertain through
  the existing path (`tools_loop.go:62-69`). If anything is uncertain, the turn
  stops `uncertain`. Otherwise it runs unstarted calls, then the model. Limits
  do not reset. Adopting work is new: `canContinue` currently rejects an open
  intent and started calls (`state.go:604`, `614-617`).
- *Accept* records a started call's result when the host knows it, for example
  from an idempotency key. It also records approvals and model reconciliations.
  Once nothing blocks the turn, it runs as it would from `waiting`.
- *Deliver* is valid; *Cancel* is valid and final, leaving started calls open
  for late results.

This refines [turn.md](turn.md)'s "never blindly repeated" rule: it still holds
for tools, but reissuing a model call without acting hosted tools is the
recovery.

**Deadline.** `Limits.Deadline` is the turn's durable budget, so reaching it is
a limit. The engine derives `WithDeadlineCause(ctx, deadline, errTurnDeadline)`
and on that cause records `StopLimited{deadline}` under the same model and tool
rules. Any other context deadline (request timeout, segment ceiling) is the
host's and gives `StopInterrupted`. Raising a limit is decision 7.

**Stream.** Breaking `Engine.Stream` interrupts the turn with cause
`ErrStreamStopped`; it does not cancel it. A reader who wants the work stopped
sends `Cancel`. A reader who may simply disconnect should follow the record by
cursor while a worker runs `Apply` (decision 3).

**Fencing** is unchanged. On lease loss the host cancels with `ErrLeaseLost`;
the engine still attempts its bounded commits and the host's recorder rejects
them if a successor holds the turn. Kernel never skips writes by cause, so the
fence must check owner and attempt, not status.

**What a host does.**

- *Drain* is deliberate. Stop claiming; have `Policy.BeforeModel` return
  `Stop{Kind: StopInterrupted, Reason: "drain"}`, which already works
  (`engine.go:516-517`) and yields at the next model boundary with nothing in
  flight; after a grace period, cancel with `errDraining`; release the lease.
- *Crash* is involuntary: after the lease expires, the reaper requeues the turn
  and the next claimant calls `Continue`.
- *Moves.* Kernel counts nothing, but abandoned attempts consume
  `ModelAttempts`, so a turn that keeps crashing in model calls ends `limited`.
  Requeue caps stay in the host.

## Alternatives considered

**A `Pause` command.** A crashed worker cannot submit one, so the crash path is
needed anyway and `Pause` would be a second route to the same state; a draining
worker already yields through policy. **Keeping cancel terminal** forces hosts
never to cancel the context (mobius-cloud's drain) or to rewrite a final stop,
leaves timeouts destroying turns, and records lease loss as a user cancel.
**Waiting for in-flight tools in the engine** duplicates the grace period the
host already sets by delaying its cancel.

## Open questions

1. Should `Continue` rerun a started call annotated read-only or idempotent, as
   nvoken does for safe MCP tools? This needs theme 3's execution identity.
2. Should abandoned attempts count toward `ModelAttempts` or a separate
   interruption allowance, so a deploy storm cannot exhaust the model budget?
3. Which hosted tools count as acting, and can the definition say so?
4. Should `Accept` on an interrupted turn run the loop or await `Continue`?

## Migration

**Tests.** Context-cancel tests move from `StopCanceled` to `StopInterrupted`,
with unstarted calls left pending and a mid-stream partial settled `Abandoned`.
They are `cancellation_test.go:13`, `71`, `141`, and `160`, and
`engine_stream_test.go:98`, `145`, and `176`. The explicit `Cancel` tests
(`cancellation_test.go:124`, `hosted_recovery_test.go:42`) are unchanged. New
tests: `Continue` after an interrupt and after a crash with an open intent or a
started call, drain through `BeforeModel`, a fenced lease loss, and
`Limits.Deadline` cutting off a running call.

**Docs.** Update the `Stream` comment (`engine.go:30-34`),
`docs/agent-turns.md:140`, `docs/guides/running-turns.md:38`, and
`design/agent-loop.md:25` and `109-112`. In `design/turn.md`, update the
cancellation sentence and the rule against blind repetition.
