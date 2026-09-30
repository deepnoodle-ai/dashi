# Attachments and operational workflow coverage

The baseline is Dashi `95ec475`, Dive `da15e91` (v1.34.0, experimental CLI 0.1.0), and Wonton v0.2.1. Inspection date: 2026-09-29. **Supported** means implemented CLI behavior. **Partial** means a narrower path. **Missing** means no CLI path, even when a library package exists. These labels do not establish live-provider or cross-terminal qualification.

**V** names files in [Dive's pinned CLI](https://github.com/deepnoodle-ai/dive/tree/da15e916148fcc93f248c08648d22dfab1a4b73a/experimental/cmd/dive). **D** names files in [Dashi's CLI](../../cmd/dashi/). Functions provide the code evidence. Slice numbers refer to the [umbrella PRD](../prds/cli-parity.md). The Dashi CLI and session keyless tests pass at this baseline.

| ID | Dive workflow | Dashi baseline | Evidence and disposition |
|---|---|---|---|
| O01 | `@file` text/image/PDF/video/document references | Missing | V `app.go:expandFileReferences`, `attachments.go:mediaContentFor`; D prompt is text. `content/mention` has text/image/PDF only. 6. |
| O02 | Dropped/absolute quoted paths, chips, prune drafts | Missing | V `app.go:captureDroppedFiles/pruneAttachments`; D no attachment draft state. 6. |
| O03 | Print positional/stdin prompt, text answer | Partial | V `main.go:getInput/runPrint`; D `-p PROMPT` and sequential piped line mode. No whole-stdin one-shot composition. 8. |
| O04 | Print JSON output, errors and usage | Missing | V `main.go:runPrintJSON`; D `printAnswer` writes text, exits nonzero for stops. Add record-derived JSON/JSONL. 8. |
| O05 | Print tool approval bypass | Supported replacement | V print auto-approves without hook. D denies unless `--yes`. Keep explicit authorization. |
| O06 | Turn/session usage, cache breakdown, context %, `/status` | Partial | V `app.go:usageReportView`, `cmd_settings.go`; D `render.go:FooterText` has per-turn count/cost. 7 after #116/#103. |
| O07 | Ended-turn manual, between-turn automatic, and active-turn compaction | Missing | V `app.go:handleCompactCommand/checkAndPerformCompaction`; `main.go` registers `compaction.MidTurnCompactionHook` in PreIteration. D does not configure Compactor. #103 covers only ended/between-turn work. Active-turn work needs a separate foundation track. |
| O08 | Subagents Explore/Plan/GeneralPurpose and task stop | Missing | V `main.go:agentFactory`, orchestration tools; D no delegate wiring. Library `delegate.Tasks` exists. 9. |
| O09 | Native background results and monitor notifications | Missing | V `app.go:runBgContinuation/runMonitorNotification`; D no monitors or task host. 9/10. |
| O10 | Web fetch, keyed Kagi/Google search, Grok hosted search | Missing | V `main.go:createTools`, `providers.go:providerServerToolset`; D workspace tools only. 11. |
| O11 | `image`: generate/edit/ref/count/aspect/format/save/open | Missing | V `cmd_image.go:runImage`; D has no media commands. Library `media` exists. 12. |
| O12 | `video`: duration/aspect/poll/save/open | Missing | V `cmd_video.go:runVideo`; D has no media commands. 12. |
| O13 | Agent image/video generation tools | Missing | V `main.go:createTools`; D no media adapter. 12. |
| O14 | Named model-only/operator reminders, `/context` | Missing | V `context_reminders.go`, `context_demo_ui.go`; D runtime ContextItem foundation only. Expose named context with origin/lifetime inspection. 13. |
| O15 | `context-demos`: workspace/pipeline/verification/recovery/security | Missing | V `context_demo_catalog.go`, `context_demo_*.go`; Replace presets with resource inspection, explicit skills, and keyless examples. See the replacement contract below. 13. |
| O16 | `/todos`, `/t` list toggle | Missing dormant producer | V `app.go:parseTodoWriteInput` exists, but `main.go:createTools` registers no TodoWrite. Do not count as usable planning parity. |

## Runtime context replacement

O15 replaces the bundled `context-demos` presets deliberately:

| Dive preset | Dashi replacement |
|---|---|
| Workspace | Explicit inspection shows the selected workspace and tools. A runnable example demonstrates a named runtime workspace snapshot. |
| Pipeline and verification | User-invoked skills read build/CI surfaces and request approved checks. Inspection shows observed outcomes and remaining checks. |
| Recovery | Recorded stops, pending calls, and next actions explain continuation. Examples demonstrate interrupted and uncertain turns without transcript repair. |
| Security | Actual approval policy controls effects. Skills can ask for a security review, but cannot authorize tools or declare a check passed. |

O14 supplies named operator and runtime context through existing ContextItem origins and lifetimes. `/context` shows loaded resources and effective named context with provenance. Summaries do not impersonate retained context messages. Changed values append under the prompt-prefix policy.

O05 preserves explicit `--yes` for unattended approvals. O16 is display code with no registered `TodoWrite` producer. Exclude a planning subsystem from parity. O07 has two acceptance parts. Ended-turn manual and between-turn automatic compaction belong to [#103](https://github.com/deepnoodle-ai/dashi/issues/103). Active-turn context management remains unresolved for a later foundation track. Dive implements it in [`MidTurnCompactionHook`](https://github.com/deepnoodle-ai/dive/blob/da15e916148fcc93f248c08648d22dfab1a4b73a/experimental/compaction/mid_turn.go). A long-running tool-loop fixture must stay within the request context budget while preserving full recorded history and recovery facts. Failure/cancellation must stop clearly without repeating effects. #103 alone does not close O07. O06 accounting belongs to [#116](https://github.com/deepnoodle-ai/dashi/issues/116).
