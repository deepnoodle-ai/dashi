# Selective request sizing and evaluation

**Status:** Accepted · **Author:** Codex · **Date:** 2026-10-07
**Workflow:** Spec, then build. The PRD review settled scope and completion.

## Context

The [PRD](../prds/selective-evaluation.md) covers issues #221–#223.
Selective question packing already counts encoded bytes. Focus-state
truncation does not. The evaluation's fake judge only knows generated needles.
The corrected original corpus and saved needles are available locally.

## Proposal

### Encoded focus

Keep the existing task, latest-request, and recent-message budgets.
Count each JSON-encoded string when cutting it. Preserve task/request prefixes
and the recent suffix, with whole UTF-8 characters. The encoded focus keeps
room for questions under `compact.Jev113`. Keep request packing and standard
JSON escaping unchanged. Update the regression that currently expects fallback
for escaped focus text.

### Case-specific fake judgment

Create an immutable fake judge for each keyless case. Match its actual needles
against decoded question segment text. Generated cases use the same path.
Keep the fake independent of live Decide and of other concurrently running cases.

A fake judge can identify an old detail without guaranteeing that its whole
segment fits. Recorded checks must distinguish budget-driven omission from
broken selection or preservation. Keep pairing, replay, fallback, and selected
content checks. Report recall honestly rather than asserting an impossible
perfect oracle score for every recorded history.

### Affordable live matrix

Keep recorded native-provider/model behavior as the default. Add an explicit
model/provider override for cheaper comparisons. Record selected models and
summary/probe output ceilings in the report. The override does not change the
source history, window calculation, valid needles, arms, prompts, or score.
Disable extra provider smoke calls unless explicitly requested.

An optional preflight uses real provider preparation with scripted model replies
and fake selection. It makes no paid calls. It checks the corpus and reports
conservative reservations for those prepared requests. Real summaries and
selection can change later payloads, so the live guard recalculates each one.

One evaluation-local ledger owns a finite USD reserve. A model-client wrapper
uses `llm.WorstCaseCost` on the prepared request's exact body and price sheet.
This existing bound covers the highest applicable input/cache rate, full output
including reasoning, tiers, and unknown provider-tool charges. Unknown prices
or charges reject dispatch. Reservations use `money.Amount` and a mutex.
Retain reservations after failures or ambiguous outcomes.

The Decide transport reserves encoded request bytes plus framing at the pinned
Jev input rate. No layer may retry a paid call, including its transport.
Disable Decide retries. Dashi provider clients already dispatch once, and the
eval does not retry model turns. All paid calls share the same ledger. For this
run the hard reserve is $0.90. Mining is forbidden in a capped run. A rejected
reservation stops the run and preserves an incomplete result.

### Private evidence

Public output uses aggregates and per-run case ordinals, such as `case-1`.
These ordinals do not derive from recorded session names, IDs, or paths.
Raw session names, IDs, and paths stay local and never appear in public reports,
diagnostics, or errors. Lost-needle messages use an index and kind, never the
answer. Pairing errors omit private call IDs.
Recorded model/provider errors omit raw provider bodies and replies. Keep any
raw diagnostics and needles outside Git, with mode 0600. Test synthetic secrets
and session identifiers through failure reports.

## Alternatives and consequences

Disabling HTML escaping would fix only one source of expansion. Encoded
truncation also handles controls and quotes while retaining existing encoding.

A shared mutable fake judge would require coordinating concurrent cases.
Immutable per-case judges keep ownership explicit and eliminate that coupling.

A new framework-wide evaluation budget would duplicate existing pricing APIs.
The local ledger composes those APIs and remains specific to the eval command.
Conservative reservations may reject a run whose actual price would fit.
Smaller output ceilings or a cheaper explicit model can make the matrix fit.
Both changes must appear in the dated results.

## Qualification and rollout

Run escaped-focus regressions, recorded fake-judge checks, privacy failures,
concurrent reservation tests, and exhausted-budget dispatch tests. Run the
original keyless corpus and `make check`. Coordinate provider replay tests
with the provider batch and qualify the combined tree before paid comparison.

Publish a separate corrected-window cheaper-model result. A complete 13-case,
64-needle, five-arm matrix resolves #221. Keep historical results labeled.
If the matrix stops early, report its boundary and keep #221 open. Review a
selective lead of 10 points over recency before changing the ship decision.
The library remains experimental throughout this work.
