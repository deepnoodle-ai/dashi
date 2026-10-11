# Saving agent costs without rewriting Dashi's billing rules

See the [main recommendations](README.md). Swarm should decide who can spend
and store its account balances. Dashi should explain what each model call did
and how that changes its cost.

## R09 — give the database adapter the changes it needs to save

[Before and after](examples/r09-database-cost-updates.md).

**Problem.** [Swarm's Recorder][recording] loads the Turn, adds a proposed Step,
rebuilds its view, scans delivered inputs, finds the model-call start, calculates
cost, and constructs database updates. The file is 184 lines including support
code. An adopter must understand Dashi's event details to write this adapter.

**Change.** Dashi should validate a proposed Step and return the resulting view,
per-call cost evidence with old/new classified totals, transcript-input receipts,
and whether the Turn ended. Swarm chooses ledger postings from that evidence,
its existing balances, units, rounding and estimate policy. Build this from
Dashi's existing replay and `ModelCallCostOf` logic.

```go
// Proposed division of work; see the linked example for the full transaction.
// Owner, duplicate/conflict and sequence checks precede projection.
// Exact duplicates return without application effects.
return commitUsingProjection(ctx, req)
```

Swarm checks owner, then exact duplicate/conflict, then expected sequence. An
exact duplicate succeeds without effects or projecting an old Step onto a newer
Turn. For a new append, the prefix matches ExpectedSeq; apply account policy,
consume input and save the Step together. Unknown acknowledgment requires reload.

**Benefit:** remove record scanning and repeated interpretation from storage
adapters. The public result types would need to evolve with Dashi's records.
This does not remove account locking or settleAttempt; the whole 184-line
Recorder is not a deletion claim. Measure incremental projection before adding it.

## R10 — handle shared-budget competition as an expected outcome

[Before and after](examples/r10-shared-budget.md).

**Problem.** Two agents can both pass a balance check before one reserves the
last money. [Swarm's database transaction][transaction] catches this, but the
runtime must recognize a particular `CommitError` plus repository error and
work out how to resume.

**Change.** Let the Recorder report a clear, definite refusal to fund a model
call. Dashi can then record “waiting for money” or “application limit reached”
without sending. Continue C is already saved: the definitely rejected model-start
transaction saves neither start nor reservation; Runner then saves a host-limit
stop. Retrying C returns saved state. Funding requires a fresh Continue D.
Saving the refusal uses the normal persistence/unknown-acknowledgment contract.

The balance check and reservation must stay in the same transaction as the
model-call start. Resuming after a definite rejection needs a fresh command;
retries should be bounded when agents keep competing. BeforeSend already handles preflight host limits; this addition handles the
transaction race. Arbitrary Recorder failures must keep their original meaning.

**Benefit:** an adopter gets one supported shared-budget recipe and fewer
special error checks. Swarm keeps role budgets, routine caps, and top-up policy.

## R11 — correct an estimated charge when the final charge arrives

[Before and after](examples/r11-final-billing.md).

**Problem.** A model request can stop with incomplete billing information.
[Swarm][reconcile] can later replace the estimate with a final charge or a
verified zero. That change lives outside Dashi, so [Swarm's watcher][watch]
uses its account data to override Dashi's displayed cost.

Dashi already accepts late results for pending/unknown model calls. The gap is
correcting the cost of a call that has already finished.

**Change.** Add a saved, accounting-only correction for a particular model call.
It should update cost evidence/spend while keeping reply, tools and finished
state intact. Cost finality does not independently make usage final. Give the application the old and new amounts so it can charge or
refund the difference in the same transaction.

The app verifies receipt authority; Source/Final fields are assertions, not proof.
Deduplicate evidence separately from CommandID; conflicting receipt reuse or
changes to authoritative final evidence fail. Preserve the current storage owner. Calculate the difference between rounded old/new totals, rather
than rounding the difference. If the final bill exceeds the amount held, save
the real bill and prevent further funded work.

**Benefit:** consistent model-cost evidence and less duplicated interpretation.
Raw nano Spend and per-call-rounded account postings can differ; a UI showing
posted charges still needs the ledger. This does not authorize uncertain retries.

## R12 — use the same cost rules for a single text request

[Before and after](examples/r12-single-request-billing.md).

**Problem.** Swarm's Writing desk calls a model without an agent tool loop.
[Its Generate path][generate] repeats rules for usage prices, incomplete costs,
and whether a failed request actually ran. It posts the whole reserved amount
as an estimate in some cases where the Turn path reports known partial cost.

**Change.** Export the shared cost calculation for a prepared request, result,
and send error. Preserve calculated/reported cost source, usage source, finality and missing
prices. An estimate is not a known lower bound. Let Swarm choose how to show and post estimates explicitly.

This could remove `generateCost`. Defer a storage-owning request executor. Posting policy receives the admitted
hold and can choose full-hold provisional charge or calculated charge plus exposure. Moving the code into a helper
alone does not solve crashes. Settlement failures must remain visible separately
from model failures, and pending settlement must finish during orderly shutdown.
Use a recorded one-call Turn when restart recovery is required.

## R13 — share small money and estimation helpers

[Before and after](examples/r13-money-units.md).

Swarm's `microUSD` converts Dashi's amounts into its database unit with rounding
and overflow checks. Quotient/remainder fixes its nanos+999 overflow locally.
An optional Units helper must support signed amounts/refunds and name rounding
direction explicitly; negative model charges are a separate validation rule.

[ReservationMicroUSD][estimate] estimates from artificial repeated-`x` input.
Defer a broad estimate API until a concrete UI caller needs it. That caller
should see assumptions, price source, output limit and why estimates are unknown. The real reservation still needs `WorstCaseCost` on the
actual prepared request, including history and tools. The preflight estimate
should make its approximation clear to the application.

[recording]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/recording.go#L14
[hook]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L638
[transaction]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/repo/entrepo/model.go#L347
[reconcile]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/repo/entrepo/model_budget.go#L315
[watch]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1354
[generate]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/generate.go#L114
[estimate]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L158
