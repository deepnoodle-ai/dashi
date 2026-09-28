# Hosted tool pricing

**Status:** Proposal · **Date:** 2026-09-28 · **Fixes:** finding 11.1 in
[gaps/F](gaps/F-example-ports.md) · **Relates to:**
[pricing](pricing-proposal.md), [model catalog](model-catalog.md),
[budgets](budgets.md)

## Problem

A **hosted tool** runs at the provider (web search, code execution), and many
bill per use on top of tokens. Today any hosted use makes the call's cost
unknown, even though its token cost is known. Three causes:

1. The catalog prices no hosted requests (`Pricing.Requests` is empty).
2. The runner counts `HostedUse` blocks by the name the model called
   (`web_search`, `bash_code_execution`), so a price sheet would have to know
   every provider's block names. It ignores what providers report: Anthropic's
   `usage.server_tool_use`, xAI's `server_side_tool_usage_details` and billed
   cost. Gemini search grounding produces no block, so it is never counted and
   a Gemini call looks fully priced when it is not.
3. `Pricing.Estimate` returns "unknown" for the whole call when one request
   key has no rate.

## Proposal

**1. Request counts use meter keys that adapters fill.** A **meter key** names
one billable unit, such as one web search, independent of block names. The
provider adapter fills `Usage.Requests` when it decodes a response, preferring
the provider's reported counters over counting blocks. The runner stops
counting blocks by name. Keys are `llm` constants:

| Key | Unit | Set by |
| --- | --- | --- |
| `web_search` | one search (query or call) | Anthropic usage; Responses `web_search_call` items; xAI usage; Gemini 3 unique non-empty `webSearchQueries` |
| `web_search_preview` | one call of OpenAI's preview tool | Responses, when the request declared that type |
| `grounded_prompt` | one response grounded by search | Gemini (2.5 bills per prompt) |
| `web_fetch`, `url_context` | one fetch | Anthropic usage; Gemini URL metadata |
| `code_execution` | one code run | Anthropic usage; Responses items; xAI usage; Gemini `executableCode` |
| `file_search`, `image_generation`, `mcp` | one call | Responses items; xAI usage; Anthropic `mcp_tool_use` |
| `x_search_post`, `x_search_profile` | one fetched item | xAI usage |

An unrecognized hosted tool is counted under its wire type, which no sheet
prices, so its cost shows as unpriced rather than free. If a call's usage never
arrived (a cut-off stream) but it has hosted blocks, the runner records their
number under `unreported`, which is never priced.

**2. The catalog prices each meter it can confirm.** Rates go in each entry's
existing `pricing.requests`, in currency per unit. Every entry stays complete;
nothing is inherited. `catalog/sources_test.go` names the page for each rate,
and a test requires every key an adapter can emit for a provider to be either
priced in all its sheets or listed as unpriced with a reason. Sources, fetched
2026-09-28:

