# Live provider qualification — draft PRD

| Field | Value |
| --- | --- |
| Status | Draft for discussion, 2026-09-28 |
| Roadmap | [Item 5](../remaining-work.md), size M, difficulty High |
| Evidence | [Source survey](05-live-qualification-research.md), [current live guide](../../docs/dev/live-tests.md), [hosted-tool plan](../2026-09-27-resources-and-hosted-tools.md) |

## User problem

A Dashi developer sees a provider route in the code or catalog and needs to know what has actually been proved. Today the answer requires reading several dated paragraphs, test names, and fixture directories. A local green test can mean a replayed response; a credential-gated live test can silently skip. This makes it hard to select a route for a real application or spot the next missing probe. The current live guide is careful about scope, but lacks one scanable status view. [Nvoken](05-live-qualification-research.md) shows a useful one-provider PASS/SKIP/FAIL workflow; Pi and ADK-Go show why broad fixture suites must remain a different evidence class.

## Goals

- Give maintainers and application developers an exact, dated answer for each **route/model/feature** Dashi claims to support.
- Let a maintainer run one bounded, opt-in paid probe and understand its cost, preconditions, and outcome without running every provider.
- Keep fixture and live evidence distinct; show `not_run` and `skipped` rather than implying success.
- Keep the workflow inside existing Go tests and `docs/dev/live-tests.md`, with no new provider testing framework.

Success: a reader can choose a route and see the feature's latest fixture result, latest live result and date, exact model, test command, and known gap in one place. A maintainer can run a single probe and update that row from its output. No row claims live qualification from a fixture, skipped test, or adjacent API route.

## Smallest proposed workflow

Add a **qualification matrix** to `docs/dev/live-tests.md` (or one directly linked short companion if the four-page documentation limit requires it). Each row names `provider / API route / exact model / feature / mode`, then `fixture` and `live` status, last live date, evidence test, and a short note. Statuses are `passed`, `failed`, `skipped`, and `not_run`; fixture and live columns never share an implicit status. A model alias also records the served model when reported. Group rows by route, not by provider name alone; Gemini API and Vertex, or OpenAI Responses and Chat Completions, are different claims. The initial matrix should enumerate every currently implemented route and the high-value capabilities Dashi advertises, explicitly showing unrun rows.

Keep the existing test entrypoints. For each selected row, the guide provides the exact targeted `go test -tags live ... -run ... -v` command, required key/model variables, expected maximum paid calls, and any side effects such as uploads. A wrapper is optional only if the existing two-module invocation becomes a real usability problem. The test checks observable facts (prepared request, server acceptance, final result, usage, recorded Turn steps where applicable) rather than exact model prose. Results show a clear subtest PASS, FAIL, or SKIP. A passed feature probe updates only its exact row; broad agent-turn probes do not qualify hosted tools or media by implication.

Maintain a short evidence rule beside the matrix: fixture pass proves Dashi encoded/parsed a known dialect; recorded cassette proves historical acceptance at its recording date; live pass proves the selected remote route/model/feature at the run date; a deployment or application E2E proves that selected end-to-end path. A provider's documentation can justify catalog evidence but is not a live result. Failures remain visible until retested; outdated passes retain their date rather than silently becoming current.

## Acceptance criteria

1. Every current Dashi provider API route appears in the matrix. Implemented but unqualified features are marked `not_run`; unavailable credentials show `skipped` for that run, without changing prior live evidence.
2. Each live `passed` cell links to a named test and records exact model, date, mode, and observed claim. Fixture-only cells never use “live” wording. The known Gemini error-probe gap remains visibly unrun until actually executed.
3. A maintainer can run one provider/feature probe without other paid subtests, from a documented command whose expected maximum calls and side effects are stated. Tests with no opt-in flag or key do no paid work and print an explicit skip reason.
4. Failure output identifies route, model, feature, and failed assertion without exposing API keys, private prompts, or full sensitive response bodies in the public matrix.
5. The initial matrix is reviewed against test source and [existing dated results](../../docs/dev/live-tests.md); no status is upgraded merely because a test exists. A small review checklist explains how to update a row after a run and how to mark a new route.

## Non-goals

No paid calls are authorized by this PRD. It does not require all providers to pass, create a release gate, run live tests in default CI, build a dashboard, or infer model support from one neighboring model. Full deployment qualification remains application-owned, as Nvoken's public API runner illustrates.

## Risks and open decisions

- **Matrix growth:** A row for every model and feature combination is too large. Start with the exact models actually probed and a separate route-level `not_run` summary for advertised but unprobed features; expand only when a claim needs proof.
- **Flaky and costly tests:** Prefer stable structural assertions, strict call bounds, and targeted filters. Do not auto-rerun paid failures; record transient failures honestly.
- **Drift:** Aliases, versions, and provider behavior change. Keep run date and served model visible. Decide later whether a stated age threshold is useful; do not invent one now.
- **Record format:** A Markdown matrix is easiest to review and sufficient initially. If maintaining it becomes error-prone, derive it from a small machine-readable file; do not add that machinery before a demonstrated need.
