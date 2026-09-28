# PRD: provider dialect conformance, first slice

| Field | Value |
| --- | --- |
| Status | Independent review incorporated; implementation pending code review |
| Date | 2026-09-28 |
| Roadmap | [Item 4](../remaining-work.md), size M / medium difficulty |
| Evidence | [Source review](04-provider-dialect-research.md), [content replay contract](../provider-content-replay.md), Dashi `018af907` |

## Problem and user

An application developer using an OpenAI-compatible endpoint expects streamed tool calls to mean the same thing as a blocking response. Some endpoints omit an index, send a tool ID or name late, interleave two calls, or emit usage after the finish reason. Dashi's current single-call tests do not prove that these bytes yield correctly paired calls and results. A bad correlation can turn fragments for two tools into one executable call. A developer needs a failure that names the route and wire case, and a saved conversation that either prepares safely for another provider or explains why it cannot.

The primary user is the developer selecting or integrating an LLM route. Adapter maintainers also need a small, reproducible corpus for fixing dialect regressions. The [research](04-provider-dialect-research.md) identifies the current index-based assembler risk and distinguishes it from verified live-provider behavior.

## Outcome and scope

Deliver a **small offline Chat Completions conformance slice** using raw JSON/SSE bytes served by local HTTP tests. Keep `llm.Input`, `Result`, `Client`, and `Prepare` as the public vocabulary. A passing suite must cover the named wire cases below, show blocking/stream parity for the valid two-tool exchange, and demonstrate one completed Chat Completions tool exchange preparing for both Anthropic Messages and OpenAI Responses. Case names should appear in failures as route/case/transport. Record for each case whether bytes are synthesized or captured, its route/endpoint identity, and the behavior it represents.

This slice is complete when the named cases pass in the root module, a regression in index/ID correlation fails a specific case, and the prepared cross-route body is the body dispatched. These are local parser and encoder claims. The [live qualification record](../../docs/dev/live-tests.md) remains the place for current provider acceptance.

## Developer flow and acceptance cases

An adapter maintainer adds one compact fixture with source notes, runs the package tests, and reads a failure naming the route, case, and expected fact. The test compares canonical tool IDs, names, JSON arguments, usage presence, served model, and stop classification rather than brittle JSON formatting. It also checks request snapshots and dispatched bytes where history is prepared for another provider.

| Case | Required behavior |
| --- | --- |
| `interleaved-two-tools` | Two indexed tool calls with split arguments and late but consistent ID/name remain distinct; equivalent blocking and stream results agree, including usage delivered after finish. |
| `missing-index-resolved` | An omitted index joins an existing call only by a matching nonempty ID. With active index 1, it must not become index 0. Empty argument fragments do not alter the resulting JSON. |
| `missing-index-no-id` | An indexless, ID-less fragment returns `ErrorProtocol` even with only one active call; no executable call may be inferred from its position. |
| `missing-index-ambiguous` | With two active calls and no usable index or ID, return `ErrorProtocol`; do not expose the ambiguous fragment as an executable call. |
| `explicit-zero-conflict` | An explicitly indexed zero fragment cannot silently join an active index 1 call merely because an ID matches; conflicting evidence returns `ErrorProtocol`. |
| `reused-index-conflicting-id` | An existing index carrying a different nonempty ID or name returns `ErrorProtocol`, retaining diagnostic source bytes. |
| `duplicate-id-across-indices` | Two distinct calls ending with the same ID return `ErrorProtocol` in blocking and streaming responses, rather than creating a replay ambiguity. |
| `finished-invalid-arguments` | A finished call with empty or malformed JSON arguments returns `ErrorProtocol` in blocking and streaming responses; no fabricated `{}` or executable call appears. |
| `interrupted-arguments` | A stream ending before its tool JSON completes reports an incomplete result with partial, non-executable content. |

For the valid two-tool result, append exactly matched tool results to the saved history. `Prepare` on Anthropic Messages and OpenAI Responses must retain both tool meanings and record any replay changes at their original positions. Snapshot restoration must preserve the frozen body, and `Send` must dispatch it once without silently changing IDs, names, arguments, or outputs. A rejected private-replay shape is already covered by the existing pair tests; keep that guard green rather than duplicating it here.

## Boundaries and design guidance

- Reuse route-owned tests and the existing `internal/providertest` helpers where they simplify the cases. Do not add Chat Completions to the broad root `cases(t)` table merely to get pair coverage: that table also drives many unrelated contracts.
- Preserve whether `index` is omitted or explicitly zero during wire decoding. Resolve an omitted index only by an existing matching nonempty ID; otherwise reject it, including the one-active-call case. Reject conflicting index, ID, or name evidence. A final call needs a provider ID, name, and valid JSON arguments. Preserve useful partial evidence without allowing an ambiguous call to execute.
- Keep missing usage unknown (`nil`). Compare reported cost only when the fixture supplies it. Do not synthesize usage from text length.
- No general compatibility-flag framework, model-name heuristics, all-route matrix, Gemini-module expansion, or paid live probes. Add a route-scoped option later only for a demonstrated wire difference that cannot be handled safely without one.

## Risks and open decisions

Synthesized fixtures prove deterministic adapter behavior, not that a named vendor emits those bytes today. Label the cases honestly and minimize captured payloads before committing any future real traces. Avoid broad parser tolerance: once two possible calls fit a fragment, rejection gives the developer an actionable protocol error and protects tool execution. During implementation, choose the smallest way to retain raw offending bytes in the error; the existing `llm.Error.Body` is preferable to a new public diagnostic type. If the current partial-result contract cannot represent a particular ambiguous fragment safely, report it through the error and keep the ambiguous content non-executable.
