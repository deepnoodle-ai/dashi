# Agent archive, phase 2: parse and convert

**Status:** Built (see "As built") · **Date:** 2026-10-04 · **Owner:** Curtis

Phase 1 ([agent-archive.md](agent-archive.md)) copies every Claude Code and
Codex session file. Phase 2 reads them: it fills the rest of `sessions`, adds
a `usage` table, and converts an agent's lines to `[]llm.Message` so phase 3
can replay real work through Dashi. The counts below come from a read-only
survey of the archive on 2026-10-04: 434 Claude Code session files, 1,466
subagent files, and 3,706 Codex files (1,765 sessions), from Claude Code
2.1.234 to 2.1.289 and Codex 0.36.0 to 0.159.2.

## Words

Phase 1's words hold. New ones:

- *Agent*: the root conversation or one subagent, named by `agent_id`; it may
  span several copies.
- *Model call*: one request to a model and its reply (often several lines).
- *Served model*: the model that answered; it can differ from the requested.
- *Fork*: an agent that starts with a copy of another agent's conversation. A
  forked root keeps `agent_kind = root`.
- *Final path*: the lines of a Claude Code file the conversation ended on. A
  rewind leaves the other lines off it.
- *Approval reviewer*: a Codex subagent (`source.subagent.other` is
  `guardian`) that judges whether a tool call may run.

## Decisions

1. **Parse a session at a time, in two passes.** Forks repeat lines across
   files and each model call counts once, so parsing reads every current copy
   of a session together. Pass 1 reads each new copy alone for phase 1's facts
   plus `parent_agent_id`. A nested Codex subagent names only its parent, so
   pass 1 walks up to the root and stores the root's id in `session_id` (22
   agents today). Pass 2 parses each session with a copy needing parsing and
   writes all its copies in one transaction.
2. **Parsing runs in `copy`, `rebuild`, and a new `parse` command.** A copy
   whose `parser_version` is not current needs parsing, and so does its
   session. Bumping the one `parserVersion` re-parses the archive (4.4 GB
   gzipped, minutes) in the next nightly run; `parse --all` forces it.
   `rebuild` applies the events, then both passes, recreating every fact.
3. **Stale facts are deleted.** A `copy_superseded` event deletes the old
   copy's `usage` rows and phase 2 columns in its transaction, and an in-place
   rewrite deletes `usage` rows with the `sessions` row. Superseded copies
   keep only phase 1 facts.
4. **Facts are stored, messages are not.** `agent-archive convert --session
   <id> [--agent <id>]` prints one JSON line per message with its `LineRef`
   (decision 6), and the reports on stderr.
5. **Packages.** `claudecode` and `codex`, one per app, and `sessionfile` for
   the types both share. Each app package has the two functions below; both
   take every current copy of one session. `Copy` is a `CopyID`, its
   `agent_id` and `started_at` from pass 1, a Claude Code subagent's
   `.meta.json`, and an `Open` function that yields its uncompressed lines. `ParsedCopy` is one copy's new
   `sessions` columns, its `usage` rows, and its skipped-lines report.
   `Convert` returns one agent (`agentID` empty for the root). The error is
   only for a copy that cannot be read.

   ```go
   func Parse(copies []sessionfile.Copy) ([]sessionfile.ParsedCopy, error)
   func Convert(copies []sessionfile.Copy, agentID string) (sessionfile.Transcript, error)
   ```

6. **Transcript.** `Messages []llm.Message`, `LineRefs []sessionfile.LineRef`,
   `SkippedLines map[string]int`, and `ChangedBlocks map[string]int`.
   `LineRefs[i]` says where `Messages[i]` came from: `CopyID`; `Lines`
   (1-based, in the uncompressed copy; a model call spans several);
   `Inherited` (repeated from the agent it forked from; kept, since it was in
   the fork's context); `CompactionSummary` (the app's summary at a
   compaction); and, on an assistant message that is not inherited, `Model`
   (served) and `Usage *llm.Usage`. `SkippedLines` maps a reason to a line
   count: `ignored:<type>`, `unknown:<type>`, `bad_line`, `rewound`,
   `duplicate_line`, `api_error`. `ChangedBlocks` maps a reason to a content
   block count: `dropped:<block>`, `reminder_as_text`, `made_up_id`.
7. **Messages are the full recorded history,** including lines from before a
   compaction that the model no longer saw; a `CompactionSummary` message
   marks where the app reset its context. Codex's `replacement_history` (what
   it kept) is not used: it repeats earlier lines, and phase 3 can read it
   through the summary's `LineRef`.
8. **Works by default.** A bad line or an unknown line type is counted and the
   run goes on; each new unknown type adds one run warning. Only an unreadable
   copy is `failed`. Converted content must pass `llm.Content.Validate`.
   Reminder names must match `[a-z][a-z0-9-]*`: a name taken from a source
   type is lowercased, other characters become `-`, and an empty result is
   `attachment`. A reminder that would fail (over 64 KiB, about 14 lines) becomes user text; a
   Codex web search without an `id` (537 of 732) gets one made from its line
   number; anything else that would fail is dropped. Each is counted.
