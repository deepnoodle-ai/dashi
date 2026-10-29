# One cost cap for all turn spend: side models and tools

Status: Draft\
PRD PR: #342  Implementation PR: TBD  Updated: 2026-10-29\
Spec: [design/decide-spend.md](../design/decide-spend.md)

## Problem

A host sets `Budget.MaxCost` to promise a customer a cost ceiling. Today
the cap covers model calls only. Two kinds of spend escape it.

**Decide requests.** The requests that Dashi's `decide/` packages send to
a decision model do not count, and most do not show in `Spend` at all:

| Package | Where it runs | Spend today |
| --- | --- | --- |
| `listtool` (`judge_items`, `pick_item`) | agent tools | `Spend.ToolCost`, only if the host writes a `Price` function |
| `contextrank` | before the first model call | `OnSelection` callback only |
| `turnwatch` | before each model call | `OnCheck` callback only |
| `toolcheck` | before shell calls, after tool results | `OnCheck` callback only |

One `judge_items` call may send 500 requests, and a turn 2,000. At 64 KiB
per item that is about 33 million input tokens: $1.38 on Jev and $7.86 on
Clef, past a $0.50 cap with no stop and no record. The dashi CLI adds up
requests and tokens itself (`cmd/dashi/decide_check.go`), because the
library gives it nothing.

**Tool costs.** A tool that calls a paid API reports its cost in
`tool.Outcome.Cost`. Dashi adds it to `Spend.ToolCost` and nothing else, so
a host that promises an all-in ceiling must build its own checks.

Dashi aims to lead in cost tracking and enforceable limits. A cap that
leaves out spend the turn caused is not the ceiling a host promises.

## Context

- `Spend.Cost` holds the turn's model calls. `TurnView.ModelCallCosts` sum
  to it, and credit-charging recorders rely on that.
- Under a cap, Dashi fails closed on spend it cannot price: an unknown or
  partial model cost, or one in another currency, stops the turn `limited`,
  and a raise cannot clear it. Since #306 this includes unpriced provider
  tool work ([provider tool pricing](../../design/hosted-tool-pricing.md)).
- A tool's cost is exact and known only after it runs. `tool.Cost` has no
  way to say "part of this had no price".
- A hook decision is recorded only when it says something. A passing check
  leaves no trace, and a run cancelled during a hook chain drops the
  chain's results.
- Decide returns `Model` and token `Usage`, but no price, and has no price
  table yet. Vendors publish input prices: Jev $0.042 per million tokens,
  Clef $0.24, Clef-flash $0.038, Clef-omni $0.15. Neither lists output.
- Dive and comparable frameworks have no way for a tool to report model
  spend. Dashi's provider tool meters are the closest precedent.

## Users and job

**Primary:** a Go developer who runs paid turns for customers, with or
without the `decide/` packages. Job: promise and enforce one cost cap over
everything the turn spends, and show the customer one figure. (Delegated
child turns keep their own caps.)
**Secondary:** the dashi CLI, which shows decide totals in `/settings` and
the turn cost in its footer.

## Concepts

- **Side model.** A model that a tool or hook calls itself, apart from the
  turn's own model calls: a decision model called through Decide, or a
  small classifier LLM in a host hook.
- **Side model call.** A record of calls a tool or hook made to a side
  model: model, number of calls, input and output tokens, and cost (or
  "unknown"). The turn record keeps each one.
- **Tool cost.** What a tool reports it spent, on its outcome.
- **Total cost.** Model cost plus side model cost plus tool cost, by
  currency (`Spend.TotalCost()`). `MaxCost` limits it.

## Use cases

### UC-1: A capped turn uses judge_items

The host starts a turn with `MaxCost` $0.50 and the `listtool` tools. The
model asks `judge_items` to judge 300 tickets.

Acceptance:
- [ ] `Spend.SideModelCost["USD"]` holds the call's cost at the built-in
      Jev price, with no price set by the host. `Spend.ToolCost` does not.
- [ ] `Spend.String()` prints it as `side models: … USD`.
- [ ] When the call could cost more than the cap has left, the tool sends
      nothing and returns a failed result that says how much is left and
      to judge fewer items. The turn goes on.
- [ ] When the cost brings spend to the cap, the turn stops `limited`
      (`LimitCost`) before the next model call, and `CostUsed` includes it.

### UC-2: Hooks spend on every turn

The host adds `toolcheck`, `turnwatch`, and `contextrank` hooks.

Acceptance:
- [ ] Every Decide request each hook sends during a run appears in
      `Spend`, including checks that passed or failed, and checks in a
      hook chain that a steer or stop cut short.
- [ ] A hook asked in a dry run (when `Advance` polls a held turn) sends
      no Decide request.
- [ ] A turn replayed or loaded from its record has the same `Spend`.
- [ ] Under a cap, a check that would not fit what is left is not sent.
      `contextrank` and `turnwatch` record `hook_failed` and the turn runs
      without them; `PromptInjection` leaves the result unchecked;
      `CommandRisk` holds the call for approval. With nothing left,
      `CommandRisk` runs unchecked instead of holding.
