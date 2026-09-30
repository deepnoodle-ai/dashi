# Read, copy, and answer in the terminal

Status: Approved\
PRD PR: [#127](https://github.com/deepnoodle-ai/dashi/pull/127) · Implementation PR: [#133](https://github.com/deepnoodle-ai/dashi/pull/133) · Updated: 2026-09-30

## Problem and context

A developer must read earlier output, inspect a proposed effect, and answer a question while using the coding agent. Dashi currently truncates tool output and approval previews. Its focused Wonton input consumes some navigation keys. Copy, selection, inline interaction, and multiselect answers are absent.

This delivers terminal rows T03–T15 and T20 in the [parity matrix](../research/cli-parity-terminal.md). Preserve existing coding, streaming, session recovery, and recorded authority. Dive supplies the baseline workflows. Wonton v0.2.1 already supplies viewport selection, clipboard support, terminal suspension, and an inline runtime.

## Concepts and use cases

**Transcript** is the conversation and tool output shown to the person. Display expansion and selection do not change recorded history. **Scrollback** is a copy shown in the terminal's own history. **Allow for this Dashi run** remembers the displayed matching rule until the process exits, including across `/new` and session switches.

### UC-1: Read and copy earlier output

While a 200-line answer streams, scroll away using wheel or Page Up. New output does not move the view. Select text by dragging, word or line clicks, and copy it. Home/End navigate the transcript when the draft is empty; Ctrl+Home/End do so with a draft. Escape returns to live output when no work or dialog needs cancellation.

Acceptance:

- [ ] Navigation works through the actual focused Wonton input and leaves the draft intact. The view shows a new-output indicator and a way to jump back.
- [ ] Drag selection auto-scrolls. Word/line selection and copy preserve Unicode and source lines. `/copy` copies the selection or lists fenced code blocks in the latest answer. `/copy N` copies one block; `/copy all` copies all those blocks without prose or fences.
- [ ] Copy uses source lines rather than wrapped cells. Cover multiple blocks, long lines, Unicode, an unfinished fence, and no available block. A newer user prompt ends the previous answer's copy window.
- [ ] `/mouse` toggles application mouse capture so native terminal selection remains available. Copy reports success or an honest fallback; it does not claim an OSC52 transport proved receipt.
- [ ] Expand and collapse a tool result without losing its full retained output. `/scrollback [raw]` shows the complete transcript, then returns with the draft and display state intact. Raw means plain transcript text, never executable terminal controls.
- [ ] Ctrl+L repaints. Narrow, empty, streaming, failed-tool, and completed states remain usable at 80×24.

### UC-2: Inspect and decide a proposed effect

Read the complete command, working directory, arguments, or diff in a bounded scrollable approval pane. Choose allow once, allow for this Dashi run, or deny. A denial may include a reason.

Acceptance:

- [ ] Long previews remain fully reachable while actions and the pending state stay visible at 80×24. Navigating or resizing cannot approve a call.
- [ ] The one-call decision refers to the original exact request. Rendering cannot alter its identity or widen policy. The remembered action shows its actual rule: all calls to the owned write/edit tool, the exact Bash command, or exact external-tool arguments.
- [ ] Test remembered file-tool permission across different paths and session switches, and Bash permission for the exact command. The UI cannot describe either rule as limited to the displayed file or current session.
- [ ] A denial reason reaches the model. Canceling the dialog does not grant permission. Interruption records the existing resumable stop and does not dispatch another effect.
- [ ] Incoming events cannot answer a newer dialog with an older response. Ending a prompt restores the prior draft and focus.

### UC-3: Answer questions

Answer free text, choose an offered option, or enter your own words. A question that explicitly permits multiple choices uses checkboxes and submits the selected labels together.

Acceptance:

- [ ] Choices and descriptions are reachable, with keyboard navigation, selection, cancellation, and an explicit submit action. Own words remain available as the existing answer contract permits.
- [ ] `workspace.Question.Multiple` defaults to false. `workspace.Answer.Selections` contains exact selected labels; `Text` contains own words instead. Existing single-choice/free-text answers keep using `Text`. Multiple mode accepts a nonempty distinct selection or nonempty own words, never both.
- [ ] The AskUser tool adds optional `multiple`. Its multiple-mode result is a JSON object with either `selections` or `text`. Labels containing commas and newlines remain distinct. Duplicate offered labels are rejected in multiple mode.
- [ ] Line mode accepts comma-separated option numbers or `other: TEXT`, rejects an empty selection, and prompts again. Print mode continues to refuse unanswered questions. Cancellation never becomes an empty submitted answer.
- [ ] A keyless example shows question `{Multiple: true, Choices: ["a,b", "c"]}`, line input `1,2`, host answer `{Selections: ["a,b", "c"]}`, and model result `{"selections":["a,b","c"]}`. An own-words alternative yields `{"text":"my answer"}`.
- [ ] Cancellation returns no fabricated answer; context cancellation promptly releases the waiting tool. Draft text is preserved around the question.

### UC-4: Use an inline terminal agent

Start with `--inline`. Finished blocks enter native scrollback; the prompt, streaming output, approvals, and questions remain interactive below them.

Acceptance:

- [ ] Use Wonton's inline runtime. Completed blocks print once; streaming updates do not duplicate completed text. Errors, interrupts, and resumption preserve the same record truth as fullscreen mode.
- [ ] Fullscreen remains the default. `--plain` remains line mode. Conflicting terminal-mode flags fail clearly. Nonterminal input/output continues to use line mode.
- [ ] Exit restores terminal modes. Inline controls preserve drafts and offer the same approval/question meanings, copy commands, and full-output inspection.

## Requirements, decisions, and limits

- Escape cancels the active dialog or interrupts running work before it changes transcript navigation. It never exits an idle session. Ctrl-C retains the existing cancellation contract; composer exit protection belongs to slice 2.
- Only an explicit user action writes a clipboard or suspends the terminal. Sanitize model/tool text for rendering and native scrollback. Do not persist display selections or infer execution from UI state.
- Reuse Wonton, the existing workspace approval policy, and recorded turn facts. Reject a replacement UI framework or independent transcript/recovery store.
- Add only the question mode needed for multiselect. Reject a general dialog framework or planning subsystem.

Composer history/completion/editor, provider settings, attachments, session search, accounting, and compaction are separate tracks. Integration follows #103 and preserves #116's footer. No permanent grants, new queue API, or database migration.

Success requires keyless workflow tests, a real Wonton key-routing fixture, independent implementation/security/taste reviews, and terminal recordings covering empty, loading, error, success, and approval/question states. Cross-terminal clipboard delivery remains a qualification limit, reported without a false success claim.
