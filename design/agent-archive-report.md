# Agent archive, phase 3: spend report

**Status:** Proposed · **Date:** 2026-10-05 · **Owner:** Curtis

The archive holds 483,632 `usage` rows across 27 models, but nothing turns
them into dollars. `agent-archive report` prices every row with Dashi's own
catalog and shows spend by model, app, or project, per day, week, or month.
It has two purposes: make the archive something we open every week, and test
Dashi's cost code against a year of real calls. Where Claude Code recorded
its own total (`sessions.app_cost_usd`), the report compares the two, so a
disagreement exposes a pricing bug on one side or the other. Counts below
come from a read-only query of the archive on 2026-10-05.

## Words

Phase 1 and 2 words hold ([agent-archive.md](agent-archive.md),
[agent-archive-parse.md](agent-archive-parse.md)). New ones:

- *List price*: what a call would cost at the provider's published API
  rates. Calls made under a subscription were not billed this way; the
  report shows what they would have cost.
- *Unpriced*: a call the catalog has no price sheet for. Never shown as $0.
- *Lower bound*: an amount known to be at or below the list price, because
  some part of it could not be priced. Marked with a trailing `+`.
- *Project*: a `working_dir`, with worktree folders rolled up (decision 6).

## Decisions

1. **Prices come from `catalog`, through `llm.Pricing.Estimate`.** The
   report adds no rates of its own. `go.work` already builds against the
   Dashi checkout (`../..`), so it imports `catalog` and `llm` directly, and
   a catalog fix shows up in the next run. The app picks the provider:
   `claude_code` is `anthropic`, `codex` is `openai`. The model is looked up
   with `catalog.Find`, then by `Entry.Serves` for dated snapshots
   (`claude-haiku-4-5-20251001`). An entry with `Pricing == nil` is unpriced.
2. **Price each row, then sum.** OpenAI's long-input tier starts at 272,001
   input tokens per request (2,108 Codex rows reach it), so summing tokens
   first would pick the wrong rate. Each row becomes an `llm.Usage`:

   | `usage` column | `llm.Usage` field | Priced as |
   | --- | --- | --- |
   | `input_tokens` | `InputTokens` | `Input` |
   | `cache_read_input_tokens` | `CacheReadInputTokens` | `CacheRead` |
   | `cache_creation_input_tokens` | `CacheCreationInputTokens` (includes 1h) | `CacheWrite` |
   | `cache_creation_1h_input_tokens` | `CacheCreation1hInputTokens` | `CacheWrite1h` |
   | `output_tokens` | `OutputTokens` | `Output` |
   | `reasoning_tokens` | `ReasoningTokens` | inside `Output`, not again |
   | `web_search_requests` | `Requests["web_search"]` | `Requests` |
   | `speed` | none | see below |

   Sums use `money.Amount`, which is exact; dollars print to the cent.
3. **What cannot be priced is shown, not dropped.**
   - *Unpriced model.* Its cells get `+`, and a second table lists each
     unpriced model with its app, calls, and token totals, and why: "not in
     catalog" or "no price in catalog". Today that is 24,089 rows (5%):
     `claude-sonnet-5-5`, `gpt-6.1-sol`, `codex-auto-review`, six older
     `*-codex` models, `gpt-5.3-codex` (in the catalog without a price), and
     109 Codex rows with no model, shown as `(none)`.
   - *Fast calls.* `speed = fast` (175 rows, all `claude-opus-5`) costs more
     than the catalog's one sheet. Priced at standard rates, marked `+`.
   - *Codex cache writes.* Codex never reports them, and phase 2 marks its
     usage `CacheCreationInputTokensUnavailable`. `Estimate` then refuses any
     sheet whose `CacheWrite` differs from `Input`, which is every
     `gpt-6-*` and `gpt-5.6-*` sheet: 216,126 rows, 45% of the archive. The
     report leaves the flag off and prices unknown writes as input, marked
     `+`. A missing price on half the archive helps nobody; a known lower
     bound does.
