# Find yesterday's work and know how to continue

Status: Approved\
PRD PR: Not opened · Implementation PR: Not opened · Updated: 2026-09-30

## Problem and context

A developer returns to an investigation among several sessions in one repository. They need to find it, recognize its last request, and understand its stop before continuing. Dashi preserves recorded work, but `/sessions` offers an unfiltered workspace list. `-c` chooses the newest session; exact IDs require the user to remember them. Titles come from the first prompt. The footer reports individual Turns; no command explains accumulated spend and the next action together.

Current session locks, transcript recovery, and provider binding already protect continuation. Reuse them. The [terminal parity matrix](../research/cli-parity-terminal.md) identifies T22; [operational parity](../research/cli-parity-operations.md) identifies O06. Dive's picker filters title/ID/workspace. The pinned [Codex study](../../design/roadmap-research/14-session-study-codex.md) separates search/read/name from resume. The [Pi study](../../design/roadmap-research/14-session-study-pi.md) separates metadata from recorded history. Adopt discovery and naming without their broader navigation or storage systems.

The primary user is the developer operating the terminal agent. The job is: find prior work, inspect whether it can continue, give it a useful name, and reopen it safely.

## Commands and concepts

| Input | User outcome |
| --- | --- |
| `/sessions [QUERY]` | Find sessions in the selected workspace and choose one to reopen. |
| `/rename TITLE` | Name the current saved session without changing its conversation. |
| `/status [ID]` | Inspect the current session, or another session in this workspace, without switching. |

**Session title** is a user-editable label, initially the shortened first prompt. **Recorded status** explains the last recorded stop and a potential next action. Session ownership and recovery checks determine whether that action is available now. It proves neither worker liveness nor external-command completion. **Recorded spend** sums this session's own Turn facts, including summary Turns; it is distinct from context occupancy and child-task spending.

## Use cases and acceptance

### UC-1: Find the earlier investigation

After several investigations, type `/sessions retry handler`. Identify the matching request or answer, review its status, and reopen it.

- [ ] Omitted/empty query lists saved sessions for the canonical selected workspace. A query matches literal, case-insensitive substrings in title, ID, or recorded user/assistant message text. Spaces remain part of the query; no regex or shell interpretation occurs.
- [ ] Search includes recorded ordinary Turn input, assistant output, and user steering. It excludes system/developer instructions, tool arguments/results, internal compaction summaries, and duplicated projected history. It never resolves attachment references or searches attached bytes.
- [ ] Results show ID, title, update time, last request preview, recorded-stop/action summary subject to UC-3 ownership rules, and a labeled match preview when text matched. A preview identifies its Turn. All terminal text is printable.
- [ ] Results sort by metadata update time descending, then ID for ties. Show at most 50 with a visible truncation notice and instruction to refine the query.
- [ ] Loading is visible; cancellation keeps the current session. No sessions and no matches are distinct states. Unreadable metadata or Turns produce a scoped warning and leave other results usable. A damaged matching session is never presented as complete.
- [ ] Search is read-only. It starts no Turn, opens no model/tool connection, changes no selection/preferences, and records no execution or approval. It never includes a different workspace, including for an exact ID query.
- [ ] A title/ID match remains discoverable if its record cannot be read; unavailable request/status facts are explicit. Failed text scans warn that matches may be missing.
- [ ] The picker works through focused Wonton input in fullscreen and inline modes at 80×24. Line mode prints the same facts and usable IDs; existing `--resume ID` completes reopening. Search/picking requires idle mode; during work, request resubmission.

### UC-2: Give the investigation a useful name

Reopen the matching session. Type `/rename rate-limit investigation`, then relaunch and find it by that title.

- [ ] Rename changes only the current saved session's title and metadata update time. ID, workspace, creation time, recorded messages, settings, effects, and accounting stay intact. Updating the title may change list recency.
- [ ] Trim surrounding whitespace. Accept printable single-line Unicode text of 1–160 runes. Reject empty, multiline, control-bearing, or longer titles with an actionable error; do not truncate silently.
- [ ] Confirm the saved title. A write failure leaves the previous disk and in-memory title intact. Reload/search show the saved title after successful exit or restart.
- [ ] Rename requires idle mode and the current session's existing process lock. Another process cannot rename it through a read-only inspector. An unsaved launch says to send its first request before naming; rename creates no empty saved session.
- [ ] Subsequent Turns do not replace an explicit title with a prompt. Renaming never calls a model or rewrites a Turn log.

### UC-3: Inspect whether and how work can continue

Type `/status` or `/status SESSION_ID`. Read the last request, recorded destination, stop, spend, and available next action.

