# Budgets

**Status:** Decided, [decisions 21–26](decisions.md) · **Date:** 2026-09-28
· **Brief:** [decision 7](decisions.md)

Kernel becomes the place where LLM spend is measured and limited. A **turn** is
one run of an agent answering one input; it may make many model calls and tool
calls, and it can stop and resume later. The application that starts a turn
gives it a **budget**: optional caps on output tokens, total tokens, and cost.
Before each model call Kernel works out the **worst case**, the most that call
could cost if the model wrote every output token it is allowed. If spent so far
plus the worst case would pass the cost cap, the turn stops as `limited` (a
resumable stop meaning "a limit was reached") and the answer so far is kept.
After each call Kernel records what it actually cost. Applications keep price
overrides, tenant ledgers, credit reservations, and the policy that decides who
may spend more.

## Users and jobs

**A per-request cap from a gateway customer.** A customer sends a request with
a $0.50 cap and `max_output_tokens: 4000`. The gateway passes both to `Start`,
the command that begins a turn. The agent makes several model and tool calls.
At $0.45 spent, the next call would most likely cost $0.03 but could cost $0.12,
so the turn stops `limited` before sending it. The stop gives the cap, the
amount used, and the worst case. The customer is never charged over 50 cents.

**Caps chosen per caller, not per agent.** A support agent's **definition** (its
saved configuration: model, instructions, tools) carries no budget. The
application's handler sets $2 on `Start` for a paid user and $0.10 for a free
user. Plan and tenant ceilings live in the application, which applies them
before calling `Start`. A turn started with no budget has no token or cost cap.

**Hold and resume after a top-up.** A tenant's credits run out mid-turn. The
application's `BeforeSend` hook, which runs just before each model call, sees
the worst case exceeds the balance and denies with the application's code,
`insufficient_credits`. The turn stops `limited`, not refused. After the user
buys credits, the application calls `Continue`, the command that resumes a
stopped turn, and `BeforeSend` checks the balance again. If instead the turn's
own cap ran out, `Continue` carries a raised budget, and the saved turn records
the raise.

**A call whose bill never arrived.** A worker is redeployed while a reply is
streaming. Many providers report usage only at the end, so Kernel saw some text
but no bill. The call is sent again on resume ([decision 9](decisions.md)); the
lost try counts as its estimated input plus the output text that arrived,
marked estimated in the running total.

## How nvoken does it today

Server behavior lives in nvoken-cloud (`nc/`); nvoken has SDK and CLI mirrors.

- **Limits.** A request sets `max_output_tokens`, `max_estimated_cost_usd`,
  and more, narrowing deployment-wide ceilings
  (`nc/internal/services/controls.go:23-181`, `turns.go:1217-1302`).
- **Worst case, reserved.** Input is one token per serialized byte plus 1,024;
  at-risk cost is the highest input-side rate times that, plus the output rate
  times maximum output (`nc/internal/domain/budget_cost.go:8-87`).
  `PrepareModelCall` locks the tenant's `credit_accounts` row and holds the
  amount in one transaction (`nc/internal/services/credit_authority.go:42-222`).
- **Call facts.** An uncertain call, or a settled call without a cost, is
  charged its full hold (`nc/.../queries/credits.sql:180-197, 227-228`).
- **Checks between steps.** `checkpointBudgetExceeded` compares iterations,
  output tokens, and summed cost after each step
  (`nc/internal/adapters/divegen/generator.go:1600-1628`). The turn's cost limit
  is never compared with the at-risk amount, so one call can overshoot.
- **Typed stops and hold.** `TurnStopReason` names `max_output_tokens`,
  `max_estimated_cost`, and `insufficient_credits`
  (`nc/internal/domain/turn.go:60-98`); a held turn resumes after a raise within
  the installation maximum or when funding arrives
  (`nc/internal/services/budget_resume.go:186-251, 342-448`).
- **Fail closed.** An unpriced model fails `cost_estimate_unavailable` when a
  cost limit or credits apply (`generation.go:513-524`,
  `credit_authority.go:143-152`).

Kernel keeps this shape and fixes two things: it compares the worst case with
the turn's own cost cap, and it charges a best guess, not a full hold, for a
call whose usage never arrived (nvoken classes every provider error as
uncertain, `generator.go:2343-2348`).

**mobius-cloud** checked metered cost before each call, accepting a one-call
overshoot and counting calls without usage as zero (removed in `41cf9a886`).
**Dive** prices usage from a global registry (`llm/cost.go:30-44`) and has no
spending limits.

## Proposal for Kernel

**Where budgets are set.** The caller sets them; the definition does not.
`Start` takes an optional `Budget` with `MaxOutputTokens` (turn total),
`MaxTotalTokens` (all input and output across calls), and `MaxCost`. The
turn-started step records it. The definition's existing limits
(`ModelAttempts`, `Deadline`, `Interruptions`) stay where they are; whether
they should also move to the caller is not decided.

**The check before each call.** `llm.MaxCostAtRisk(Request, Pricing)` values a
prepared request: estimated input at the highest input-side rate, plus the
effective `MaxTokens` at the output rate, in the price tier the input selects.
Input size starts as a safe, size-based estimate (nvoken's byte bound); a
provider token count may replace it later. Prices come from the ported model
catalog ([decision 4](decisions.md)), and an application supplies its own
through the client's model info. With `MaxOutputTokens` set, Kernel lowers the
request's `MaxTokens` to the remaining token allowance and records a warning.
It never lowers `MaxTokens` to fit a cost shortfall. Then, if `MaxCost` is set:

- **No price for the model:** the turn stops `limited` before its first call.
  Reason: "no price for model acme-7b; the $0.50 cost cap cannot be enforced;
  supply this model's prices to run capped turns."
- **Worst case does not fit:** the turn stops `limited` before the call. The
  reason says what to change: "next call could cost up to $1.02 (64,000 output
  tokens); $0.50 of the $0.50 cap remains; an output-token allowance of about
  29,000 would fit." `LimitDetail.FitTokens` carries that number.
