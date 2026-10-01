# Workspace session inspection

**Status:** Accepted · root checkpoint approved\
**Author:** Codex · **Date:** 2026-09-30\
**Workflow:** Spec → wait for feedback → build. Named PRD review is ready; root accepted the technical direction before implementation.\
**Reviewers:** Root delivery lead; implementation, security, and taste reviewers.

## Context and boundaries

The [approved PRD](../prds/cli-session-inspection.md) lets a developer find yesterday's investigation, name it, inspect its cost and stop, and reopen safely. Current `Store.List` silently skips unreadable sessions, `filerecorder.New` creates/syncs storage, and `Engine.Last` assumes exclusive ownership when interpreting replay. These are unsuitable foundations for another session's read-only status.

Add scoped metadata/search/report helpers and a read-only recorder opener. Reuse Turn replay, `Spend.Add`, the existing process lock, and destination recovery. No execution facts, durable index, provider connections, child-inclusive ledger, or historical records change. This track follows attachments and active context; task inspection remains a later track.

## Read interfaces and storage access

Add `filerecorder.OpenReader(dir string) (*Reader, error)` and `Reader.Close() error`. `Reader` implements only existing `dashi.Loader` and `dashi.SessionIndex`, exposing `Load(ctx, turnID)` and `LastTurn(ctx, sessionID)`. It opens an existing directory; missing storage returns a recognizable not-found error. It performs no mkdir, sync, commit, claim, repair, or execution-ownership acquisition. It exposes neither its backing Recorder nor write methods.

Share log scanning/decoding/validation and hashed Turn paths with Recorder. Reads take existing shared log locks and honor cancellation while waiting, reading chunks, and decoding steps. Complete acknowledged lines form the snapshot; a torn final line follows existing reader semantics without truncation. `LastTurn` retains existing highest-number discovery and adds context checks. A claim-only, corrupt, or otherwise unreadable indexed Turn counts as omitted, not as an empty session. Closing releases directory handles, with no persistent cache/index writes. Recorder construction and Go consumers retain existing behavior.

In `cmd/dashi/session/inspection.go`, add:

```go
func (st *Store) Inspect(ctx context.Context, workspace, id string) (Inspection, error)
func (st *Store) Search(ctx context.Context, workspace, query string) (SearchResult, error)
```

`Inspection` contains copied metadata; indexed/readable/omitted Turn counts; latest execution and ordinary-request previews with Turn IDs/revisions; own-session Spend and model-call count; and scoped problems. `SearchResult` contains ordered matches, warnings, and `Truncated`. Each match carries copied session metadata (ID, title, update time), the latest ordinary-request preview with Turn ID/revision, and recorded-stop/next-step observations with scoped completeness problems. These card fields use the same per-session snapshot and ownership/liveness caveats as `Inspection`. Optional `{TurnID, role, preview}` evidence identifies a text match. Reports contain no live-worker assertion or executable command. CLI-owned presentation adds action guidance through the same ownership adapter used by status.

Validate IDs as the existing CLI-generated timestamp/hex single directory component. Reject traversal, absolute paths, separators, invalid encoding, and metadata IDs that differ from the directory name. Resolve the selected workspace canonically using the same launch rule and compare recorded metadata workspace before reading logs or showing title/content. Reopening uses this same validated scoped lookup. Do not widen discovery to other workspaces through an exact ID.

Open metadata and logs relative to validated store/session directory handles; reject symlinked session/metadata/turn directories and escaping log paths. Reuse attachment-track rooted-storage helpers where applicable. Bound metadata reads to 1 MiB, reporting larger or malformed files explicitly. Invalid metadata receives a sanitized directory-ID warning without exposing its title/content; its unknown workspace cannot become a result. A valid metadata/title match remains available when logs fail. Missing turns storage on saved metadata is an unavailable-record problem. An unsaved current launch is handled explicitly as having no recorded request/spend.

## Snapshots, search, and aggregation

