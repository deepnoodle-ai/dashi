# Draft PRD — Prompt-cache request controls

| Field | Value |
| --- | --- |
| Status | Draft for discussion, 2026-09-28 |
| Roadmap | [Item 3](../remaining-work.md) |
| Evidence | [Source research](03-prompt-cache-research.md); [model-catalog pricing decision](../model-catalog.md) |
| Primary user | Go application developer running many turns on a stable prompt and tool set |

## User problem

Dashi reports cache usage and prices it, but a developer cannot ask it to keep a reusable prefix, choose a supported lifetime, or route calls from one conversation to the same provider cache. Nvoken and Mobius each added a hashed key around Dive because their agents are recreated per turn. Developers should be able to set this once in a Dashi call or Agent configuration, inspect what Dashi actually sent, and trust that budget estimates reflect the selected write rate. A cache miss is expected behavior, not an error.

## Goal and boundaries

**Goal:** In a two-call stable-prefix fixture for each supported route, the second prepared request preserves the cacheable prefix and the correct key/markers/TTL; usage and cost distinguish 5-minute from 1-hour Anthropic writes. Live tests should observe a cache read where provider thresholds and timing permit, but hit rate is diagnostic rather than a correctness gate.

**In scope:** Anthropic Messages and native OpenAI Responses first; direct `llm.Input` and Runner-generated model calls use the same setting; provider capability and prepared-request evidence; per-call usage, pricing, and budget behavior. **Out of scope:** hosted cache warming, Gemini named cached-content resource management, shared server-side sessions, user-placed arbitrary breakpoints, and any promise that a provider will cache a request. Add compatible routes only after fixture and live qualification.

## Smallest API direction

Add one optional `llm.Settings.PromptCache *llm.PromptCache` value:

```go
type PromptCache struct {
    // Stable application scope; Dashi hashes it before it reaches a provider.
    Affinity string
    // Zero uses provider default. Other values request an exact supported TTL.
    TTL time.Duration
}

input.Settings.PromptCache = &llm.PromptCache{
    Affinity: conversationID,
    TTL: time.Hour,
}
```

The Runner already builds model calls from `Agent.Settings` (`runner.go:1004-1009`), so that same field supplies its policy across Turns. `nil` preserves today's provider-default behavior. A non-nil empty value opts into Dashi's route policy without an affinity key or extended TTL. Dashi hashes nonempty affinity to a bounded provider-safe wire key. The original affinity remains in `RequestSnapshot.Input` and a recorded `Agent`; applications should supply an opaque or already hashed value if those records need to exclude their raw conversation ID. This is a routing hint, not an authorization boundary or a cache-hit guarantee.

**Route behavior:** Anthropic places automatic/explicit breakpoints at the end of stable system/tool instructions and the growing conversation tail, with bounded anchors for long tool-result sequences. It requests 5 minutes when `TTL` is zero or 5 minutes, and 1 hour only when requested and supported. OpenAI Responses forwards the derived key and a supported exact retention value (for example, 24 hours); automatic provider caching remains possible without a key. A known unsupported exact TTL fails before dispatch under either capability policy; substituting another duration changes caller intent. A route that can cache but cannot honor an affinity hint may omit that hint with a structured warning under adjust policy, while strict policy rejects it. Unknown support follows Dashi's unverified/strict rules. Keep the encoded prefix stable across later-turn reminders and deterministic tool ordering. Today `Runner` conditionally modifies a single system string (`runner.go:1521-1548`); the implementation must keep that changing text beyond the stable boundary. If the prompt is too short to cache, return the provider's ordinary response or typed rejection; no invisible paid retry in this slice.

## Acceptance criteria

1. A caller can set `PromptCache` in a direct `llm.Input` or `Agent.Settings`; two separate Runner instances using the same affinity derive the same provider key, and two different affinities derive different keys. No raw affinity appears in the provider wire body. Documentation makes its presence in the recorded input and Agent explicit.
2. `Prepare` exposes the effective cache behavior (markers, key, and TTL) in its exact request snapshot and records any adjustment. An unsupported exact TTL is rejected before dispatch under either policy; an omitted affinity hint is visible as a warning under adjust and rejected under strict.
3. Anthropic request fixtures cover stable system prefix, tool definitions, moving tail, many tool-result blocks, reminders, final-answer mode, 5-minute and 1-hour TTLs, and each route's breakpoint limit. Caller content is not mutated; snapshot and Agent settings remain independently owned.
4. OpenAI Responses fixtures cover key forwarding, length limits, TTL omission/default, and supported extended retention. Unsupported OpenAI-compatible endpoints do not receive native-only fields without explicit qualification.
5. Anthropic usage retains the existing total cache-write count and adds a measured 1-hour subset (or separate disjoint buckets); price sheets have a verified 1-hour rate. Cost estimates and `MaxCost` reserve at the correct worst-case write price; missing split or rate produces partial/unknown cost rather than a false precise total.
6. A live qualification records model, endpoint, exact request, timing, first write, second read, and billed usage for each supported route. A miss alone does not fail qualification when provider minimums or eviction can explain it.

## Risks and open decisions

- **Exact TTL vocabulary:** Verify current supported values and costs with official provider docs before settling the type; `time.Duration` is the proposed user language, not a claim that every duration is accepted. The API may need route-specific choices if exact duration cannot be stated consistently.
- **Prefix ownership:** `Runner` currently appends conditional reminder and final-answer text to the single `Input.System` string. Decide the smallest internal representation that allows a stable prefix before that text. A public arbitrary content-block boundary should wait for a consumer that needs it.
- **Privacy and affinity:** Dashi's wire hash is deterministic across processes, but raw affinity in `Settings` enters the durable Turn and request snapshot. Applications must scope affinity by tenant and can pre-hash sensitive IDs. Decide whether an opaque-key helper is useful. Matching prefixes are still required for a hit.
- **Price correctness:** A 1-hour request without a 1-hour usage split or rate can weaken enforced budgets. Ship the accounting change in the same PR or hold extended TTL support.
- **Provider drift:** Caching capabilities and thresholds change. Distinguish fixture evidence from paid live results and keep route capability evidence dated.