- **It fits:** `BeforeSend` receives the worst case and the remaining budget in
  a `SendPlan`, and the recorded call keeps it as `ModelFact.AtRisk`.

These stops depart from warn-and-adjust (decision 4) on purpose: a cap is a
promise, and the reason gives the application the fix.

**The check between steps.** Where the engine checks attempts and the deadline
(`engine.go:456-461`), it compares recorded totals with each cap. Tools report
their own costs, which count here; they are never estimated ahead.

**How a stop is represented.** `StopLimited` gains a typed `Limit` detail:
kind, cap, used, worst case. `ModelAuthorization` gains the same detail, so a
spending deny from `BeforeSend` becomes `StopLimited` with kind `host` and the
application's code, not `StopRefused` (`engine.go:558-559`), which stays for
refusals.

**Actual and estimated spend.** After each call Kernel records its charge: the
provider-reported cost, else the price sheet applied to reported usage. A call
whose usage never arrived (a crash, or a cut-off before the provider reported
it) is charged a best guess: estimated input plus the output text that did
arrive, marked estimated. The consequence, accepted: for such calls the cap is
not a strict guarantee, since the guess can undercount. It holds for every call
whose usage Kernel saw. `TurnView.Spend` replaces the token-only `Usage`
(`state.go:115-121`): token totals, cost per currency, and counts of estimated
calls and calls with unknown cost. A cost in another currency counts as unknown.
`llm.Usage` gains hosted-tool request counts (such as web searches) and
`Pricing` per-request rates; `tool.Outcome` gains an optional `Cost`.

**Raising a budget and resuming.** `Continue` takes an optional `Budget` that
raises caps. Nothing bounds a raise, since there is no definition ceiling; who
may raise, and by how much, is the application's authorization. The saved turn
records every raise, with the new amounts and when it happened, so raises show
up in an audit. `canContinue` (`state.go:603-609`) lets a limited turn continue
once the reached cap has room. A host limit needs no raise.

**What the application owns.** Price overrides, ledgers, markups, balances, and
plan ceilings. The credit recipe: `BeforeSend` compares `AtRisk` with the
balance, denies with a host limit when short, and returns `Admission` naming the
account and amount. The recorder reserves that amount in the same transaction
as the call's intent and rejects the write if it cannot. Recording the result
turns the hold into the charge. The application may keep a hold on a call whose
usage never arrived until it learns the real bill. The attempt ID is the
idempotency key.

```go
type Budget struct {
    MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
    MaxTotalTokens  int    `json:"max_total_tokens,omitempty"`
    MaxCost         *Money `json:"max_cost,omitempty"` // Amount, Currency
}
type BudgetRaise struct { Budget Budget; At time.Time }
type LimitDetail struct {
    Kind                LimitKind // model_attempts, deadline, interruptions,
                                  // output_tokens, total_tokens, cost, host
    Code                string    // application code for a host limit
    Limit, Used, AtRisk float64
    FitTokens           int       // output allowance that would fit a cost cap
    UnpricedModel       string
}
type Stop struct { Kind StopKind; Reason string; Limit *LimitDetail }
type Spend struct {
    Usage                  llm.Usage
    Cost                   map[string]float64 // by currency
    Estimated, CostUnknown int
}
// Start.Budget, Continue.Budget *Budget; ModelFact.AtRisk, .Charge *llm.Cost;
// TurnView.Budget, .Raises []BudgetRaise, .Spend; BeforeSend(ctx,
// SendPlan{Request, Turn, AtRisk, Remaining}); ModelAuthorization.Limit.
```

## Alternatives considered

**Budgets as pure policy hooks.** `BeforeSend` can already deny. Every
application would still write the estimator, the totals, the unseen-usage rule,
and typed reasons. That is Dive's position and the work our production hosts
each repeated.

**A definition ceiling that `Start` may only lower.** The first proposal. It
gives a reusable agent a safe default, but budgets are a property of who is
calling and what they paid for, which the application already knows. A ceiling
would also block finishing a nearly done turn, since a turn keeps its
definition for life. Rejected ([decision 25](decisions.md)).

## Staged plan

1. **Accounting.** Record each call's charge, add `TurnView.Spend` with
   estimated and unknown counts, and add `Stop.Limit` for existing limits and
   host denies. Test replay with missing usage, a best-guess charge, mixed
   currencies, and a spending deny.
2. **Token caps and resume.** `Budget` on `Start` and `Continue`, recorded
   raises, `MaxOutputTokens`, `MaxTotalTokens`, the `MaxTokens` lowering, and
   the between-step check. Test a raise after a stop and resume after an
   attempt limit.
3. **Cost caps.** `MaxCostAtRisk`, `SendPlan`, `ModelFact.AtRisk`, the stop
   with `FitTokens`, the unpriced stop, hosted request counts, tool cost, and a
   recorder example that reserves credits.

## Open questions

- Should `ModelAttempts`, `Deadline`, and `Interruptions` also move from the
  definition to the caller?
- May `Continue` lower a budget, or only raise it?
- Child turns: how their spend rolls up to a parent waits until Kernel has
  child turns.
