# Budgets

**Status:** Proposal · **Date:** 2026-09-27 · **Brief:** [decision
7](decisions.md)

Kernel should become the place where LLM spend is measured and limited. A turn
declares token and cost limits; the engine estimates the worst case before each
model call, counts actual use after it, and stops with a typed, resumable
reason. Applications keep only prices they override, tenant ledgers, credit
reservations, and the policy that decides who may spend more.

## Users and jobs

**A per-turn cap from an API caller.** A gateway passes `max_output_tokens:
4000` and a 0.50 USD cost limit to `Start`. Before the call that could cross 50
cents in the worst case, the turn stops limited, with a `cost` detail giving the
limit, the amount used, and the amount at risk. The answer so far is kept.

**A definition ceiling that callers can only tighten.** A support agent's
definition allows 2 USD per turn. A free-tier caller starts a turn at 0.10 USD;
a caller asking for 5 USD is rejected at `Start` before anything is recorded.

**Hold and resume after a top-up.** A tenant's credits run out mid-turn.
`BeforeSend` sees the at-risk amount exceeds the balance and denies with a host
limit, `insufficient_credits`. The turn stops limited, not refused. After the
user buys credits, the application sends `Continue` and the next call is
admitted. If the turn's own cost limit ran out, `Continue` carries a raise.

**An uncertain call.** A worker dies after a model intent was recorded, so the
provider may have billed it. Until the host reconciles the attempt, the turn
counts it at its recorded at-risk amount and reports one uncertain attempt; the
application's reservation stays held. Reconciliation replaces the hold with the
actual charge.

## How nvoken does it today

Server behavior lives in nvoken-cloud (`nc/`); nvoken has SDK and CLI mirrors.

- **Limits.** A turn carries `max_output_tokens`, `max_estimated_cost_usd`
  (stored in micro-USD), `max_iterations`, and timeouts
  (`nc/internal/services/controls.go:23-30`). Behavior limits resolve against
  deployment-wide ceilings (`controls.go:109-181`,
  `nc/cmd/nvokend/config.go:80-88`); a request may only narrow them
  (`nc/internal/services/turns.go:1217-1302`).
- **Worst case, reserved.** Input is one token per serialized byte plus 1,024;
  at-risk cost is the highest input-side rate times that, plus the output rate
  times maximum output (`nc/internal/domain/budget_cost.go:8-87`).
  `PrepareModelCall` locks the tenant's `credit_accounts` row and holds the
  amount in one transaction (`nc/internal/services/credit_authority.go:42-222`).
- **Call facts.** `model_call_facts` rows move `prepared`, `started`, then
  `settled`, `not_started`, or `uncertain`
  (`nc/internal/domain/credit.go:79-87`,
  `nc/internal/adapters/postgres/queries/credits.sql:98-235`). An uncertain
  call, or a settled call without a cost, is charged its full hold
  (`credits.sql:180-197, 227-228`).
- **Checks between steps.** `checkpointBudgetExceeded` compares iterations,
  output tokens, and summed cost after each step
  (`nc/internal/adapters/divegen/generator.go:1600-1628`). The turn's cost limit
  is never compared with the at-risk amount, so one call can overshoot.
- **Typed stops and hold.** `TurnStopReason` names `max_output_tokens`,
  `max_estimated_cost`, and `insufficient_credits`
  (`nc/internal/domain/turn.go:60-98`). With `on_budget_exhausted: hold` the
  turn parks in `budget_hold` instead of ending `incomplete`
  (`nc/internal/services/generation.go:1496-1500`). `POST .../resume` raises the
  exhausted limit above use and within the installation maximum, but not against
  the behavior (`nc/internal/services/budget_resume.go:186-251`); credit holds
  resume when funding arrives (`budget_resume.go:342-448`).
- **Fail closed.** An unpriced model fails `cost_estimate_unavailable` when a
  cost limit or credits apply (`generation.go:513-524`,
  `credit_authority.go:143-152`).

Kernel should keep this shape and fix three things: compare at-risk cost with
the turn's own limit, bound a raise by the definition, and charge a full hold
only when dispatch is genuinely unknown (nvoken classes every provider error as
uncertain, `generator.go:2343-2348`).

**mobius-cloud** metered cost after each call and checked it before the next
through Dive's pre-iteration hook, without reservation, accepting a one-call
overshoot; calls without usage counted as zero. The meter was removed in
`41cf9a886` (was `internal/loop/executors/agent_budget_meter.go`). **Dive**
fills `Usage.Cost` from a global price registry (`llm/cost.go:30-44`), flags
unseen usage with `TurnOutcome.UsageUnknown` (`outcome.go:169-172`), and has no
spending limits.

## Proposal for Kernel

**Where limits are declared.** `Definition.Limits` is the ceiling;
`Start.Limits` may narrow it for one turn, and `StartFact` records the effective
limits. Validation: a new field set on the definition bounds the same field at
`Start`, an unset one leaves `Start` free, and a deadline may only move earlier.
A widening `Start` fails before the first write. Installation and tenant
ceilings stay in the application, which narrows before `Start`. New fields
beside `ModelAttempts` and `Deadline`: `MaxOutputTokens` (turn total),
`MaxTotalTokens` (all input and output across attempts), and `MaxCost`.

