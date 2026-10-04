# Agent archive, phase 2: parse and convert

**Status:** Proposed · **Date:** 2026-10-04 · **Owner:** Curtis

Phase 1 ([agent-archive.md](agent-archive.md)) copies every Claude Code and
Codex session file. Phase 2 reads them: it fills the rest of `sessions`, adds a
`usage` table, and converts a session to `[]llm.Message` so phase 3 can replay
real work through Dashi. The counts below come from a read-only survey of the
archive on 2026-10-04: 434 Claude Code session files, 1,466 subagent files, and
3,706 Codex files (1,787 sessions), from Claude Code 2.1.234 to 2.1.289 and
Codex 0.36.0 to 0.159.2.

## Words

Phase 1's words still hold. New ones:

- *Agent*: the root conversation or one subagent; one row of `sessions`.
- *Response*: one model reply, often spread over several lines.
- *Fork*: a subagent that starts with a copy of its parent's conversation.
- *Active branch*: the lines of a Claude Code file the conversation ended on. A
  rewind leaves other lines on an *abandoned branch*.
- *Approval review*: a Codex subagent (`source.subagent.other` is `guardian`)
  that judges whether a tool call may run.

## Decisions

1. **Parse a session at a time.** Forks repeat lines from sibling files, and
   usage must count each response once, so parsing reads every current copy of a
   session together. Superseded copies keep only phase 1 facts.
2. **Parsing runs in `copy`, `rebuild`, and a new `parse` command.** A copy
   whose `parser_version` is not current is pending and makes its whole session
   pending. One `parserVersion` covers both apps; bumping it re-parses the
   archive (4.4 GB gzipped, minutes) in the next nightly run. `parse --all`
   forces it.
3. **Facts per copy, conversion on demand.** `sessions` and `usage` are stored.
   Messages are not: `agent-archive convert --session <id> [--agent <id>]`
   prints one JSON line per message with its line reference (decision 5), and
   the skip report on stderr.
4. **Packages.** `claudecode` and `codex`, one per app, and `sessionfile` for
   the types both share. Each app package has the two functions below; both take
   every file of one session. `File` is a copy's `CopyID` and an `Open` function
   that yields its uncompressed lines. `Facts` is one copy's new `sessions`
   columns, its usage by model (`map[string]llm.Usage`), and its skip report.
   `Convert` returns one agent (`agentID` empty for the root). The error is only
   for a file that cannot be read.

   ```go
   func Parse(files []sessionfile.File) ([]sessionfile.Facts, error)
   func Convert(files []sessionfile.File, agentID string) (sessionfile.Conversation, error)
   ```

5. **Conversation.** `Messages []llm.Message`, `LineRefs []sessionfile.LineRef`,
   and `Skipped map[string]int`. `LineRefs[i]` says where `Messages[i]` came
   from: `CopyID`, `Lines` (1-based numbers in the uncompressed copy; a response
   spans several), `Inherited` (repeated from the agent it forked from; kept,
   since it was in the fork's context), and `Compaction` (the app's summary at a
   compaction). `Skipped` maps a reason to a line count: `ignored:<type>`,
   `unknown:<type>`, `bad-line`, `abandoned-branch`, `duplicate-line`,
   `api-error`, `dropped:<block>`.
6. **Works by default.** A bad line or an unknown line type is counted in
   `Skipped` and the run goes on; each new unknown type adds one run warning.
   Only an unreadable file is `failed`. Converted content must pass
   `llm.Content.Validate`; content that would not is dropped and counted.
7. **Module.** `devtools/agent-archive/go.mod` requires
   `github.com/deepnoodle-ai/dashi` at the current release, and its `go.work`
   adds `../..`, as `examples/decide` does. No `replace` lines in any `go.mod`.
8. **Fixtures are hand-written,** one JSONL file (or folder, for multi-file
   cases) per case in the table below, under each package's `testdata/`, with
   placeholder text and made-up ids. Nothing is cut from real sessions.

