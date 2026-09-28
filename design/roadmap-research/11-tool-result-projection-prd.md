# 11 — Tool-result size and projection: draft PRD

**Status:** draft. **Size / difficulty:** M / high. **Research:** [tool-result projection findings](11-tool-result-projection-research.md). **References:** [roadmap item 11](../remaining-work.md), [adjacent-library findings](../gaps/C-adjacent-libraries.md), [design principles](../design-principles.md). No approved design exists.

## User problem

An application developer runs a tool that returns a large log, search result or document extract. Today Dashi records that full output and sends it back to the model unchanged. A single result can exceed the model's context window and stop the turn. If the application truncates inside the tool, it loses the exact outcome needed for audit and recovery. Mobius encountered this as a session-poisoning failure; its spill/fetch path shows why the model also needs a clear way to request detail.

## Goals and non-goals

**Goals:** bound ordinary text sent to the next model call; preserve the original accepted tool outcome; record exactly what the model saw and why it differed; make omission visible to the model and host; preserve tool-call pairing, success/error state, streaming parity and replay determinism. Give developers a simple limit and a clear path to provide a durable reference when detail must remain retrievable.

**Non-goals for the first slice:** LLM-generated summaries, a Dashi artifact store, automatic file/URL fetching, token-perfect context budgeting, rewriting provider-hosted opaque results, automatic media transcoding or general session compaction. This feature protects one local/external result as it enters the next request; session history reduction is separate.

## Proposed smallest API and user flow

1. Add one `Agent.Limits.ToolResultTextBytes` setting, with a conservative default text budget (candidate: 64 KiB of UTF-8 text per result) and an override for applications with known provider windows. The budget counts **all text blocks of one result**, including errors. Resolve and pin the size and projection version at turn start so recovery cannot silently use a new threshold. No per-tool callback stack is required initially.
2. Keep `tool.Outcome.Output.Content` as the **original accepted result** in `StepToolSettled`. At settlement, compute a deterministic model view and store it with that same step: full content if it fits; otherwise a bounded, valid-UTF-8 head/tail preview and an explicit marker giving original bytes, omitted bytes, and a recommendation to narrow or page the source. Record a digest of the original, the projection mode/version and the exact projected blocks. `Project` reads the recorded view; it never recomputes from current policy. `Output.Display` remains a presentation hint, not the model view.
3. If the tool or host supplies a durable reference to the full result, include that reference and its expiry in the omission marker. Dashi validates its shape and stated lifetime; the application owns storage and authorizes each read. Dashi does not invent a fetch tool or imply that a local temporary path survives recovery. A follow-up read is an ordinary tool call. Without a reference, the marker says the model must narrow/re-run the tool, and the host can still inspect the recorded full outcome.
4. Start with text blocks. For structured or multimodal content, preserve types and report measured size; do not slice JSON, image or provider-private replay into invalid data. If an explicit configured request-byte ceiling would be exceeded by unsupported content, stop before send with a typed projection-required error naming the call and content kind. A subsequent proposal can add typed reference/thumbnail rules once a real host flow needs them.

Illustrative configuration and result behavior:

```go
agent := dashi.Agent{
    Limits: dashi.Limits{ToolResultTextBytes: 64 << 10},
    // Other existing Agent fields omitted.
}

// A 400 KiB log is recorded in the settled tool fact. The model sees a
// bounded preview: "[Tool output shortened: 409600 bytes, 344064 omitted.
// Narrow or page the original query to inspect missing detail.]"
```

The example is directional, not a committed Go signature. A 64 KiB cap is a starting point informed by pi and Mobius; acceptance must use a representative Dashi consumer and provider route to choose the default. If a tool's result is semantically unsafe to abbreviate, its author should return a concise status plus a retrievable reference rather than depend on a “verbatim unlimited” escape hatch.

## Acceptance criteria

1. A tool returning large UTF-8 text records its full accepted output and one explicit model projection in an acknowledged settlement. `Project`, Runner continuation and cold replay produce byte-identical projected content, in call order, without re-running the tool or changing `IsError`.
2. For text under the threshold, projected content equals the original. For text over it, output stays within the stated byte budget, remains valid UTF-8, includes a visible omission count, and never silently changes a successful tool into a failed one. Multi-block and error outputs are covered.
3. Model `Prepare` and any stream path use the same recorded projection. Tests confirm a large result no longer enters the next provider request verbatim; a provider that cannot fit the *whole* request still reports its normal context-limit outcome rather than a false guarantee from the per-result cap.
4. The host can inspect original and projected content plus source digest, size, reason and policy version. A supplied reference is included only after its shape and expiry are validated; the host remains responsible for ownership checks on retrieval. Missing or expired references are not presented as fetchable. The full result remains in the Turn record for the first slice; record growth is measured and documented.
5. Structured or media results are never byte-sliced into malformed blocks. If they exceed an explicit request-byte ceiling and no valid projection exists, the run stops before model dispatch with a typed, actionable error while preserving the settled tool fact for audit. Hosted opaque results remain untouched.
6. At least one Mobius-style large-log fixture demonstrates the complete user flow: tool settles, host sees full result, model receives a visible preview, and the model can narrow or page through an application tool when detail is needed.

## Risks and open decisions

- **Default and record size:** 64 KiB is a candidate, not a universal context fit. Full outcomes can still make Turn records large. Measure a real consumer, then decide whether a later optional host artifact reference should replace large inline *recorded* bytes; retention and digest checks must be explicit.
- **Atomic settlement:** original and projection should commit together. If recording fails after a tool effect, existing uncertainty rules apply; do not retry an effect merely because its large result was hard to persist.
- **Error results and secrets:** a large error may contain sensitive data. Projection does not provide redaction; tool authors and host policy remain responsible for what is allowed into the durable record. The model marker must not reveal hidden text.
- **Custom summaries:** a developer-supplied semantic summary might help more than head/tail clipping, but creates another source of ungrounded claims and a second cost path. Defer until a concrete tool cannot use a concise native result or a durable reference.
