# One-shot input and recorded automation results

Status: Accepted · Author: Codex · Date: 2026-09-30\
Workflow: Spec, root checkpoint, then build. Named CI/spend clarity review returned ready. Root accepted it.\
Requirements: [Run one coding request from a script](../prds/cli-automation.md)

## Shape and ownership

Private CLI helpers select input, execute through Engine, and project record snapshots into a versioned public result.
Keep the Turn journal, Session, recorder fencing, and task host authoritative. Add no record format or root API.
New files: `automation_input.go`, `automation_report.go`, `automation_output.go`, and `ui_automation.go`.
Root schedules `main.go`, `options.go`, `ui_line.go`, and `engine.go`; app/wire integration is conditional on the preceding task host.
No screen/composer changes belong here. Update the CLI guide and add keyless examples/transcripts with the implementation.

## Input route before application startup

Add `--output-format text|json|jsonl`, exact lowercase, default text. Retain whether the flag was explicit.
Parse conflicts before environment loading, destination lookup, tool construction, or session creation.
Explicit format conflicts with `--plain`, `--inline`, and the separate `models` listing. CLI `--help` remains human usage and starts nothing.
Do not read terminal stdin, even when stdout is redirected.

| Route | Input and behavior |
| --- | --- |
| `-p` or explicit format | One-shot. Read redirected stdin once, then combine it with the optional prompt. |
| Nonterminal stdin, no `--plain` | One-shot text, including stdin alone. |
| `--plain`, no `-p` or format | Preserve existing line/command/approval input. |
| Remaining terminal routes | Preserve fullscreen/inline or terminal-input line fallback. |

Read at most `16 MiB + 1` before admission. Reject overflow, NUL, invalid UTF-8, or read error with code 2 and no shortened request.
Read in bounded chunks and check cancellation. The process adapter owns pipe stdin and closes it on cancellation to unblock reading.
Never close terminal stdin. Inject cancellable pipes and finite readers in tests; retain no blocked reader after the invocation.
Join nonempty parts as `prompt + "\n\n" + stdin`; preserve every byte of each part. Reject whitespace-only input unless resuming without a payload.
Attachment parsing receives only explicit prompt/flags through preceding snapshot helpers. Append literal stdin afterwards, before the one Start.
Raw stdin never enters command dispatch, attachment mention parsing, or approval input.

Check slash-prefixed explicit `-p` separately. Empty stdin permits existing local help/quit aliases and other-command rejection.
Nonempty stdin rejects this mixed command/data invocation. Command results contain no Turn IDs and bypass live application setup.
An explicitly recognized machine format emits structured preflight errors; argument parse failures or `--help` may produce no machine result.

## Record-only open, then optional execution

For resume, validate the session ID and canonical workspace using the inspection track's scoped loader.
Acquire the existing exclusive session lock before deciding readiness. Preserve one lease across reporting and any subsequent live open.
Load existing logs through `filerecorder` and `dashi.Replay`, not Engine.Open/buildApp or a new log parser.
Require the record directory exists before constructing the reader; do not create missing records, edit metadata, claim a worker, or repair damaged logs.
Read ownership excludes another CLI driver. Recorder revision/owner checks still fence independent writers.

Derive the original request from the latest root user Start, treating empty legacy origin as user.
Scheduled task-notice and compaction Starts never impersonate that request. Follow recorded context/notice IDs and Parent links, not display text.
Traverse task-creating calls using recorded definitions and `delegate.ChildID`; load children from the parent recorder and verify their Start.Parent.
Derive stable TaskID only after a child Start exists. An unsettled creation with no child Start remains the parent call's unknown admission.
Read receipt identities from parent context items and queue Start command IDs using the task track's existing notice logic.
Do not call Tasks.Recover, BindServices, resource discovery, credential lookup, MCP startup, or content resolvers to build a snapshot.

Without payload, return completed/blocked snapshots offline when no permitted ready work needs execution.
Completed children with missing recorded notice delivery are unfinished invocation work; report that prerequisite or drive it only through live admission.
Return code 1 for unreadable required latest/child records, retaining readable facts with omitted-count/incomplete evidence.
Never substitute an earlier readable stop for a corrupt latest Turn.

Only a ready/interrupted continuation or validated new request enters live setup. Preserve the claimed lease when Engine adopts the session.
Recheck selected record revisions and existing recovery/attachment/destination bindings before dispatch.
Unavailable live bindings affect that execution target; they cannot invalidate already-readable completed snapshots.
Continue with plain `dashi.Continue{}` only when Next/readiness permit and the stop is ready/interrupted.
Do not use Engine.Resume's attempt-limit doubling, retryable-failure heuristic, Settle, discard, budget raise, or silent closure of unfinished work.
New payload blocks on unresolved parent/child admission rather than letting Engine.Prompt close it.
Retain the exact pending Start/CommandID on unknown acknowledgement. Reload establishes admission before any repeat submission.

