# Reliable selective compaction and recorded evaluation

Status: Draft
PRD PR: Pending · Implementation PR: Pending · Updated: 2026-10-07

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
This batch has a $0.90 reserve.

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

- [ ] Focus fields obey their byte budgets after JSON encoding.
- [ ] Truncation preserves valid UTF-8, task/request prefixes, and the recent tail.
- [ ] Escaped text fits the encoded request limits without unnecessary fallback.
- [ ] Genuinely oversized segments keep the existing short-form and fallback behavior.

### UC-2: Check recorded sessions without API keys

The maintainer supplies recorded folders and the existing needles file.

- [ ] The fake judge uses each case's valid needles, including non-generated values.
- [ ] JSON escaping cannot hide a needle from the fake judge.
- [ ] Concurrent cases cannot change one another's fake judgments.
- [ ] Original-corpus checks complete without paid calls or new mining.
- [ ] Checks still detect broken pairing, replay, fallback, and selected-content preservation.
- [ ] Reports distinguish plumbing evidence from model recall quality.

### UC-3: Refresh affordable evidence

The maintainer chooses cheaper models while retaining recorded inputs and needles.

- [ ] Default recorded-model behavior remains available for the original comparison.
- [ ] Changed models, output limits, cases, and needle counts appear in the report.
- [ ] Every paid dispatch reserves its conservative maximum cost atomically.
- [ ] The reserve covers full output, reasoning, cache creation, and retries.
- [ ] Missing prices or exhausted reserves prevent dispatch. Failed calls retain their reserve.
- [ ] This run spends at most $0.90. It reuses needles and disables paid retries and extra smoke calls.
- [ ] A cheaper-model or partial comparison appears separately from the historical totals.
- [ ] The docs state whether the requested original comparison remains unperformed.
- [ ] Issue #221 stays open unless its required comparison is complete.

## Decisions

- Measure encoded focus bytes. Keep standard JSON encoding and existing request packing.
  Disabling HTML escaping alone does not bound quotes, controls, or complete JSON fields.
- Give each fake judge immutable case needles. Reject a mutable global corpus during concurrent execution.
- Keep evaluation controls inside the eval command. Do not add framework APIs for one benchmark's spending.
- Prefer `claude-haiku-5-5`, `gpt-6-luna`, and pinned `jev-1.13.0`, subject to verified request prices.
  Reject rerunning expensive recorded models within this assignment.
- Use a conservative guard even if it limits the run to fewer cases. Reject an estimated total without dispatch enforcement.
- Preserve the experimental library decision. Changed-model evidence cannot silently replace the original benchmark or authorize CLI shipping.

## Not in scope

Provider replay conversion belongs to the provider interoperability batch.
New sessions, new needle mining, CLI integration, and stock-compactor redesign
do not resolve these defects.

## Success

Selective compaction handles escaped text predictably. Recorded checks catch
real plumbing defects offline. Published evidence identifies exactly what ran
and what remains unverified, within the assigned spending limit.
