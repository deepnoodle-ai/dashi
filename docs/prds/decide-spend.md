# Count decision-model spend in a turn's Spend and cost cap

Status: Draft\ PRD PR: #TBD Implementation PR: #TBD Updated: 2026-10-29\
Spec: [design/decide-spend.md](../design/decide-spend.md)

## Problem

A host sets `Budget.MaxCost` to promise a customer a cost ceiling. Today
the requests that Dashi's `decide/` packages send to a decision model do
not count against it, and most of them do not show in `Spend` at all:

| Package | Where it runs | Spend today |
| --- | --- | --- |
| `listtool` (`judge_items`, `pick_item`) | agent tools | `Spend.ToolCost`, outside `MaxCost`, only if the host writes a `Price` function |
| `contextrank` | before the first model call | `OnSelection` callback only |
| `turnwatch` | before each model call | `OnCheck` callback only |
| `toolcheck` | before shell calls, after tool results | `OnCheck` callback only |

One `judge_items` call may send 500 requests, and a turn 2,000. At 64 KiB
per item that is about 33 million input tokens: $1.38 on Jev and $7.86 on
Clef, past a $0.50 cap with no stop and no record. A hook that checks
every tool result spends on every turn, and nothing in the turn's record
says so. The dashi CLI adds up requests and tokens itself
(`cmd/dashi/decide_check.go`), because the library gives it nothing. Dashi
aims to lead in cost tracking and enforceable limits, so spend that a
ready-made Dashi package causes must be counted by Dashi.

## Context

- `Spend.Cost` holds the turn's model calls; `MaxCost` limits it.
  `TurnView.ModelCallCosts` sum to it, and credit-charging recorders rely
  on that. `Spend.ToolCost` holds local tool charges, outside `MaxCost`.
- Under a cap, Dashi fails closed on spend it cannot price: an unknown or
  partial cost stops the turn `limited` ([provider tool
  pricing](../../design/hosted-tool-pricing.md)).
- A hook decision is recorded only when it says something. A check that
  passes ("no opinion") leaves no trace. When a run is cancelled during a
  hook chain, the chain's results are dropped and the chain runs again.
- Decide returns `Model` and token `Usage`, but no price. Decide has no
  price table yet; its roadmap plans one. Vendors publish input prices:
  Jev $0.042 per million tokens, Clef $0.24, Clef-flash $0.038, Clef-omni
  $0.15. Neither lists an output price.
- Dive and comparable frameworks have no way for a tool to report model
  spend. Dashi's provider tool meters are the closest precedent.

## Users and job

**Primary:** a Go developer who runs paid turns for customers and uses the
`decide/` packages. Job: promise and enforce one cost cap that holds
whatever the agent does, and show the customer one spend figure.
**Secondary:** the dashi CLI, which shows decide totals in `/settings` and
the turn cost in its footer.

## Concepts

- **Side model.** A model that a tool or hook calls itself, apart from the
  turn's own model calls. A decision model called through Decide is the
  first case; a small classifier LLM in a host hook is another.
- **Side model call.** A record of calls a tool or hook made to a side
  model: the model, the number of calls, input and output tokens, and the
  cost (or "unknown"). The turn record keeps each one.
- **Side model cost.** The sum of side model calls' costs in `Spend`, a
  line next to model cost and tool cost. It counts toward `MaxCost`. Side
  model tokens do not count toward the token caps.

## Use cases

### UC-1: A capped turn uses judge_items

The host starts a turn with `MaxCost` $0.50 and the `listtool` tools. The
model asks `judge_items` to judge 300 tickets.

Acceptance:
- [ ] `RunResult.Spend.SideModelCost["USD"]` holds the call's cost at the
      built-in Jev price, with no price set by the host.
- [ ] `Spend.String()` prints it as `side models: … USD`.
- [ ] `Spend.ToolCost` no longer holds Decide spend.
- [ ] When the call could cost more than the cap has left, the tool
      sends nothing and returns a failed result that says how much is
      left and to judge fewer items. The turn goes on.
- [ ] When the side model cost brings spend to the cap, the turn stops
      `limited` with `LimitDetail.Kind == LimitCost`, before the next
      model call, and `CostUsed` includes the side model cost.

### UC-2: Hooks spend on every turn

The host adds `toolcheck`, `turnwatch`, and `contextrank` hooks.

