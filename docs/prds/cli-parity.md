# A Dashi terminal agent with Dive workflow parity

Status: Approved\
PRD PR: [#125](https://github.com/deepnoodle-ai/dashi/pull/125) · Implementation PR: Separate slice PRs · Updated: 2026-09-29

## Problem and baseline

A developer can code and resume recorded work with Dashi, but cannot bring their full Dive terminal workflow. Dashi wires workspace tools and OpenAI Responses only. Its provider controls, MCP, skills, attachments, terminal copy, tasks, and media workflows remain incomplete.

The user needs one terminal agent for coding, tools, approvals, sessions, and provider work. Preserve Dashi's durable Turn/Session foundation while completing the useful Dive workflows. The [terminal](../research/cli-parity-terminal.md), [integration](../research/cli-parity-integrations.md), and [operational](../research/cli-parity-operations.md) matrices cover all 55 discovered rows with code evidence and intentional replacements. They use Dashi `95ec475`, Dive `da15e91`, and Wonton v0.2.1. Dormant TodoWrite display code is explicitly excluded.

## Requirements and delivery

- R-1: Every matrix row finishes with implemented acceptance evidence or its specified intentional replacement. Planned gaps prevent full umbrella completion.
- R-2: Wonton remains the UI framework. Turn records remain the authority for history, recovery, execution, and accounting. CLI preferences, resource bytes, and display state cannot substitute for recorded facts.
- R-3: Keep recorded-model continuation, uncertain-call handling, session locks, damaged-log warnings, and steering/queue behavior. Clearing the active conversation preserves the old session.
- R-4: Tool, instruction, attachment, and provider integrations retain origin, authority, credential, and prompt-prefix boundaries. Model-controlled terminal text remains printable. Approval remains explicit in unattended runs. No blanket approval or permanent grant is introduced.
- R-5: Each slice needs reviewed requirements before public feature implementation. Each slice then needs a technical spec, independent implementation/acceptance review, evidence, and documentation.

| Wave | Ranked slices and outcome |
|---|---|
| First | **1** `cli-transcript`: transcript/dialogs. **2** `cli-composer`: composer/discovery. **3** `cli-providers`: providers/generation. **4** `cli-resources`: instructions/skills. **5** `cli-mcp`: MCP wiring. |
| Next | **6** Attachments. **7** Session search/naming and inspection. **8** Response-only JSON/JSONL automation. **9** Durable task control. |
| Later | **10** Output monitors. **11** Web tools. **12** Media commands/tools. **13** Explicit context/resource inspection, skills, and examples. **14** Remote MCP compatibility. |

This umbrella defines program requirements, not a combined implementation track or PR. Slice specs define the architecture; there is no umbrella implementation or spec. Composer is an L track because it changes UI/CLI contracts.

The first wave completes when all five slices pass their reviewed acceptance criteria. Independent reviews must have no blocking findings. Evidence and docs must match the integrated CLI. Existing coding, recovery, approval, and queue tests must pass. Completing this wave does not establish full parity.

## Use cases and acceptance groups

- **A — Read and interact (T01–T25; slices 1/2/7).** Managed-screen and interactive-scrollback modes remain available. A 200-line streamed fixture remains readable while scrolled away. Focused input permits navigation, source-preserving copy, full approvals, and selection/multiselect dialogs at 80×24. Suspend/return and external editing preserve drafts. Relaunch restores input history. Completion discovers commands. Session search/name/pick preserves workspace boundaries.
- **B — Select a model (I01–I08; slice 3).** Keyless fixtures cover Dive's configured provider routes, local/compatible destinations, settings precedence, and next-turn switches. Invalid models fail clearly. Effective settings and provenance are inspectable. Resume retains the recorded destination. Unsupported controls warn or fail before dispatch according to the existing capability policy. Credential values do not enter records or diagnostics.
- **C — Load resources (I09–I14; slices 4/5/14).** Startup lists instruction, skill, and tool sources and effective scopes without secrets. Skills substitute arguments without shell execution. Supporting-file read access stays explicit. MCP fixtures prove project approval, fingerprint changes, failure isolation, qualified names, timeouts, inspection, and cleanup. Third-party read-only claims do not bypass policy. Remote auth/transport gaps remain visible until their foundation dependency lands.
- **D — Supply and automate work (O01–O06; slices 6/7/8).** Mentions/drops show removable attachments and bounded warnings. Reopened sessions retain immutable bytes. Multiline stdin plus a prompt produces one turn. Response-only JSON and complete-message JSONL stdout parse without notices. Exact process exits and printable stderr carry failures/attention. Inspection and task surfaces retain recorded IDs, stops, destination, and usage completeness. Interrupt/resume repeats no recorded effect.
- **E — Delegate and generate (O08–O13; slices 9–12).** Fixtures show stable task IDs, approvals, stop/status, and one completion notice across reopen. Monitors deliver bounded recorded notices. Web results show sources. Fake media jobs demonstrate references, outputs, cancellation, polling, and explicit viewer opening. Aggregate spend counts once. Paid qualification requires a separately authorized acceptance matrix.
- **F — Inspect context (O07/O14–O16; slice 13 and dependencies).** Inspection shows resource/context provenance and recorded next actions. Explicit skills/examples replace the five scenario presets as specified in the operational matrix. Unchanged context preserves prefixes. Ended/between-turn compaction and active-turn context management retain full history. A long tool-loop fixture stays within request context limits; failure/cancellation stops clearly without repeated effects. No reminder grants permission or invents verification success.

## Dependencies and ownership

[#103](https://github.com/deepnoodle-ai/dashi/issues/103) owns ended-turn manual and between-turn automatic Session/CLI compaction; it is implementing. O07 also requires a later active-turn context foundation track. Its contract remains unresolved. Keep O07 incomplete until active-turn long-loop and recovery acceptance pass; #103 cannot close it.

[#116](https://github.com/deepnoodle-ai/dashi/issues/116) implements the accounting contract approved in merged [#123](https://github.com/deepnoodle-ai/dashi/pull/123). Its [PRD UC-2](spend-usage-accounting.md#uc-2-store-reopen-and-report) and [report spec](../design/spend-usage-accounting.md#compatibility-and-reports) own `Spend.UsageIncomplete` and minimal completeness warnings in `Spend.String`, CLI `FooterText`, and delegate summaries. Acceptance requires usage-only overflow to disclose `usage incomplete` without qualifying independently exact money. The footer must replace an affected token total with the warning. Formatter [#118](https://github.com/deepnoodle-ai/dashi/pull/118) already supplies the cost summary; #116 updates it for the new accounting distinction.

O06's richer turn/session usage, cache breakdown, context percentage, and `/status` belong to slice 7. Slice 9 owns task-report presentation. Their fixtures must match replayed accounting, disclose incomplete usage, and preserve separate model/tool money. Slices 7/9 consume #123's contract and #103's compaction facts without copying foundation logic. Neither dependency alone closes O06.

Slice 8 owns O03/O04 whole-stdin requests, response-only JSON, and complete-message JSONL. Exact process exits and printable stderr carry errors/attention. The [response-only requirements](cli-automation.md) supersede earlier slice 8 execution-report and usage schemas. Slice 8 does not complete O06 or export inspection/accounting fields.

Each slice owns its new feature modules. Shared integration edits need one scheduled owner: slice 1 owns viewport/dialog changes, 2 owns composer/dispatch, 3 owns client/settings controls, 4 owns instruction/skill wiring, and 5 owns MCP configuration/lifecycle. Screen/composer integration follows #103. Provider, resource, and MCP helpers can proceed independently. Their shared-file integration uses one owner at a time. Schedule `screen.go`, `control.go`, `wire.go`, `app.go`, and `engine.go` edits with #103/#116. SQLite migration, queue API extensions, forks, and remote MCP transport/auth are separate foundation dependencies.

## Decisions and limits

- Use `/new` instead of Dive's destructive `/clear`. Reject deletion of completed records to clear a screen.
- Use configured provider identities and explicit environment-file loading. Reject prefix routing, silent model fallback, and automatic project credential ingestion.
- Read workspace AGENTS.md, otherwise CLAUDE.md. Show the selected source. Skills and instructions do not widen write roots or grant authority.
- Adopt external editing, loaded-resource inventory, session search/naming, and JSONL. These have bounded user outcomes. [Codex editor](https://learn.chatgpt.com/docs/cli-customization), [Codex automation](https://learn.chatgpt.com/docs/non-interactive-mode), [Pi sessions](https://github.com/badlogic/pi-mono/blob/1b347794e2a630e4359f2584f4eea388145d0ddf/packages/coding-agent/docs/sessions.md), [Claude controls](https://code.claude.com/docs/en/interactive-mode).
- During a running turn, immediate commands run only when supported. Other commands clearly request a retry. Reject messages that promise automatic execution without a queued command.
- Replace scenario presets with explicit inspection, skills, and examples. Reject bundled coaching theater, shell expansion in skills, app-local transcript repair, and dormant TodoWrite parity.

Branching/rewind, RPC, plugin marketplaces, themes, provider subscription sign-in, and cloud handoff need separate proposals. Rewind does not reliably undo shell or child effects. [Claude checkpoint limits](https://code.claude.com/docs/en/checkpointing).

Success means a reviewer can account for every matrix row and reproduce each acceptance group with keyless fixtures and Wonton recordings. Code-backed support labels alone do not prove live-provider or cross-terminal parity. No open question blocks the umbrella requirements review.
