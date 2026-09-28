# Generation controls and provider options — draft PRD

| Field | Value |
| --- | --- |
| Status | Draft for discussion, 2026-09-28 |
| Roadmap | [Item 2](../remaining-work.md), size L, difficulty High |
| Evidence | [Source survey](02-generation-controls-research.md), [model controls](../model-controls.md), [provider gaps](../gaps/E-providers-cost-budgets.md) |

## User problem

A Go developer can set temperature in Dashi but cannot ask a model to stop at a delimiter or control nucleus sampling. To use a route's seed or service tier, they would need to bypass Dashi's provider API or fork it. They also need to know whether a requested control reached the model, especially when a model cannot combine sampling with reasoning. A request that succeeds while silently discarding a setting is difficult to debug and cannot be audited from the Turn record. Dive and the surveyed libraries show both sides: broad vendor escape hatches are useful, while silent ignore and raw maps are hard to reason about. See the [research table](02-generation-controls-research.md).

## Goals and success criteria

- Let callers express the most broadly useful missing controls in the same `llm.Settings` they already use.
- Expose a small, typed way to request route-specific controls without allowing arbitrary body or header overrides.
- Make `Prepare` the single place that checks combinations, captures requested and effective values, and explains every safe change before dispatch.
- Qualify each supported route with body fixtures and, separately, dated live probes. A green fixture means exact encoding, not live model compliance.

Success: a developer can configure a supported stop or `top_p` in one `Input`, inspect the prepared body and adjustments, and find the same effective settings on the recorded model attempt. Unsupported required behavior fails before network I/O with a field and route named in the error. No new control can silently disappear from a request.

## Proposed smallest API and user flow

Add `TopP *float64` and `Stop []string` to `llm.Settings`; a nil pointer or nil slice means “provider default.” Keep explicit `TopP=0` distinguishable. Use canonical `stop` wording in Dashi and translate to `stop_sequences`, `stop`, or other wire names per route. `Input.Validate` checks finite range and non-empty stop strings. Capability facts name each control and known route/model bounds. `Prepare` either encodes it, records an allowed adjustment with a warning, or returns an unsupported-control error. A stop sequence must **not** be omitted as an “equivalent” default: that changes completion semantics. No provider fallback should emulate it by trimming output after generation.

For controls that do not have a stable shared meaning, start with provider-owned **typed** options: e.g. `openai.Options{Seed: &seed, ServiceTier: openai.TierFlex}` attached to a call through a provider helper. The exact attachment signature is an open design choice; its contract is not: options carry a route tag, are validated before sending, cannot set fields already in `llm.Settings`, and produce a canonical serializable value in `RequestSnapshot`. Unknown option fields and wrong-route use fail. No `map[string]any`, `extra_body`, or arbitrary request headers in this first slice. Add only options backed by one real Dashi consumer or a provider fixture. Service tier's requested value and provider-reported value remain distinct, with cost/pricing qualified by tier.

Conceptual use, subject to API design:

```go
topP := 0.9
input.Settings.TopP = &topP
input.Settings.Stop = []string{"</answer>"}
input = openai.WithOptions(input, openai.Options{ServiceTier: openai.TierFlex})

request, err := client.Prepare(ctx, input)
// Inspect request.Snapshot() for effective settings and adjustments.
// Then authorize and send the same prepared request.
```

The options helper should be unnecessary for ordinary temperature, `top_p`, and stops. Prompt-cache keys and breakpoints belong to [item 3](../remaining-work.md).

## Acceptance criteria

1. `TopP` and `Stop` round-trip through input cloning, validation, capability checks, `Fit`, `Adjustment`, `RequestSnapshot`, replay, and recorded `ModelFact`. Original and effective values remain distinct.
2. Each route explicitly declares support. At minimum, one stop-capable route and one `top_p`-capable route send exact expected bytes in streaming and non-streaming preparation tests; unsupported routes return a field-specific error before network I/O. Coverage identifies API route as well as provider.
3. Valid `Stop` is never silently dropped or adjusted. `top_p` may be adjusted only within a known numeric bound and only under Dashi's existing `CapabilityAdjust` rules; strict policy rejects the same change. Reasoning/sampling conflicts use explicit errors or recorded omissions consistent with current temperature behavior.
4. One provider-owned option (service tier is the preferred first example) is type-safe to construct, wrong-route use is rejected, and its exact effective value appears in the prepared snapshot. Tests prove it cannot override canonical settings or inject credentials into the record.
5. API docs give a short supported/unsupported example and make clear that seed is best-effort and service tier may change pricing. Live qualification is reported separately from fixture qualification.

## Non-goals

This item does not add prompt-cache controls, arbitrary headers/body maps, a generic provider option registry, server-side model capability discovery, or deterministic-output guarantees. It does not change the existing Turn submission contract or make all controls portable across every model.

## Risks and open decisions

- **Surface size:** A tagged provider option type needs a serializable representation without making `llm` import every provider. Choose a minimal route-bound value/helper after a concrete OpenAI call site is sketched; avoid a general option framework.
- **Support variance:** Some routes accept `top_p` but certain reasoning models reject it. Catalog evidence must be exact-model and route scoped; unknown support follows Dashi's existing policy, with the unverified fact visible.
- **Stop behavior:** Provider limits and whether matched delimiters appear in output vary. Document wire behavior per route and do not imply uniform tokenization or inclusion semantics.
- **Pricing:** Requested service tier may differ from reported tier. Cost estimation must use the qualifying price sheet and state uncertainty when the provider does not report the served tier.
- **Decision to settle:** Should seed be a typed provider option in the first implementation? Add it only with a route and consumer that need it; do not present it as a reproducibility guarantee.
