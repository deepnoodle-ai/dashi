# Side model and tool spend in MaxCost

**Status:** Draft · **Author:** Claude · **Date:** 2026-10-29\
**Workflow:** Spec, then wait for Curtis's review of the PRD PR, then
build.\ **PRD:** [decide-spend](../prds/decide-spend.md)

## Context

`MaxCost` limits model calls only. Tool costs (`tool.Outcome.Cost`) sit
in `Spend.ToolCost`, outside it; Decide requests reach `Spend` only
through `listtool`'s optional `Price`, and hook spend only `On…`
callbacks. The root must not import Decide ([decision
models](../../design/decide/README.md)), so the mechanism is generic: a
**side model** is any model a tool or hook calls itself. Goal: `MaxCost`
limits **total cost** (model, side model, and tool cost), and
`ModelCallCosts` still sum to `Spend.Cost`.

## Proposal

### 1. Adding a side model call (root)

```go
// SideModelCall records calls that a tool or hook made to a side model.
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
// or fails. Outside a running turn, or after the run's last commit, it
// does nothing.
func AddSideModelCall(ctx context.Context, c SideModelCall)

// RemainingCost is MaxCost minus total cost, including side model calls
// not yet committed. It is zero when nothing is left or the cost is
// unknown, and false when the turn has no cost cap or ctx serves no
// running turn.
func RemainingCost(ctx context.Context) (Money, bool)
```

`AddSideModelCall` appends to a pending list under a lock. The next
committed step of any kind takes it as `Step.SideModelCalls`: `commit`
moves the list into the step under the lock before `Recorder.Commit` (a
tool goroutine may add while the recorder writes; a failed commit ends
the run). `commit` merges entries that share `CallID`, `Hook`,
`HookPoint`, `Provider`, `Model`, and cost currency; nil-cost entries
merge only with each other. `dropHooks` keeps the list: a chain cut short
still cost money. An invalid `Cost` becomes nil with a
`side_model_call_invalid` warning.

**Dry runs.** When `Advance` polls a held turn's hooks in memory,
`ModelPlan.DryRun` is true: `AddSideModelCall` does nothing, and the
decide hooks send nothing and give no opinion.

