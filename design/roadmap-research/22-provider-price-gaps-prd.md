# 22. Qualify displayed provider cost — draft PRD

**Status:** Narrow slice accepted for implementation, 2026-09-28; implementation review pending. **Size/difficulty:** S / Low. **Research:** [comparative findings](22-provider-price-gaps-research.md). **References:** `design/remaining-work.md:252-262`, `design/model-catalog.md:111-134`, `design/pricing-proposal.md:89-100`. This slice qualifies the existing estimate path; **no new provider price is qualified, and roadmap item 22 remains open**.

## User problem

The runnable `examples/providers` program prints an answer, the model that served it, and a cost. At the `f6098c0` baseline, when a provider did not report cost, it selected the *requested* model's catalog sheet and estimated from response usage without checking the *served* model (`examples/providers/main.go:139-163`). A server can return a different model or omit it. The displayed amount then looks complete even though the sheet may describe another model. The same formatter treated any non-nil `Result.Cost` as complete, although `Cost.Partial()` explicitly means its total is a lower bound. Developers are likely to copy this example into billing and monitoring code.

The larger price gaps remain real. OpenAI's Responses image-generation tool uses its own image model and billable image tokens. Its [official guide](https://developers.openai.com/api/docs/guides/image-generation) says billed cached tool-input token counts are omitted from the Responses output. The current one-call `image_generation` meter cannot be multiplied by an image-token rate. Adding such a rate would make the apparent coverage worse.

## Goal and scope

**Goal:** The provider example must show a catalog list-price estimate only when the response's served model is covered by the selected catalog entry; its output must identify unknown and partial cost plainly. This is a correctness and developer-experience fix to the documented flow, with no new public API.

**In scope:** The example's cost-selection/formatting path, a brief guide example or wording that shows the safe pattern, and focused offline tests for its decision branches. Keep the catalog's exact model/API/endpoint provenance and default-tier assumptions intact. A provider-reported amount remains preferred and labeled by its `Cost.Source`.

**Out of scope:** New catalog rates, a multi-model hosted-tool price engine, invoice reconciliation, changing `llm.Pricing.Estimate`, pricing custom proxies automatically, or changing hard-budget behavior. Future per-route additions still require a documented billable unit, rate, served-model identity, and measurable response field.

## Proposed developer flow

1. If `Result.Cost` is present, show its source. If `Partial()` is true, say “at least” and name its unpriced hosted meter counts. A reported total does not need the catalog sheet to be displayed.
2. Otherwise, get `client.ModelInfo(requestedModel)`. If it has no price sheet, show `Cost: unknown (no qualified price sheet)`.
3. For this example, accept only a catalog-owned sheet. Compare the returned `ModelInfo` to `catalog.Lookup(info.Evidence.API, info.Evidence.Endpoint, requestedModel)`: require catalog owner, requested model, `client.API()`, identical owner/model/API/endpoint evidence, and an identical pricing sheet. This rejects changed `Config.Models` prices or provenance even when they cite the catalog. Then find the exact entry for `client.Provider()`, require its sheet to match the lookup, and require `entry.Serves(result.Served.Model)` plus matching requested/served provider identity. An empty or unrelated served model yields unknown. Application-owned sheets need their own qualification and show unknown here.
4. With the example's default-configured public clients, accept only these explicit tier strings: `default` for OpenAI and Grok; `standard` for Anthropic. Empty means the client requested its configured default and the adapter reported no tier, including for Meta and Mistral. Any other explicit tier yields unknown. Only then call `Pricing.Estimate(result.Usage)`. If it returns false, show unknown usage/cost. Format a partial `Cost` from either source with “at least” and `UnpricedRequests` sorted by meter key; otherwise display the amount and `Cost.Source`.

For example, a request for `gpt-5-mini` served by a different model must print **unknown**, even if both models have catalog entries. A matching request with `image_generation: 1` may print “at least $X USD; image_generation unpriced.” A provider-reported `$Y` displays `$Y (provider_reported)` without substituting a list estimate. The exact wording can fit the existing CLI; the distinction is the requirement.

## Acceptance criteria

1. An offline example test with a matching served model prints a complete list-price estimate and source. A documented alias snapshot accepted by `Entry.Serves` also works.
2. Empty, different, or cross-provider served identity never yields a list-price amount from the requested model's sheet. A `Config.Models` override with a changed sheet or changed evidence does not pass catalog qualification. An explicitly non-default or unrecognized tier does not use a default-tier sheet.
3. A non-nil provider-reported or provider-estimated `Result.Cost` takes precedence and retains its source. Any `Cost.Partial()` path prints a lower-bound label and unpriced meter/counts in stable sorted order, including when the cost came from `Result.Cost`.
4. Missing usage, missing price sheet, and a valid sheet with unpriceable token detail have distinct unknown paths; none prints `$0` as if known free. A nonnil zero `Usage` yields a known zero estimate. A hosted image call remains an unpriced count, with the priced mainline portion retained when estimable.
5. `docs/guides/model-calls.md` and the runnable example agree on the served-model check and how to present partial cost. Tests use local fixtures or stubs; no paid provider call is required. No public API or catalog rate is added in this slice.

## Risks and open decisions

- **Snapshot versus proxy identity:** `Entry.Serves` knows documented alias snapshots, while `Result.Served` cannot identify a proxy's upstream. Keep the example on its existing public-endpoint clients; do not silently qualify custom endpoints.
- **Tier terminology:** The Responses adapter currently omits the wire `service_tier` field, so an empty result is accepted here only because `newClient` configures the public clients without a non-default tier. If a client begins requesting another tier, this example's decision must change with it. Avoid a generic tier registry for this small fix.
- **Further price coverage:** OpenAI image-tool cost and Meta's per-query search mapping still lack enough observed billing evidence for a new completeness claim. Keep those decisions in the research inventory and revisit only with a positive usage/billing example or authoritative provider total.