| Provider | Priced | Unpriced, and why |
| --- | --- | --- |
| Anthropic ([pricing](https://platform.claude.com/docs/en/about-claude/pricing.md)) | web_search $0.01; web_fetch $0 | code_execution: billed per container-hour (5-minute minimum, 1,550 free hours/org/month, then $0.05/h); responses report no duration. mcp: no published rate |
| OpenAI ([pricing](https://developers.openai.com/api/docs/pricing.md)) | web_search $0.01; web_search_preview $0.01 (reasoning models) or $0.025 (others); file_search $0.0025; mcp $0 ([guide](https://developers.openai.com/api/docs/guides/tools-connectors-mcp.md)) | code_execution: billed per container session by memory size and minutes, and a session spans calls. image_generation: billed as image tokens not in `usage`. web_search on gpt-4.1-mini: billed as a fixed 8,000-token block we cannot see |
| Gemini ([pricing](https://ai.google.dev/gemini-api/docs/pricing)) | 3.x: web_search $0.014, grounded_prompt $0; 2.5: web_search $0, grounded_prompt $0.035; code_execution, url_context, file_search $0 | none |
| xAI ([pricing](https://docs.x.ai/developers/pricing)) | web_search, code_execution $0.005; file_search $0.0025; x_search_post $0.005; x_search_profile $0.01; mcp $0 | image_generation: Imagine per-image rates vary by resolution |
| Meta ([pricing](https://dev.meta.ai/docs/pricing-rate-limits.md)) | web_search $0.0025 | none |

Mistral's hosted connectors exist only in its Conversations API, which Dashi
does not use. Free allowances (Gemini's monthly and daily free searches,
Anthropic's free container hours) are ignored: a list-price estimate is an
upper bound, which is the safe side for budgets. Where a provider reports its
own counter (Anthropic, xAI), Dashi uses it; xAI's counts only successful
calls. Counting Responses items includes failed calls, which may overstate.

**3. xAI's billed cost is used directly.** xAI reports `cost_in_usd_ticks`
(10^10 ticks per dollar), inclusive of tool charges. The Responses adapter
records it as a provider-reported `Result.Cost` with no breakdown, rounded half
to even to the nano. Provider-reported cost already wins over estimates.

**4. A partly priced call reports its known cost and marks the rest.**

```go
type Cost struct {
    // ...existing fields...
    // Unpriced counts hosted requests the sheet has no rate for, by meter
    // key. When it is non-empty, Total is a lower bound.
    Unpriced map[string]int `json:"unpriced,omitempty"`
}
func (c Cost) Partial() bool

type Spend struct {
    // ...existing fields...
    CostPartial int            `json:"cost_partial,omitempty"` // calls
    Unpriced    map[string]int `json:"unpriced,omitempty"`     // by key
}
```

`Pricing.Estimate` still returns false when tokens cannot be priced. When only
hosted keys lack rates, it returns true with `Unpriced` set, `Hosted` holding
the priced requests, and `Total` the known part. Before, `Spend.Cost` omitted
such calls entirely and counted them in `CostUnknown`; now their known part
adds to `Spend.Cost` and they count in `CostPartial`.

**5. Budget rule: a partial cost never counts as verified spend.** A cost cap
is an upper bound; a partial total is a lower bound, so it cannot prove a cap
holds. Under `MaxCost`:

- Before dispatch, unchanged: a request that declares hosted tools stops
  `limited`. Neither the number of hosted requests nor the tokens they inject
  (search results are billed input) can be bounded, so `MaxCostAtRisk` has no
  honest worst case. The stop reason is reworded to say so.
- After a call, a partial charge is treated like an unknown one: its known
  part counts toward `CostUsed`, the cap is reported as unverifiable, and the
  turn stops `limited` before the next call. `Continue` cannot clear this by
  raising the cap, as with an unknown charge today.

So a cap is never silently exceeded by unpriced hosted work. Without a cap,
partial costs are reported and the turn runs normally.

## Example

```go
view, _ := dashi.Replay(outcome.Turn)
for currency, amount := range view.Spend.Cost {
    fmt.Printf("Cost: %s %s\n", amount, currency)
}
if view.Spend.CostPartial > 0 {
    fmt.Printf("  plus unpriced hosted requests: %v\n", view.Spend.Unpriced)
}
```

With Claude, web search, and code execution this now prints a dollar amount
with `code_execution` marked unpriced, instead of "cost unknown".

## Changes

- `llm`: meter key constants; `Cost.Unpriced`, `Cost.Partial`, clone and
  validation; `Estimate` partial rule; doc comments on `Usage.Requests` and
  `Pricing.Requests`.
- Adapters: Anthropic usage and `mcp_tool_use`; Responses items and the
  preview key; xAI usage counters and cost; Gemini grounding, URL context, and
  code execution in both the REST and streaming paths (separate module).
- Runner: drop name-based counting in `budget_accounting.go`; `unreported`;
  `Spend.CostPartial` and `Spend.Unpriced`; `costIn` and `canContinue` treat
  partial as unverifiable; reworded hosted-tool stop.
- Catalog JSON for Anthropic, OpenAI, Google, Grok, and Meta; source and
  coverage tests. `examples/hosted_tools` prints the partial cost. Short
  updates to the pricing, catalog, and budgets design notes.

**Recorded format:** `Usage.Requests` keys change meaning (meter keys, not
tool names); `Cost` gains `unpriced`; `Spend` gains `cost_partial` and
`unpriced`; Grok model facts gain a provider-reported cost. No migration,
since Dashi has no users yet.

**Tests:** estimate with priced, unpriced, and mixed keys; partial cost JSON
and clone; adapter fixtures per provider (Anthropic `server_tool_use` counts
and error exclusion; Responses item keys and preview; xAI counters and tick
conversion; Gemini 3 unique queries and 2.5 grounded prompt); replay with a
partial charge with and without a cap, including `Continue`; interrupted call
with hosted blocks gives `unreported`; catalog key coverage.

## Risks and open questions

- Whether OpenAI bills `open_page` and `find_in_page` actions of a
  `web_search_call` is not stated; counting every item may overstate.
- Meta bills per search query; its docs do not say whether one
  `web_search_call` can hold several queries.
- A mixed-currency or partial charge now blocks further capped calls, as an
  unknown charge already does; this is intended but strict.
- `Spend` gains two fields; the outcome stream exposes `Spend` on `Outcome`.
