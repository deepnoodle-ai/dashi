# 22. Remaining provider price gaps — draft PRD

**Status:** Draft, 2026-09-28. **Size/difficulty:** S per qualified route / Medium. **Research:** [comparative findings](22-provider-price-gaps-research.md). **References:** `design/remaining-work.md:209-217`, `design/model-catalog.md:111-152`, `design/hosted-tool-pricing.md:60-97`. No broader pricing redesign is proposed.

## User problem

An application displaying Dashi cost must know whether the amount covers the whole provider call. OpenAI's hosted image-generation tool currently appears as one unpriced request even when the mainline model's tokens are priced. A developer can find an image-model price online, but cannot safely multiply it by that request count: the tool has its own model, consumes image and text tokens, and may have billed cached inputs that the Responses output omits. Similar gaps arise when a tool is billed by container session, a fixed token block, or a model-specific image count. Showing `$0` or a complete-looking total would mislead a user and weaken a budget cap.

## Goals and non-goals

**Goals:** Keep every price tied to a documented billable unit and qualified provider/model/API/endpoint/tier; make the known amount and unpriced portion legible; give maintainers a short, repeatable path for adding one route; preserve hard-cap behavior when cost is a lower bound.

**Non-goals:** Infer a tool's cost from output image count, invent unpublished rates or cache discounts, substitute a requested model for an unverified served tool model, add a general billing engine, price media outside the current call surface, or require paid probes as part of this work.

## Proposed smallest workflow and user behavior

1. **Open one route-specific evidence record** in `catalog/sources_test.go` (or the existing unpriced table). Record provider page URL/date, exact route and model, billing unit, endpoint/service tier, the response field that measures it, and known omissions. Confirm a saved nonzero response fixture or a documented shape; zero-only fixtures cannot prove mapping. Never infer a rate from a cost example alone.
2. **Classify it:** (a) fully measurable with a qualified rate, (b) provider-reported total available but no breakdown, (c) partially measurable, or (d) not measurable. Keep (c)/(d) in `unpricedMeters` with a plain reason. A provider-reported charge may be used as reported cost with its source intact; it is not recast as a list-price estimate.
3. **For a qualified existing per-request meter**, update that model's JSON `pricing.requests` and source row together. Test the exact positive count, explicit zero, unknown key, model mismatch, and partial estimate. Do not alter unrelated provider rows.
4. **For hosted image tokens, wait for the missing evidence.** If a supported route exposes the tool model and complete nonzero text/image/cache token buckets, add the smallest scoped tool-usage field and image-model sheet needed to estimate *that route*. Keep mainline tokens under the mainline sheet. If cached billed tokens are missing, as OpenAI currently documents for GPT Image 2/2.5 in Responses, keep `image_generation` unpriced unless the provider reports an authoritative total covering it. Do not add its published token rate to `pricing.requests`, whose unit is one request.

Developer flow: `Entry.Bind` and `Entry.Serves` continue to qualify a mainline sheet. The caller gets `Result.Cost` when the provider reports an authoritative charge, or explicitly invokes `Pricing.Estimate(Result.Usage)` for a list-price estimate. A hosted image use with incomplete evidence yields `Cost.Partial()==true`, `UnpricedRequests["image_generation"] > 0`, and a known lower-bound `Total`; UI copy can say “at least $X; image generation unpriced.” Under `MaxCost`, the existing fail-closed rule applies. The application can supply its own qualified model facts for a custom endpoint without implying that Dashi's public-endpoint rate applies there.

## Acceptance criteria

1. Every catalog addition has an official provider source, checked date, exact model/route/tier, billable unit, and response-to-meter mapping. A CI test rejects an emitted hosted meter that is neither priced nor explicitly unpriced with a reason.
2. The OpenAI hosted image tool remains partial until its separate model and complete billable token usage, or an authoritative total, can be established. A positive image call does not become “one image” or a mainline-model image modality token.
3. For any newly qualified price, tests cover nonzero usage, zero usage, missing/unknown usage, served-model mismatch, and stream versus blocking terminal usage where relevant. Cost and source are identical for the same provider response.
4. Unknown units are omitted from the numerical estimate and visible through `UnpricedRequests`/`Cost.Partial()`. The known `Total` is described as a lower bound. A hard cap never admits a route on that lower bound alone.
5. No new catalog rate is committed solely because a price page lists it. The diff includes only the qualified route and its provenance/test changes; no paid request is needed to complete the documentation pass.

## Risks and open decisions

- **OpenAI image tool:** The [official guide](https://developers.openai.com/api/docs/guides/image-generation) says cached image-tool input may be billed without a count in Responses output. Decide whether a future provider-reported total, invoice reconciliation, or a deliberately conservative upper bound is the first usable route. A list-price **exact** estimate cannot claim coverage from the current response alone.
- **Tool model identity:** `ImageGenerationToolOptions.Model` can be empty (provider default). A future tool token sheet must use an explicit or reported model, and fail closed if the actual model cannot be qualified. This does not need a generic multi-model pricing registry yet.
- **Timing and tiers:** Prices and supported service tiers change. The checked date is provenance, not a validity promise. A changed tier or endpoint gets a separate sheet only when Dashi can identify it from the request/result.
- **Product billing:** Nvoken and Mobius have application-specific fallback credit policies. Dashi should expose coverage and evidence so those applications can make their own billing decision; a fallback class rate must not masquerade as a provider charge.