## Public version 1 projection

JSON/JSONL serialize a fixed CLI run report, independent of the task. See the PRD's trimmed examples and CI field mapping.
Optional `structured_output` passes through an already-recorded typed model answer. This slice adds no CLI task-schema selection.
`exit_code` describes Dashi execution, not build/test success or diagnosis correctness.
Use tagged private DTOs and copying functions, never direct Turn/Outcome/Event/Block/Agent/tool/model encoding.
Version 1 permits additive fields. Incompatible meaning requires a new version.
Stable IDs order arrays. Integers remain JSON numbers; currency amounts remain canonical decimal strings.

| Object | Version 1 fields and meaning |
| --- | --- |
| Result envelope | `version:1`, `type:"result"`, `mode:"execution"\|"report"\|"command"\|"error"`, `exit_code`, `session_id?`, `request_turn_id?`, `answer_turn_id?`, `answer`, `structured_output?`, `quiet`, `turns`, `tasks`, `affected`, `totals`, `persistence`, `error?`, `warnings`. Absent identities mean unavailable, never invented. |
| Turn snapshot | `turn_id`, `revision`, `origin`, `parent?` (Turn/call IDs), `destination` (recorded ID/API/model), `stop` (kind/reason/terminal/projected/ref/limit), `answer`, `structured_output?`, `public_reasoning?`, `pending`, `model_attempts`, `spend`, `required_action`. Values cover the full replayed Turn, not streaming increments. |
| Task snapshot | Recorded `task_id`, `session_id`, current `turn_id`, parent Turn/call, subagent, actual state, notice-delivery state, child Turn IDs, and their snapshot links. No truncated TaskView answer substitutes for a loaded Turn answer. |
| Affected target | Turn/task/call IDs, stop, rank, and structured action (`continue`, `approve`, `answer`, `reconcile`, `new_request`, or `inspect`), with prerequisites. `primary:true` marks one deterministic target; retain every other affected target. Actions describe remedies, never authorize them. |
| Spend | Explicit usage buckets/maps and all completeness flags, `usage_state` (complete/estimated/incomplete/unavailable), `usage_incomplete`, `estimated`, decimal-string `model_cost`/`tool_cost` currency maps, `cost_unknown`, `cost_partial`, `unpriced_requests`, plus omitted-record evidence. Retained zero counts are not complete measurements when state is partial. |
| Totals | Covered Turn IDs/revisions, invocation `parent`/`children`/`inclusive`, and separate cumulative session-own/children/inclusive scopes. These are whole-record snapshots, including earlier facts on resumed Turns, not incremental invoices. |
| Persistence/error | Acknowledged/rejected/unknown command outcome, affected target/known revision, stable error code, sanitized diagnostic. Preserve CommitError classification separately from the record's actual stop. Uncertain output delivery does not rewrite persistence. |

Invocation coverage includes its original/continued request, its admitted compaction/notice Turns, and reachable children.
Session totals include every readable owned parent Turn and each verified child Turn once. Sort IDs before `Spend.Add` for stable overflow retention.
`/status` keeps `Inspection` own-session spend, excluding children. Task reporting supplies parent/child/inclusive scopes through `delegate.Report`.
Automation projects invocation and cumulative own/child/inclusive totals from those records. `/status` and ledger ownership remain unchanged.
Parent/inclusive totals never sum each other. Use replayed Spend and `delegate.Report` semantics, never add deltas, previews, or TaskView plus child snapshots.
Summary-purpose model attempts are already in their owning Turn's Spend. Between-Turn compaction remains a distinct parent Turn.
Preserve usage-only incompleteness independently from exact money. Include omitted records as explicit missing contributions, never exact zero.
Top-level answer comes from the last permitted completed ordinary/notice answer in this invocation, with its own ID; never from a summary attempt.
Default results omit reasoning. Explicit existing visibility permits public reasoning summaries only, in a separate labeled field.
Exclude system prompts, attachment bytes, tool args/output dumps, private reasoning, replay artifacts, full prepared requests, and configuration/environment credentials.
Project endpoint identity from credential-free recorded routing fields. Diagnostics redact credential-bearing URLs/headers without consulting live bindings.
JSON encoding escapes controls. Terminal stderr/text use `workspace.PrintableText`; public answer/structured data retain their content semantics.

## JSONL writing, signals, and exits

