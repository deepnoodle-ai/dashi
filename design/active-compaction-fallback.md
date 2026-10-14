# Active compaction fallback

**Status:** Accepted · **Date:** 2026-10-13 · **Follows:** [durable
summarization](durable-summarization.md), which made compaction *turns*
unable to fail.

## Problem

Active compaction summarizes a turn's own finished work while the turn runs
(`Agent.ActiveCompaction`; see `docs/design/active-turn-context.md`). Until
now any unusable summary ended the turn `failed` / `compaction_failed`: a
send error, an empty or tool-calling or unfinished summary, a summary that
did not shrink the next request, or a second output-limit cut-off. So did a
resumed run whose re-check of a valid summary failed: after a crash between
the summary's result and its `context_compacted` step, the run rebuilds the
next request without the `BeforeModel` hook changes the original had, so a
valid summary can look like it saves nothing.

Compaction must never fail. A shorter history of lower fidelity beats a
failed turn.

## Decision

Every failure of an in-turn summary applies a **summary fallback**, the same
model-free stand-in compaction turns use, and the turn goes on.

- The run still asks once for a shorter summary after an output-limit
  cut-off, when the turn can afford two more model calls and its budget is
  not spent. Every other failure, and a failed retry, falls back.
- The fallback replaces the covered messages with
  `compactiontext.Fallback`: the fallback note, then the user's messages, the
  tools called, and the last reply, cut to the summary's output allowance in
  bytes. If that does not make the next request smaller, the note alone
  stands in.
- The step that applies it records `WarningSummaryFallback`, so the turn's
  `RunResult` and view say it happened.
- Whether the shortened request fits the window is the next request's
  ordinary check. If protected input alone does not fit, the turn stops
  `limited` / `context_window`, which says what is wrong; it no longer stops
  `compaction_failed`.
- Interruptions and unknown results are not failures: an interrupted first
  summary is planned again on `Continue` (an interrupted retry falls back),
  and an unknown result waits for the host, as for any model call.

## Record format

`ContextCompactionFact` gains `SummaryFallback string`. Exactly one of
`Summary` and `SummaryFallback` is set. A fallback is replayed as recorded,
without the summary frame, since it is not a summary. Replay accepts a
fallback only when its `ModelCallID` is a summary call that failed, or one
whose summary was recorded but not yet applied, and only when the text
starts with the fallback note. It does not require savings, and an
`EstimatedAfter` of 0 means the size could not be measured.

The fallback text is recorded, not rebuilt on replay, so a change to the
builder cannot change an old turn's history.

`compactiontext.Fallback` moved from package `compaction` to
`internal/compactiontext`, so the engine and compaction turns share one
builder.

## Rejected

- **Record the original request's shape on the plan** so a resumed run can
  verify a valid summary exactly. It would keep a valid summary in a rare
  crash window, but it fixes only that case and adds recorded fields; the
  fallback covers it and every other failure.
- **Fall back to the uncompacted request when it still fits.** The turn
  then compacts again on the next request and pays for another summary.
- **Retry the summary on any failure.** It costs money and can fail too.