9. **Module.** `go.mod` requires Dashi's current release; `go.work` adds
   `../..`.
10. **Fixtures are hand-written,** one per tricky case below, in each
    package's `testdata/`, with placeholder text and made-up ids. A
    fixture per reminder source (`isMeta`, an attachment type with `_`,
    Codex `agent_message`) checks that its reminder passes validation and
    survives conversion.

## Database

New `sessions` columns (`models` from the phase 1 outline is dropped: `usage`
lists them). "Meta" is the subagent's `.meta.json`.

| Column | Meaning; Claude Code source / Codex source |
| --- | --- |
| `agent_kind` | `root`, `subagent`, `fork`, `approval_reviewer`; meta `isFork` / `source`, `forked_from_id` |
| `parent_agent_id` | agent that started it, empty if the root did; meta `parentAgentId` / `parent_thread_id` |
| `parent_tool_call_id` | the tool call that started it; meta `toolUseId` / the call whose output names it |
| `forked_from_session_id` | another session this root was forked from; none / `forked_from_id` (5 roots) |
| `entrypoint` | how the app was run; `entrypoint` / `originator` |
| `git_branch`, `git_remote_url` | `gitBranch`, last `pr-link` repository / `git.branch`, `git.repository_url` |
| `title` | last `custom-title`, else `ai-title`; meta `description` / `session_index.jsonl` `thread_name` |
| `ended_at` | timestamp of the last line |
| `user_prompts` | `user` text lines on the final path, minus slash-command wrappers, local command output, interrupt markers, and task notifications / `UserMessage` items, else user messages minus injected context |
| `tool_calls`, `tool_errors` | `tool_use`, `is_error` results / calls, outputs with a nonzero exit code |
| `compactions` | `compact_boundary` lines / own `compacted` lines |
| `app_cost_usd` | last `cost-state.totalCostUSD` (root file) / empty |
| `usage_source` | where usage came from: `message_usage` (assistant lines), `token_usage_record`, `token_count`, or `none` |
| `skipped_lines` | the skipped-lines report as JSON |

`usage` has one row per model call, so phase 3 sees the input size at each
call and can price it: long-context price bands (OpenAI's from 272,001 input
tokens) apply per request. Totals are SQL sums. A model call is counted once
per session, on the copy of the earliest-started agent that has it.

| Column | Claude Code | Codex |
| --- | --- | --- |
| `copy_id`, `line` | first line of the model call | the token line |
| `model_call_id` | `message.id` | `response_id`, empty for `token_count` |
| `model` | served model | latest `turn_context.model` |
| `input_tokens` | `input_tokens` | `input_tokens` − `cached_input_tokens` |
| `cache_read_input_tokens` | same name | `cached_input_tokens` |
| `cache_creation_input_tokens` | same name | 0 (`cache_write_input_tokens` is always 0) |
| `cache_creation_1h_input_tokens` | `cache_creation.ephemeral_1h_input_tokens` | 0 |
| `output_tokens` | `output_tokens` | `output_tokens` |
| `reasoning_tokens` | `output_tokens_details.thinking_tokens`, else 0 | `reasoning_output_tokens` |
| `web_search_requests` | `server_tool_use.web_search_requests` | `web_search_call` items in the call's reply |
| `speed` | `usage.speed` (`fast` on 287 lines) | empty |

Token columns mean the `llm.Usage` fields of the same name. Reasoning is part
of output in both apps, as `ReasoningTokens` is in `OutputTokens` (checked:
`thinking_tokens` ≤ `output_tokens` on every line). `web_search_requests` is
`Requests["web_search"]`. `speed` has no `llm.Usage` field and Dashi pricing
has no speed variants, so phase 3 can only flag fast calls. Codex usage sets
`CacheCreationInputTokensUnavailable`. A recursive SQL query walks the agent
tree by `parent_agent_id`.

## Mapping lines to messages

| Claude Code line | Becomes |
| --- | --- |
| `user` text | user `llm.Text` |
| `user` with `isMeta` | user `llm.Reminder` named `claude-code-meta`, tier `ReminderContextual` |
| `user` with `isCompactSummary` | user text, `CompactionSummary` |
| `user` `tool_result` | `llm.ToolResult` with `IsError`; `tool_reference` blocks dropped |
| `user` `image` (base64) | `llm.Image` with inline data |
| `assistant` lines of one model call | one message; nonempty `thinking` as `llm.Reasoning`; `tool_use` as `tool.Call`; `Source` gets model and `message.id` |
| `attachment` with `rendered` | user `llm.Reminder` named after its type (decision 8) |
| other attachments, `system`, metadata lines | `ignored:<type>` |

| Codex line | Becomes |
| --- | --- |
| `response_item` `message` | user, developer, or assistant text; `input_image` as `llm.Image` |
| adjacent `reasoning`, assistant `message`, `*_call` items | one message; `summary` as `llm.Reasoning`; web search as `llm.ProviderToolUse` |
| `*_output` items | `llm.ToolResult`, `IsError` on a nonzero exit code |
| `agent_message` | user `llm.Reminder` named `agent-message`, `From` the author |
| `compacted` | user text of its `message`, `CompactionSummary`; an empty one (3,574 of 3,576) gives a message with no content, kept as the marker |
| `event_msg`, `turn_context`, and the rest | `ignored:<type>` |