## Database

New `sessions` columns (`models` from the phase 1 outline is dropped: `usage`
lists them):

| Column | Meaning; Claude Code source / Codex source |
| --- | --- |
| `agent_kind` | `root`, `subagent`, `fork`, `approval_review`; meta `isFork` / `source`, `forked_from_id` |
| `parent_agent_id` | agent that started it, empty if the root did; meta `parentAgentId` / `parent_thread_id` |
| `parent_tool_call_id` | the call that started it; meta `toolUseId` / spawn call whose output names it |
| `forked_from_session_id` | another session this root was forked from; none / `forked_from_id` (5 roots) |
| `entrypoint` | how the app was run; `entrypoint` / `originator` |
| `git_branch`, `git_remote_url` | `gitBranch`, last `pr-link` repository / `git.branch`, `git.repository_url` |
| `title` | last `custom-title`, else `ai-title`; meta `description` / `session_index.jsonl` `thread_name` |
| `ended_at` | timestamp of the last line |
| `user_prompts` | typed prompts on the active branch / `UserMessage` items, else user messages minus injected context |
| `tool_calls`, `tool_errors` | `tool_use`, `is_error` results / calls, outputs with a nonzero exit code |
| `compactions` | `compact_boundary` lines / own `compacted` lines |
| `app_cost_usd` | last `cost-state.totalCostUSD` (root file) / empty |
| `usage_source` | `message_usage`, `token_usage_record`, `token_count`, or `none` |
| `skipped` | the skip report as JSON |

`usage`, one row per copy and served model; the token columns mean what the
`llm.Usage` fields of the same name mean:

| Column | Claude Code | Codex |
| --- | --- | --- |
| `copy_id`, `model` | served model | latest `turn_context.model` |
| `responses` | distinct `message.id` | records counted |
| `input_tokens` | `input_tokens` | `input_tokens` − `cached_input_tokens` |
| `cache_read_input_tokens` | same name | `cached_input_tokens` |
| `cache_creation_input_tokens` | same name | `cache_write_input_tokens` (always 0) |
| `cache_creation_1h_input_tokens` | `cache_creation.ephemeral_1h_input_tokens` | 0 |
| `output_tokens` | `output_tokens` | `output_tokens` |
| `reasoning_tokens` | 0 (not reported) | `reasoning_output_tokens` |
| `web_search_requests` | `server_tool_use.web_search_requests` | `web_search_call` items |

The agent tree is `parent_agent_id` in `sessions`; a recursive CTE walks it.
Session totals join `usage` to `sessions` by `copy_id`.

## Mapping lines to messages

| Claude Code line | Becomes |
| --- | --- |
| `user`, text a person typed | user `llm.Text` |
| `user` with `isMeta` | user `llm.Reminder` named `claude-code-meta` |
| `user` with `isCompactSummary` | user text, `Compaction` |
| `user` `tool_result` | `llm.ToolResult` with `IsError`; `tool_reference` blocks dropped |
| `user` `image` (base64) | `llm.Image` with inline data |
| `assistant` lines of one response | one message; `thinking` as `llm.Reasoning`, signature as `Replay`; `tool_use` as `tool.Call`; `Source` gets model and `message.id` |
| `attachment` with `rendered` | user `llm.Reminder` named after its type (`_` becomes `-`) |
| other attachments, `system`, metadata lines | `ignored:<type>` |

| Codex line | Becomes |
| --- | --- |
| `response_item` `message` | user, developer, or assistant text; `input_image` as `llm.Image` |
| adjacent `reasoning`, assistant `message`, `*_call` items | one message; `summary` as reasoning, `encrypted_content` as `Replay`; web search as `llm.ProviderToolUse` |
| `*_output` items | `llm.ToolResult`, `IsError` on a nonzero exit code |
| `agent_message` | user `llm.Reminder` named `agent-message`, `From` the author |
| `compacted` | user text of its `message`, `Compaction`; `replacement_history` ignored |
| `response_item` `compaction` (encrypted) | `llm.Opaque`, `Compaction` |
| `event_msg`, `turn_context`, and the rest | `ignored:<type>` |