4. **One price per model, today's.** Each sheet has one `updated_at`, and
   the catalog keeps no history, so April's `gpt-5.5` calls are priced at
   the 2026-09-09 sheet. The `--json` output names each sheet's date. Price
   history waits until a real price change makes it matter.
5. **Days come from the model call, not the session.** `usage` has no time,
   and 213 sessions (96,631 rows, 20%) span more than one day. Phase 2 adds
   `usage.called_at`, the timestamp of the row's line (both apps write one
   on every line), and bumps `parserVersion` so the next run fills it.
   Periods use the local time zone; weeks start on Monday.
6. **Worktrees roll up to their project.**
   - `…/.claude/worktrees/<name>[/…]` becomes the part before
     `/.claude/worktrees` (343 sessions).
   - `~/.codex/worktrees/<id>/<name>[/…]` (1,064 sessions) does not name
     its project. It becomes the one other `working_dir` in the archive
     whose last element is `<name>`. When several match (`dive` has three),
     the one whose sessions share the row's `git_remote_url`; when none
     does, it stays `~/.codex/worktrees/*/<name>`, visible rather than
     guessed.
   - The home folder prints as `~`. Subfolders of a project are not merged.
7. **Compare with Claude Code's own total, per session.** `app_cost_usd` is
   on 395 of 440 Claude Code root files and none of Codex's. The Dashi side
   is every priced row with the same `session_id`, subagents included. They
   agree when they differ by at most 1% or $0.01, whichever is larger. A
   session with an unpriced or fast row is "not compared", since its Dashi
   amount is only a lower bound. Every report ends with one line counting
   sessions that agree, are lower, are higher, or were not compared;
   `--by session` lists them with both amounts and the difference, largest
   first.
8. **Read-only.** The report opens `archive.sqlite` read-only, so it runs
   while `copy` does, and never writes a cost back: a stored price goes
   stale when the catalog changes.

## Command

```text
agent-archive report [--by model|app|project|session] [--per day|week|month]
                     [--since <date>] [--until <date>] [--json]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--by` | `model` | What each row of the table is. |
| `--per` | `week` | What each column is. `--by session` has none. |
| `--since` | 8 periods back | First day included (`2026-09-01`). |
| `--until` | today | Last day included. |
| `--json` | off | One JSON object per row and period instead of a table. |

Rows sort by total, largest first. The columns are one per period
(`2026-09-28` names the week starting that Monday), then `Total`. The last
row is `Total`. `--by session` has the columns `Session`, `App`, `Project`,
`Started`, `Dashi cost`, `App cost`, `Difference`. JSON amounts are decimal
strings, as `money.Amount` writes them, with `lower_bound`, `unpriced_calls`,
and the price sheet's `updated_at` per model.

## What a first run already shows

A rough SQL version of decision 7, run while writing this note, already
finds a gap. Of 196 sessions with no subagents and no long requests, 147
agree (average Dashi/app ratio 0.998). Of 76 sessions with subagents and
requests over 200,000 input tokens, none agree (average 0.881). Either
subagent usage is missing or counted once too few times in phase 2, or
Claude Code bills something Dashi's sheet lacks, such as a long-context
rate. The report makes finding out a routine command.

## Out of scope

Budgets and limits, charts, a web UI, price history, other currencies, and
editing the catalog. Missing prices are added to `catalog` in their own PR,
from the provider's price page, never guessed in this tool.

## Open questions

1. **Codex cache writes:** price unknown writes as input and mark `+`
   (recommended), or show 45% of the archive as unpriced?
2. **Model aliases:** `codex-auto-review` and the `*-codex` models have no
   published price. Leave them unpriced (recommended), or let the report map
   each to a priced model, such as `gpt-5.2-codex` to `gpt-5.2`?
3. **Default range:** 8 weeks, or the whole archive?
