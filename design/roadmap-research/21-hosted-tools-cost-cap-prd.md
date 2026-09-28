# 21. Hosted tools under a cost cap — draft PRD

**Status:** proposal for review; no API approved. **Size/difficulty:** M / high ([roadmap](../remaining-work.md)). **Reference:** [research](21-hosted-tools-cost-cap-research.md), [hosted-tool pricing](../hosted-tool-pricing.md), [budgets](../budgets.md). Dashi source baseline: `015b0ec56c8d`.

## User problem

An app wants to offer provider-hosted search in a Turn with `Budget.MaxCost`. Today Dashi correctly refuses the call because the provider can perform more billable work inside one request than the app can see or interrupt. The developer needs a way to use a *specific, proven bounded route* without weakening the meaning of `MaxCost`. They also need a clear refusal when a route, model, tool mix, price, or provider response makes the bound unknowable. An end user should see an intelligible stop reason and cost status rather than a reassuring number that omits hosted charges.

## Goals and non-goals

- Admit only exact provider/API/model/hosted-tool configurations whose complete worst-case cost can be computed before dispatch from provider-enforced controls and current prices.
- Include **every internal model pass**, repeated input and cache billing, output tokens, all hosted fee units, and rounding in that risk amount. Reserve it through the existing model admission path; settle to the actual reported or fully estimated charge after the call.
- Preserve existing partial-cost and interruption behavior. A missing final counter or unrecognized meter remains unknown spend under a cap, even if a reservation existed.
- Provide a concise diagnostic naming the missing proof or price when capped admission fails, and document the supported configuration explicitly.

No universal hosted-tool cap; no app-side cancellation claimed to stop a provider-internal pass; no new budget DSL, plugin registry, automatic provider switching, or rewriting a partial charge as a fully known reserved charge. `MaxCost` remains a cost upper bound, not an approximate spending target.

## Smallest API and user flow

Keep the caller-facing request and `Budget.MaxCost` unchanged. The developer declares a hosted tool exactly as for an uncapped Turn. Dashi's adapter adds a **route-qualified risk calculation** to the existing `llm.MaxCostAtRisk` path, using a narrowly reviewed profile for one provider API + model family + hosted tool and explicit request controls. The profile is internal versioned knowledge, not a new public configuration surface. `PreparedRequest`/recorded model intent retain the effective pass/use/output limits and the pricing reference so replay explains the admission decision.

Start with **OpenAI Responses `web_search` as an investigation candidate**, because the API exposes `max_tool_calls`. This is *not an enabled route yet*: [the documented limit](https://platform.openai.com/docs/api-reference/responses) covers processed built-in calls but does not settle what an ignored later attempt costs in model tokens. Enable this route only after fixtures plus dated live qualification establish a finite bound on all internal passes and rebilling, and show how the model's output allowance applies across those passes. If qualification fails, keep the current pre-dispatch `StopLimited` and investigate another route; do not ship a best-effort switch under `MaxCost`.

For a qualified route, preflight computes the maximum token charge with the most expensive applicable input/cache/output tiers across all allowed passes, plus maximum hosted request charges and any fixed fees. Use exact/ceiling arithmetic and reject overflow, missing rates, incompatible tool mixes, or unbounded controls. `checkSendBudget` compares this risk with remaining `MaxCost` and passes it to `Runner.Hooks.BeforeSend`; the host returns its reservation terms in `SendDecision.Admission`. The ordinary model result and `Spend` retain *actual* known cost, not the reservation amount; the host releases unused reservation according to its ledger policy. A route qualification is invalidated when model pricing or provider control semantics change until rechecked.

Illustrative flow (names beyond existing API are intentionally not proposed):

```go
runner := dashi.Runner{Agent: agent, Budget: &dashi.Budget{MaxCost: cap}}
// agent's model request declares provider-hosted web_search.
// Qualified exact route: risk fits -> BeforeSend reserves -> call -> settle.
// Any other hosted route: StopLimited before dispatch with a specific reason.
```

The stop reason should say, for example, “`web_search` on this OpenAI Responses model cannot run under the USD cost cap: no qualified bound for billable internal passes.” If the bound exists but exceeds the remainder, report `CostAtRisk` and the remaining amount. Do not suggest `FitOutputTokens` when hosted fees or internal pass costs cannot be safely reduced by lowering output alone.

## Acceptance criteria

- A qualification record names the exact provider API, model set, tool, control values, pricing date, source links, captured response IDs (redacted), fixture names, and live probe date. It records observed billed *cumulative input*, cache, output, hosted counters and total for one use, sequential uses, a refused/ignored extra attempt, and any parallel-use path. A reviewer can reproduce the bound calculation from that record. No route is enabled solely from documentation or a single happy-path fixture.
- On the qualified route only, `MaxCostAtRisk` returns a finite conservative amount incorporating worst-case pass count, maximum input per pass including tool-result growth/re-reading, all output, every hosted meter, cache/tier rates and rounding. A test adversarially sets every component to its permitted maximum and verifies the amount fits only when `used + risk <= cap` without overflow.
- An allowed capped call persists its effective controls and price identity in the model intent; `BeforeSend` receives the risk for atomic app admission. A provider rejection before dispatch charges zero. A completed call settles once from provider-reported total where authoritative, otherwise fully priced final usage. Unused reservation is distinguishable from actual spend.
- Unknown model price, expired/unqualified route, unsupported tool mix, absent or ineffective provider limits, unbounded result/input growth, and a risk above the remaining cap all stop **before dispatch** with distinct actionable reasons. Existing uncapped hosted calls still run and report priced or partial spend.
- A truncated stream, missing final hosted counter, or new meter preserves the observed known cost and marks the remainder unknown/partial; the turn stops under `MaxCost` and cannot be resumed merely by raising the cap. If a complete provider-reported total exceeds the qualified ceiling, record that actual known charge and flag a cap breach. Either anomaly suspends that route's qualification pending investigation. Tests prove no zero-cost inference from a missing terminal event.
- Dated live qualification tests run only through an explicit paid-test gate with a small configured ceiling; normal CI uses captured fixtures. Documentation separates fixture support, prior live observation, and currently qualified hard-cap routes.

## Risks and open decisions

The crucial unanswered question is whether any provider route can prove a finite **total** charge, including extra model passes after a tool limit is reached. OpenAI `max_tool_calls` may bound tool fees without bounding rebilling; xAI `max_turns` bounds turns but [not the number of parallel tool calls in a turn](https://docs.x.ai/developers/tools/tool-usage-details). If neither yields a complete bound, this item remains deferred and the existing refusal is the correct behavior. Provider prices, model context limits, cache treatment, and tool-result size can drift; qualification needs a review owner and a dated expiry or explicit recheck on change. Decide whether route qualification belongs in catalog metadata or an internal adapter table only after the first route is proven; one hard-coded qualified slice is preferable to a general policy framework at this stage.