- [ ] The model call after a `BeforeModel` hook is checked against a cap
      that includes that hook's spend.

### UC-3: A model with no price

The host pins a model with no built-in price, such as a new Clef, or sets
a cap in a currency the price table lacks (it holds USD).

Acceptance:
- [ ] Without a cap, the call counts in `Spend.CostUnknown`; the turn runs.
- [ ] With a cap, the decide packages send nothing. `judge_items` returns
      a failed result that names the model. Hooks run as if Decide were
      not set up and warn once per turn. An unknown side model cost never
      stops the turn, because none is sent.
- [ ] A host can supply prices with a `LookupPricing` function.

### UC-4: A host tool reports a cost

The host's `send_fax` tool charges $0.20 a page and returns
`Outcome.Cost`. The turn has `MaxCost` $0.50.

Acceptance:
- [ ] The cost counts toward `MaxCost`; `CostUsed` and
      `SendPlan.Remaining` include it.
- [ ] Once committed spend is over the cap, no further tool call in the
      batch starts, with the default concurrency of 1 too, and the turn
      stops `limited` (`LimitCost`). A raise through `Continue` runs the
      calls that did not start.
- [ ] Spend at or over the cap stops the turn before the next model call.
- [ ] A tool can read `dashi.RemainingCost(ctx)` and refuse work that
      would not fit.
- [ ] Under a cap, a tool cost in another currency, or one marked
      partial, stops the turn `limited` as an unknown model cost does.

### UC-5: The dashi CLI shows library numbers

Acceptance:
- [ ] `/settings` decide totals (requests, tokens, cost) come from the
      turns' `Spend`, not CLI counters.
- [ ] The footer shows one figure, `Spend.TotalCost()`.
- [ ] The decide check run when approving a stopped task's call
      (`/task ID continue`) is counted in that turn's `Spend`.

## Requirements

- R-1: Any tool or hook can add a side model call while it runs. Dashi
  records it on the next step it commits, so it is durable and replays.
- R-2: `MaxCost` limits total cost: model, side model, and tool cost.
  Every cost check and figure uses the same sum.
- R-3: The four `decide/` packages add the cost of every request they
  send, priced from a built-in, dated table that a host can override.
- R-4: Under a cap, the decide packages check the remaining cost before
  they send. Tools return an error to the model, hooks fail open, and
  `CommandRisk` holds when the check does not fit. A missing price is a
  setup gap, so hooks fail open.
- R-5: Tool cost follows the model-call rules for unknown, partial, and
  other-currency amounts. Decide spend is counted once, as side model cost.
- R-6: Child turns count their own spend; delegation reports include it.

## Not in scope

- Pricing local tools, or holding their cost before they run. The host
  reports what a tool spent; a tool can check `RemainingCost` first.
- Reserving spend across concurrent work. Tools and side model calls that
  run together each see the same remaining amount, so together they can
  pass the cap by what they spend. The next check stops the turn.
- Spend from a check run outside any turn and never passed to Dashi.
- Spend still running when a run commits its last step, such as a stop
  that does not wait for running tools, or a tool that returns an error
  after it spent. It is lost, as a crash loses it.
- A parent cap over its delegated child turns. Each child has its own.

## Decisions

Reasons and rejected options are in the spec's Alternatives.

- **Tool cost counts toward `MaxCost`** (Curtis, 2026-10-29): the cap
  covers everything the turn spends.
- **Side model tokens stay out of the token caps** (Curtis, 2026-10-29):
  `MaxTotalTokens` sizes the agent's context, not a judge's input.
- **The CLI footer shows one combined cost** (Curtis, 2026-10-29).
- **A generic side model in the root**, not a Decide feature, with its
  own `Spend` line so `ModelCallCosts` still sum to `Spend.Cost`.
- **A tool can mark its cost partial** (`tool.Cost.Partial`), to say
  "spent, not all of it priced"; it fails closed under a cap.
- **Decide prices live in `decide/pricing` for now**, dated and linked;
  output counts as zero, since no vendor lists it.
- **Added through the context**, so passed, failed, and dropped checks
  count.
- **No price, or nothing left, under a cap means "not set up"**: hooks
  fail open instead of holding every shell call.

## Success

In a capped turn, once committed total cost is over the cap, no tool call
starts and no model call is sent; work already running finishes, so one
paid call can pass the cap. `Spend` equals list price for every request
the run recorded, plus what tools reported; the lost cases are under Not
in scope. Model-call accounting, `ModelCallCosts`, and token caps behave
as before.

## Risks and open questions

- The cap's meaning changes for every host that reports tool costs: turns
  stop sooner. Dashi has no external users yet; the CHANGELOG calls it
  out.
- Open: keep `tool.Cost.Partial`? No planned caller sets it today. It is
  the only way a tool can say "spent, not fully priced" and fail closed
  under a cap, as unpriced provider tool work does. Recommended: keep.
- Vendor prices change. The table carries a check date and a test that
  links each row to its source; a host can override.
- A worker crash between a paid request and the next committed step loses
  that cost, as an interrupted model call loses unreported usage.
