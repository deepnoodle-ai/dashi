# 17. Incremental projection — draft PRD

**Status:** Gated proposal, 2026-09-28. **Roadmap:** [item 17](../remaining-work.md). **Basis:** [source research](17-incremental-projection-research.md). **Decision sought:** Profile long Turns before changing projection; if justified, add only a private, disposable in-run cache.

## User problem

A developer should be able to run a long tool-heavy Turn without library bookkeeping becoming visible as CPU load or delay before the next model call. Dashi currently reconstructs Turn state from step one at several execution points, and `Project` scans the record for every model request. This is a plausible scaling issue, not a measured one. An optimization that shows stale approvals, drops a tool result, miscounts spend or changes recovery behavior would be worse than the delay it removes.

The target is the application developer running many-step Turns, and the operator investigating latency. They need a stable public API and a profile that distinguishes Dashi projection work from provider/network time and application tool time.

## Goals and non-goals

**Goals:** quantify local replay/projection cost on representative records; keep `Replay` and `Project` as the authoritative cold path; improve hot-path cost only if the profile warrants it; prove identical model requests and Turn views across interruption and continuation.

**Non-goals:** changing the Turn record, persisting projections, a public cache API, application Session history, changing compaction or tool-result semantics, or weakening recorder validation. No optimization should make a failed write appear acknowledged.

## Proposed flow and measurement gate

1. Add a reproducible benchmark/profile fixture outside production code: short Turns and 50/200/500-step tool-heavy Turns, including large accepted outputs, context snapshots, parallel tool settlement and one interrupted/continued case. Measure replay validation, `Project`, `MemoryRecorder.Commit`, request preparation/cloning and total local CPU/allocation separately; exclude provider wait and tool execution. Record machine, Go version and exact Dashi commit.
2. **Gate:** proceed only if replay/projection is a material local cost on a production-like trace: for example, over 10% of local Turn CPU or a >10 ms p95 per projection/commit at 200 steps, with a plausible application workload. These are proposed thresholds, not observed results. If the gate is not met, publish the numbers and close the item without a cache.
3. If met, first try a private cache on the Runner's *one invocation*, keyed by `(Turn ID, acknowledged revision)` for the view and also by that call's history fingerprint for model messages. Reuse a validated `replayState` and projected message result only for repeated reads of that exact revision. Clone on outward return so callers cannot mutate it. Invalidate after every acknowledged commit and rebuild from the Turn on the next revision; also rebuild after a recorder conflict, unknown persistence outcome, cold load or new `Run`. This is deliberately narrower than an incremental step-transition engine. It avoids duplicate same-revision work without making cache state authoritative.
4. Reprofile. If same-revision reuse does not address the measured cost, stop and write a separate design for incremental transition/projector updates. That design must specify retroactive invalidation for context snapshots, tool result pairing, hosted approvals and interrupted model output before implementation.

The public `Runner`, `Recorder`, `Replay` and `Project` signatures stay the same. A developer opts into nothing; a cold call uses the same code as today. Operators can compare a profile's phase timings without reading cache internals. No cache bytes or hit counts enter the Turn record.

## Acceptance criteria

1. A checked-in or reproducible benchmark reports time and allocations by phase for the specified Turn shapes, with a baseline commit and a clear go/no-go result. A claimed speedup includes both isolated projection and complete local Turn cost.
2. If the gate passes, hot-path and cold `Replay`/`Project` produce equivalent `TurnView`, `Stop`, spend, warnings and ordered model messages at every acknowledged revision in representative traces, including pending/settled tools, context snapshots, hosted approval, cancellation, interruption and `Continue`.
3. A failed or uncertain recorder commit never advances the cache. Reloading a Turn on another process or owner reconstructs the same view from steps alone. An invalid record still fails validation; the cache cannot hide it.
4. The optimization is private and bounded to one invocation. It never retains caller-owned prior conversation history after that call or stores derived state durably.
5. If profiling finds no material benefit, the outcome is documented with no production cache change.

## Risks and open decisions

- **Cost may be elsewhere.** Full request assembly and provider serialization necessarily walk messages; `MemoryRecorder` validation, JSON deep copies or application I/O may dominate instead. Optimize the measured phase only.
- **Same-revision reuse may be too narrow.** Most work might occur once per new revision. Escalating to incremental `replayState.apply` is a separate correctness design, not a hidden expansion of this PRD.
- **Cache invalidation is subtle.** A later settlement can make an earlier assistant call legal to project; a named snapshot can replace earlier context. Full replay/projection remains the oracle, with differential tests across prefixes and restarts.
- **Instrumentation overhead:** Keep benchmark and optional phase timing out of the default hot path unless its cost is measured. Choose an application trace for the go/no-go decision before tuning synthetic fixtures.