Lossy by design: Claude Code thinking text is empty in 60,249 of 62,496
blocks; Codex reasoning is encrypted in all 353,340 items and summarized in
67,612. Encrypted reasoning, thinking signatures, and Codex's encrypted
`compaction` items are dropped and counted: nothing here can replay them.

## Tricky cases

| Case | Count | Rule |
| --- | --- | --- |
| Claude Code model call split over lines | 71,609 on 170,581 lines | Merge by `message.id`. `output_tokens` grows (40,092), stays (31,484), or drops (33): take the maximum, and the same for `thinking_tokens`. (The phase 1 outline said the last value; the maximum is deliberate.) |
| Claude Code fork repeats lines | 41 forks, 2,703 lines; 13 subagents, 1,052 | Shared with sibling subagent files under each file's own `agentId`. A `uuid` seen in an earlier-started agent is `Inherited`. Usage goes by `message.id` instead: 19 of 146 shared ids appear under different `uuid`s. |
| Codex token sources | both 1,267 files; `token_count` only 2,272; none 167 | Prefer `token_usage_record` (its `usage`). Use a `token_count`'s `last_token_usage` only when its total changes (11,764 repeats; 678 lack `info`). |
| Codex session in two files | 16 sessions | Same `session_meta.id`, no shared lines, one after the other: convert in time order. |
| Codex fork prefix | 337 of 369 forks | Codex records the parent's line count as the fork's start `ordinal`. In forks only, lines with a lower `ordinal` are inherited. No prefix holds token lines; its `web_search_call` items are not counted. |
| Codex `ordinal` not a prefix | 32 forks, 1,195 approval reviewers, 146 subagents; 27 files lack it | The start `ordinal` equals the file's own line count, or is absent (0.140.0-alpha.2): ignored. |
| Claude Code parallel lines in the `uuid` tree | 4,528+ results, 1,704 lines | A walk up `parentUuid` from the last line misses parallel tool results and split lines. Final path: that walk (crossing `compact_boundary` by `logicalParentUuid`), plus lines of its model calls or answering its calls, until none are added. |
| Claude Code rewound lines | 401 lines in 165 files (288 prompts) | Off the final path: skipped as `rewound`. |
| Claude Code served model differs | 4,554 lines | `usage.iterations[].model` differs from `message.model`; usage goes to it. |
| Claude Code fork by reference | 18 forks | `fork-context-ref` names `parentLastUuid`; `Convert` prepends the parent's final path up to it, `Inherited`. |
| Claude Code error lines | 7 | Model `<synthetic>`, `isApiErrorMessage`: `api_error`, no usage. |
| Claude Code duplicated lines | 2 files, 1,024 `uuid`s | First copy wins: `duplicate_line`. |
| Codex tool errors | 9,840 nonzero exits | No error flag; read the exit code from output text or JSON `metadata`. |
| Codex subagent link | 72 by id, 623 by path | The output of the tool call that started it gives `agent_id` (older) or `task_name`, equal to the child's `agent_path`. |

## Resolved questions

Each followed its recommendation.

1. **Claude Code `git_remote_url`** comes only from `pr-link`; phase 3
   groups the rest by `working_dir`, merging `.claude/worktrees/<name>` paths.
2. **List-price cost** is phase 3's: it prices `usage` rows with Dashi's
   catalog. Nothing is stored here.
3. **`tool-results/` files** are not inlined: the pointer text is what the
   model saw.
4. **Codex tool errors** come from exit codes in output text. `status` warns
   when a Codex version has 200 or more tool calls and no tool errors.
5. **Tool definitions** are phase 3's, built from the calls seen.

## As built

- Pass 1 stores a Codex subagent's parent thread in `parent_agent_id`; the
  link step walks it to the root, and pass 2 rewrites it. Pass 2 sets
  `parser_version`, so a crash between passes parses the session again.
- One message per line, except a model call's lines. Adjacent tool results
  are not merged.
- Every Claude Code token count of a split call takes its maximum, not only
  `output_tokens` and `thinking_tokens`. `git_branch` is the first seen.
- A fork by reference leaves out parent lines written after its fork point.
- Codex can write a reply's token line after its tool outputs, so a token
  line's usage goes to the last reply since the previous token line. A
  token line before any line names the model gets the first model named.
- A Codex code mode output starts "Script completed" or "Script failed" and
  holds the script's printed exec results; a failed script, or any nonzero
  `exit_code` key in them, is a tool error.
- Codex tool names join an MCP namespace and name with `__`; a custom tool's
  input becomes a JSON string; `tool_search_call` is a call named
  `tool_search`. `tool_calls` leaves out `web_search_call`, which the
  provider runs.
- `rewound` also counts attachments on a rewound branch: 741 lines in 183
  files, of which 415 are `user` or `assistant` (the survey's 401).
