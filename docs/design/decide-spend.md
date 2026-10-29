# Side model spend in Spend and MaxCost

**Status:** Draft · **Author:** Claude · **Date:** 2026-10-29\
**Workflow:** Spec, then wait for Curtis's review of the PRD PR, then
build.\ **PRD:** [decide-spend](../prds/decide-spend.md)

## Context

The `decide/` packages send requests to decision models, but only
`listtool` can put that spend in `Spend` (in `ToolCost`, outside
`MaxCost`, and only with a host `Price` function). Hook spend reaches only
`On…` callbacks, and a hook that passes leaves no record. The root package
must not import Decide ([decision models](../../design/decide/README.md)),
so the mechanism is generic: a **side model** is any model a tool or hook
calls itself, apart from the turn's model calls.

## Goals

- Every Decide request a `decide/` package sends shows in `Spend` and
  replays from the turn record, whatever the hook's answer or error.
- `MaxCost` covers model cost plus side model cost, before and between
  steps.
- Under a cap, the decide packages never send a request whose worst case
  does not fit, nor one with no price.
- `ModelCallCosts` still sum to `Spend.Cost`; token caps do not change.

## Non-goals

- `ToolCost` toward `MaxCost`; reservations across concurrent side model
  work; a per-cost list on `TurnView` for credit recorders (a recorder
  reads the step).

## Proposal

### 1. Reporting: `AddSideModelCost` (root)

```go
// SideModelCost is spend on a side model: a model that a tool or hook
// calls itself, such as a decision model.
type SideModelCost struct {
    Provider     string `json:"provider,omitempty"` // "typesafe"
    Model        string `json:"model"`              // "jev-1.13.0"
    Calls        int    `json:"calls"`
    InputTokens  int    `json:"input_tokens,omitempty"`
    OutputTokens int    `json:"output_tokens,omitempty"`
    Cost         *Money `json:"cost,omitempty"` // nil: price unknown
    // Set by Dashi from the context, never by the caller:
    CallID    string    `json:"call_id,omitempty"`
    Hook      string    `json:"hook,omitempty"`
    HookPoint HookPoint `json:"hook_point,omitempty"`
}

// AddSideModelCost adds c to the turn that ctx serves. It never blocks or
// fails; outside a running turn, or after the turn has stopped, it does
// nothing.
func AddSideModelCost(ctx context.Context, c SideModelCost)
```

The runner already puts the call (`CurrentCall`) or the hook's turn
(`CurrentTurn`) in the context; it adds the hook's name and point the same
way. `AddSideModelCost` appends to the runtime's pending costs under a
lock. The next committed step, of any kind, takes them as
`Step.SideModelCosts`, as it takes `HookResults` and `Warnings`. Unlike
hook results, `dropHooks` keeps them: a chain cancelled mid-way still
spent. A cost with zero calls and no `Cost` is ignored. A negative count
is raised to zero, and an invalid `Cost` is cleared to unknown with a
`side_model_cost_invalid` warning (works by default).

Why the context rather than a field on each decision and on
`tool.Outcome`: a hook that returns an error, a check that passes, a chain
that is dropped, and a tool cancelled before its outcome all lose a
returned value but not a reported one. `tool.Report` is the precedent.
`tool` cannot import `dashi` or `llm`, so an `Outcome` field would also
need its own copy of the type.

`Resolve` gains `SideModelCosts []SideModelCost`, recorded on the step the
command commits, for a host that ran a check outside a run. This covers
issue #309's path: `/task ID continue` runs `CommandRisk.Check` with no
turn in its context, then sends `Resolve{Approval, SideModelCosts:
{v.Cost}}`. `ApprovalDecision` does not change. A `Resolve` with an
invalid entry is rejected before anything is written, like any invalid
command.

### 2. Spend

```go
type Spend struct {
    // ...existing fields...
    // SideModelCost holds known side model costs by currency. MaxCost covers it.
    SideModelCost  map[string]money.Amount `json:"side_model_cost,omitempty"`
    // SideModelUsage counts side model calls and tokens. Token caps ignore it.
    SideModelUsage SideModelUsage `json:"side_model_usage,omitzero"`
}
type SideModelUsage struct{ Calls, InputTokens, OutputTokens int }
```

`calculateSpend` adds every `Step.SideModelCosts` entry in record order.
An entry with a nil `Cost` adds one to `CostUnknown`. Under a cap, a cost
in another currency also counts as unknown, as for model calls. Counts
saturate, and money overflow adds to `CostUnknown`, per [spend
aggregation](spend-usage-accounting.md). `Clone` and `Add` cover both
fields, so `delegate.Report` and task totals include them.

`Spend.String()` adds a section between model and tools, qualified by
`CostUnknown` like tool amounts: `model: 0.12 USD; side models: 0.0063
USD; tools: 2 USD`.

### 3. MaxCost

`costIn(currency)` returns `Cost[c] + SideModelCost[c]`, known only when
`CostUnknown` is zero and neither map holds another currency. Every
existing check uses it, so:

- **Before a model call**, `checkSendBudget` also adds the pending costs
  (from `BeforeModel` hooks such as `contextrank`), so the hooks' spend
  and the call's worst case are checked together. If the call does not
  fit, the `limited` stop step carries those costs.
- **Between steps**, `budgetStop` stops the turn `limited` (`LimitCost`)
  once model plus side model cost reaches the cap. A tool or hook that
  brings spend to the cap finishes; the turn stops at the next check.
- `LimitDetail.CostUsed` and `SendPlan.Remaining` include side model cost.

A new `TurnView.RemainingCost() (Money, bool)` returns the cap minus
`costIn`, floored at zero; zero when spend is unknown; false with no cost
cap. It reads the recorded view, so two side model calls that start
together each see the same amount (see Tradeoffs).

