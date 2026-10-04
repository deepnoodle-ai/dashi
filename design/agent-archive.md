# Agent archive

**Status:** Proposed · **Date:** 2026-10-03 · **Owner:** Curtis

We want to study a year of real coding-agent work from Claude Code and
Codex. The first use is to test selective compaction (`decide/selective`) on
real sessions instead of generated ones. Phase 1 copies every session file
into an archive we control, phase 2 parses the sessions and converts them to
Dashi messages, and phase 3 builds analysis tools.

## Why now

Claude Code deletes session files after 30 days by default
(`cleanupPeriodDays`). On 2026-10-03 the oldest one on this machine was 30
days old. That day we set `cleanupPeriodDays` to 3650 and took a snapshot of
both apps' session files (3.0 GB and 11 GB) at
`~/agent-archive/snapshots/2026-10-03/`. Older Claude Code sessions are
gone; only their typed prompts survive in its `history.jsonl`.

## Words

- *App*: Claude Code or Codex.
- *Original*: a file an app writes under `~/.claude` or `~/.codex`.
- *Archive*: `~/agent-archive/`, holding copies of Claude Code and Codex
  files plus a database.
- *Copy*: one byte-exact version of an original in the archive.
- *Snapshot*: the one-time copy of 2026-10-03, in `snapshots/`, apart from
  the copies.
- *Line*: one JSON object on one line of a session file.
- *Session*: one conversation plus every subagent it starts, as both apps
  use the word; `session_id` is the app's id for it. Each subagent also has
  its own *agent id*.
- *Inherited lines*: lines a forked file repeats from its parent.

## Phase 1: copy everything, lose nothing

### Decisions

1. **Copy, don't convert.** A lost original can't come back; a conversion
   can be redone any time.
2. **What is copied.** From Claude Code: everything under `projects/`
   (session files, `subagents/`, `tool-results/`, and memory files),
   `history.jsonl`, `file-history/`, and `paste-cache/`. From Codex:
   `sessions/`, `archived_sessions/`, `session_index.jsonl`,
   `history.jsonl`, `attachments/`, `generated_images/`, and `memories/`.
   Session lines point into these folders, and the extra folders are small
   (about 80 MB). Not copied: Codex's SQLite files (built from the session
   files) and its `notes/`.
3. **Layout mirrors the originals.** `claude-code/…` follows `~/.claude`.
   Codex session files go under `codex/sessions/YYYY/MM/DD/` by the date in
   their name, whichever folder they are in, so archiving one in Codex never
   makes a second copy. `.jsonl` copies are gzipped; other files are copied
   as they are.
4. **Copy rule.** Compare each original with its newest copy:
   - Same size and modified time: skip.
   - Same size and same hash (SHA-256 of the uncompressed bytes): update
     the modified time only. Some originals change only their time.
   - Larger, and its first *old size* bytes hash to the stored hash: it was
     appended to. Rewrite the copy.
   - Anything else: keep the old copy, renamed with the UTC time added,
     mark it superseded, and write a new copy.

   Copy exactly the size read at the start, so a session still being
   written yields a clean prefix. Write to a temporary file, then rename.
   When an original disappears, keep the copy and record when.
5. **A copy is identified by its archive path,** not its original path,
   because Codex moves files between `sessions/` and `archived_sessions/`.
6. **A copy log keeps what copies can't.** `copy-log.jsonl`, append-only,
   beside the copies, holds every fact about an original or a run; the
   database is built from it plus the copies. One event per line, each with
   `event`, `time`, and `run_id`:
   - `run_started`, and `run_ended` with `originals_seen`,
     `originals_copied`, and `warnings`. A run with no `run_ended` was
     interrupted; its `ended_at` stays empty.
   - `copy_written`: `copy_path`, `original_path`, `app`, `kind`,
     `original_bytes`, `modified_at`, `hash`. Its `time` is `copied_at`.
   - `copy_superseded` (`copy_path` before and after the rename),
     `modified_at_updated`, and `original_gone`.

   Everything else is recomputed from the copies by the current parser:
   `parse_status`, `parse_error`, `parser_version`, `line_types`, and the
   `sessions` table. So `rebuild` reproduces the recorded facts exactly and
   parses with today's parser.

   Order of writes: copy to a temporary file, sync, rename; append the
   event and sync; then update SQLite, which stores how far into the log it
   has applied. Recovery at the start of each run:
   - A torn last log line (crash mid-append) is dropped.
   - Log events past the database's position are applied.
   - A leftover temporary file is deleted.
   - A copy file the log doesn't know (crash between rename and append) is
     hashed and logged with `recovered: true`; a renamed old copy is logged
     as superseded.
7. **Retire the snapshot by verifying it.** Every snapshot file still has
   its original, so the first daily copy captures everything.
   `verify -snapshot <dir>` accepts a snapshot file when a copy, current or
   superseded, equals it or begins with it (the original grew since).
   Curtis may delete the snapshot once `verify` passes; files that fail are
   listed and kept.

### Database