Lossy by design: Claude Code thinking text is empty in 60,249 of 62,496 blocks;
Codex reasoning is encrypted in all 353,340 items and summarized in 67,612.
Replay data's `Origin` names the app as its scope, so no provider replays it.

## Tricky cases

| Case (outline cases first) | Count | Rule |
| --- | --- | --- |
| Claude Code response split over lines | 71,609 on 170,581 lines | Merge by `message.id`. `output_tokens` grows (40,092), stays (31,484), or drops (33): take the maximum. |
| Claude Code fork repeats lines | 41 forks, 2,703 lines; 13 subagents, 1,052 | Shared with sibling subagent files under each file's own `agentId`. A `uuid` belongs to the session's earliest-started agent; later files mark it `Inherited`, without usage. |
| Codex token sources | both 1,267 files; `token_count` only 2,272; none 167 | Prefer `token_usage_record`. Count a `token_count` only when its total changes (11,764 repeats; 678 lack `info`). |
| Codex session in two files | 16 sessions | Same `session_meta.id`, no shared lines, one after the other: convert in time order. |
| Codex fork prefix and `ordinal` | 1,710 files with a start ordinal | Trusted only for forks: in 337 of 369, lines with `ordinal` < start are inherited. In 32 forks, 1,195 approval reviews, and 146 spawns it equals the line count: ignored. 27 files (0.140.0-alpha.2) have no `ordinal`. No prefix holds token lines. |
| Claude Code siblings in the `uuid` tree | 4,528+ results, 1,704 lines | A walk up `parentUuid` from the last line misses parallel tool results and split lines. Active branch: that walk (crossing `compact_boundary` by `logicalParentUuid`), plus lines of its responses or answering its calls, until none are added. |
| Claude Code abandoned branches | 401 lines in 165 files (288 prompts) | Skipped, counted as `abandoned-branch`. |
| Claude Code served model differs | 4,554 lines | `usage.iterations[].model` differs from `message.model`; usage goes to it. |
| Claude Code fork by reference | 18 forks | `fork-context-ref` names `parentLastUuid`; `Convert` prepends the parent's branch up to it, `Inherited`. |
| Claude Code error lines | 7 | Model `<synthetic>`, `isApiErrorMessage`: `api-error`, no usage. |
| Claude Code duplicated lines | 2 files, 1,024 `uuid`s | First copy wins: `duplicate-line`. |
| Codex usage has no model | all 88,754 records | Model from the latest `turn_context`. |
| Codex tool errors | 9,840 nonzero exits | No error flag; read the exit code from output text or JSON `metadata`. |
| Codex spawn link | 72 by id, 623 by path | Spawn output gives `agent_id` (older) or `task_name`, equal to the child's `agent_path`. |
| Large Claude Code tool output | 741 results | The result text points into `tool-results/`; kept as is. |

## Open questions

1. **Claude Code `git_remote_url`.** Lines carry no remote; only 164 files have
   a `pr-link`. Recommendation: leave the rest empty and let phase 3 group by
   `working_dir`, rather than run `git` in folders that may have changed.
2. **List-price cost.** Codex has no `app_cost_usd`. Recommendation: phase 3
   prices `usage` with Dashi's catalog, so both apps are costed one way; not
   stored here.
3. **Inline `tool-results/` files** in converted tool results? Recommendation:
   no for now; add a `convert` flag if phase 3 needs the full text.
4. **Codex tool-error heuristic.** It reads text; a format change would silently
   zero `tool_errors`. Recommendation: accept it, and have `status` warn when a
   Codex version shows calls but no exit codes.
