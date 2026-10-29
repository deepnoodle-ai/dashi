# Side model and tool spend in MaxCost

**Status:** Draft · **Author:** Claude · **Date:** 2026-10-29\
**Workflow:** Spec, then wait for Curtis's review of the PRD PR, then
build.\ **PRD:** [decide-spend](../prds/decide-spend.md)

## Context

`MaxCost` limits model calls only. Tool costs (`tool.Outcome.Cost`) sit
in `Spend.ToolCost`, outside it. The `decide/` packages' requests reach
`Spend` only through `listtool`'s optional `Price` function, and hook spend
reaches only `On…` callbacks. The root must not import Decide ([decision
models](../../design/decide/README.md)), so the mechanism is generic: a
**side model** is any model a tool or hook calls itself.

## Goals

`MaxCost` limits **total cost** (model, side model, and tool cost); every
Decide request shows in `Spend` and replays; `ModelCallCosts` still sum to
`Spend.Cost`; token caps do not change. Use cases are in the PRD.

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

// RemainingCost is the turn's MaxCost minus its total cost, including
// side model calls added but not yet committed. It is zero when nothing
// is left or the cost is unknown, and false when the turn has no cost
// cap or ctx serves no running turn.
func RemainingCost(ctx context.Context) (Money, bool)
```

The runner already puts the tool call (`CurrentCall`) or the hook's turn
(`CurrentTurn`) in the context; it adds the hook's name and point the same
way. `AddSideModelCall` appends to a pending list under a lock. The next
committed step, of any kind, takes it as `Step.SideModelCalls`: `commit`
moves the list into the step and empties it under the lock before
`Recorder.Commit`. (A tool goroutine may add while the recorder writes, so
the copy-then-clear used for `HookResults` would lose entries. A failed
commit ends the run, so nothing is put back.) `commit` merges entries that
share `CallID`, `Hook`, `HookPoint`, `Provider`, `Model`, and cost
currency, so a judge over 300 items records one entry per step; entries
with a nil `Cost` merge only with each other. Unlike hook results,
`dropHooks` keeps the list: a chain cut short still cost money. An entry
with zero calls and no `Cost` is ignored; a negative count becomes zero,
and an invalid `Cost` becomes nil with a `side_model_call_invalid` warning.

**Dry runs.** When `Advance` polls a held turn's hooks in memory
(`askSend`), `ModelPlan.DryRun` is true: `AddSideModelCall` does nothing,
and the decide hooks send nothing and give no opinion.

**Outside a run.** `Resolve` gains `SideModelCalls []SideModelCall`,
recorded on the step the command commits. This covers issue #309's path:
`/task ID continue` runs `CommandRisk.Check` with no turn in its context,
then sends `p.Approve().WithRules(version, finding)` (from #343) with
`SideModelCalls` set to `{v.SideModel}`. The rules fields say what the
check found; `SideModelCalls` says what it cost. `ApprovalDecision` gets
no cost fields.

### 2. Spend

```go
type Spend struct {
    // ...existing fields...
    // SideModelCost holds known side model cost by currency.
    SideModelCost  map[string]money.Amount `json:"side_model_cost,omitempty"`
    // SideModelUsage counts side model calls and tokens. Token caps
    // ignore it.
    SideModelUsage SideModelUsage `json:"side_model_usage,omitzero"`
}
type SideModelUsage struct{ Calls, InputTokens, OutputTokens int }