Acceptance:
- [ ] Every Decide request each hook sends during a run appears in
      `Spend`, including checks that passed or failed, and checks in a
      hook chain that a steer or stop cut short.
- [ ] A hook asked in a dry run (when `Advance` polls a held turn) sends
      no Decide request.
- [ ] A turn replayed or loaded from its record has the same `Spend`.
- [ ] Under a cap, a hook whose check would not fit what is left skips
      the check and acts as it does without Decide. `CommandRisk` holds
      the call for approval instead, as it does for any check failure the
      input can cause.
- [ ] The model call after a `BeforeModel` hook is checked against a cap
      that includes that hook's spend.

### UC-3: A model with no price

The host pins a model that has no built-in price, such as a new Clef, or
sets a cap in a currency the price table lacks (it holds USD).

Acceptance:
- [ ] Without a cap, the call is recorded with an unknown cost and counts
      in `Spend.CostUnknown`; the turn runs normally.
- [ ] With a cap, the decide packages send nothing. `judge_items` returns
      a failed result that names the model. Hooks, `CommandRisk`
      included, run as if Decide were not set up, and warn once per turn.
      The turn is never stopped by an unknown side model cost.
- [ ] A host can supply prices with a `LookupPricing` function on each
      config.

### UC-4: The dashi CLI shows library numbers

Acceptance:
- [ ] `/settings` decide totals (requests, input and output tokens) and
      a cost come from the turns' `Spend`, not CLI counters.
- [ ] The footer's turn cost includes side model cost.
- [ ] The decide check run when approving a stopped task's call
      (`/task ID continue`) is counted in that turn's `Spend`.

## Requirements

- R-1: Any tool or hook can add a side model call while it runs. Dashi
  records it on the next step it commits, so it is durable and replays.
- R-2: `MaxCost` covers model cost plus side model cost.
- R-3: The four `decide/` packages add the cost of every request they
  send, priced from a built-in, dated price table that a host can
  override.
- R-4: Under a cap, the decide packages check the turn's remaining cost
  before they send, and fail as their designs already say: tools return an
  error to the model, hooks fail open, `CommandRisk` holds when the check
  does not fit. A missing price is a setup gap, so hooks fail open.
- R-5: Child turns count their own side model calls; delegation's spend
  reports include them.

## Not in scope

- Counting `Spend.ToolCost` toward `MaxCost`. It changes what a cap means
  for every host tool; this PRD covers only model spend.
- Reserving side model spend across concurrent work. Side model calls that
  start together each check the same remaining amount, so together they
  can pass the cap by their worst case. The next check stops the turn.
- Spend from a check run outside any turn and never passed to Dashi.
- Requests still running when a run commits its last step, such as a stop
  that does not wait for running tools. They are lost, as a crash loses
  them.

## Decisions

- **A generic side model, not a Decide feature in the root.** The root
  must not import Decide, and a host's own classifier hook has the same
  need. Rejected: folding Decide spend into `Spend.Cost`, which would
  break the rule that `ModelCallCosts` sum to it.
- **Side model tokens stay out of the token caps.** A judge over 300 items
  uses many cheap tokens; counting them against `MaxTotalTokens`, which
  sizes the agent's context, would stop turns for the wrong reason.
- **Prices live in Dashi for now** (`decide/pricing`), dated and linked to
  vendor pages, not in the model catalog: the catalog holds models Dashi
  can call, by exact ID, and TypeSafe prices Jev for every version. When
  Decide ships its own table, Dashi uses it.
- **Unpublished output prices count as zero.** Both vendors list only
  input prices, and Decide answers are a few tokens.
- **Added through the context.** Then a failed hook, a passed check, and a
  dropped hook chain still count. Rejected: a field on each decision and
  on `tool.Outcome`, which loses those cases.
- **No price under a cap means "not set up".** No input causes it, so
  hooks fail open as on a paused check. Rejected: holding every shell call
  for approval, which would deny them all in an unattended run.

## Success

A capped turn that uses any `decide/` package never ends with spend above
its cap by more than the worst case of side model calls that ran at once,
and its `Spend` equals the sum of what the vendors bill at list price.
Model-call accounting, `ModelCallCosts`, and token caps behave as before.

## Risks and open questions

- Vendor prices change. The table carries a check date and a test that
  links each row to its source; a host can override.
- A worker crash between a Decide request and the next committed step
  loses that cost, as an interrupted model call loses unreported usage.