### 4. Decide packages

New package `decide/pricing`: `Lookup(model string) (llm.Pricing, bool)`,
USD per million tokens, input only (no vendor lists an output price):

| Model | Input | Source, checked 2026-10-29 |
| --- | --- | --- |
| `jev-latest`, `jev-<version>` | 0.042 | typesafe.ai ("$42 per billion") |
| `clef` / `clef-flash` / `clef-omni` | 0.24 / 0.038 / 0.15 | Workers AI pricing page |

Jev matches by prefix because TypeSafe prices Jev, not a version. Other
models, such as OpenAI's `gpt-6-luna` through the beta Decisions API, are
unpriced. The model catalog is the wrong home: it lists models Dashi's
clients call, by exact ID. A provenance test, like
`catalog/sources_test.go`, pins each row. When Decide ships its own table,
`Lookup` delegates to it.

Each config (`listtool.Config`, `contextrank.Ranker`, `turnwatch.Watcher`,
`toolcheck.CommandRisk`, `toolcheck.PromptInjection`) gets `LookupPricing
func(model string) (llm.Pricing, bool)`; nil means `pricing.Lookup`. It
replaces `listtool.Config.Price`. A shared `decide/internal/spend` helper:

1. **Before sending**, if `RemainingCost` reports a cap: look up the price
   of the client's model (`DefaultModel()`, or the pinned one); none means
   "no price". Else estimate input tokens as text bytes / 4 plus the
   question text, price them, and compare with what is left.
2. **After**, build the `SideModelCost` from the response's `Model` and
   summed `Usage`, priced with `Pricing.Estimate`, and call
   `AddSideModelCost`. Provider comes from `defaultclient.Setup` when the
   default client is used, else from the model prefix.

What each package does when step 1 says no, per its design:

| Package | Outcome |
| --- | --- |
| `listtool` | failed result to the model: "this call could cost up to X; Y of the turn's cost cap is left; judge fewer items" (or "no price for model M") |
| `contextrank`, `turnwatch` | hook returns an error; Dashi records `hook_failed` and the turn runs without the hook |
| `PromptInjection` | unchecked, as on a paused check |
| `CommandRisk` | holds the call for approval, as for any failure the input can cause |

The result types (`listtool.Result`, `contextrank.Selection`,
`turnwatch.Check`, `toolcheck.Verdict`) gain `Cost dashi.SideModelCost`,
the exact record reported. `listtool` stops setting `Outcome.Cost` and
drops `cost_dropped` from its data. Costs are added in `Check` itself, so
a host that calls `Check` from its own hook is counted too.

### 5. CLI

The CLI keeps `OnCheck` for checked, flagged, and paused counts, and drops
`decideTotals`. `/settings` "this run" sums `Spend.SideModelUsage` and
`SideModelCost` over the turns the process ran, child tasks included. The
footer's `costText` sums `Cost` and `SideModelCost` per currency. The
recorded-approval path passes `Verdict.Cost` in `Resolve`.

### 6. Record and replay

New fields: `Step.SideModelCosts`, `Resolve.SideModelCosts`,
`Spend.SideModelCost`, `Spend.SideModelUsage`. Replay validates each entry
(non-empty `Model`, non-negative counts, valid `Money`) and rejects a turn
with an invalid one, as it does for any invalid fact; the runtime never
writes one. Costs on steps after the stop that ended a turn still count,
like late model results. No migration: Dashi has no users yet.

### 7. Child turns

A hook or tool in a child turn gets the child's context, so its costs go
to the child's `Spend` and the child's own `MaxCost`. A parent's cap does
not include children, as today. `delegate`'s summary for the parent model
(`formatSpend`) adds side model cost.

## Alternatives considered

- **Fold side model cost into `Spend.Cost`.** No new field, and `MaxCost`
  works unchanged. Rejected: `ModelCallCosts` would no longer sum to
  `Spend.Cost`, and credit recorders rely on that.
- **Count all `ToolCost` toward `MaxCost`.** Simplest rule. Rejected: it
  changes the cap for every host tool, an open product question.
- **Put Decide models in the catalog.** Rejected above; prices also belong
  in Decide in the long run.
- **Reserve side model spend in the runtime** (`Hold`/`Release`). Exact
  under concurrency, but a new API for amounts that are cents. Deferred.

## Tradeoffs and consequences

- **Bounded overshoot.** Concurrent side model calls each check the same
  recorded amount, so a cap can be passed by the worst case of side model
  calls that run at once. The between-step check then stops the turn. The
  model-call check stays exact.
- **Estimates.** The pre-check uses 4 bytes per token, like
  `llm.EstimatedInputTokens`; a request that tokenizes worse can exceed
  it.
- **Lost costs.** A worker crash between a request and the next commit
  loses that cost, as an interrupted model call loses unreported usage.
- **Prices go stale.** Dated rows, a provenance test, and a host override.

## Rollout

One implementation PR: root (`AddSideModelCost`, `Spend`, `costIn`,
`RemainingCost`, `Resolve`), `decide/pricing`, the four packages, the CLI,
and docs: `docs/guides/budgets.md`, `design/decide/README.md` decision 5
and its open questions, and the `CHANGELOG`. Tests: replay of costs on
each step kind; a dropped hook chain still counts; a capped turn stopped
by side model cost before the next model call; a `listtool` refusal; an
unpriced model with and without a cap; `Spend.Add`, `Clone`, `String`, and
JSON round trip; a child turn's costs in `delegate.Report`. All keyless
with `decidetest`; no live calls.

## Open questions

None block the build. Curtis may want to confirm the PRD's two product
choices: side model tokens stay out of token caps, and the CLI footer
shows model plus side model cost as one figure.
