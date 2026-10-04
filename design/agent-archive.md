# Agent archive

**Status:** Proposed · **Date:** 2026-10-03 · **Owner:** Curtis

We want to study a year of real coding-agent work, from Claude Code and
Codex, with tools built in this repo. The first use is to test selective
compaction (`decide/selective`) on real sessions instead of generated ones.
The work has three phases: capture everything (phase 1), convert it to
Dashi messages (phase 2), and build analysis tools (phase 3).

## Why now

Claude Code deletes its transcripts after 30 days by default
(`cleanupPeriodDays`), and on 2026-10-03 the oldest one on this machine was
exactly 30 days old. That day we set `cleanupPeriodDays` to 3650 in
`~/.claude/settings.json` and made a one-time copy at
`~/agent-history/snapshots/2026-10-03/{claude,codex}`: Claude Code's
`projects/` and `history.jsonl` (3.0 GB), and Codex's `sessions/`,
`archived_sessions/`, `session_index.jsonl`, and `history.jsonl` (11 GB).
Codex keeps sessions back to September 2025. Claude Code sessions before
early September 2026 are gone; only their typed prompts survive, in Claude
Code's `history.jsonl`.

## Words

- *Archive*: the directory of copies plus the database, `~/agent-archive/`.
- *Original*: a file that Claude Code or Codex writes, in place under
  `~/.claude` or `~/.codex`.
- *Copy*: one compressed, byte-exact version of an original in the archive.
- *Line*: one JSON object on one line of a session file.
- *Session*: one conversation with Claude Code or Codex, as each names it.
  It is the same idea as a Dashi `session.Session`, so the word is kept.
- *Database*: `archive.sqlite`, one row per session and per copy.

