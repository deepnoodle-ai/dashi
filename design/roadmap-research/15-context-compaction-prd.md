# Draft PRD: recorded context compaction

**Roadmap item 15 · Draft · 2026-09-28.** Grounded in [comparative research](15-context-compaction-research.md). **Dependency:** settle item 14, the optional Session/history ownership boundary, before fixing the API. Dashi's caller-owned `Start.History` is already a decision; this PRD does not reverse it.

## User problem

Long conversations eventually exceed a model's context window. A Dashi host can shorten the `History` slice today, but it has to invent the summary, remember which messages it covers, keep raw evidence, avoid breaking tool-call pairs, account for the summarizer call, and reproduce the same model view after a crash. If it merely passes a new slice on `Continue`, Dashi detects a history mismatch but cannot explain a legitimate compaction. Users need a visible “context shortened” event and an honest outcome when no space was saved.

## Goals and non-goals

- **Goal:** A stable, inspectable summary record that identifies its exact source prefix, producer, model/usage, and retained tail; raw history stays available to authorized host readers.
- **Goal:** One projection rule makes summary plus tail into valid `llm.Message` history for `Start` and `Continue`, with no orphaned tool calls/results and no silent change on recovery.
- **Goal:** Manual or threshold compaction **between settled turns** is understandable to app developers and to users; failures leave the prior view usable or report overflow clearly.
- **Non-goal for first slice:** Mid-turn rewriting, automatic compact-and-retry, branch-aware partial ranges, a general pipeline of strategies, transcript erasure policy, artifact storage, or summary-triggered tool permission changes. Oversized individual tool results are item 11.

## Conditional product direction

First settle who owns the conversation log and its write serialization. If Dashi gains an **optional Session layer**, put the record and projection there. If applications continue to own the log, provide the same small record/projection contract as a helper that a host can persist atomically with its own session sequence. Do not add compaction state to `Turn` or mutate `Runner.History` behind the caller. The API shape below is conceptual until ownership is decided:

```go
// Session owner loads an immutable prefix and chooses a cut at a settled turn.
candidate := history.Prefix(throughSequence)
draft, err := compaction.Prepare(candidate, keepRecentTurns)
summary, usage, err := hostSummarizer(ctx, draft.Messages)
record := compaction.Record{
    CoversThrough: draft.CoversThrough,
    SourceDigest:   draft.SourceDigest,
    Summary:        summary,
    Model:          summaryModel,
    Usage:          usage,
}
// Owner atomically appends record if the prefix is still current.
// Later Start/Continue receive Project(history, record) as History.
```

This is a user flow, not committed Go signatures. The **session owner** chooses threshold and summary model, invokes the existing `llm.Client` path with its normal credentials/accounting, bounds summarizer input, and appends either an applied record or a visible failed attempt. A successful record includes format version, stable sequence/ID bounds, digest of the summarized input, summary text, producer kind, model/provider, usage/cost, and optional previous-summary ID. Projection uses the newest valid record plus untouched tail. The owner commits with compare-and-append or a lease so concurrent turns cannot apply a summary over a changed prefix. The full log is never rewritten by compaction.

The initial cut includes **whole settled turns only**; it cannot cover the current turn or divide a tool exchange. Keep the current question and a small recent tail verbatim. Preserve operator instructions and runtime reminders according to their original origin instead of summarizing them as user statements. If the summary is empty, invalid, or fails to shrink, append a failed/no-op outcome and keep the earlier projection. A provider overflow on the next turn remains `ReasonContextWindowExceeded`; automatic retry waits for a separately reviewed design because it changes model-call count, spending, and continuation identity.

## Acceptance criteria

1. With a settled multi-turn session, a manual or threshold request creates one versioned summary record covering an exact prefix. Reprojecting after process restart yields byte-equivalent model history from that record and the same retained tail. Raw source entries remain readable to the host.
2. A source change, missing/erased covered entry, stale head, or digest mismatch prevents application of a new record and surfaces a specific error. A concurrent writer cannot silently apply a summary to the wrong prefix.
3. The projected history passes Dashi's history validation and provider adapters: no tool result lacks its call, no call loses its result, and opaque/signed recent content is preserved unchanged. Tests include multiple tool calls in one assistant message and a resumed Turn using `HistoryMismatchReject`.
4. The summarizer call has bounded input and output, attributable model usage/cost, and a finite timeout. An invalid, empty, cancelled, or non-shrinking summary cannot replace history. Failure is visible to the host without reporting the user turn as completed compaction.
5. Hosts can expose `started`, `applied` (with approximate before/after tokens), `no change`, and `failed` distinctly. An accepted request is not reported as applied until the record is committed and read back. The UI may offer a manual action only when no active turn can race it.
6. Documentation shows one straightforward setup in the chosen ownership model and states the fidelity tradeoff: a shorter prompt may omit details, while full source history remains available for inspection.

## Risks and open decisions

- **Session owner:** Item 14 decides whether Dashi offers an optional Session or only host-owned history references. This determines who stores/serializes the compaction record; no second history authority should be introduced.
- **Current-turn pressure:** Between-turn compaction does not rescue a single long tool loop. Item 11 reduces one oversized result. A later mid-turn design must commit a coverage fact into the Turn before changing its model projection, with a safe point after all tool results settle.
- **Summarizer quality:** Summary text is lossy and can contain prompt injection from transcript content. Treat it as derived history, retain provenance, and never promote it to operator authority. Evaluate on real long Nvoken/Mobius sessions before choosing default prompts or thresholds.
- **Model/provider drift:** Use stable source IDs and versioned digests. Do not base coverage on timestamps alone or assume a model's reported token count exactly predicts another provider's window.