Capture each session's highest indexed Turn once. Read each Turn through that number once, in ascending numeric order; retain its loaded revision. Concurrent appends may advance afterward, so display “recorded snapshot,” never a cross-session transaction or instantaneous total. The report's latest execution is the highest indexed Turn, including a summary. Failure there leaves its stop/destination unavailable; an older readable Turn cannot impersonate it. Walk recorded ordinary Starts to retain the latest ordinary request separately; skipped newer Turns qualify that preview as possibly incomplete.

Search matches literal substrings after Unicode case lowering, without regex/shell expansion. Trim surrounding query whitespace; preserve internal spaces. Title/ID matching does not require readable logs. Message matches traverse ordinary Start inputs, recorded user-origin steering/context, and ordinary assistant model results. Extract public text blocks only: omit reasoning, opaque replay, references, tool calls/results, system/developer messages, runtime/agent context, between-Turn summary Starts, and active-summary-purpose results. Never resolve attachments or inspect projected history. Deduplicate recorded deliveries by their existing IDs, not by equal text; identical independent user messages remain independent evidence. Text fragments of one message are joined in order, without matching across separate messages. Record the first matching message's Turn/role and a bounded preview around the match.

Sort candidate metadata by Updated descending and ID ascending; scan until 51 matching sessions are found. Return 50 and a refine-query notice when the extra match exists. Report failures encountered while scanning and warn that text matches may be missing. No total-match count is invented for unscanned sessions. Empty query and no-match/no-session/error states differ. Load one Turn at a time; keep summaries/previews and at most 51 matches, rather than full transcripts. Large individual Turns still incur existing decoder memory costs; cancellation is the escape route. No silent text cutoff hides matches.

Derive every matched session's card fields during that same scan, including title/ID-only matches. Mark unavailable request/stop facts explicitly when records fail. The UI renders the returned fields without reopening sessions or rereading Turns for individual cards.

Replay each readable Turn, adding `TurnView.Spend` once via `Spend.Add` in numeric Turn order. Include both summary kinds; omit delegate-inclusive reports. Count distinct model AttemptIDs with recorded dispatch evidence, excluding `NoDispatch`; show unresolved intents separately. This is a call count, not `ModelAttempts`, which excludes interrupted calls. Latest-Turn figures use the latest indexed Turn alone; if unreadable, show unavailable. Whole-session figures retain readable amounts plus omitted count and a separate report-completeness flag. Do not fabricate extra accounting facts/counters for omitted records.

Render model/tool money separately by sorted currency, without conversion. `CostUnknown`/`CostPartial` qualify money as a lower bound; omitted Turns also make displayed readable money a lower bound. `UsageIncomplete` qualifies retained token counts only. `Estimated`, missing cache-creation reporting, and token mismatch remain visible; reasoning/output subsets are not added twice. Omitted Turns and integer count overflow mark report counts incomplete. Usage-only failure leaves independently exact money exact. Empty readable history has zero recorded spend; unreadable history does not.

Context occupancy is a separate “latest model request” observation, labeled with Turn/Attempt/purpose. Use that call's own valid complete `Result.Usage`: sum disjoint input buckets and output once with checked arithmetic. Use the call's recorded `ModelInfo.Capabilities.ContextWindow`, capped by the same Turn's recorded positive `ActiveCompactionWindow` where present; if only the latter is known, label its recorded policy provenance. Never consult today's catalog or use session Spend. Missing/partial usage, absent window, mismatch, or unresolved intent makes occupancy unavailable; fallback usage is explicitly estimated and has no exact percentage. A summary request is labeled summary, not presented as the next ordinary request's occupancy.

## Status and recovery authority

`cmd/dashi/session_inspection.go` formats a report plus a separate `inspectionOwner` snapshot: target ID, current owned ID, controller input mode, live activity, and existing recovery assessment. Capture these under existing short mutexes; perform disk reads asynchronously without holding engine/input locks. Every heading/action names its session. State can advance after inspection; command handlers still revalidate current records.

Distinguish an actual recorded Stop from Replay's projected Stop. For an unowned session, a missing Stop means “no run stop recorded”; pending work and projected next steps are observations, with “worker liveness unknown.” A recorded interrupted Stop stays recorded interruption, without claiming the current worker is interrupted. Do not use `LastTurn.StopText`/`CanGoOn` ownership assumptions for external reports, construct an Exclusive Session, probe/take locks, or interpret a file's existence as worker death.

