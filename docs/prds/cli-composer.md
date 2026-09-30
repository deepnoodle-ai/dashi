# Edit prompts and discover commands

Status: Approved\
PRD PR: [#131](https://github.com/deepnoodle-ai/dashi/pull/131) · Implementation PR: Not opened · Updated: 2026-09-30

## Problem and context

A developer writes long coding prompts, reuses earlier requests, and discovers commands without leaving the terminal agent. Dashi already accepts multiline input. It loses input history at exit, has no completion or external editor, and exits immediately on idle Ctrl-C. During a turn, some command notices promise execution that never occurs.

This slice completes T16–T19, T21, and T25 in the [terminal parity matrix](../research/cli-parity-terminal.md). Dive persists interactive history and completes commands and workspace paths. Its manual history navigation does not restore the current draft, and it has no external-editor path. Wonton v0.2.1 already supplies draft-preserving history and completion through focused `InputField` hooks. [Pi](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/usage.md) and [Claude Code](https://code.claude.com/docs/en/interactive-mode) provide external editing and command discovery.

**Draft** means text in the main prompt before submission. **Input history** means earlier submitted drafts for recall. It is separate from recorded session history and never proves delivery, execution, or recovery.

## Use cases and acceptance

### UC-1: Write and reuse a prompt

Write a multiline Unicode draft. Browse earlier input with Up/Down, return to the current draft, and submit it. Relaunch Dashi and recall the submitted prompt.

- [ ] Enter submits. Shift+Enter inserts a newline. Bracketed paste preserves Unicode, tabs, and line breaks without submitting.
- [ ] Up/Down first move within wrapped or multiline text. At its first/last visual row, they browse history. Moving beyond the newest entry restores the original draft exactly.
- [ ] Recall and editing never rewrite stored entries. History records submitted main-prompt text, including slash commands, ordinary steers, and urgent input. It excludes empty submissions, dialog answers, and automatic queue replay.
- [ ] History preserves submitted draft text, including whitespace and newlines. Existing prompt/command interpretation remains unchanged. Consecutive identical entries appear once.
- [ ] New and resumed sessions share up to the user's latest 1,000 input entries within 64 MiB of serialized storage. Retention removes oldest whole entries, preserving retained text exactly. Missing storage starts empty. Load/save failures show a warning and keep composing usable.
- [ ] History files are private to the user. A failed write preserves the previous valid file. An entry that cannot fit by itself warns and remains available in memory. History neither stores unsent drafts nor recovers pending or queued input.

### UC-2: Discover and complete available input

Type `/`, then Tab, to discover commands. Narrow a command name, or type a trailing `@` workspace path fragment and complete it.

- [ ] `/help` and `/?` list each currently implemented command, its aliases, arguments, and whether it runs during a turn. They include themselves and the composer keyboard controls. Commands from unfinished slices are absent.
- [ ] Slash completion matches the leading command name. Exact and prefix matches rank before fuzzy matches. Candidate order is stable. Completion does not change arguments or other draft text.
- [ ] Tab accepts one candidate or starts cycling several candidates. Tab/Shift+Tab and Down/Up cycle. Escape restores the original draft. Enter submits the displayed input. No match preserves the draft and reports that no completion exists.
- [ ] Workspace completion replaces only a trailing `@fragment`, preserving the rest of the draft. It supports Unicode paths and spaces. Suggestions stay within the workspace and exclude `.git` and generated dependency trees. Completion never reads file contents, attaches bytes, or grants access.
- [ ] History navigation cannot accidentally invoke a recalled command or start completion. Completion takes navigation precedence only while actively cycling.
- [ ] Unknown slash commands report an error with relevant available candidates or `/help`. They never become prompts or steers. Unsupported known commands during a turn request resubmission after it ends. No notice promises unqueued execution.
- [ ] `/q` and `/exit` behave as `/quit`; `/?` behaves as `/help`, including while a turn runs. `/new` continues to preserve the previous session.

### UC-3: Edit a longer draft externally

While idle, press Ctrl+G. Edit the draft in a terminal editor. Return to Dashi and review the edited text before submitting.

- [ ] The editor opens the current draft, including an empty draft, using `$VISUAL`, then `$EDITOR`, then `vi`. Editor arguments are supported without shell expansion or execution of prompt text.
- [ ] Successful editor exit replaces the draft with the saved UTF-8 text. Exit without saving retains the original text. Nonzero exit, launch/read failure, or invalid edited text retains the original draft and shows the cause. No return path submits automatically.
- [ ] Terminal modes, focus, transcript position, and draft editing return correctly after success, cancellation, or failure. Temporary draft files are private and removed after return.
- [ ] Ctrl+G during a turn preserves the draft and says to press Ctrl+G again after the turn ends. It never queues an editor action. Dialog input uses the existing dialog controls.
- [ ] A real terminal editor receives sole keyboard ownership. This requires the Wonton handoff dependency below; simulated or noninteractive editing cannot close this acceptance criterion.

### UC-4: Exit without losing a draft accidentally

Press Ctrl-C while idle with a draft. Read the warning, continue editing, or confirm exit.

- [ ] The first idle Ctrl-C preserves the draft and shows “press Ctrl-C again within 2 seconds to exit.” The second idle Ctrl-C within that interval exits. Editing or another action resets confirmation.
- [ ] When draft, pending prompts, or queued input would be lost, the first notice identifies all affected memory-only input. Confirmation accepts that loss once. Input history is never described as recovery storage.
- [ ] Ctrl-D deletes the character at the cursor in nonempty input; at the end it leaves the draft intact. With empty input it requests exit and retains existing pending/queued-loss protection. Explicit exit commands retain that protection too.
- [ ] Running-turn Ctrl-C retains interruption and resumption semantics. It does not arm idle exit. Main-composer Escape first cancels active completion. Otherwise, approval, question, selection, and transcript Escape controls retain slice 1's precedence and meaning.

## Decisions, boundaries, and dependency

Use Wonton's `InputField.History`, `OnKey`, and `OnComplete`. Native in-buffer cycling finishes discovery without another menu framework. Use Ctrl+G for external editing so Ctrl+E remains line-end editing. Keep user-wide history, matching Dive's recall workflow, with bounded retention. Reject using turn records as an input-history cache.

Path completion inserts text only. Slice 6 owns attachment interpretation, bytes, chips, and attachment-specific completion. Exclude configurable keymaps, reverse history search, draft autosave, `/clear`, new queue APIs, and changes to viewport/dialog behavior. Plain/print modes gain truthful command dispatch and aliases; interactive composition applies to fullscreen and slice 1's inline mode.

The orchestrator owns a Wonton terminal/input handoff foundation change before composer integration. Current `Runtime.Suspend` retains stdin and routes decoded events; it remains valid for transcript viewing, but cannot safely launch an interactive editor. Do not race stdin or add a private reader. The technical spec must pin sole input ownership and restoration with slice 1, and qualify Wonton's Ctrl-D/delete mapping. Other composer helpers can proceed independently. Shared `screen.go`/`control.go` integration follows compaction #103 and preserves pending-input identities, ordering, and warnings.

Success requires keyless workflows through actual focused Wonton input, an example/transcript for command dispatch, and terminal recordings at 80×24. Cover empty draft/history, active work, no completion, history/editor failure, editor cancel/save, and exit confirmation. Preserve existing recovery, transcript, compaction, steering, and queue tests. The independent PRD review found no blocking findings; implementation, acceptance, security, and taste reviews precede completion.
