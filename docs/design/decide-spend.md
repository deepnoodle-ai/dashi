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

- Every Decide request a `decide/` package sends during a run shows in
  `Spend` and replays from the turn record, whatever the hook answered.
- `MaxCost` covers model cost plus side model cost, before and between
  steps.
- Under a cap, the decide packages never send a request whose worst case
  does not fit what is left.
- `ModelCallCosts` still sum to `Spend.Cost`; token caps do not change.

Non-goals are in the PRD.

## Proposal

### 1. Adding a side model call (root)

```go
// SideModelCall records calls that a tool or hook made to a side model:
// a model it calls itself, such as a decision model.
type SideModelCall struct {
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

// AddSideModelCall adds c to the turn that ctx serves. It never blocks
// or fails. Outside a running turn, or after the run has committed its
// last step, it does nothing.
func AddSideModelCall(ctx context.Context, c SideModelCall)

// RemainingCost is the turn's MaxCost minus model and side model cost,
// including side model calls added but not yet committed. It is zero
// when spend is unknown, and false when the turn has no cost cap or ctx
// serves no running turn.
func RemainingCost(ctx context.Context) (Money, bool)
```

The runner already puts the tool call (`CurrentCall`) or the hook's turn
(`CurrentTurn`) in the context; it adds the hook's name and point the same
way. `AddSideModelCall` appends to the runtime's pending list under a
lock. The next committed step, of any kind, takes the list as
`Step.SideModelCalls`. Under the lock, `commit` moves the list into the
step and empties it before `Recorder.Commit`; if the commit fails, it puts
the entries back. A tool goroutine may add while the recorder writes, so
the copy-then-clear used for `HookResults` would lose entries. `commit`
merges entries with the same source, provider, model, and currency
(summing calls, tokens, and cost), so a judge over 300 items records one
entry per step, not 300. Unlike hook results, `dropHooks` keeps the list:
a chain cut short still spent. An entry with zero calls and no `Cost` is
ignored. A negative count becomes zero, and an invalid `Cost` becomes nil
(unknown) with a `side_model_call_invalid` warning.

A context call, like `tool.Report`, rather than a field on each decision
and on `tool.Outcome`: an error, a passed check, and a dropped chain lose
a returned value but not an added one.

**Dry runs.** `Advance` asks a held turn's hooks, in memory, whether its
next model call may go (`askSend`); nothing is committed. `ModelPlan`
gains `DryRun bool`. In a dry run, `AddSideModelCall` does nothing, and
the decide hooks send nothing and give no opinion. Without this, each poll
of a held turn would spend with no record.

**Outside a run.** `Resolve` gains `SideModelCalls []SideModelCall`,
recorded on the step the command commits, and validated with the command.
This covers issue #309's path: `/task ID continue` runs
`CommandRisk.Check` with no turn in its context, then sends
`Resolve{Approval, SideModelCalls: {v.SideModel}}`. `ApprovalDecision`
does not change, and #309 adds no cost to it.

### 2. Spend

```go
type Spend struct {
    // ...existing fields...
    // SideModelCost holds known side model cost by currency.
    // MaxCost covers it together with Cost.
    SideModelCost  map[string]money.Amount `json:"side_model_cost,omitempty"`
    // SideModelUsage counts side model calls and tokens. Token caps
    // ignore it.
    SideModelUsage SideModelUsage `json:"side_model_usage,omitzero"`
}
type SideModelUsage struct{ Calls, InputTokens, OutputTokens int }
```

`calculateSpend` adds every `Step.SideModelCalls` entry in record order.
An entry with a nil `Cost` adds one to `CostUnknown`. Under a cap, a cost
in another currency also counts as unknown, as for model calls. Counts
saturate, and money overflow adds to `CostUnknown`, per [spend
aggregation](spend-usage-accounting.md). `Clone` and `Add` cover both
fields, so `delegate.Report` and task totals include them.
`uncheckableCostReason` gains a cause: "a side model call had no price
(model M)".

`Spend.String()` adds a section between model and tools, qualified by
`CostUnknown` like tool amounts: `model: 0.12 USD; side models: 0.0063
USD; tools: 2 USD`.

### 3. MaxCost

`costIn(currency)` returns `Cost[c] + SideModelCost[c]`, known only when
`CostUnknown` is zero and neither map holds another currency. Every
existing check uses it, so:

- **Before a model call**, `checkSendBudget` also counts the pending list
  (from `BeforeModel` hooks such as `contextrank`), so their spend and the
  call's worst case are checked together. If the call does not fit, the
  `limited` stop step carries the list.
