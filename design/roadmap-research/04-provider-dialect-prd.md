# PRD: provider dialect conformance

| Field | Value |
| --- | --- |
| Status | Draft for discussion |
| Date | 2026-09-28 |
| Roadmap | [Item 4](../remaining-work.md), size M / medium difficulty |
| Evidence | [Source review](04-provider-dialect-research.md), [content replay contract](../provider-content-replay.md) |

## User problem

An application developer expects `llm.Client` to keep a complete tool call, result, usage and stop reason when they change models or use an OpenAI-compatible endpoint. Real streams vary: tool IDs or indices may appear late or repeat, arguments may be empty or split oddly, and usage may arrive after a finish reason. Today Dashi covers important examples, but the fixture set cannot demonstrate that these variants preserve the same facts. A defect can surface as a broken turn or an unexecutable partial tool call, with the application left to diagnose the provider dialect.

## Goal and scope

Make a **small offline conformance corpus** that catches dialect regressions and makes support claims explainable. Keep the existing public `llm.Input`, `Result`, `Prepare` and `Client` concepts. Expand `internal/providertest` and route-owned fixtures rather than designing a plugin system or a general-purpose fixture language. Use `llm.ModelInfo` evidence and exact request snapshots to describe what was tested. A green local test means conformance with fixture bytes, not live provider qualification; live probes belong to [item 5](../remaining-work.md).

## Proposed developer flow

An adapter author adds a small named case with a raw complete body or SSE sequence, expected normalized `Result` and deltas, and a source note (recorded route/date or hand-built edge case). The shared check prepares the request, serves bytes from a local HTTP server, compares blocking and streaming terminal facts, and verifies that `Send` uses the frozen body. A replay case feeds the resulting assistant message and matched tool result to each eligible destination's `Prepare`, checking either a semantically equivalent body with explicit `ContentChanges` or a typed rejection. Separate-module adapters such as Gemini run the same contract helpers in their module. The case name appears in test failure output, for example `completions/late-tool-id/stream`, so the developer sees the broken dialect and exact behavior.

Begin with the known missing cases: parallel tool calls with missing/reused indices or IDs, empty argument fragments, out-of-order metadata, partial JSON on interruption, an in-stream error after text, and tool results after model handoff. Distinguish a temporarily incomplete call from a completed invalid call. Preserve unknown private replay as data; never infer an executable tool call from it. Keep source and target model/endpoint identities in the fixture notes.

## Acceptance criteria

1. A shared offline check runs complete/stream parity for Anthropic Messages, OpenAI Responses, Chat Completions and the separate Gemini module, with named raw-wire cases. It reports route and case on failure.
2. Tool-call cases prove stable correlation of IDs, names and JSON arguments when fragments are unambiguous. Ambiguous reused indices or IDs, malformed calls, and incomplete calls cannot become executable tool calls; they produce an explicit error or non-executable partial content, with no silent drop or fabricated call ID.
3. At least one cross-route history case per pair checks `Prepare` decisions and exact dispatched bytes, including a valid tool exchange and a rejected private replay. Extend the existing pair test rather than duplicating it.
4. Fixtures compare final usage, cost availability, served model, stop and error classification across streaming and blocking where the provider supplies equivalent facts. Missing usage remains unknown rather than zero.
5. Each route has a short coverage note listing fixture-backed behaviors, source of bytes and date. Live qualification is linked separately and never inferred from the fixture suite.

## Non-goals, risks and open decisions

- **No blanket compatibility flag bag.** pi shows why dialect traits help, but its size also shows maintenance cost. Add a typed, route-scoped option only for a demonstrated fixture-backed difference; expose the effective choice in `Prepare` evidence. A generic map or URL-sniffing policy is out of scope.
- **No all-model live matrix here.** The pair matrix uses deterministic representative histories. Paid calls, credentials, model drift and current provider acceptance are item 5.
- **Corpus maintenance:** Recorded responses may contain secrets, unstable IDs or large payloads. Review and minimize each fixture before committing it; record whether it was captured or synthesized. Keep the exact raw fragments needed to reproduce the behavior.
- **Open decision:** Decide which incompatibilities deserve normalization and which should reject with `ErrorProtocol` or `ErrorUnsupported`. The rule should follow user meaning and replay safety, not mere parser permissiveness.
