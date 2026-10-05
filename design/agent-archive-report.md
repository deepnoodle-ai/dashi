# Agent archive, phase 3: spend report

**Status:** Accepted · **Date:** 2026-10-05 · **Owner:** Curtis

The archive holds 483,632 `usage` rows across 27 models, but nothing turns
them into dollars. `agent-archive report` prices every row with Dashi's own
catalog and shows spend by model, app, or project, per day, week, or month.
It makes the archive worth opening weekly, and tests Dashi's cost code
against a year of real calls: where Claude Code recorded its own total
(`sessions.app_cost_usd`), a disagreement exposes a pricing bug. Counts below
come from the archive on 2026-10-05, before PR #197 (which drops 1,498 Codex
rows that forks copied from their parents, and adds `app_usage`).

## Words

Phase 1 and 2 words hold ([agent-archive.md](agent-archive.md),
[agent-archive-parse.md](agent-archive-parse.md)). New ones:

- *List price*: what a call would cost at the provider's published API
  rates. Calls made under a subscription were not billed this way; the
  report shows what they would have cost.
- *Unpriced*: a call the catalog has no price sheet for. Never shown as $0.
- *Lower bound*: an amount known to be at or below the list price, because
  some part of it could not be priced. Marked with a trailing `+`.
- *High estimate*: an amount priced at the top of a known range, so at or
  above the list price. Used where the logs cannot say which of two rates
  applied; we prefer showing a little too much to showing too little.
- *Project*: a `working_dir`, with worktree folders rolled up (decision 6).
- *Calls not in session files*: model calls Claude Code counts in its own
  total but never writes to a session file (decision 7).

## Decisions

1. **Prices come from `catalog`, through `llm.Pricing.Estimate`.** The
   report adds no rates of its own. Through `go.work` it imports the Dashi
   checkout's `catalog` and `llm`, so a catalog fix shows in the next run.
   The app picks the provider: `claude_code` is `anthropic`, `codex` is
   `openai`. The model is looked up with `catalog.Find`, then by
   `Entry.Serves` for dated snapshots (`claude-haiku-4-5-20251001`). An
   entry with `Pricing == nil` is unpriced.
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
     catalog" or "no price in catalog". Today that is 22,602 rows (5%):
     `gpt-6.1-sol`, `codex-auto-review`, six older
     `*-codex` models, `gpt-5.3-codex` (in the catalog without a price), and
     109 Codex rows with no model, shown as `(none)`.
   - *Fast calls.* `speed = fast` (175 rows, all `claude-opus-5`) has no
     catalog price yet. Priced at standard rates, marked `+`.
   - *Codex cache writes.* Codex never reports them, and phase 2 marks its
     usage `CacheCreationInputTokensUnavailable`. `Estimate` then refuses any
     sheet whose `CacheWrite` differs from `Input`, which is every
     `gpt-6-*` and `gpt-5.6-*` sheet (`CacheWrite` is 1.25 × `Input`):
     216,126 rows, 45% of the archive. The report leaves the flag off and
     prices every uncached input token as a cache write: a high estimate, at
     most 25% over on those tokens. `--json` also gives the low amount, with
     all of them priced as input.
4. **One price per model, today's.** The catalog keeps no history, so
   April's `gpt-5.5` calls are priced at the 2026-09-09 sheet, whose date
   `--json` names. Price history waits until a real price change matters.
5. **Days come from the model call, not the session.** `usage` has no time,
   and 213 sessions (96,631 rows, 20%) span more than one day. Phase 2 adds
   `usage.called_at`, the timestamp of the row's line (both apps write one
   on every line). `CREATE TABLE IF NOT EXISTS` leaves an existing table
   alone, so first, opening the database adds the nullable column with
   `ALTER TABLE`, as `addSessionColumns` does for `sessions`; then a
   `parserVersion` bump makes the next `copy` parse again and fill it. A
   `NULL` falls on its session's `started_at` day. Periods use the local
   time zone; weeks start on Monday.
6. **Worktrees roll up to their project.**
   - `…/.claude/worktrees/<name>[/…]` becomes the part before
     `/.claude/worktrees` (343 sessions).
   - `~/.codex/worktrees/<id>/<name>[/…]` (1,064 sessions) does not name
     its project. It becomes the one other `working_dir` in the archive
     whose last element is `<name>`. When several match (`dive` has three),
     the one whose sessions share the row's `git_remote_url`; when none
     does, it stays `~/.codex/worktrees/*/<name>`, visible rather than
     guessed.
   - Home prints as `~`; a project's subfolders are not merged.
7. **Calls not in session files get their own rows.** A rough SQL check
   found sessions with subagents about 12% below Claude Code's total: calls
   Claude Code never writes to a session file. Most are background
   subagent summaries; the rest are compaction, prompt suggestions, memory
   extraction, and the Haiku calls behind WebFetch and WebSearch, with all
   web search fees. PR #197 stores Claude Code's per-model totals in
   `app_usage`. Per session and model, the report subtracts the archived
   rows (subagents included), clamps each token column at zero (7 of 372
   sessions go below, by small amounts, mostly cache writes), and prices the
   rest as one `llm.Usage`. Model names drop `[1m]`; Anthropic's current
   models have one price at any length. The amount falls on the session's
   last call day, in its own row, such as
   `claude-haiku-4-5 (not in session files)`.
   - *Cache writes are a range.* `app_usage` has no one-hour split (Claude
     Code's `modelUsage` records none), and archived calls write one-hour
     cache heavily (46.2M of 179.5M `claude-opus-5-5` write tokens), so
     neither rate can be assumed. The writes are priced twice: all at the
     5-minute rate (low) and all at the one-hour rate (high). The table
     shows the high estimate; `--json` gives both.
8. **Compare with Claude Code's own total, per session.** `app_cost_usd` is
   on 395 of 440 Claude Code root files and none of Codex's. The Dashi side
   is every priced row with the same `session_id`, subagents included, plus
   its calls not in session files, so the check tests prices. They agree
   when `app_cost_usd` is within 1% or $0.01 (whichever is larger) of the
   Dashi amount, or of its range when calls not in session files write
   cache; lower and higher are relative to that range. A session with an
   unpriced or fast row is "not compared". Every report ends with one line
   counting sessions that agree, are lower, are higher, or were not
   compared; `--by session` lists them with both amounts and the
   difference, largest first. Tests cover a session with such writes whose
   app cost falls inside the range (agrees) and outside it (lower, higher).
9. **Read-only.** The report opens the database read-only, so it runs while
   `copy` does, and never stores a cost, which would go stale.

## Command

```text
agent-archive report [--by model|app|project|session] [--per day|week|month]
                     [--since <date>] [--until <date>] [--json]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--by` | `model` | What each row of the table is. |
| `--per` | `week` | What each column is. `--by session` has none. |
| `--since` | 8 periods | Start of the eighth period back, counting the current one (`2026-08-17` for weeks on 2026-10-05). |
| `--until` | today | Last day included. |
| `--json` | off | One JSON object per row and period instead of a table. |

Rows sort by total, largest first. The columns are one per period
(`2026-09-28` names the week starting that Monday), then `Total`. The last
row is `Total`. `--by session` has the columns `Session`, `App`, `Project`,
`Started`, `Dashi cost`, `App cost`, `Difference`. JSON amounts are decimal
strings, as `money.Amount` writes them, with `lower_bound`,
`unpriced_calls`, `low` and `high` where a high estimate applies, and the
price sheet's `updated_at` per model.

## Out of scope

Budgets and limits, charts, a web UI, price history, other currencies, and
editing the catalog. Missing prices go in `catalog` in their own PR.