`archive.sqlite`, built from the copy log and the copies (decision 6) by
`copy` and `rebuild`. Phase 1 reads only paths
and the first few lines of each session file; a fact missing there stays
empty. DuckDB and `zcat | jq` read the raw lines.

| Table | One row per | Columns |
| --- | --- | --- |
| `copies` | copy | `copy_id`, `app` (`claude_code`, `codex`), `copy_path` (unique), `original_path`, `kind` (`session`, `subagent`, `subagent_meta`, `tool_result`, `prompt_log`, `session_titles`, `file_history`, `paste`, `attachment`, `generated_image`, `memory`, `other`), `original_bytes`, `modified_at`, `hash`, `copied_at`, `superseded_at`, `original_gone_at`, `parse_status` (`parsed`, `failed`, `skipped`), `parse_error`, `parser_version`, `line_types` (JSON: type to count) |
| `sessions` | session file | `copy_id`, `app`, `session_id`, `agent_id` (empty for the root), `working_dir`, `started_at`, `app_version` |
| `copy_runs` | `copy` run | `run_id`, `started_at`, `ended_at`, `originals_seen`, `originals_copied`, `warnings` (JSON) |

`line_types` shows format changes in SQL: a new line type is a new key.

### Command

`agent-archive`; the archive defaults to `~/agent-archive`.

- `copy`: copy new and changed originals and update the database.
- `rebuild`: recreate the database from the copies and the copy log.
- `verify`: re-hash every copy against the database; `-snapshot` also checks
  a snapshot (decision 7).
- `status`: last copy run, counts, warnings, and failed files.
- `schedule install` / `schedule remove`: manage the daily LaunchAgent
  (03:00, also at login; launchd runs a missed job at wake). Output goes to
  `~/agent-archive/logs/`.

Parsing never fails a run: a bad line is counted and skipped; an unreadable
file is marked `failed` with its error.

### Module

A new module, `devtools/agent-archive`, never tagged, so its SQLite driver
(`modernc.org/sqlite`, pure Go) stays out of released modules. Like
`examples/decide`, it has its own `go.mod` (no `replace` lines) and
`go.work`. Also:

- `scripts/sync_module_versions.py`'s `tracked_modules()` skips `devtools/`,
  as it skips `examples/`.
- The Makefile gets a separate list of untagged modules, used by `test` and
  `vet` (not `SUB_MODULES`), so `make check` tests the copy rule: the only
  code here that can lose data.
- AGENTS.md's "Modules and releases" section describes `devtools/`.

### Privacy

The archive holds client code and probably secrets from tool output.

- It stays local: never in this repo, a hosted database, or a synced folder.
  The archive is `0700` and files `0600`; `copy` fixes looser permissions
  and warns.
- Test fixtures are hand-written, never cut from real sessions.
- Committed docs and eval results name fields and give totals only: no
  needle text, paths, session ids, or project names from real sessions.

## Phase 2 outline: parse and convert

Parsers, one package per app, fill the rest of `sessions` (`entrypoint`,
`git_branch`, `git_remote_url`, `title`, `models`, `user_prompts`,
`tool_calls`, `tool_errors`, `compactions`, `app_cost_usd`), the subagent
and fork tree (`parent_agent_id`, `parent_tool_call_id`,
`forked_from_session_id`, kind `approval_review`), and a `usage` table by
session and model whose columns mean what `llm.Usage` fields mean. A
converter turns one session's copies into `[]llm.Message`, with a parallel
slice linking each message to its line, and reports what it skipped. Once
the module imports Dashi, its `go.work` adds `../..`.

What the parsers must handle, each with a hand-written fixture:

- Claude Code splits one response over several `assistant` lines sharing a
  `message.id`. `output_tokens` grows across them; take the last line's.
- Forked Claude Code subagents repeat the parent's lines (same `uuid` and
  `message.id`). Count each once per session.
- Most Codex files have `event_msg/token_count`, including every file that
  has `token_usage_record`. Use one source per file, never both. Codex
  counts cached input inside `input_tokens`; subtract it.
- Some Codex sessions span two files.
- Forked Codex files start with inherited lines, up to
  `subagent_history_start_ordinal`, but some lines have no `ordinal`.

## Phase 3 outline: analysis tools

- **Selective compaction on real sessions.** Cut a session where its lines
  fill the window. *Needles* are exact strings (paths, error text, ids) the
  agent used next that appeared only before the cut. Score whether each arm
  keeps each needle; the app's own summary is one more arm.
- **Tune `decide/turnwatch` and `decide/contextrank`.**
- **Session stats:** tool error rates, cost per project (`git_remote_url`,
  else `working_dir`), compaction frequency.

Before session text goes to a model, a run names its projects (an
allowlist), and a secret scan drops matching lines.

## Open questions

1. **Backup.** This Mac has no Time Machine destination, so the archive is
   on one disk, and "local only" rules out a cloud copy. Recommendation: an
   encrypted external disk (or another encrypted backup Curtis controls),
   synced after each daily copy. Or accept the risk in writing.
2. **Which models may phase 3 send session text to?** Proposed: only the
   provider that already saw that session, for allowlisted projects;
   Decide's providers need an explicit flag per run.