- [ ] Show session ID/title/workspace, readable Turn count, and the latest ordinary Turn's request preview. If the last execution is a summary Turn, label it separately and retain the ordinary request. An empty session explicitly has no recorded request or spend.
- [ ] Show the latest recorded execution's destination ID/API/model when present. Show saved next-turn destination/model separately when they differ. Unknown legacy fields stay unknown; current defaults never substitute for recorded identity. Endpoint display excludes credentials, userinfo, and query text.
- [ ] Derive the recorded stop from replayed facts. Distinguish completed, interrupted, model-attempt limit, pending approval, uncertain external effect, and failed/unreadable records. Label this process's live activity separately. For any session this process does not own, report “worker liveness unknown”; an unfinished record cannot establish interruption or readiness to continue.
- [ ] Every action names its target session. Direct Enter, `/continue`, approval, `/done`, or `/discard` instructions appear only for the current owned session when its input mode and existing recovery checks permit them. During active work, report its live state and the prerequisite to taking another action. Never claim an unobserved command finished, authorize an automatic retry, or treat permission as granted.
- [ ] For another session, guidance names its ID and explains reopening first: use `/sessions ID` and choose that session while idle, or exit and run `dashi --resume ID`. Continuation remains conditional on acquiring its lock and passing existing recovery checks. The report offers no direct action that could change the current session.
- [ ] Fixture: A stays current while `/status B_ID` inspects B with a recorded interrupted Turn. The report names B, requires reopening B before continuation, and offers no Enter, `/continue`, `/done`, or `/discard` instruction that acts on A. Neither session nor its preferences changes.
- [ ] Fixture: another process owns B while A inspects it. The report labels B's recorded state separately from unknown worker liveness. It never claims B is presently interrupted or ready to continue. Reopening fails through the existing busy-session check, preserving A and both sessions' facts.
- [ ] Show latest-Turn and accumulated session model/tool money by currency, model-call count, uncached input, cache reads/writes, and output tokens. Include summary Turns once. Never sum child report totals into this session's own totals or convert currencies.
- [ ] Preserve `Spend.UsageIncomplete`, estimated usage, missing cache reporting, `CostUnknown`, and `CostPartial` meanings. Incomplete usage never appears as a complete token total; usage-only incompleteness does not label independently exact money uncertain. Keep model and tool money separate.
- [ ] If a Turn is unreadable, disclose the omitted count and label all affected aggregates incomplete. Display readable monetary amounts as a lower bound; do not mutate accounting facts or relabel them as exact zero. No read failure causes a different Turn's stop to impersonate the latest one.
- [ ] Context occupancy uses only the latest model call's own recorded usage and recorded window when available, never summed session tokens. If usage/window is unknown or incomplete, show unavailable/estimated explicitly rather than an exact percentage.
- [ ] Inspection works during work and performs no switch, lock takeover, write, compaction, dispatch, or continuation. Different-workspace IDs fail before showing content. Loading/empty/error/success states are readable at 80×24 in both Wonton modes and line mode.

### UC-4: Reopen with the original execution boundary

Choose the inspected session, or use its ID with `--resume`. Continue an interrupted Turn even after changing the default provider.

- [ ] Reopening retains existing workspace selection, session locking, pending-input loss warnings, damaged-log handling, and uncertain-effect guards. Cancellation or lock failure preserves the current session and its preferences.
- [ ] Continuation uses the recorded destination/definition bindings, independent of current defaults. Missing credentials, changed definitions, or unavailable destinations produce the existing clear stop/error. Never fall back to another provider or repeat recorded effects to make reopening succeed.
- [ ] Completed work uses saved next-turn preferences for a new request. Status clearly distinguishes that choice from the last execution's recorded provider/model. Search/rename/inspection do not persist new provider choices.

## Decisions, limits, and success

Use literal scoped search over readable recorded conversation text. Reject tool-output search, cross-workspace discovery, a full-text database, and a new durable index in this slice. They add storage/access concerns beyond finding an investigation. Rename only the current session under its existing lock; reject an ID-addressed write API. Read-only inspection reports a snapshot. This process knows live activity only for its owned current session. For other sessions, worker liveness stays unknown until successful reopening and existing recovery checks. Reject inferring liveness from a stopped or unfinished record; record and worker states can differ.

Consume the [approved accounting contract](spend-usage-accounting.md), existing replay/Next behavior, and compaction origin facts. Add no second ledger or recovery state. JSON/JSONL automation, exports, forks, rewind, deletion, durable queues, attachments, task inspection, and accounting repair remain separate.

Success is a keyless recorded scenario: several sessions across two workspaces, text search, rename, interruption, changed default provider, status, and safe reopen. Fixtures also cover A inspecting B without switching, externally owned B, a busy current session, damaged metadata/logs, estimated/incomplete usage, multiple currencies, summary cost, and unknown effects. Record actual focused Wonton flows at 80×24. Named independent requirements re-review is ready. The [technical spec](../design/cli-session-inspection.md) awaits root checkpoint; implementation/acceptance, security, and taste reviews precede completion. No product scope changed after review.
