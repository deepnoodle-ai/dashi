# Saving agent costs without rewriting Dashi's billing rules

See the [main recommendations](README.md). Swarm should decide who can spend
and store its account balances. Dashi should explain what each model call did
and how that changes its cost.

## R09 — give the database adapter the changes it needs to save

**Problem.** [Swarm's Recorder][recording] loads the Turn, adds a proposed Step,
rebuilds its view, scans delivered inputs, finds the model-call start, calculates
cost, and constructs database updates. The file is 184 lines including support
code. An adopter must understand Dashi's event details to write this adapter.

**Change.** Dashi should validate a proposed Step and return the resulting view,
money changes, delivered-input IDs, and whether the Turn ended. Build this from
Dashi's existing replay and `ModelCallCostOf` logic.

```go
// Proposed API, illustrating the desired division of work.
changes, err := dashi.ProjectCommit(turn, req.Step)
if err != nil { return rejected(err) }
return repo.Commit(ctx, req, changes) // save record, money, and delivered inputs together
```

Swarm's transaction still checks the expected sequence and current worker,
then saves everything together. A duplicate Step must not charge twice. The
calculated changes become real only when the transaction commits.

**Benefit:** remove record scanning and repeated interpretation from storage
adapters. The public result types would need to evolve with Dashi's records.
A later optimization could update the view incrementally instead of replaying
all records; measure that need before implementing it.

## R10 — handle shared-budget competition as an expected outcome

**Problem.** Two agents can both pass a balance check before one reserves the
last money. [Swarm's database transaction][transaction] catches this, but the
runtime must recognize a particular `CommitError` plus repository error and
work out how to resume.

**Change.** Let the Recorder report a clear, definite refusal to fund a model
call. Dashi can then record “waiting for money” or “application limit reached”
without sending the request. If saving that outcome fails, use the ordinary
persistence-error contract.

The balance check and reservation must stay in the same transaction as the
model-call start. Resuming after a definite rejection needs a fresh command;
retries should be bounded when agents keep competing. A missing acknowledgment
is a different problem: reload before deciding whether anything was saved.

**Benefit:** an adopter gets one supported shared-budget recipe and fewer
special error checks. Swarm keeps role budgets, routine caps, and top-up policy.

## R11 — correct an estimated charge when the final charge arrives

**Problem.** A model request can stop with incomplete billing information.
[Swarm][reconcile] can later replace the estimate with a final charge or a
verified zero. That change lives outside Dashi, so [Swarm's watcher][watch]
uses its account data to override Dashi's displayed cost.

Dashi already accepts late results for pending/unknown model calls. The gap is
correcting the cost of a call that has already finished.

**Change.** Add a saved, accounting-only correction for a particular model call.
It should update usage and spend while keeping the reply, tools, and finished
state intact. Give the application the old and new amounts so it can charge or
refund the difference in the same transaction.

Repeated billing evidence must not apply twice; conflicting evidence should
fail clearly. Calculate the difference between rounded old/new totals, rather
than rounding the difference. If the final bill exceeds the amount held, save
the real bill and prevent further funded work.

**Benefit:** one consistent cost in the Turn and the account, with less override
code. This does not make an uncertain model request safe to repeat.

## R12 — use the same cost rules for a single text request

**Problem.** Swarm's Writing desk calls a model without an agent tool loop.
[Its Generate path][generate] repeats rules for usage prices, incomplete costs,
and whether a failed request actually ran. It posts the whole reserved amount
as an estimate in some cases where the Turn path reports known partial cost.

**Change.** Export the shared cost calculation for a prepared request, result,
and send error. Return the known cost, whether it is final, and how much remains
uncertain. Let Swarm choose how to show and post estimates explicitly.

This could remove `generateCost`. A convenience request executor may follow,
with explicit reservation and settlement storage. Moving the code into a helper
alone does not solve crashes. Settlement failures must remain visible separately
from model failures, and pending settlement must finish during orderly shutdown.
Use a recorded one-call Turn when restart recovery is required.

## R13 — share small money and estimation helpers

Swarm's `microUSD` converts Dashi's amounts into its database unit with rounding
and overflow checks. A checked `money.Amount` conversion with an explicit unit
and rounding rule would remove this small duplicate helper.

[ReservationMicroUSD][estimate] estimates from artificial repeated-`x` input.
Expose estimate assumptions, price source, effective output limit, and reasons
an estimate is unknown. The real reservation still needs `WorstCaseCost` on the
actual prepared request, including history and tools. The preflight estimate
should make its approximation clear to the application.

[recording]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/recording.go#L14
[hook]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L638
[transaction]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/repo/entrepo/model.go#L347
[reconcile]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/repo/entrepo/model_budget.go#L315
[watch]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1354
[generate]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/generate.go#L114
[estimate]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L158
