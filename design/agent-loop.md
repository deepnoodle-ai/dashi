# Agent definition and loop

**Status:** Local loop implemented; host and live provider qualification pending · **Date:** 2026-09-27
**Workflow:** Specified, then built. Public Go signatures remain provisional.

## Shape and ownership

An **agent definition** is immutable reusable behavior: identity/revision,
instructions, model choice and settings, tool source, and turn-wide limits. It
contains no mutable history, recorder, observer, or tenant credentials. One
definition may serve concurrent turns. An **engine** runs one turn through
ordinary Go code using supplied runtime bindings. It owns the order of model
calls and client tool calls, while `llm` owns one prepared model call and `tool`
owns tool definitions and outcomes. A simple facade may assemble an in-memory
engine; a session runner may add history, storage, and claims. Neither changes
the loop's semantics.

The initial package shape is a small root `kernel` package for definition,
turn, commands, outcomes, policy, engine, and private replay validation, with
the existing `llm`, `tool`, and `content` packages below it. A later `session`
package can compose the same engine with history and a store. Dependencies
point toward `llm`/`tool` and never back into the engine from them.

```text
definition + turn record + command + bindings
                  │
                  ▼
       validate → project → prepare → authorize/record → send
                  ▲                                  │
                  └──── settle/record ← tool batch ←┘
```

The engine dispatches model calls and local tools by default. It receives an
acknowledged recorder so an application can make each effect boundary part of
its transaction. `tool.KindExternal` returns identified pending work for a host
worker; `Accept` delivers the later result. The state transition logic should
remain isolated internally, but a public host-driven model-dispatch protocol is
deferred until a real consumer demonstrates that the recorder and external-tool
seams cannot meet its needs. This chooses the [conceptual model's](../tmp/research/design/2026-09-26-dive-v2-conceptual-model.md)
smaller engine boundary while retaining the [companion design's](../tmp/research/design/2026-09-26-dive-v2-conceptual-design.md)
intent-before-effect constraint.

## One iteration

1. Replay and validate the turn. Apply the command through the recorder.
   Invalid commands fail before effects; a rejected or unknown write stops the
   invocation with recording status intact.
2. If a committed model result has client calls, resolve their recorded
   definitions and validate arguments. Transformations produce new effective
   arguments, then authorization evaluates those exact arguments. A denial is
   recorded as confirmed nonexecution. A deferred approval is a pending
   decision bound to the call, effective arguments, and policy revision; no
   executor starts before `Accept` records the decision.
3. Record each permitted tool start before invocation. Execute local calls with
   bounded concurrency; record each known result as it finishes. A Go executor
   error means uncertain outcome, not a known model-facing failure. External
   calls and waits become pending subjects. Complete siblings are recorded
   immediately, while model projection returns tool results in call order. No
   next model call occurs until every client call in the batch is resolved.
4. Otherwise, check turn-wide limits and a requested soft stop. Resolve tools
   and context for this call, apply typed request policy, and prepare one
   `llm.Request`. Preparation rejects unsupported controls and records any
   allowed adjustments. Authorize the effective request, then acknowledge its
   safe intent and attempt ID together with any host spending reservation.
   Call `Send` once. Commit the complete `llm.Result` or a typed failure before
   acting on its content. Stream deltas are tentative observations tied to the
   same attempt and message IDs; observer failure or
   disconnection cannot change execution or the committed result.
   On failure, retain any partial result and usage. Proven non-dispatch and
   explicit rejection are known failures; other receipt states need
   reconciliation. Preview text alone is never a committed answer.
5. Classify the **full** result. `tool_use` and recognized finished responses
   with client calls enter step 2. A provider pause may repeat through the same
   preparation and recording path under a separate pause limit. Refusal,
   context/output limits, unknown stops with calls, and incomplete streams stop
   with their distinct evidence. A finished response with no calls completes
   only if it contains a useful result or an explicitly accepted tool-only
   outcome; otherwise report an empty-response failure without inventing prose.
   A provider-hosted approval request is a pending decision about provider
   continuation, not a client tool to execute; an explicit `Accept` supplies
   its bound response before another model call.

Model retry is a new recorded attempt of a prepared request and consumes the
turn's call allowance. A known failure stops the invocation; `Continue` may
authorize a new attempt. Ambiguous receipt needs a verified late result or
nonexecution fact before advancement. It cannot infer free or exactly-once
execution from a transport error. The same `Prepare`/`Send` and recording pipeline handles
normal calls, any summarizing call, and child turns; hidden extra calls are not
allowed.

## Control and limits

Typed policy owns request adjustment, tool authorization, and whether to
continue after a candidate final answer. It reads an immutable turn view and
returns a decision; a later rewrite invalidates earlier authorization. A
policy infrastructure error stops the loop. An observer receives isolated
committed events and bounded, lossy previews with no control return. Queueing
new context through `Deliver` is safe at the next step boundary; it cannot
mutate a model call already prepared or sent.

Limits count across starts, retries, waits, continuations, and policy-driven
extra calls. Begin with model attempts, provider pauses, tool concurrency, and
deadlines; distinguish an unset bound from zero. A turn deadline persists across
invocations. Account for known usage without promising a hard token/cost ceiling
from post-call measurements. A host can refuse a model intent when it cannot
reserve spending authority. A deadline for a local machine tool is separate
from an open-ended human decision. Cancellation prevents new effects and marks
in-flight effects uncertain until settled; late facts remain recordable but
cannot restart canceled work.

The first implementation should prove: ordinary and streamed results agree;
tool batches preserve completion evidence and call order; approval binds the
effective call; recording failure prevents the next effect; duplicate commands
do not repeat effects; recovery never automatically replays an uncertain tool;
limits do not reset on continuation; and a changed definition revision is
rejected. These are contract tests, not claims of live provider qualification.
Local fixtures now cover these cases, canceled execution under a stubborn
executor, lost admission acknowledgement with budget and owner takeover, and
matching final facts from three provider wire formats. Production storage,
real worker takeover, and selected live routes still need qualification.

**Tradeoff:** keeping dispatch in the engine gives small callers one clear loop
and leaves a durable host less freedom over model worker placement. Revisit that
boundary with a host-shaped integration test before exposing a public reducer.

Related: [turn record](turn.md), [foundation model-call boundary](foundation.md),
[design principles](design-principles.md), and [integrator feedback](../tmp/research/design/2026-09-26-dive-v2-integrator-feedback.md).

The implemented root package covers in-memory and host-owned recording,
recoverable commands, local and external client tools, approvals, and the
ordinary model loop. Session storage, child execution, and compaction remain
separate follow-on areas; their calls must use these same boundaries.
