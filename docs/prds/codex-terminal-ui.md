# Read and compose comfortably in the terminal

Status: Approved\
PRD PR: [#211](https://github.com/deepnoodle-ai/dashi/pull/211) · Implementation PR: [#215](https://github.com/deepnoodle-ai/dashi/pull/215) · Updated: 2026-10-06

## Problem and context

Developers need to distinguish their prompts, agent replies, and tool activity while composing the next request. The user supplied a Codex screenshot as the visual target and reported selection, copying, and large-paste deficiencies.

Dashi already supports fullscreen and inline Wonton interfaces, source-based transcript selection, clipboard commands, multiline input, and approval dialogs. Its current display uses divider rules, bright markers, and a dense status line. Inline completion renders through a separate native-scrollback path. A real PTY audit preserved 1 MiB pastes in both modes, but ordinary large text triggered a misleading file-selection warning.

The reference uses a compact identity header, shaded prompt bands, plain assistant prose, indented tool details, and quiet help below the composer. Preserve Dashi's identity and existing interaction meanings.

## Users and job

Optimize for a developer working in a repository: submit a request, follow work, inspect results, answer an approval, and reuse output in another tool.

## Use cases and acceptance

### UC-1: Start, follow work, and continue

Start an empty session, submit a multiline request, watch a reply and tool output, then compose another request.

- [ ] A compact Dashi header identifies the application and workspace. The composer and selected model are immediately visible.
- [ ] User prompts and the composer have a continuous, restrained background across their width, including wrapped lines. Assistant prose remains visually quieter.
- [ ] Tool headers clearly show the action and running, successful, failed, denied, or waiting state. Indented details remain readable and expandable.
- [ ] The bottom area shows model, workspace, relevant work state, and discoverable help without competing with the transcript. Session identity remains available through existing inspection.
- [ ] Completed inline output and live output use the same visual hierarchy. Completed blocks print once, without duplicate streaming text.
- [ ] Empty, streaming, approval, success, interrupted, and error states remain usable at 80×24. At 40×16, input, pending decisions, and essential status remain reachable without overlap.
- [ ] Resizing preserves the draft and transcript position. NO_COLOR preserves readable hierarchy and state labels without relying on colored backgrounds.

### UC-2: Select and copy useful output

Select a wrapped answer or tool result, copy it, and paste it into another application.

- [ ] In fullscreen mode, drag, double-click, and triple-click select text, words, and logical lines. Dragging beyond the viewport scrolls selection.
- [ ] `/copy` sends the selected source text to the clipboard without role markers, display indentation, or inserted wrap breaks. Cover Unicode, tabs, Markdown, multiple blocks, and resize.
- [ ] With main-composer focus and an active selection, Ctrl-C copies selected source without changing the draft. A confirmed local clipboard write clears selection. Failed writes and unconfirmed tmux or OSC52 requests preserve it for retry.
- [ ] Without selection, Ctrl-C retains interruption and exit behavior. Dialog focus retains cancellation even with a background selection. Escape still interrupts running work.
- [ ] A visible selection hint and help explain copying and cancellation. Selection remains usable while output streams; copying never approves a request.
- [ ] Inline native selection and fullscreen `/mouse` native selection remain available for transcript and composer text. With main-composer focus, F2 toggles mouse capture without changing the draft and reports whether capture is on or off; inline reports that native selection is available. Help explains both routes. Existing `/copy N`, `/copy all`, and `/scrollback raw` retain complete source access.
- [ ] Help makes the copy route clear. A failed clipboard write offers actionable guidance. A terminal clipboard request never claims confirmed receipt.

### UC-3: Paste and edit a large prompt

Paste a large log or document into an existing draft, edit it, then explicitly submit it.

- [ ] In both modes, bracketed paste retains a payload of at least 256 KiB and 2,000 lines, including Unicode, tabs, blank lines, and shell-like text.
- [ ] Paste inserts once at the cursor and preserves surrounding draft text. Newlines do not submit prompts, execute commands, or answer dialogs.
- [ ] The composer stays within the screen. Clearing a 256 KiB line with Ctrl-U or Ctrl-K must not block subsequent input or resize. Its cursor, bottom status, and subsequent edit remain usable. After the paste ends, an appended character appears within two seconds in the keyless local PTY fixture.
- [ ] The fixture verifies the complete editable draft before submission and the complete submitted payload after existing prompt normalization. No silent truncation is permitted.
- [ ] Pasting a recognized slash command or alias inserts editable command text; it does not become an attachment or run before Enter.
- [ ] Ordinary large text does not produce file-selection warnings. Actual file drops retain existing attachment validation and explicit preview behavior.
- [ ] Question and denial fields preserve multiline paste without accidentally choosing or submitting an answer. Canceling restores the prior main draft.

## Requirements and decisions

- Keep Wonton and existing workflows. Add selected-text Ctrl-C copying because fullscreen mouse capture prevents ordinary terminal copying. Preserve explicit interruption routes.
- Keep pasted text directly editable. The measured multiline audit meets the responsiveness bound without folding; reject placeholders that need an unexplained editing workflow.
- Use the supplied screenshot as the visual reference. Do not copy Codex branding or introduce tool grouping that could hide execution facts.
- Keep fullscreen as the default and inline as opt-in. Changing the default would alter scrollback behavior beyond the request.
- Fix reproducible interaction defects within these workflows. Add focused runtime or PTY regressions where existing unit tests miss input routing or rendering behavior.
- Preserve approval descriptions, scope, explicit choices, and cancellation. Navigation, paste, copying, and visual changes cannot grant permission.
- Preserve line and JSON automation output, Turn records, prompt history, attachments, steering, queueing, completion, and editor handoff contracts.

## Scope and success

No new public API, CLI flags, transcript persistence, execution semantics, or general editor features. Framework defects may require a focused Wonton fix; identify that dependency before implementation.

Success requires independent implementation and taste review, `make check`, and before/after terminal evidence. Capture each UC-1 state, plus recordings of selection/copy and large paste in both modes. Clipboard tests capture exact payloads without overwriting the user's clipboard. Record the terminal, dimensions, payload sizes, and observed latency; do not imply every terminal emulator was qualified.