**Outside a run.** `Resolve` gains `SideModelCalls`, recorded on the step
the command commits. For issue #309, `/task ID continue` runs
`CommandRisk.Check` with no turn in its context, then sends
`p.Approve().WithRules(version, finding)` (from #343) with
`SideModelCalls: {v.SideModel}`. `ApprovalDecision` gets no cost fields.

### 2. Spend

```go
type Spend struct {
    // ...existing fields...
    SideModelCost  map[string]money.Amount `json:"side_model_cost,omitempty"`
    SideModelUsage SideModelUsage          `json:"side_model_usage,omitzero"` // token caps ignore it
}
type SideModelUsage struct{ Calls, InputTokens, OutputTokens int }

// TotalCost sums Cost, SideModelCost, and ToolCost by currency: the
// amount MaxCost limits. A lower bound when CostPartial or CostUnknown
// is nonzero.
func (s Spend) TotalCost() map[string]money.Amount
```

`calculateSpend` adds each `Step.SideModelCalls` entry; a nil `Cost` adds
one to `CostUnknown`. `Clone` and `Add` cover the new fields, so
`delegate.Report` includes them. `Spend.String()` adds `side models: …`
between model and tools; either unknown counter marks all three "at
least".

### 3. The cap check: reached means stop

The existing rule is "reached": spending up to the cap is allowed (a call
whose worst case lands exactly on it may run), and once spend equals the
cap, no new model call starts (`budgetSpentStop`). Only the check before
tools uses "exceeded" (`budgetOvershotStop`), because tools were free to
the cap. Now they are not, so every check uses "reached":

- `costIn(currency)` reads `TotalCost()`; it is known only when
  `CostUnknown` is zero and no amount is in another currency. So
  `LimitDetail.CostUsed`, `SendPlan.Remaining`, and `RemainingCost` cover
  total cost. `spendWithCallInFlight` adds the in-flight worst case to
  `Spend.Cost[currency]`, not to the `costIn` total, so nothing counts
  twice.
- **Before a model call**, `checkSendBudget` also counts pending side
  model calls from `BeforeModel` hooks; a `limited` stop step carries them.
- **Before tools** (the run loop) and **within a batch**, for every call,
  the runner checks spend ≥ cap on state replayed after each commit:
  before the call's `BeforeTool` hooks, and again after `drainOne` frees
  a slot, just before `StepToolStarted` or an external hand-off. (With
  the default `ToolConcurrency` of 1, the previous call's cost commits
  during that wait.) When the check fails, no further call starts;
  running calls finish; the turn stops `limited` (`LimitCost`). Calls
  that did not start stay open, and a raise through `Continue` runs them.
  Batch-end precedence: unknown result, fatal error, cost stop, pause,
  wait.

`uncheckableCostReason` takes the replay state, so it can name "a side
model call had no price (model M)" or "a tool reported a partial cost
(tool T)".

### 4. Tool cost

A tool's cost arrives on a succeeded or failed outcome, from the executor
or from `Resolve.Outcome` (external and detached calls), and counts when
`StepToolFinished` commits. `tool.Report` carries progress only. A tool
that returns an error, is cancelled, or finishes after the turn ended
records no cost; the guide says a tool that spent returns `tool.Failed`
with `Cost`.

`tool.Cost` gains `Partial bool`: "Amount is a lower bound; some work had
no price"; `{Amount: 0, Partial: true}` means "spent, amount unknown".
Tool costs follow the model-call rules:

| Tool cost | No cap | Under a cap |
| --- | --- | --- |
| exact, cap's currency | `ToolCost` | counts toward the cap |
| nonzero, another currency | `ToolCost` | also `CostUnknown`: uncheckable stop |
| `Partial` | also `CostPartial` | also `CostUnknown`: uncheckable stop |

A raise cannot clear an uncheckable stop. Only the tool knows its price,
so a tool can call `RemainingCost(ctx)` and fail when its price does not
fit. There is no hold.

**No double counting.** `listtool` drops `Config.Price`, `Outcome.Cost`,
and `cost_dropped`; Decide spend goes only to `SideModelCost`. A tool that
adds side model calls reports only its other costs in `Outcome.Cost`.

### 5. Decide packages

New package `decide/pricing`: `Lookup(model string) (llm.Pricing, bool)`,
USD per million input tokens; no vendor lists an output price, so output
counts as zero.

| Model | Input | Page that states it, checked 2026-10-29 |
| --- | --- | --- |
| `jev-latest`, `jev-<version>` | 0.042 | https://typesafe.ai/ (cost section, "$42 per billion input tokens"; no separate pricing page exists) |
| `@cf/cloudflare/clef` / `clef-flash` / `clef-omni` | 0.24 / 0.038 / 0.15 | https://developers.cloudflare.com/workers-ai/platform/pricing/ ("Other model pricing") |

Jev matches by prefix: TypeSafe prices Jev, not a version. Other models
are unpriced. A provenance test, like `catalog/sources_test.go`, pins each
row. Each config gets `LookupPricing func(model string) (llm.Pricing,
bool)`; nil means `pricing.Lookup`. A shared `decide/internal/spend`
helper:

1. **Before sending**, under a cap: price the model asked for (pinned,
   else `DefaultModel()`). No price, a price in another currency, or zero
   left is a **setup gap**: no input causes it. Else estimate input as
   text bytes / 4, price it, and compare with what is left.
2. **After each request**, add a `SideModelCall` from the response's
   `Model` and `Usage` (falling back to the asked-for model's price), so a
   `judge_items` call stopped mid-way still counts finished requests.

| Package | Setup gap | Does not fit what is left |
| --- | --- | --- |
| `listtool` | failed result naming the model, or "nothing is left of the cost cap" | failed result: "could cost up to X; Y is left; judge fewer items" |
| `contextrank`, `turnwatch` | fail open, as with no client | `hook_failed`; the turn runs without the hook |
| `PromptInjection` | unchecked | unchecked |
| `CommandRisk` | no price: unchecked; zero left: no opinion, and the batch stop (section 3) keeps the call from starting | holds the call for approval |

Zero left cannot reach a starting shell call: the check in section 3 runs
before its hooks and again before it starts. So no shell call runs
unchecked at the cap, and none is held for approval only because the cap
is used up. Hooks warn once per turn. Result types (`listtool.Result`,
`contextrank.Selection`, `turnwatch.Check`, `toolcheck.Verdict`) gain
`SideModel SideModelCall`. Costs are added in `Check` itself, so a host
calling `Check` from its own hook is counted too.

### 6. CLI, replay, and child turns

The CLI drops `decideTotals`: `/settings` sums `SideModelUsage` and
`SideModelCost` over the turns it ran, and the footer shows
`Spend.TotalCost()`. Replay rejects an invalid side model call (empty
`Model`, negative counts, invalid `Money`). No migration: Dashi is tagged
but has no external users. A child turn's spend goes to the child's
`Spend` and its own `MaxCost`; a parent's cap does not include children.

## Alternatives considered

- **Keep tool cost outside `MaxCost`.** Rejected by Curtis: a cap that
  leaves out tool spend is not the ceiling a host promises.
- **A separate `MaxToolCost`.** Hosts would add two caps to get one
  promise.
- **Fold side model cost into `Spend.Cost`.** Breaks `ModelCallCosts`
  summing to it.
- **At the cap, run shell calls unchecked or hold them.** Unchecked drops
  the risk check; holding denies the calls in unattended runs. Rejected
  for the reached rule.
- **Reserve spend in the runtime** (`Hold`/`Release`). Deferred.

## Tradeoffs and consequences

- **Overshoot.** The guarantee is a behavior: once committed total cost
  reaches the cap, no tool call starts and no model call is sent. Work
  already running finishes, and an external call's cost can arrive later,
  so one call can pass the cap by any amount.
- **The cap stops more.** Turns that report tool costs stop sooner. A
  model call that lands exactly on the cap no longer gets its tool calls
  run: the turn stops `limited`, and a raise runs them. A tool cost in
  another currency stops a capped turn.
- **Credit recorders** also charge each step's `SideModelCalls` and each
  tool's `Outcome.Cost`, since `ModelCallCosts` no longer sum to the cap.

## Rollout

One implementation PR. Docs to update: `docs/guides/budgets.md` (total
cost, the reached rule for tools, `Partial`, `tool.Failed` with `Cost`,
child turns, credit recorders, `Spend.String()` example),
`design/budgets.md`, `design/decisions.md` decision 21,
`design/decide/README.md` decision 5 and open questions,
`design/decide/list-tools.md`, `budget_spend.go` doc comments, and
`cmd/dashi/sessionstore/inspection_format.go`. `listtool` surface to
remove: `Price` (`config.go`), `CostDropped` (`record.go`), their tests
(`record_test.go`, `pick_test.go`), `doc.go`, and the example.

CHANGELOG, Changed: `MaxCost` covers tool and side model cost;
`CostUsed`, `SendPlan.Remaining`, and the footer show total cost; tool
calls stop once spend reaches the cap; another-currency tool costs stop
capped turns. Removed: `listtool.Config.Price`.

Tests (keyless, `decidetest`): entries on each step kind, during a
commit, and from a dropped chain; a dry-run poll; a stop before the next
model call; a batch with concurrency 1 stopped by a tool cost, then run
after a raise; a shell call that would start at exactly the cap (no
check sent, no hold, `limited` stop); partial and other-currency tool
costs; a side model call with a model call in flight; a `listtool`
refusal; an unpriced model with and without a cap; `Spend` methods and
JSON; a child turn in `delegate.Report`.
