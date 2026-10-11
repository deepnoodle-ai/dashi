# Admission, initial context, and saved model failure facts

Status: Accepted  Author: Codex  Date: 2026-10-11\
Workflow: spec, independent review checkpoint, then build.

## Context and goals

The [PRD](../prds/swarm-turn-inputs.md) replaces paused admission and initialization hooks.
It also preserves useful model failure facts through restart.
The design reuses Start, Recorder, ContextItem, and Next.
It preserves application ownership, scheduling, tool execution, and recovery authority.
It changes no Swarm code, account rules, or recorder interfaces.

## Admission and retry matching

Add `Runner.Admit(context.Context, Turn, Start, ...RunOption) (RunResult, error)`.
Share `Run`'s validation, recorder setup, owner forwarding, deduplication, and error handling.
Admit validates all accepted inputs, saves `StepTurnStarted`, then saves
`StepTurnStopped` with `StopReady = "ready"`. It returns before the execution loop.
It performs no hooks, tool discovery, steering calls, content loading, Prepare, or Send.
Configured hook names may be validated and recorded as existing Start metadata.
Execution controls such as pause and streaming do not convert admission into execution.

`Next` maps StopReady to NextContinue. Replay permits a ready stop only before any
execution work. Continue from it consumes no resume. Continue and Cancel retain
their existing client checks, limits, history checks, and late-result behavior.
A repeated Start through either Run or Admit acknowledges accepted work.
The caller uses a new Continue command to execute it.

Resolve duplicate admission before consulting current clients or defaults:

1. Replay the supplied snapshot and, when Recorder implements Loader, load its saved Turn.
   A load failure other than `ErrTurnNotFound` rejects the call.
2. Find the original Start by command ID. Resolve the incoming Start's zero/default
   Agent fields against that Start's recorded Agent, including limits, version,
   and active-compaction defaults. Preserve explicitly supplied values.
3. Compare the resulting Start hash with the saved command hash. Input, history,
   schema, parent, origin, compaction terms, and initial context still participate.
   A mismatch conflicts. Never accept a new ID as a retry.
4. Return the latest saved outcome, including its original resolved model and window.
   Do not validate today's Client or re-read its catalog on this path.

New admissions resolve ordinary defaults once. Without a Client, provider, API,
and model must be explicit. An enabled ActiveCompactionPolicy additionally needs
an explicit valid Window. Use that configured bound as the resolved window.
With a Client, retain the current smaller-of-policy-and-catalog rule.
Save the result only in existing `StartFact.ActiveCompactionWindow`.
`Agent.ActiveCompaction.Window` remains the configured cap. The fact is the selected bound.
Absent or disabled protection and StartCompaction record zero as today.
No second window field or ModelInfo copy is necessary.
Document that an explicit window comes from application destination evidence,
and does not authorize an endpoint or a model.

The two commits retain existing partial-write semantics. If only Start survives,
the missing stop gives NextRecover. An exact retry returns that saved state.
It does not repair records or execute work. The host reloads after uncertain writes,
then chooses Continue or Cancel under its existing worker ownership rule.

## Initial context

Add `Context []ContextItem` to Start and StartFact, with JSON `context,omitempty`.
Validate the entire batch with existing ContextItem rules before the first commit.
Reject repeated nonempty IDs. IDs remain optional. Nil and empty slices omit
the field, preserving existing Start hashes.
Include the complete ordered items in new Start hashes and deep-copy their messages.
Store them atomically with input on the Start step.

Set that Start step's existing `Version` to 2 whenever Context is nonempty,
whether Run or Admit records it. Older execution readers reject version 2 before replay.
Updated replay accepts version 1 without initial context and version 2 only
for a Start with nonempty initial context. Reject unknown versions, a version-1
Start carrying context, and a version-2 Start with missing or empty context.
An older mirror cannot strip the items and leave an accepted version-2 Start.
Other steps retain version 1.
Step.Version is outside the command hash. Nil and empty context keep their old hashes.
This protects ordinary Run records and a partially written admission containing only Start.

Extend `Step.Items()` to return a Start's items. Replay then applies existing
ID tracking to initial and later additions. Track named initial items at the
Start sequence for Context lookup and NextCallOnly expiry.
Project items immediately after Start.Input using the existing context-message
identity, role, snapshot, and lifetime rules. TurnView.Input includes initial
ContextUser messages, as it includes later application-delivered user items.
Other initial items remain context. Authority comes from ContextItem.Origin,
independently of Start.Origin. Continue rebuilds these facts instead of adding them again.
Named initial context stays protected during active compaction like later named items.

## Safe failure metadata