Only current owned idle status can offer the existing Enter, limit `/continue`, approval, `/done CALL_ID`, or `/discard` route, according to existing recovery checks and exact pending-call identity. Active/approval input modes show the live state and appropriate wait/answer/stop prerequisite. Unknown effects require verifying the exact call; permission and completion are never inferred. Other-session guidance only names reopening: `/sessions ID` while idle and choosing that ID, or exiting and `dashi --resume ID`; lock acquisition and recovery remain prerequisites. It offers no current-session action for another target.

Decode recorded destination/API/model through existing agent route-binding helpers; legacy missing identity remains unknown. Decode saved NextTurn separately and show differences. Endpoint display strips userinfo, query, and fragment and omits credential fields; malformed endpoints show unavailable. Continue requires existing recorded destination/definition checks. Inspection neither resolves credentials nor persists preferences. Reopening prepares candidate preferences, obtains the existing session lock, and switches only on success; cancellation/busy/error preserves the current session and selection. Existing damaged-log and pending-input loss safeguards remain.

## Rename and terminal integration

Add `Engine.RenameCurrent(title string) error`, called only by the idle controller. Validate trimmed printable one-line Unicode, 1–160 runes; reject control/format characters and invalid encoding. Require saved metadata and `unlock != nil` for the same current session under `e.mu`. Do not expose an ID-addressed rename command or acquire a second lock.

Clone metadata and NextTurn bytes, then save the candidate title/update time through Store's existing temporary-file/atomic-rename path. Publish the in-memory candidate only after successful rename. All failures before replacement preserve the old title on disk and in memory; cleanup removes temporary files. There is no subsequent fallible step reported as an unsuccessful rename. Retain ID/Dir/Created/settings exactly. Future `Engine.record` fills only an empty title, so a successful explicit title persists. No new metadata field/migration is needed.

Implement `/sessions [QUERY]`, `/rename TITLE`, `/status [ID]` with common report blocks across fullscreen, inline, and line UI. Search/picker and rename require idle; status works during work without steering. Focused Wonton picker labels wrap/clip previews while retaining ID/status, scroll at 80×24, and preserve selection identity independent of row number. Line mode prints IDs and resume instructions. Escape terminal controls before preview/title/endpoint/error rendering. Async request generations discard canceled/stale results; loading/cancel keeps current draft, attachment selections, preferences, and session. No typing/completion/history browsing performs disk search.

## Alternatives, costs, and delivery

A persisted full-text index would improve large-history latency but adds invalidation/migration authority beside append-only facts. Direct `filerecorder.New` avoids a small Reader type but can create/sync missing storage. Reusing `Engine.Last` for external status is concise but imports an exclusive-worker claim. The scoped scan and explicit ownership adapter avoid those costs while retaining linear scan latency and one-Turn decoder memory; concurrent reports can lag newly appended facts.

Ship additively with no metadata migration. This documentation PR stacks on remote attachments PRD #145 as a provisional dependency. Root rebases implementation onto the final reviewed attachment feature and grants shared CLI edits. Add Reader/store/report modules first, then integrate commands and guide examples. No flag is required; existing `/sessions`, `-c`, and `--resume` keep their meanings. Implementation authorization and final attachment integration remain separate gates.

Proof is keyless: fixtures verify no writes/claims/connections from search/status; scoping/traversal/symlink rejection; literal and steering matches with excluded summaries/tools/attachments; malformed/latest unreadable logs; cancellation/50-result truncation; rename rollback/restart; multi-currency/incomplete/cache/summary accounting; owner A inspecting B and externally owned B; exact unknown-call guidance; and reopening with changed defaults/busy lock. Record real focused Wonton search/name/status/reopen at 80×24 in both modes plus line transcripts. Run root and affected-module formatting, vet, build, race tests, then required named implementation/security/taste reviews. No paid-provider proof is claimed.

Title-only and text-match fixtures assert complete card fields from one scan, optional text evidence, and no per-card reopen or Turn reread. Unreadable title matches retain metadata and label unavailable request/stop facts.
