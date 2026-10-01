# Spend aggregation completeness

Status: Accepted · Author: Codex · Date: 2026-09-29\
Workflow: Write the spec, wait for the orchestrator checkpoint, then implement.

## Context and boundaries

The [approved PRD](../prds/spend-usage-accounting.md) separates failed usage aggregation from uncertain monetary totals.
`Spend.Add` currently conflates these conditions. Replay rejects cumulative overflow and can discard a valid settlement at recorder admission.
The design preserves atomic `llm.Usage.Add`, exact `money.Amount`, and the existing record of individual execution facts.
No provider behavior, price selection, currency conversion, billing ledger, or SQL schema changes.

## Aggregation

Add `UsageIncomplete bool` with JSON tag `usage_incomplete,omitempty` to Spend.
`Clone` preserves the value. `Add` ORs both operands' flags and sets the flag when `Usage.Add` fails.
Failure retains the complete prior Usage. Monetary maps continue independently, and a usage error alone never increases either cost uncertainty counter.
Directly constructed invalid usage also fails addition. This report signal never makes invalid individual turn facts acceptable.

Replay keeps `validateModelAccounting` and existing tool/record validation before accumulation.
Each reported Usage and Charge must be valid independently. Invalid individual facts still produce `ErrInvalidTurn`.
Validate complete fallback usage before aggregation too. Invalid fallback remains an accounting error. Derivation overflow follows the separate rule below.

Distinguish invalid recorded Usage from overflow while deriving fallback usage, including inferred hosted-request counts in `addUnreportedRequests`.
Use checked arithmetic throughout derivation. Return a distinct private incompleteness signal from `attemptSpend` rather than constructing wrapped, invalid Usage.

Derive transactionally from a copy. On overflow, discard enrichment mutations and retain the original valid reported Usage as a partial contribution.
Mark Spend.UsageIncomplete. Without reported Usage, omit the failed derived contribution. The valid recorded fact remains replayable and recordable.

Preserve independent settlement Charge or reported Cost with existing precedence. Existing partial charges retain their monetary uncertainty and unpriced-request evidence.
Skip fallback pricing when derivation is incomplete. Without an independent charge, preserve CostUnknown instead of estimating a falsely complete or zero charge.
Do not erase hosted-cost uncertainty merely because its inferred request count cannot fit.

After valid inputs pass, failed usage addition marks incompleteness instead of rejecting the turn.
Reuse a private accumulation helper across Add and replay to keep arithmetic and flags consistent.
Keep `attemptSpend` responsible for nondispatch, estimated usage, and per-attempt price selection.
Never price an incomplete cumulative Usage to replace independently recorded charges.

For each currency, failed monetary addition retains that currency's prior amount and increases `CostUnknown`.
Other currencies and the other monetary category still accumulate.
Replay uses the same rule instead of returning an error solely for a monetary aggregate overflow.
Use checked, saturating addition for `Estimated`, `CostUnknown`, and `CostPartial`: overflow yields `math.MaxInt`, never a wrapped counter.
Use the same checked rule for increments. Positive uncertainty must never disappear through report addition.
`UnpricedRequests` sums saturate independently by meter. A saturated count is a lower bound, and the charge remains partial.
Document this exceptional count behavior. Do not clear partial-cost evidence or reject valid settlements for diagnostic count overflow.

Replay retains its lexical attempt-ID order, then lexical local-tool call-ID order.
Rejected additions leave a prior subtotal; later representable contributions can still add to it.
The signal remains true for that aggregation. Reconciliation rebuilds from the new authoritative facts and may restore completeness.
Spend.Add retains caller order and never mutates either operand. Do not promise associative retained subtotals after overflow.

## Admission and terminal stops

`budgetStop` checks UsageIncomplete before reading output allowance or total tokens whenever a token cap is active.
Choose `LimitOutputTokens` when that cap exists; otherwise choose `LimitTotalTokens`.
Use the reason: `usage aggregation is incomplete; this token cap cannot be enforced; resolve outstanding effects as required, then start a new turn with prior history`.
Retained counts in `LimitDetail.Used` are lower bounds, not complete consumption. Do not derive remaining allowance or FitTokens from them.
Apply this check to both spent and overshot checks, preparation clamping, and steering admission.
Unknown cumulative usage with only MaxCost does not block independently exact monetary admission.
Keep current monetary risk bounds and uncertainty checks.

The run loop preserves unresolved-model recovery and uncertain stops before a terminal budget stop.
This permits valid reconciliation to repair usage before the terminal stop is recorded.
Continue may recover an existing intent or record the terminal stop after a crash following settlement.
Readiness for this recovery does not admit new execution; dispatch guards still prevent new work with incomplete usage.
After a valid settlement, check budgets before executing further tools, classifying output, or dispatching another model call.
A token stop caused by UsageIncomplete records `Terminal: true` even if its retained count is below the cap.