Add an optional `Failure *Failure` to ModelFact, Stop, and TurnView.
Failure contains `Category llm.ErrorCategory`, `Reason llm.ErrorReason`, `Status int`,
`SendStatus llm.SendStatus`, `Retryable bool`, `RetryAfter time.Duration`, and
`RecordedAt time.Time`. JSON keys follow the existing snake-case field convention.
It contains no arbitrary strings from Code, Body, Cause, Provider, or endpoints.
Use known category/reason values and a valid HTTP status. Unknown classification
becomes empty. Negative delay becomes zero. Retryable remains advice, never permission.

For failed sends, save metadata on the existing model finish.
Derive SendStatus through current `sendStatusOf`, after folding partial output and usage.
It must agree with ModelFact.SendStatus and ResultUnknown.
Keep ErrorReason for old consumers, consistent with Failure.Reason.
For Prepare errors, save metadata on the failed stop with SendStatusNotSent.
No model call began, so add no model-start/finish record or spend.
An untyped Prepare/Send error records unclassified metadata and a fixed safe summary.
Typed errors use a fixed summary containing only validated classification and status.
Apply the same safe summary to the associated Stop.Reason and ModelFact.Error.
Historical strings stay unchanged.

At commit, set Failure.RecordedAt from the enclosing Step.At.
Replay checks equality and validates metadata's location and send evidence.
TurnView exposes the failure causing its current stopped run, from its preparation
stop or model finish. It clears that projection when a new Continue starts,
execution succeeds, or a host resolution replaces the failed model outcome.
Old records expose nil rather than inventing classification or retry timing.
Deep-copy metadata when returning a Turn or hook view.

Next, ReadyToContinue, Pending, and accounting continue to use their existing facts.
Metadata cannot clear an unknown effect. A partial result defeats a rejection label.
The application saves `RecordedAt.Add(RetryAfter)` and its retry policy in its own storage.
Dashi neither sleeps nor schedules retries.

## Alternatives, tradeoffs, and rollout

The closed-pause path needs no new stop, but mislabels queued work as interrupted.
A separate Admit command duplicates Start identity. Initial AddMessages is reusable,
but splits accepted input across writes. Persisting llm.Error retains more details,
but arbitrary provider fields can contain secrets. Automatic retry hides host policy.
The chosen shape adds one stop and optional facts while preserving those boundaries.
An optional initial-context field alone permits silent omission by older readers.
Reuse the existing step version to reject unsupported semantics before execution.
Versioning every unchanged step would create a larger compatibility boundary without additional protection.

No migration is needed. Updated replay accepts existing version-1 records and
new version-2 Starts. Older execution readers reject these Starts and StopReady.
Deploy support to every worker, view/history builder, and saved-command consumer
before any writer emits initial context or ready stops.
Follow and Tail remain raw readers that check cursor identity and sequence.
They forward records without interpreting versions. Their consumers must upgrade
replay/view support before using those records for execution or presentation.
Old command JSON decoders may ignore new fields. Do not enqueue context-bearing
Starts where an older consumer can decode or execute them.
Enable new writers only after applications establish that deployment boundary.
Mixed-version execution is unsupported. New failure metadata remains optional advice.
Keep the closed-pause behavior for existing callers, while guides use Admit for new code.

Implementation owns `command.go`, `run_` admission/model paths, `turn_` facts,
replay, views, validation, and the initial-item hunk in `history_turn_messages.go`.
Coordinate that hunk with the history track. Keep version gating in replay.
The result-conversion track owns the shared Follow/Tail loop and preserves its raw-reader behavior.
Do not modify history policy, compaction/, feed/, mcp/, or run_follow.go.

Verify owner forwarding, no external calls, stale snapshots, loader failures,
lost acknowledgments at both admission writes, old hashes, changed defaults,
context authority/IDs/lifetime, JSON reload, 429 timing, preparation failures,
unknown/partial sends, host resolution, and secret-bearing error fixtures.
Use a pinned legacy reader harness to reject a serialized context-bearing Start
from ordinary Run before any model work. Repeat with an admission's Start-only prefix.
Verify version-1 legacy records, nil/empty-context hashes, and supported version-2 replay.
Reject downgraded context-bearing Starts, stripped/empty-context version-2 Starts,
and unknown versions before execution. Check that raw Follow/Tail transport remains unchanged.
Commit short offline Example tests, update running/recovery/steering guides and
CHANGELOG, then run focused race tests and `make check`.
Independent implementation and security reviews check recorded input and failure safety.
The planning checkpoint accepted this design. Implementation awaits the orchestrator's dispatch.
