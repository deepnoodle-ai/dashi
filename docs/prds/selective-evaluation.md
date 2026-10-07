# Reliable selective compaction and recorded evaluation

Status: Approved
PRD PR: #230 · Implementation PR: #235 · Updated: 2026-10-07

## Problem

Library hosts need selective compaction to handle ordinary transcript text
without unnecessary fallback. Its focus state truncates text before JSON
encoding. Escaping `<`, `>`, quotes, or control characters can exceed Decide's
limit afterward ([#222](https://github.com/deepnoodle-ai/dashi/issues/222)).

Maintainers also need recorded-session checks that run without credentials.
The fake judge recognizes generated needles only. The original recorded
corpus now produces 13 cases and 64 valid needles, but every case fails its
preservation check ([#223](https://github.com/deepnoodle-ai/dashi/issues/223)).

Published live totals use the previous window calculation. Issue
[#221](https://github.com/deepnoodle-ai/dashi/issues/221) requests a corrected
comparison. The user permits cheaper models and at most $1 across all batches.
The completed corrected run used GPT-6 Luna with a $0.913183975 cap after
holding $0.086816025 for two incomplete attempts. Total conservative exposure
is $0.369979852; observed priced usage is $0.238994204, within the $1 ceiling.

## Context

`selective.Compactor` already sizes encoded questions and complete requests.
Only focus-state truncation uses raw text budgets. It falls back to stock
compaction when selection fails and reports the cause through `OnSelection`.

The evaluation has stock, request-aware stock, recency, selective, and
summary-first arms. Recorded cases retain their own provider and model by
default. Saved needles avoid new mining. All seven original session folders,
saved needles, and required credential names are available locally.

## Users and job

Library hosts preserve useful old details within a predictable request limit.
Maintainers check compaction plumbing offline and compare selection quality
without confusing a changed benchmark with the historical result.

## Use cases and acceptance

### UC-1: Compact text that expands during JSON encoding

The host calls `Compactor.Start` with an existing history and reads selection.

- [x] Focus fields obey their byte budgets after JSON encoding.
- [x] Truncation preserves valid UTF-8, task/request prefixes, and the recent tail.
- [x] Escaped text fits the encoded request limits without unnecessary fallback.
- [x] Genuinely oversized segments keep the existing short-form and fallback behavior.

### UC-2: Check recorded sessions without API keys

The maintainer supplies recorded folders and the existing needles file.

- [x] The fake judge uses each case's valid needles, including non-generated values.
- [x] JSON escaping cannot hide a needle from the fake judge.
- [x] Concurrent cases cannot change one another's fake judgments.
- [x] Original-corpus checks complete without paid calls or new mining.
- [x] Checks still detect broken pairing, replay, fallback, and selected-content preservation.
- [x] Reports distinguish plumbing evidence from model recall quality.
- [x] Public reports and diagnostics use aggregate results and per-run case ordinals, such as `case-1`.
- [x] Raw recorded session names, IDs, and paths stay local and never appear in public reports, diagnostics, or errors.
- [x] Synthetic private-needle and session-identifier failures never expose private text in public output.
- [x] Raw diagnostics and saved needles stay outside Git with mode 0600.

### UC-3: Refresh affordable evidence

The maintainer chooses cheaper models while retaining recorded inputs and needles.

- [x] Default recorded-model behavior remains available for the original comparison.
- [x] Changed models, output limits, cases, and needle counts appear in the report.
- [x] Every paid dispatch reserves its conservative maximum cost atomically.
- [x] The reserve covers full output, reasoning, and cache creation.
- [x] No paid call is retried, including transport-level retries.
- [x] Missing prices or exhausted reserves prevent dispatch. Failed calls retain their reserve. Only a complete, verified successful model result can settle unused risk.
- [x] All three attempts together remain within $1. The final corrected run reserves at most $0.913183975, reuses needles, and disables paid retries and extra smoke calls.
- [x] A cheaper-model or partial comparison appears separately from the historical totals.
- [x] The docs state whether the requested original comparison remains unperformed.
- [x] All five arms on the corrected original 13-case, 64-needle corpus can close #221 with cheaper models.
- [x] An incomplete corpus or matrix keeps #221 open.
- [x] A selective recall lead over recency of 10 points requires ship-decision review.
- [x] Credentials and private session or needle text never enter Git or PR evidence.

## Decisions

- Measure encoded focus bytes. Keep standard JSON encoding and existing request packing.
  Disabling HTML escaping alone does not bound quotes, controls, or complete JSON fields.
- Give each fake judge immutable case needles. Reject a mutable global corpus during concurrent execution.
- Keep evaluation controls inside the eval command. Do not add framework APIs for one benchmark's spending.
- Use `gpt-6-luna` with reasoning disabled and pinned `jev-1.13.0`, with verified request prices.
  Reject rerunning expensive recorded models within this assignment.
- Use a conservative guard even if it limits the run to fewer cases. Reject an estimated total without dispatch enforcement.
- Preserve the experimental library decision. A complete cheaper-model comparison resolves #221. Keep the old benchmark separate and review any new ship-decision evidence.

## Not in scope

Provider replay conversion belongs to the provider interoperability batch.
New sessions, new needle mining, CLI integration, and stock-compactor redesign
do not resolve these defects.

## Success

Selective compaction handles escaped text predictably. Recorded checks catch
real plumbing defects offline. Published evidence identifies exactly what ran
and what remains unverified, within the assigned spending limit. The corrected
13-case, 64-needle matrix completed all five arms. Selective recalled 24/64
versus recency 29/64, so the package remains experimental without CLI integration.
See the [dated results](../dev/selective-compaction-eval.md#live-run-4-2026-10-07-corrected-recorded-corpus-cheaper-model).