Prospective intents need a separate guard because replay includes their fallback usage before dispatch.
Before committing StepModelCallStarted, validate its candidate accounting against the current record.
If it would newly make usage incomplete under an active token cap, return a PersistenceRejected CommitError with that proposed step.
Do not call the recorder or model. This preserves the existing rejected-intent safety case.
The existing record remains complete, so do not fabricate a terminal stop based on facts that were never committed.
Apply this runner guard with either recorder. Cost-only and uncapped intents retain the broader aggregation behavior.
If a recorded intent needs recovery, establish its execution outcome under existing nondispatch/reconciliation rules before deciding a terminal token stop.

Separate stop validation from the ended-turn predicate in `state.go` and `runtime.terminalAfter`.
At StepTurnStopped, calculate terminality from that step's prefix and compare it with its supplied Terminal value.
An incomplete-usage token stop is terminal only when that prefix has incomplete usage and the corresponding active token cap.
After successful validation, the recorded Terminal flag remains authoritative for execution, continuation, and Next.
Do not recompute an accepted ended state from later corrected accounting.

Permitted late results of started work update Spend but leave the stop and fixed answer unchanged.
An ended view keeps `ReadyToContinue == false`, `NextNone`, and `ErrTurnEnded` for Continue, including a budget raise.
Keep `checkStillOpen` and `checkLateModelStep`: a late result cannot silently convert an existing open limited stop into a terminal one.
Reject such a transition before storage under the existing safeguards.
No new stop field, reason parser, or step version is needed.

## Compatibility and reports

Existing valid stops retain their prefix-based validation. Ordinary token-limit stops with complete usage remain resumable.
Existing cost-terminal rules remain conservative. Never accept an invalid Terminal value merely because its record is old.
The intentional compatibility expansion accepts individually valid records that previously failed only derived or cumulative arithmetic.
It does not reinterpret any recorded stop or repair malformed facts.

Standalone Spend JSON gains one optional field. Missing fields decode false, including old zero values.
Preserve every old positive CostUnknown value without deduction, even if it might describe a usage-only failure.
Turn files store raw facts; replay derives the new signal without adding another stored total or changing format 1.
Old binaries ignore the field and cannot enforce its new token-safety contract. Hosts consuming affected snapshots must upgrade readers together.

`Spend.String` appends `usage incomplete` after existing counters. Existing monetary qualification still depends only on CostPartial/CostUnknown.
The CLI footer replaces its falsely complete token total with `usage incomplete` when flagged.
The delegation summary appends the same marker without combining model and local tool money.
`delegate.Report` and task summaries gain propagation through their existing Add calls.
Update public comments, the budgets guide, and the formatter's earlier design/PRD description to identify the superseding accounting behavior.

## Verification and tradeoffs

Use real overflow fixtures with independently valid inputs, including tokens, request meters, modality detail, and both monetary categories.
Test operand independence, zero/clone propagation, counter saturation, and retained totals in deterministic replay order.
Round-trip new and old Spend JSON. Round-trip turns through MemoryRecorder and reopened FileRecorder, then compare replayed signals and money.
Add a valid report with `Requests[llm.MeterUnreported] == math.MaxInt`, `RequestsFinal == false`, and a declared hosted tool.
Its inferred increment must not wrap or reject the settlement. Assert unchanged reported Usage and retained original counts after discarded enrichment mutations.
Both recorders and replay retain the raw fact, signal incomplete usage, and preserve independent charges.
Repeat without an independent charge and with pricing configured. Assert that incomplete derivation bypasses pricing and retains monetary uncertainty.
Cover delegation totals, a rejected prospective intent, no dispatch under either token cap, and exact cost-only admission.
Test prefix validation of old stops, reconciliation before stopping, terminal Next, rejected raises, and permitted late accounting correction without revival.
Malformed individual usage and charges must still fail. Run a provider-free Example, then formatter, vet, build, and root race tests.

Reject per-bucket flags: atomic usage failure omits the whole contribution, so one completeness signal is sufficient.
Reject an error-returning Spend.Add: it would change callers and still need a durable report signal.
Reject recomputing terminality after late settlement: it can revive an ended turn and invalidate recorded lifecycle facts.
The chosen design retains useful accounting but cannot recover complete usage totals beyond the integer range.
Saturation loses diagnostic count precision while preserving conservative uncertainty. It does not saturate money or Usage totals.

Ship code, tests, Example, and guidance together. No dependency, provider call, migration, or feature flag is required.
Independent implementation and billing-security reviews must verify admission timing and record compatibility.
No unresolved design decision blocks the orchestrator checkpoint.