- **Between steps**, `budgetStop` stops the turn `limited` (`LimitCost`)
  once model plus side model cost reaches the cap. A tool or hook that
  brings spend to the cap finishes; the turn stops at the next check.
- `LimitDetail.CostUsed` and `SendPlan.Remaining` include side model cost.

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

1. **Before sending**, if `RemainingCost` reports a cap: find the price of
   the model asked for (the pinned model, else `DefaultModel()`). No
   price, or a price in another currency than the cap, is a setup gap.
   Else estimate input tokens as text bytes / 4 plus the questions, price
   them, and compare with what is left.
2. **After each request**, build a `SideModelCall` from the response's
   `Model` and `Usage`, priced with `Pricing.Estimate`, and add it. When
   the response's model has no price, use the price of the model asked
   for. Provider comes from `defaultclient.Setup` for the default client,
   else from the model prefix.

Adding each request as it finishes, not one sum at the end, means a
`judge_items` call stopped mid-way has its finished requests on the stop
step. What each package does when step 1 says no:

| Package | Setup gap (no price) | Does not fit what is left |
| --- | --- | --- |
| `listtool` | failed result: "no price for model M under the turn's cost cap" | failed result: "this call could cost up to X; Y of the cap is left; judge fewer items" |
| `contextrank`, `turnwatch` | fail open, as with no client | hook error; Dashi records `hook_failed`; the turn runs without the hook |
| `PromptInjection` | unchecked, as on a paused check | unchecked |
| `CommandRisk` | runs unchecked, as on a paused check | holds the call for approval, as for any failure the input can cause |

A setup gap is like a paused check (design decision 3 of [decision
models](../../design/decide/README.md)): no input causes it, so the hooks
fail open. They warn once per turn: a hook that finds its own earlier
warning in the turn view does not warn again.

The result types (`listtool.Result`, `contextrank.Selection`,
`turnwatch.Check`, `toolcheck.Verdict`) gain `SideModel SideModelCall`,
the sum of what the package added. `listtool` stops setting `Outcome.Cost`
and drops `cost_dropped` from its data. Costs are added in `Check` itself,
so a host that calls `Check` from its own hook is counted too.

### 5. CLI

The CLI keeps `OnCheck` for checked, flagged, and paused counts, and drops
`decideTotals`. `/settings` "this run" sums `Spend.SideModelUsage` and
`SideModelCost` over the turns the process ran, child tasks included. The
footer's `costText` sums `Cost` and `SideModelCost` per currency. The
recorded-approval path passes `Verdict.SideModel` in `Resolve`.

### 6. Record and replay

New fields: `Step.SideModelCalls`, `Resolve.SideModelCalls`,
`ModelPlan.DryRun`, `Spend.SideModelCost`, `Spend.SideModelUsage`. Replay
validates each entry (non-empty `Model`, non-negative counts, valid
`Money`) and rejects a turn with an invalid one, as for any invalid fact;
the runtime never writes one. Entries on steps after the stop that ended a
turn still count, like late model results. No migration: Dashi has no
users yet.

### 7. Child turns

A hook or tool in a child turn gets the child's context, so its side model
calls go to the child's `Spend` and the child's own `MaxCost`. A parent's
cap does not include children, as today. `delegate`'s summary for the
parent model (`formatSpend`) adds side model cost.

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

- **Bounded overshoot.** `RemainingCost` counts pending entries, but
  requests in flight at once each see the same amount. A cap can be passed
  by their worst case; the between-step check then stops the turn. The
  model-call check stays exact.
- **Estimates.** The pre-check uses 4 bytes per token, like
  `llm.EstimatedInputTokens`; a request that tokenizes worse can exceed
  it.
- **Lost costs.** A request that finishes after the run commits its last
  step (a stop that does not wait for running tools), or before a worker
  crash, is not recorded, as an interrupted model call loses unreported
  usage.
- **Fewer checks under a cap with no price.** The hooks run unchecked and
  warn; a host supplies `LookupPricing` to turn them back on.
- **Credit recorders.** `ModelCallCosts` no longer sum to what `MaxCost`
  limits. A recorder that charges credits also charges each step's
  `SideModelCalls`.
- **Prices go stale.** Dated rows, a provenance test, and a host override.

## Rollout

One implementation PR: root, `decide/pricing`, the four packages, the CLI,
and docs (`docs/guides/budgets.md` with the credit-recorder rule,
`design/decide/README.md` decision 5, `CHANGELOG`). Tests, all keyless
with `decidetest`: entries on each step kind, during a commit, and from a
dropped chain; a dry-run poll; a capped stop before the next model call; a
`listtool` refusal; an unpriced model with and without a cap; `Spend`
`Add`, `Clone`, `String`, and JSON; a child turn in `delegate.Report`.