JSON writes one envelope followed by LF. JSONL writes typed `progress`, optional `gap`, then one identical `result` envelope.
Each progress object has version, monotonically increasing invocation-local order, target IDs, recorded anchor sequence, event kind, and provisional flag.
Allow bounded public text deltas, call/task lifecycle names, and status/accounting previews. No raw event payload enters output.
Apply the allowlist synchronously to Engine events and task OnChild events; child deltas never join the parent's answer.
Callbacks do not block or retain source facts. Bound queued progress to 64 entries/256 KiB, each text chunk at 4 KiB UTF-8.
Coalesce/drop progress under pressure and emit a gap with targets and dropped count. Final snapshots supersede previews, including corrections.
One writer owns stdout. Reserve final-result delivery outside the progress queue; JSON mode need not observe deltas.
For process pipes use cancellable writes with a 10-second inactivity deadline, renewed when bytes advance.
Regular files use checked synchronous writes. Refuse unsupported uninterruptible pipe configurations before model/tool execution.
The final object may exceed the progress limit; serialize/write it once and propagate encoding, write, and flush failures.
No failed/partial stdout write is retried as another result. Report known saved session/uncertainty on printable stderr.

Register SIGINT/SIGTERM before reading. Remember the first signal and cancel execution/readers.
Close the task host through its accepted bounded shutdown path, settle/interrupt owned work where possible, then reload the latest facts.
Retain unknown shutdown/persistence state explicitly. Emit a final snapshot only if stdout remains usable; release the session lease after shutdown.
Code priority: first signal (130/143), output/persistence failure (1), then preflight error (2) or ranked affected work.
Recorded ranks (highest first): 1 failed, 2 waiting/uncertain/interrupted needing action, 3 limited, 4 refused, 5 canceled.
Ranks select the primary target. The respective exit codes remain 1, 3, 4, 4, and 4.
Equal rank prefers original parent, then lexical notice/child Turn/task/call IDs. Historical unrelated session stops are not invocation exit candidates.
Code 0 requires completed invocation work, quiet tasks/notices, and successful final delivery. Preserve all stops even when one rank wins.

## Accepted task integration and proof

Use the accepted preceding `docs/design/cli-tasks.md`: `delegate.Tasks` drives children; Engine alone drives parent commands/notices/queue Turns.
Keep Config.Session nil, NoticeReady, the bounded pending-notice map, original-call approval broker, and Tasks.Run/TaskTarget fencing.
Automation adds no second RunUntilQuiet caller. Extend private CLI Result with RequestTurnID, captured from its pending Start's acknowledged CommandID.
Engine's task-host quiet path preserves it while Outcome advances to the last answer. Resume initializes it from the loaded original request.
Quiet requires children/report callbacks, pending notices, pending Start, and parent queue all clear.
`delegate.AttentionError` contributes every withheld task as affected work; reload those child Turns for exact stops/limits, then stop remaining live children.
Without payload, recover only permitted ready/interrupted child work; first scan attention/unknown effects before Tasks.Recover can dispatch.
Explicit --yes retains existing original-call approval policy. No stdin line or report remedy becomes an approval/settlement.

Keyless acceptance covers exact multiline/slash/mention input, no-admission failures, offline completed reports with invalid live bindings/storage, framing/gaps/slow and broken readers, every exit/tie, and parent-completed failed/waiting children.
Pin mixed limited/refused/canceled targets against opposing parent/ID order: limit wins, then refusal, with code 4 and all stops/actions retained.
Interrupt/reopen fixtures count one write and notice, stable targets/routes, no implicit raises, and one authoritative spend contribution.
Include incomplete usage/exact money, missing cache reporting, multicurrency, unreadable records, active/between-Turn compaction, and child notice costs.
PRD fixture: USD 1/2/4 yields `/status` own 3, task/automation invocation 2/4/6, and automation cumulative 3/4/7.
Duplicate references/offline reports count the verified child Turn once. Complete zero tool money stays separate.
Run affected formatting/vet/build/race gates after code; independent implementation/acceptance, security, and taste reviews remain required.

## Alternatives and checkpoint

Internal events expose hidden requests. Blocks lose record identity/completeness. Offline projection avoids current tools/credentials.
Bounded progress can drop previews. Final snapshots load full covered records. Explicit --plain preserves scripted line workflows without a record migration.
No RPC, daemon, transcript repair, credential ingestion, new grant flags, automatic remedy, or family budget is introduced.
Base: attachment PRD #145 at `f2256f9ac2c8b522958b3669f8130307456efcce`, including merged inspection/task PRDs #146/#148. Rebase onto the reviewed task feature before shared CLI integration and final gates. Root schedules their settled private seams.
The accepted technical checkpoint retains prerequisite integration, writer qualification, and implementation authorization gates.
