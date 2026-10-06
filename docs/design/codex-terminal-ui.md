# Terminal presentation and input reliability

Status: Accepted · Author: Codex · Date: 2026-10-06\
PRD: [approved in #211](https://github.com/deepnoodle-ai/dashi/pull/211).

## Context and boundaries

The [PRD](../prds/codex-terminal-ui.md) specifies a quieter terminal hierarchy and reliable copying and composition. Keep Wonton responsible for terminal decoding, focus, cursor movement, and rendering. Dashi remains responsible for presentation, attachment recognition, and application controls. No change affects Turn records, approval authority, automation output, or session persistence.

The independent audit reproduced three defects: selected-text Ctrl-C invokes cancellation, pasted `/status` becomes a file drop, and ordinary large text emits a file-selection warning. A 64 KiB single-line paste also exposes quadratic Ctrl-U deletion in Wonton. Multiline 1 MiB pastes retain content in both modes; the subsequent edit appears within 0.4 seconds on the audit machine.

## Presentation

Keep the existing `Block` transcript and its source offsets. Move view helpers from `screen.go` into a focused private presentation file before parallel work. This isolates appearance from event routing without changing the model.

Use a compact Dashi/workspace header, full-width shaded user prompts and composer, neutral assistant markers, indented tool details, and restrained state colors. Keep explicit state words for waiting, failure, and denial. Use the existing muted foreground without dim; reserve color for emphasis. Render shaded multiline text inside a bounded panel so shorter rows retain the band without filling other viewport rows.

Retain the model, workspace, and useful work state in the bottom area. Session identity remains available through `/status`. Replace composer divider rules with padding and a background. Keep help discoverable and contextual selection/cancellation instructions visible. Approval descriptions and choices retain priority over decoration.

Fullscreen and inline share block/composer styles. Native completed output retains its source-preserving Markdown route, with matching markers and indentation. Use the actual terminal width when available rather than forcing 80 columns. Bound composer and live-output heights from available rows; dialogs keep choices outside their scrolling content. Small terminals drop optional metadata before obscuring input or decisions.

NO_COLOR suppresses terminal colors without losing labels, markers, spacing, or permission choices. The visual change does not alter `FormatBlock`, line-mode output, or JSON/JSONL contracts.

## Selection and clipboard

Handle Ctrl-C selection copying before ordinary interruption only when main-composer focus is active. Dialogs, pickers, and inspection inputs retain their existing cancellation priority. With no selection, existing Ctrl-C behavior remains unchanged. Escape retains immediate running-work interruption.

Factor the existing `/copy` clipboard dispatch so the shortcut uses the same exact source text without submitting or replacing the draft. Return a structured private clipboard result distinguishing a confirmed local write from an unconfirmed terminal request. Do not infer confirmation from human-facing notice text. Local process success permits selection clearing; tmux and OSC52 retain selection with an honest retry notice.

Associate asynchronous copy results with the selection that started them. A stale completion must not clear a newer selection or one in another session. Copy failures preserve selection and report the existing native-scrollback fallback. Clipboard tests inject a writer and compare exact payloads without modifying the user's clipboard.

F2 toggles fullscreen mouse capture directly with main-composer focus and preserves the draft. Reuse `/mouse` behavior and report capture on/off. Inline reports that native selection is already available. This provides native draft copying without inventing a second editor selection model.

## Paste and editing

Keep pasted content directly editable; do not enable blanket multiline placeholders. In the main input paste hook, preserve recognized slash commands and aliases as editable text before absolute-path drop recognition. Pasting never invokes command dispatch. Keep actual path-drop validation and attachment preview behavior.

Large ordinary text bypasses the file-drop heuristic without the misleading 64 KiB warning. Do not claim an unavailable undo action. Keep Wonton's existing newline normalization and retain all remaining content, including Unicode, tabs, and blank lines. Test insertion at an interior cursor, explicit submission, history, dialogs, and resize through actual focused input.

The Ctrl-U/K defect belongs to Wonton's private text-input editor: it deletes each grapheme separately while repeatedly scanning or copying the line. Bulk deletion must use one range removal, preserve grapheme boundaries and placeholder segments, and emit one change notification. The same audit reproduced the defect in Ctrl-W word deletion; the upstream fix covers that route too. Dashi's published InputField hook has no cursor access, so a local workaround must not mirror or guess cursor state.

The dependency audit found no public bulk-edit API and no fix in Wonton v0.7.1. The independently reviewed [upstream fix](https://github.com/deepnoodle-ai/wonton/pull/79) now ships in [Wonton v0.7.2](https://github.com/deepnoodle-ai/wonton/releases/tag/v0.7.2). All four Dashi modules consume that published version. Final PTY qualification uses the published module without workspace replacements. No module-cache edits, vendored editor, or `go.mod` replacements are needed.

## Alternatives and consequences

Replacing Wonton would duplicate focus and editor behavior. Folding every multiline paste would improve rendering but obscure editing and leave large single-line deletion unfixed. Native selection alone would simplify copying but remove existing application selection and tool interaction. Keep both selection routes and repair the narrow defects.

The copy shortcut adds focus-dependent Ctrl-C behavior. Visible hints, confirmation-based clearing, and explicit Escape interruption make that distinction discoverable. An upstream editing fix may delay delivery, but cursor mirroring would create lasting correctness risk.

## Verification and rollout

Use deterministic clients and injected clipboard writers. Capture before/after empty, streaming, approval, completed, interrupted, and error states at 80×24 and 40×16. Capture NO_COLOR and resize behavior, plus interactive selection/copy and paste recordings in both modes.

Runtime tests cover selection source fidelity, clipboard failure/unconfirmed receipt, stale completion, dialog cancellation, F2 draft preservation, slash-command paste, ordinary large paste, and approval safety. PTY tests verify 256 KiB/2,000-line insertion and complete submission, subsequent edit latency, large single-line deletion, and terminal restoration. Record measurements separately from universal performance claims.

Run `make check`, focused race tests, and independent implementation/acceptance and taste review. Update CLI guides and the changelog. No migration, new flag, or mode-default change is required.
