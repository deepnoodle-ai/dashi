# Hosted tool pricing

**Status:** Reporting implemented; bounded admission deferred (see below) ·
**Date:** 2026-09-28 · **Fixes:** finding 11.1 in
[gaps/F](gaps/F-example-ports.md) · **Relates to:**
[pricing](pricing-proposal.md), [model catalog](model-catalog.md),
[budgets](budgets.md)

## Problem

A **hosted tool** runs at the provider (web search, code execution), and many
bill per use on top of tokens. Before this change, any hosted use made a
call's cost unknown although its token cost was known. The catalog priced no
hosted requests; the runner counted `HostedUse` blocks by the name the model
called (`bash_code_execution`), so a price sheet would have had to know block
names; it ignored the counters providers report; Gemini search grounding
produced no block, so a grounded Gemini call looked fully priced when it was
not; and one unpriced key made `Pricing.Estimate` give up on the whole call.

## Counting: meter keys, filled by adapters

A **meter key** names one billable unit, independent of block names:
`web_search`, `web_search_preview`, `grounded_prompt`, `web_fetch`,
`url_context`, `code_execution`, `file_search`, `image_generation`, `mcp`,
`x_search_post`, `x_search_profile` (constants in `llm/meter.go`). Each provider
adapter fills `Usage.Requests` when it decodes a response, preferring the
provider's counters: Anthropic `usage.server_tool_use`, OpenAI
`tool_usage.web_search.num_requests`, xAI `server_side_tool_usage_details`.
Otherwise it counts items: Responses `web_search_call` items with a `search`
action, not `open_page` or `find_in_page` (our reading of the docs, matched by
OpenAI's counter in a captured response); the key is `web_search_preview` when
that tool was declared, alone or with `web_search`, since its rate is never
lower. Meta counts the queries each search item shows, at least one
(unverified). Gemini counts every non-empty `webSearchQueries` entry (at least
one when web sources appear), plus one `grounded_prompt` for a response
grounded by web search. Gemini fixtures are
built from the documented response shape, since no key was available; a
live-tagged test checks them. The runner no longer counts blocks.

Two **reserved keys** are never priced (`Pricing.Validate` rejects them):
`unrecognized:<wire type>` for hosted work an adapter does not recognize, and
`unreported` for hosted uses in a call whose final usage never arrived (a
cut-off stream, or a response without usage). The runner records the
difference between hosted blocks and counted requests under `unreported`, so
such a call shows as partial, not complete. Anthropic's first streamed usage
frame has no request counts, so this is the common case there.

## Prices

Rates go in each entry's `pricing.requests`, per unit; each entry stays
complete. `catalog/sources_test.go` has one provenance row per provider and
key (source, models, rate, date checked, verified or not) and fails when the
catalog differs; a coverage test requires every key an adapter can emit to be
priced or listed as unpriced with a reason. Token `updated_at` dates are
unchanged. Checked 2026-09-28:

| Provider | Priced (USD per unit) | Unpriced, and why |
| --- | --- | --- |
| Anthropic | web_search 0.01; web_fetch 0 | code_execution: container-hours, duration not reported. mcp: no published rate |
| OpenAI | web_search 0.01; web_search_preview 0.01 reasoning, 0.025 other; file_search 0.0025; mcp 0 | code_execution: container sessions span calls. image_generation: image tokens. web_search on gpt-4.1-mini: fixed 8,000-token block not in usage |
| Gemini 3.x | web_search 0.014 per query; grounded_prompt, url_context, code_execution, file_search 0 | none |
| Gemini 2.5 | grounded_prompt 0.035; web_search, url_context, code_execution, file_search 0 | none |
| xAI | web_search, code_execution 0.005; file_search 0.0025; x_search_post 0.005; x_search_profile 0.01; mcp 0 | image_generation: per-image rates vary |
| Meta | web_search 0.0025 per query | none |

Free allowances are ignored, so estimates err high. xAI also reports its billed
amount (`cost_in_usd_ticks`, 10^10 per dollar, tool charges included); the
adapter records it as a provider-reported `Result.Cost`, which wins over any
estimate.

## Partial cost

`Pricing.Estimate` fails only when tokens cannot be priced. Hosted requests
without a rate are left out of `Hosted` and `Total` and listed in
`Cost.UnpricedRequests`; `Cost.Partial()` reports that `Total` is a lower
bound. `Spend.Cost` includes the known part, `Spend.CostPartial` counts such
calls, and `Spend.UnpricedRequests` sums their counts.

**Budget rule.** A cap is an upper bound and a partial total a lower bound,
so a partial charge never counts as verified spend. Before dispatch, a request
that declares hosted tools under `MaxCost` still stops `limited`. After a call,
a partial charge under a cap counts as unknown: its known part is `CostUsed`,
the turn stops `limited`, and a raise cannot clear it. The reason names the
unpriced keys and says to start a new turn with the history. Without a cap,
partial costs are reported and the turn runs normally.

## Bounded admission: deferred, with evidence

The review proposed admitting hosted tools under a cap when each has a
provider-enforced use limit, priced meters, and a per-use token allowance,
reserving uses × price + uses × allowance × input rate. Live calls
(claude-haiku-4-5, `web_search_20250305`) show that formula understates the
worst case:

- **Input is billed per internal pass.** Two sequential searches: 27,702
  input tokens billed, while the final context was 16,037 tokens; the prompt
  and each result were re-read on every pass.
- **`max_uses` limits billed uses, not passes.** With `max_uses: 1`, the model
  tried a second search, got `max_uses_exceeded` (not billed:
  `web_search_requests` was 1), and ran a third pass: 21,440 input tokens
  against a final context of 9,762.
- No provider documents a limit on passes per call, and Anthropic web search
  documents no per-search token limit.

A safe bound therefore needs a pass limit the provider enforces (xAI's
`max_turns` is one; OpenAI's `max_tool_calls` says further attempts are
"ignored" without saying whether they cost a pass). Until then, the
pre-dispatch stop above is the only rule that cannot be silently exceeded, so
bounded admission, the default use limit under a cap, and settling at
max(known, reserved) are left for a follow-up.

## Changes and tests

Recorded format: `Usage.Requests` keys are meter keys; `Cost` gains
`unpriced_requests`; `Spend` gains `cost_partial` and `unpriced_requests`;
Grok results gain a provider-reported cost. No migration (no users yet).
Tests cover: estimates with priced, unpriced, and reserved keys; round trip and
clone; adapter fixtures per provider (captured for Anthropic, OpenAI, xAI, and
Meta); partial spend with and without a cap, including a refused raise; a
cut-off call recording `unreported`; catalog provenance and coverage.

## Follow-ups

- Anthropic code execution is free when `web_search_20260209` or
  `web_fetch_20260209` (or later) is in the request; Dashi sends older
  versions. Moving to them would let code execution be priced at zero there.
  Execution time is billed when files are attached, even if the tool is not
  called.
- Bounded admission, once a provider-enforced pass limit is available.