Avoided: *history* (a turn's input in Dashi), *catalog* (Dashi's model
catalog), *transcript* (Dashi's `Transcript`), *trace*, *corpus*, and
*source* (`llm.MessageSource`).

## Phase 1: capture, losing nothing

### What the files look like

**Claude Code.** `projects/<encoded cwd>/<session id>.jsonl`, one file per
session; the file's `sessionId` always equals its name. A session directory
beside it holds `subagents/agent-<id>.jsonl` (with a `.meta.json` naming the
parent's `toolUseId`, `parentAgentId`, `spawnDepth`, `isFork`, `model`) and
`tool-results/` (large tool outputs, images, and PDFs that the transcript
points to). The snapshot holds 420 session files, 1,447 subagent files, and
859 tool-result files. Lines carry `type`, `uuid`, `parentUuid`, `timestamp`,
`cwd`, `gitBranch`, `version`, and `isSidechain`. Of about 25 `type`
values, `attachment` (hook output, reminders) is the most common, then
`assistant` and `user`, then state lines without timestamps (`ai-title`,
`permission-mode`, `cost-state`). One API response is split across
several `assistant` lines, one per content block, each repeating the same
`message.id`, `message.model`, and `message.usage`, so usage must be counted
once per `message.id`. Tool calls are `tool_use` blocks; results are
`tool_result` blocks (with `is_error`) in `user` lines, plus a structured
`toolUseResult`. Compaction appends a `system` line of subtype
`compact_boundary` (`compactMetadata` with `trigger`, `preTokens`,
`postTokens`) and a `user` line with `isCompactSummary`; the full earlier
history stays in the file. Subagent lines share the parent's `sessionId` and
add an `agentId`. No `uuid` appears in two session files, so one session does
not span files. The snapshot spans 32 Claude Code versions in 30 days.

**Codex.** `sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl`, plus a flat
`archived_sessions/`; archiving moves a file between them, and the two sets
share no IDs. Every line has `timestamp`, `ordinal`, `type`, and `payload`.
`session_meta` holds `id`, `session_id` (the root of a subagent tree),
`parent_thread_id`, `forked_from_id`, `cwd`, `git` (`branch`, `commit_hash`,
`repository_url`), `cli_version`, `originator`, and `source`. The model is
not there; it is in each `turn_context` line, with the approval and sandbox
policy. About 1,000 files are approval-review subagents
(`thread_source: guardian_review`). A forked file starts with its own
`session_meta`, then a copy of the parent's, then the parent's history up to
`subagent_history_start_ordinal`. Usage is in `token_usage_record` lines
(per response, with `thread_id`) and, in older files only, running totals in
`event_msg/token_count`; Codex counts cached input inside `input_tokens`.
Tool calls are `response_item` `function_call` / `custom_tool_call` with a
`call_id`, and a matching `…_output`; exit codes are in
`event_msg/item_completed`. Compaction is a `compacted` line carrying the
`replacement_history`; newer server-side compaction is encrypted.
`session_index.jsonl` maps `id` to `thread_name`.

**Both are append-only while a session runs.** Later the same day, every
Claude Code original was identical to its snapshot file or a pure append
(no Codex file had changed). Codex's state database builds its views from
rollouts by byte offset, which assumes appends, but its
`rollout_migration_*` tables show it may rewrite old files.

### Decisions

1. **Copy, don't convert.** Both formats change often, and any conversion
   drops thinking, permission modes, Codex turn context, and UI-only events.
   A lost original can't be recovered; a conversion can be rebuilt any time.
2. **Archive layout mirrors the originals, plus `.gz`.**
   `claude-code/projects/…`, `claude-code/history.jsonl.gz`,
   `codex/rollouts/YYYY/MM/DD/<rollout name>.jsonl.gz`, and
   `codex/{session_index,history}.jsonl.gz`. Codex rollouts go under one tree
   by the date in their name, so archiving one never makes a second copy;
   the database records which directory it was in. Gzip because the standard
   library writes it and `zcat`, `jq`, and DuckDB read it; samples shrank 1.5x
   (base64 images) to 7x.
3. **Copy everything under `projects/`, not just `.jsonl`.** Tool-result files
   and subagent metadata are part of the session.
4. **Copy rule.** Skip an original whose size and modified time match its
   copy. If it grew and its first *old size* bytes hash to the stored hash,
   it was appended to: replace the copy. Any other change keeps the old copy
   as `<name>.<UTC time>.jsonl.gz` and adds a new one. Copy exactly the size
   read at the start, so a session still being written yields a clean
   prefix; readers skip a final line with no newline. Write to a temporary
   file, then rename; set the copy's modified time to the original's. When an
   original disappears, keep the copy and record when.
5. **Import the snapshot first.** `agentarchive copy -from <dir>` reads a
   directory laid out like the snapshot. Delete the snapshot only after
   `agentarchive verify` confirms each of its files has an identical copy.
6. **The database is derived.** Everything in it comes from the copies and
   their paths, so `agentarchive rebuild` can recreate it, for example after
   a reader changes. Raw lines stay in the copies; query them with DuckDB
   (`read_ndjson_objects('…/*.jsonl.gz')`) or `zcat | jq`.

### Database

| Table | One row per | Columns |
| --- | --- | --- |
| `files` | copy | `agent`, `original_path` (relative to `~/.claude` or `~/.codex`), `copy_path`, `kind` (session, subagent, subagent-meta, tool-output, prompt-log, index, other), `size`, `modified_at`, `hash` (SHA-256 of the uncompressed bytes), `copied_at`, `replaced_at`, `original_gone_at`, `parse_status` (parsed, unparsed, not-a-session), `parse_error`, `line_types` (JSON: type → count), `reader_version` |
| `sessions` | session | `agent`, `session_id`, `kind` (main, subagent, review), `file_id`, `project_dir`, `git_branch`, `git_commit`, `git_remote`, `client` (cli, desktop, vscode, exec), `agent_version`, `title`, `started_at`, `ended_at`, `models` (JSON list), `user_messages`, `assistant_messages`, `tool_calls`, `tool_errors`, `compactions`, `parent_session_id`, `parent_tool_call_id`, `forked_from_id`, `spawn_depth`, `agent_cost_usd` |
| `usage` | session and model | `model_calls`, `input_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `output_tokens`, `reasoning_tokens` |
| `jobs` | `copy` command run | `started_at`, `finished_at`, `files_seen`, `files_copied`, `warnings` (JSON) |

A view, `session_totals`, joins `sessions` with summed `usage`. Notes:

- `usage` columns mean what `llm.Usage` fields mean: input excludes cache
  reads and writes, output includes reasoning. The Codex reader subtracts
  `cached_input_tokens` from `input_tokens`. Phase 3 prices tokens with
  Dashi's model catalog; `agent_cost_usd` is Claude Code's own figure from
  its last `cost-state` line, kept for comparison.
- A Claude Code subagent's `session_id` is its `agentId`;
  `parent_session_id` is the shared `sessionId`. Codex subagents use their
  own `id` and `parent_thread_id`.
- Codex counts skip lines copied in from a parent: usage records whose
  `thread_id` is another thread, and lines before
  `subagent_history_start_ordinal`. Claude Code's `<synthetic>` model lines
  are not model calls.
- `line_types` makes format drift visible in SQL: a new type shows up as a
  new key.

### Readers

One package per agent, `claudecode` and `codex`. Each decodes a line into a
small struct with only the fields above and ignores the rest. A bad line is
counted and skipped; a file that can't be read at all is marked `unparsed`
with the error, and the run warns and keeps going. Readers never fail a run.

### Command

`agentarchive`, all flags optional (archive defaults to `~/agent-archive`):

- `copy`: copy new and changed originals, then update their database rows.
  What the schedule runs.
- `rebuild`: recreate the database from the copies.
- `verify`: re-hash every copy and check it against the database (and,
  with `-from`, against another directory).
- `status`: last job, counts, warnings, and unparsed files.
- `schedule install` / `schedule remove`: manage the daily job.

### Schedule

`schedule install` writes a LaunchAgent,
`~/Library/LaunchAgents/ai.deepnoodle.agentarchive.plist`, with the binary's
absolute path, `StartCalendarInterval` at 03:00, and
`RunAtLoad`, then loads it with `launchctl bootstrap gui/<uid>`. launchd runs a
job missed during sleep at wake. Output goes to `~/agent-archive/logs/`. A
daily copy leaves 29 days of margin even if `cleanupPeriodDays` is reset.

### Module

A new module, `devtools/agentarchive`
(`github.com/deepnoodle-ai/dashi/devtools/agentarchive`), never tagged or
released, so its SQLite driver stays out of released modules. It follows
`examples/decide`: its own `go.mod` without `replace` lines, and its own
`go.work` (`use .`, and `../..` once phase 2 imports Dashi), because the root
`go.work` does not list it. `scripts/sync_module_versions.py`'s
`tracked_modules()` must skip `devtools/` as it skips `examples/`; otherwise
its test, which compares tracked modules with the tagged list, fails
`make check`. `make check` does not build or test the module in phase 1 (it
imports nothing from Dashi); `fmt-check` still covers its files. The driver
is `modernc.org/sqlite` (pure Go, no cgo, JSON functions built in; v1.60.1 is
current, and another of our repos already uses it). If importing Claude Code
or Codex sessions becomes a Dashi feature, the readers can move into a
released package then.

### Privacy

The archive holds client code and probably secrets from tool output.

- It stays local: never in this repo, never in a hosted database, and not
  under an iCloud or Dropbox folder. The archive is `0700`, files `0600`;
  `copy` warns about and fixes looser permissions.
- Test fixtures in `testdata/` are hand-written, never cut from real
  sessions. Repo docs describe field names only.
- Phase 3 tools decide per run what may reach a model (below).

## Phase 2 outline: Dashi messages

A converter turns one session's copies into `[]llm.Message`, rebuilt on
demand. Each message links to its line (copy path and line number) in a
parallel slice, not a new `llm.Message` field. Claude Code's per-block
`assistant` lines merge by `message.id`; Codex calls pair with outputs by
`call_id`; forked Codex files drop the copied-in parent history. Each
conversion reports what it skipped, by line type. Compaction, token
estimates, `decide/listtool`, and replay then run on real work, and each gap
in Dashi's message types (encrypted reasoning, free-text tool input) becomes
a note for `llm/`. Once the module imports Dashi, add it to `make check`.

## Phase 3 outline: analysis tools

Small `agentarchive` commands on top of phases 1 and 2.

- **Selective compaction on real sessions** (`decide/selective/eval`,
  `docs/dev/selective-compaction-eval.md`). Cut a session where its history
  fills the window; Claude Code's `compact_boundary` marks where the real
  agent cut. *Needles* are exact strings (paths, error text, identifiers,
  hashes, URLs) the agent used in the next turns that appeared only in the
  old part and that no fresh tool result supplied again. Score whether each
  arm's output still contains each needle: free, deterministic, paired
  across arms. The real agent's own summary (`isCompactSummary`, Codex's
  `replacement_history`) is one more arm.
- **Tune `decide/turnwatch`** (`SameCalls` 3, `MaxReminders` 2) from real
  repeated failing calls, and calibrate `decide/contextrank`'s
  `MinProbability`.
- **Usage stats**: tool failure rates, cost per project, compaction
  frequency; mostly SQL.
- **Decide labels** over many sessions, such as "did the user correct the
  agent here?"

Before any session text goes to a model, a run must name its projects
(an allowlist), and a secret scan drops matching lines.

## Open questions

1. **Rename `~/agent-history` to `~/agent-archive`?** Yes, with one `mv`
   before the first `copy`; "history" means a turn's input in Dashi.
2. **Copy more than sessions?** Yes for Claude Code's `file-history/` and
   `paste-cache/` and Codex's `attachments/` and `generated_images/`: small,
   and session lines point into them. No for memory and notes folders.
3. **Read Codex's SQLite files?** No. `thread_history_1.sqlite` and
   `state_5.sqlite` are built from the rollouts; the only extra facts are
   pins and sections, and `session_index.jsonl` carries thread names.
4. **Load raw lines into SQLite?** No. It would double the storage; DuckDB
   reads the copies directly.
5. **Which models may phase 3 send session text to?** By default, only the
   provider that already saw that session, and only for allowlisted
   projects. Decide's providers need an explicit flag per run.