**The pre-call estimate.** `llm.MaxCostAtRisk(Request, Pricing)` values a
prepared request in the worst case: estimated input at the highest input-side
rate, plus the effective `MaxTokens` at the output rate, in the tier the input
selects. It reports false without a sheet or an effective output limit. The
sheet is the prepared request's `ModelInfo.Pricing`, filled for known models by
the ported catalog ([decision 4](decisions.md)); an application overrides it
through the client's model info. With `MaxOutputTokens` set, the engine first
lowers the request's `MaxTokens` to the remaining allowance and records an
adjustment. Before `BeforeSend`, the engine stops limited if `MaxCost` is set
and the estimate is unavailable (fail closed), or if spent plus at-risk exceeds
it. Otherwise `BeforeSend` receives the amount and the remaining allowance in a
`SendPlan`, and the intent records it as `ModelFact.AtRisk`.

**The between-step check.** Where `run` checks attempts and the deadline
(`engine.go:456-461`), the engine compares recorded totals with each limit.
Tools settle between attempts, so their recorded costs count.

**How a breach is represented.** `StopLimited` gains a typed `Limit` detail:
kind, limit, used, and at risk. Today's reason strings become kinds.
`ModelAuthorization` gains the same detail, so a spending deny becomes
`StopLimited` with kind `host` and an application code rather than `StopRefused`
(`engine.go:558-559`), which stays for refusals. The answer produced so far is
kept, as the reserved final answer already does.

**Unknown and uncertain usage.** Settlement records the attempt's charge on the
settled `ModelFact`: the provider-reported cost, else `Pricing.Estimate` over
the recorded sheet, else nil. `TurnView.Spend` replaces the token-only `Usage`,
which drops nil usage (`state.go:115-121`): token totals, cost per currency, and
counts of attempts with unknown usage, unknown cost, and uncertain outcome. For
limit checks an uncertain or unmeasured attempt is charged its at-risk amount
and effective `MaxTokens`; a cost in another currency counts as unknown.

**Raising a limit and resuming.** `Continue` gains optional `Limits`. Each field
must be at least the effective value and within the definition ceiling; the
continuation step records the new values. `canContinue` (`state.go:603-609`)
lets a limited turn continue once the breached limit has room, including attempt
limits. A host limit needs no raise, since `BeforeSend` decides again. Who may
raise, and by how much, is the application's authorization.

**What the application owns.** Price overrides, ledgers, markups, and credit
balances. The recipe: `BeforeSend` compares `AtRisk` with the balance, denies
with a host limit when short, and returns `Admission` naming the account and
amount. `Recorder.Commit` of the intent reserves that amount in the same
transaction and rejects the write if it cannot. Commit of the settlement turns
the hold into the recorded charge. An uncertain attempt keeps its hold until
`Accept` reconciles it. The attempt ID is the idempotency key.

**Tool and hosted-tool costs.** `llm.Usage` gains request counts for hosted
tools such as web search, and `Pricing` per-request rates, so the settlement
charge covers them. `tool.Outcome` gains an optional `Cost` that adds to the
turn's spend. Tool costs are checked between steps, never estimated ahead.

```go
type Limits struct {
    // existing: ModelAttempts, ProviderPauses, ToolConcurrency,
    // ReserveFinalAnswer, Deadline
    MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
    MaxTotalTokens  int    `json:"max_total_tokens,omitempty"`
    MaxCost         *Money `json:"max_cost,omitempty"` // Amount, Currency
}
type LimitDetail struct {
    Kind                LimitKind // model_attempts, deadline, provider_pauses,
                                  // output_tokens, total_tokens, cost, host
    Code                string    // application code for a host limit
    Limit, Used, AtRisk float64
    Unpriced            bool
}
type Stop struct { Kind StopKind; Reason string; Limit *LimitDetail }
type Spend struct {
    Usage                            llm.Usage
    Cost                             map[string]float64 // by currency
    UsageUnknown, CostUnknown, Uncertain int
}
// Start.Limits, Continue.Limits *Limits; ModelFact.AtRisk, .Charge *llm.Cost;
// TurnView.Limits, .Spend; BeforeSend(ctx, SendPlan{Request, Turn, AtRisk,
// Remaining}); ModelAuthorization.Limit *LimitDetail.
```

## Alternatives considered

**Budgets as pure policy hooks.** `BeforeSend` can already deny. Every
application would still write the estimator, the totals, the unknown-usage rule,
and typed reasons, and no limit could be raised because the definition is pinned
for the turn (`engine.go:90`). That is Dive's position and the work nvoken and
mobius each repeated.

**Start only.** A reusable agent could not carry a safe default, and any caller
could start an unbounded turn; the ceiling moves into every handler.

**Definition only.** Today's shape. A per-request cap needs a definition
revision per request, and raising a limit mid-turn is impossible.

## Staged plan

1. **Accounting.** Record the settlement charge, add `TurnView.Spend` with
   completeness counts, and add `Stop.Limit` for existing limits and host
   denies. Test replay with nil usage, uncertain attempts, mixed currencies, and
   a spending deny.
2. **Token limits and resume.** `MaxOutputTokens`, `MaxTotalTokens`,
   `Start.Limits`, `Continue.Limits`, the `MaxTokens` clamp, and the
   between-step check. Test ceiling rejection, monotonic raise, and resume after
   an attempt limit.
3. **Cost limits.** `MaxCostAtRisk`, `SendPlan`, `ModelFact.AtRisk`, `MaxCost`
   checks before and between calls, fail-closed pricing, hosted request counts,
   tool cost, and a recorder example that reserves credits. It uses catalog
   prices once ported and supplied model info before then.

## Open questions

- Estimate input with a provider count call, a tokenizer, or a byte bound like
  nvoken's? An overestimate stops turns early.
- Should the engine lower `MaxTokens` to fit the remaining cost, not only the
  remaining output tokens, instead of stopping?
- May an operator-origin raise exceed the definition ceiling?
- How do child turns roll up into a parent's spend and limits?
- Is an in-stream check worth it while cancellation ends the turn ([decision
  6](decisions.md))?
