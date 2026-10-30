# One cost cap for all turn spend: side models and tools

Status: Draft\
PRD PR: #342  Implementation PR: TBD  Updated: 2026-10-29\
Spec: [design/decide-spend.md](../design/decide-spend.md)

## Problem

A host sets `Budget.MaxCost` to promise a customer a cost ceiling. Today
the cap covers model calls only. Two kinds of spend escape it.

**Decide requests** from Dashi's `decide/` packages mostly do not show in
`Spend`: `listtool` (`judge_items`, `pick_item`) puts them in
`Spend.ToolCost` only if the host writes a `Price` function, and
`contextrank`, `turnwatch`, and `toolcheck` report only to callbacks. One
`judge_items` call may send 500 requests: at 64 KiB per item, about 33
million input tokens, $1.38 on Jev and $7.86 on Clef, past a $0.50 cap
with no stop and no record. The dashi CLI counts requests itself
(`cmd/dashi/decide_check.go`) because the library gives it nothing.

**Tool costs** that a tool reports in `tool.Outcome.Cost` go to
`Spend.ToolCost` and nowhere else, so a host that promises an all-in
ceiling must build its own checks.

Dashi aims to lead in enforceable cost limits. A cap that leaves out
spend the turn caused is not the ceiling a host promises.

## Context

- `TurnView.ModelCallCosts` sum to `Spend.Cost`; credit recorders rely on
  that.
- The cap rule is "reached": a call may spend up to the cap, and once
  spend equals it no new model call starts. Under a cap, Dashi fails
  closed on cost it cannot price (unknown, partial, or another currency;
  since #306 also unpriced provider tool work), and a raise cannot clear
  that stop.
- A tool's cost is exact and known only after it runs.
- A passing hook check leaves no record, and a run cancelled during a
  hook chain drops the chain's results.
- Decide returns `Model` and token `Usage` but no price. Vendors publish
  input prices only: Jev $0.042 per million tokens, Clef $0.24,
  Clef-flash $0.038, Clef-omni $0.15.

## Users and job

**Primary:** a Go developer who runs paid turns for customers. Job:
enforce one cost cap over everything the turn spends, and show the
customer one figure. (Delegated child turns keep their own caps.)
**Secondary:** the dashi CLI, which shows decide totals in `/settings`
and the turn cost in its footer.

## Concepts

- **Side model:** a model a tool or hook calls itself, such as a decision
  model through Decide or a host's classifier LLM.
- **Side model call:** a record of such calls: model, count, tokens, and
  cost (or "unknown"). The turn record keeps each one.
- **Total cost:** model plus side model plus tool cost, by currency
  (`Spend.TotalCost()`). `MaxCost` limits it.

## Use cases

### UC-1: A capped turn uses judge_items

`MaxCost` $0.50; the model asks `judge_items` to judge 300 tickets.

- [ ] `Spend.SideModelCost["USD"]` holds the cost at the built-in Jev
      price, with no host setup, and `Spend.String()` shows `side models:`.
      `Spend.ToolCost` does not hold it.
- [ ] A call that could cost more than is left sends nothing and returns
      a failed result saying how much is left. The turn goes on.
- [ ] When the cost brings spend to the cap, the turn stops `limited`
      (`LimitCost`) before the next model call; `CostUsed` includes it.

### UC-2: Hooks spend on every turn

The host adds `toolcheck`, `turnwatch`, and `contextrank`.

- [ ] Every Decide request a hook sends during a run is in `Spend`,
      including passed checks and checks in a chain cut short.
- [ ] A dry run (`Advance` polling a held turn) sends no Decide request.
- [ ] A replayed or loaded turn has the same `Spend`.
- [ ] Under a cap, a check that does not fit what is left is not sent:
      `contextrank` and `turnwatch` record `hook_failed`;
      `PromptInjection` leaves the result unchecked; `CommandRisk` holds
      the call for approval.
- [ ] Once spend reaches the cap, the next tool call does not start, shell
      calls included: no check is sent, no call runs unchecked, none is
      held for approval. The turn stops `limited`; a raise resumes it.

### UC-3: A model with no price

The host pins a model with no built-in price, or caps in a currency the
table lacks.

- [ ] Without a cap, the call counts in `Spend.CostUnknown`.
- [ ] With a cap, nothing is sent: `judge_items` fails naming the model;
      hooks run as if Decide were not set up and warn once per turn.
- [ ] A host can supply prices with `LookupPricing`.

### UC-4: A host tool reports a cost

`send_fax` returns `Outcome.Cost` $0.20 per page; `MaxCost` $0.50.

- [ ] The cost counts toward the cap, `CostUsed`, and
      `SendPlan.Remaining`.
- [ ] Once committed spend reaches the cap, no further call in the batch
      starts (concurrency 1 too), and the turn stops `limited`. A raise
      runs the calls that did not start.
- [ ] A tool can read `dashi.RemainingCost(ctx)` and refuse first.
- [ ] Under a cap, a tool cost in another currency, or marked partial,
      stops the turn as an unknown model cost does.

### UC-5: The dashi CLI shows library numbers

- [ ] `/settings` decide totals come from `Spend`, not CLI counters.
- [ ] The footer shows one figure, `Spend.TotalCost()`.
- [ ] The check run by `/task ID continue` counts in that turn's `Spend`.

## Requirements

- R-1: Any tool or hook can add a side model call while it runs; Dashi
  records it on the next committed step, so it replays.
- R-2: `MaxCost` limits total cost, and every check uses the "reached"
  rule.
- R-3: The decide packages price every request from a dated, overridable
  table and check the remaining cost before they send.
- R-4: Tool cost follows the model-call rules for partial and
  other-currency amounts. Decide spend is counted once.
- R-5: Child turns count their own spend; delegation reports include it.

## Not in scope

- Pricing a tool, or holding its cost before it runs.
- Reserving spend across concurrent work.
- A parent cap over delegated child turns.
- Spend lost when a run ends with work running, a worker crashes, or a
  tool returns an error after it spent.

## Decisions

Reasons and rejected options are in the spec's Alternatives.

- **Tool cost counts toward `MaxCost`** (Curtis, 2026-10-29).
- **Side model tokens stay out of the token caps** (Curtis, 2026-10-29).
- **The CLI footer shows one combined cost** (Curtis, 2026-10-29).
- **A reached cap stops the work** (orchestrator, 2026-10-29): at the cap
  the next tool call does not start, rather than running unchecked or
  waiting for approval.
- **A generic side model in the root**, with its own `Spend` line.
- **`tool.Cost.Partial`** lets a tool say "not fully priced" and fail
  closed under a cap.
- **Decide prices live in `decide/pricing` for now**; output counts as
  zero.
- **No price under a cap is a setup gap**: hooks fail open.

## Success

In a capped turn, once committed total cost reaches the cap, no tool call
starts and no model call is sent; work already running finishes, so one
call can pass the cap. `Spend` equals list price for every recorded
request plus what tools reported. `ModelCallCosts` and token caps behave
as before.

## Risks and open questions

- Turns that report tool costs stop sooner; the CHANGELOG calls it out.
- Vendor prices change: dated rows, a provenance test, a host override.
- Open: keep `tool.Cost.Partial`? No planned caller sets it yet, but it
  is the only way a tool can fail closed on unpriced work. Recommended:
  keep.