// TotalCost sums Cost, SideModelCost, and ToolCost by currency: the
// amount MaxCost limits. It is a lower bound when CostPartial or
// CostUnknown is nonzero.
func (s Spend) TotalCost() map[string]money.Amount
```

`calculateSpend` adds every `Step.SideModelCalls` entry in record order.
A nil `Cost` adds one to `CostUnknown`. Counts saturate, and money
overflow adds to `CostUnknown`, per [spend
aggregation](spend-usage-accounting.md). `Clone` and `Add` cover the new
fields, so `delegate.Report` and task totals include them.

`Spend.String()` adds a section between model and tools: `model: 0.12
USD; side models: 0.0063 USD; tools: 2 USD`. `CostPartial` or
`CostUnknown` marks all three sections "at least".

### 3. MaxCost

`costIn(currency)` reads `TotalCost()[currency]`. It is known only when
`CostUnknown` is zero and no section holds a nonzero amount in another
currency. Every existing check reads it, so `LimitDetail.CostUsed`,
`SendPlan.Remaining`, and `RemainingCost` cover total cost.
`spendWithCallInFlight` today writes `costIn` plus the in-flight worst
case into `Spend.Cost`; it adds the worst case to `Spend.Cost[currency]`
instead, so side model and tool cost are not counted twice.

- **Before a model call**, `checkSendBudget` also counts the pending side
  model calls (from `BeforeModel` hooks such as `contextrank`). If the
  call does not fit, the `limited` stop step carries them.
- **Between steps**, `budgetStop` stops the turn `limited` (`LimitCost`)
  once total cost reaches the cap. The check before running tools stays
  "over the cap", as today: a model call that lands exactly on the cap
  still gets its tool results.
- **Within a tool batch**, the runner runs the same over-the-cap check
  twice for each call, on state replayed after every committed result:
  before the call's `BeforeTool` hooks, and again after it waits for a
  free slot (`drainOne`), just before `StepToolStarted` or the hand-off of
  an external call. With the default `ToolConcurrency` of 1, the previous
  call's cost is committed only during that wait. Once the check fails, no
  further call starts; running calls finish and are recorded, then the
  turn stops `limited`. At the end of a batch an unknown result or a fatal
  error wins over the cost stop, which wins over a pause or a wait. Calls
  that did not start stay open, and a raise through `Continue` runs them.

`uncheckableCostReason` takes the replay state instead of `Spend`, so it
can name causes: "a side model call had no price (model M)" and "a tool
reported a partial cost (tool T)".

### 4. Tool cost

A tool's cost arrives on a succeeded or failed outcome: from the
executor, or in `Resolve.Outcome` for an external or detached call.
`tool.Report` carries progress only. The cost counts when the
`StepToolFinished` step commits. No cost is recorded for a tool that
returns an error (`ErrFatal` included), is cancelled before its outcome,
or whose result is refused after the turn ended. The guide says: a tool
that spent returns `tool.Failed` with `Cost`, not an error.

`tool.Cost` gains `Partial bool`: "Amount is a lower bound; some of the
work had no price." `{Amount: 0, Partial: true}` says "spent, amount
unknown"; a nil `Cost` still means nothing was spent. `calculateSpend`
treats a tool cost as a model cost:

| Tool cost | No cap | Under a cap |
| --- | --- | --- |
| exact, cap's currency | `ToolCost` | `ToolCost`, counts toward the cap |
| nonzero, another currency | `ToolCost` | also `CostUnknown`: the turn stops, uncheckable |
| `Partial` | `ToolCost`, `CostPartial` | also `CostUnknown`: the turn stops, uncheckable |

As for model calls, a raise cannot clear an uncheckable stop.
`CostPartial` now counts model and tool calls.

Only the tool knows its price, so Dashi cannot check a tool's cost before
it runs. A tool can call `RemainingCost(ctx)` and return a failed result
when its price does not fit, as the decide packages do. There is no hold.

**No double counting.** `listtool` drops `Config.Price` and stops setting
`Outcome.Cost`; Decide spend goes only to `SideModelCost`. A tool that
adds side model calls reports only its other costs in `Outcome.Cost`.

### 5. Decide packages

New package `decide/pricing`: `Lookup(model string) (llm.Pricing, bool)`,
USD per million input tokens (no vendor lists an output price):

| Model | Input | Source, checked 2026-10-29 |
| --- | --- | --- |
| `jev-latest`, `jev-<version>` | 0.042 | https://typesafe.ai/ ("$42 per billion") |
| `clef` / `clef-flash` / `clef-omni` | 0.24 / 0.038 / 0.15 | https://developers.cloudflare.com/workers-ai/platform/pricing/ |

Jev matches by prefix: TypeSafe prices Jev, not a version. Other models
(`gpt-6-luna`) are unpriced. A provenance test, like
`catalog/sources_test.go`, pins each row. A future Decide table replaces it.

Each config (`listtool.Config`, `contextrank.Ranker`, `turnwatch.Watcher`,
`toolcheck.CommandRisk`, `toolcheck.PromptInjection`) gets `LookupPricing
func(model string) (llm.Pricing, bool)`; nil means `pricing.Lookup`. A
shared `decide/internal/spend` helper:

1. **Before sending**, if `RemainingCost` reports a cap: price the model
   asked for (the pinned model, else `DefaultModel()`). No price, or a
   price in another currency than the cap, is a setup gap. Zero left
   (used up, or unknown) is like a setup gap too: no input causes it, and
   the turn stops at its next check. Else estimate input tokens as text
   bytes / 4 plus the questions, price them, and compare with what is left.
2. **After each request**, add a `SideModelCall` from the response's
   `Model` and `Usage`, priced with `Pricing.Estimate`; when that model has
   no price, use the price of the model asked for. Adding each request as
   it finishes means a `judge_items` call stopped mid-way still counts its
   finished requests.

| Package | Setup gap (no price, zero left) | Does not fit what is left |
| --- | --- | --- |
| `listtool` | failed result: "no price for model M under the turn's cost cap", or "nothing is left of the cost cap" | failed result: "this call could cost up to X; Y of the cap is left; judge fewer items" |
| `contextrank`, `turnwatch` | fail open, as with no client | hook error; `hook_failed`; the turn runs without the hook |
| `PromptInjection` | unchecked, as on a paused check | unchecked |
| `CommandRisk` | unchecked, as on a paused check | holds the call for approval |

A setup gap is like a paused check (design decision 3 of [decision
models](../../design/decide/README.md)): no input causes it, so hooks fail
open, and `CommandRisk` never holds a call only because the cap is used
up. They warn once per turn: a hook that finds its own earlier warning in
the turn view does not warn again.

The result types (`listtool.Result`, `contextrank.Selection`,
`turnwatch.Check`, `toolcheck.Verdict`) gain `SideModel SideModelCall`,
the sum of what the package added. Costs are added in `Check` itself, so
a host that calls `Check` from its own hook is counted too.

### 6. CLI

The CLI keeps `OnCheck` for checked, flagged, and paused counts, and drops
`decideTotals`. `/settings` "this run" sums `SideModelUsage` and
`SideModelCost` over the turns the process ran, child tasks included. The
footer's `costText` shows `Spend.TotalCost()`. The recorded-approval path
passes `Verdict.SideModel` in `Resolve` (section 1).

### 7. Record, replay, and child turns

New fields: `Step.SideModelCalls`, `Resolve.SideModelCalls`,
`ModelPlan.DryRun`, `tool.Cost.Partial`, `Spend.SideModelCost`,
`Spend.SideModelUsage`. Replay rejects an invalid side model call (empty
`Model`, negative counts, invalid `Money`), as for any invalid fact.
Entries on steps after the stop that ended a turn still count, like late
model results. No migration: Dashi is tagged but has no external users.

A hook or tool in a child turn gets the child's context, so its spend
goes to the child's `Spend` and the child's own `MaxCost`. A parent's cap
does not include children, as today. `delegate`'s summary for the parent
model (`formatSpend`) shows total cost.

## Alternatives considered

- **Keep tool cost outside `MaxCost`.** No change for hosts. Rejected by
  Curtis: a cap that leaves out tool spend is not the ceiling a host
  promises.
- **A separate `MaxToolCost`.** Rejected: hosts would add two caps up to
  get one promise, and Decide spend would still need a home.
- **Fold side model cost into `Spend.Cost`.** Rejected: `ModelCallCosts`
  would no longer sum to `Spend.Cost`, and credit recorders rely on that.
- **Reserve spend in the runtime** (`Hold`/`Release`). Exact, but a new
  API most tools do not need. Deferred.

## Tradeoffs and consequences

- **Overshoot.** The guarantee is a behavior: once committed total cost
  is over the cap, no tool call starts and no model call is sent. The
  model-call check stays exact. Anything already running finishes: tools
  and side model calls running at once each see the same remaining
  amount, a tool's cost counts only once it finishes, and an external
  call's cost can arrive later through `Resolve`. Dashi cannot bound one
  tool's cost, so the overshoot has no fixed limit.
- **The cap means more.** Hosts that report tool costs see capped turns
  stop sooner, and a tool cost in another currency now stops a capped turn.
- **Estimates and lost costs.** The Decide pre-check uses 4 bytes per
  token and can run low. Costs listed under the PRD's Not in scope are lost.
- **Credit recorders.** `ModelCallCosts` no longer sum to what `MaxCost`
  limits. A recorder that charges credits also charges each step's
  `SideModelCalls` and each finished tool's `Outcome.Cost`.

## Rollout

One implementation PR: root, `tool.Cost.Partial`, `decide/pricing`, the
four packages, the CLI, and docs. Docs: `docs/guides/budgets.md` (tool
cost inside `MaxCost`, the in-batch stop, `Partial`, `tool.Failed` with
`Cost`, child turns outside the parent cap, the credit-recorder rule, the
`Spend.String()` example), `design/budgets.md`, `design/decisions.md`
decision 21, `design/decide/README.md` decision 5 and its open questions,
`design/decide/list-tools.md`, `listtool`'s `doc.go` and example (which
use `Price`), the `Spend` doc comments in `budget_spend.go`, and the
session inspection's money lines (`inspection_format.go`).

CHANGELOG: under Changed, `MaxCost` covers tool and side model cost;
`LimitDetail.CostUsed`, `SendPlan.Remaining`, and the footer show total
cost; a capped batch stops before the next tool call once over the cap;
a tool cost in another currency now ends a capped turn. Under Removed,
`listtool.Config.Price`.

Tests, keyless with `decidetest`: entries on each step kind, during a
commit, and from a dropped chain; a dry-run poll; a capped stop before
the next model call; a tool cost that stops a batch with concurrency 1
and runs the rest after a raise; partial and other-currency tool costs
under a cap; a side model call plus a model call in flight that still
fits; `CommandRisk` with nothing left; a `listtool` refusal; an unpriced
model with and without a cap; `Spend` `Add`, `Clone`, `TotalCost`,
`String`, and JSON; a child turn in `delegate.Report`.
