# Terminal transcript and dialogs

Status: Accepted · Author: Codex · Date: 2026-09-30\
Workflow: Spec before implementation; incorporate independent requirements review before building.

## Context and boundaries

The [PRD](../prds/cli-transcript.md) restores the useful Dive terminal workflows without a separate execution store. Existing `Transcript` blocks remain the display projection. Turn facts remain the authority. Wonton v0.2.1 supplies the viewport, selection, focused input hooks, fullscreen runtime, and inline runtime.

New helpers live in `cmd/dashi`: transcript actions/copy parsing, clipboard transport, bounded dialog views, and inline runtime adaptation. Screen integration follows #103. Do not edit accounting's `FooterText` or retain competing compaction logic. No new module or database format.

## Transcript interaction

Keep expansion state keyed by a block's stable identity or transcript index, reset on a transcript reset. `screen.Item` selects expanded rendering; the underlying block retains complete text. The renderer must stop discarding tool result lines before this point. Default collapsed output stays concise and shows how many lines are hidden. Invalidated block sizes update the Wonton viewport without forcing follow mode.

Route viewport shortcuts from `InputField.OnKey` before Wonton consumes them. Return false for ordinary editing keys. Empty-draft Home/End and Ctrl+Home/End navigate; Page Up/Down always navigate outside a dialog. Handle Escape in this order: active dialog, running work, active selection, transcript bottom. Ctrl+L invalidates the terminal from the event loop. Mouse handlers use Wonton hit testing and selection APIs; tool-header clicks toggle expansion without confusing a completed drag with a click. Selected output is copied only by an explicit user gesture or command. Disable capture for native selection with `/mouse`.

Display the viewport's away-from-bottom/new-output state without making incoming blocks jump the position. Test Unicode selection and anchoring with the runtime, not merely `screen.key` calls.

`/copy` uses current selection, otherwise lists fenced blocks in the last assistant answer after the most recent user input. `/copy N` and `/copy all` extract original source. Adapt Dive's small literal fence parser, preserving longer nested fences and an unfinished streamed block. Format lists as ordinary notices; invalid indices and empty results produce useful messages. Copy cannot dispatch a model command.

Clipboard transport returns an outcome that distinguishes native/tmux process success from an unconfirmed OSC52 request. Use Wonton OSC52 encoding/limit. Bound native/tmux commands by context and pipe-wait time. Feed source through stdin, never a shell. Write OSC52 on the terminal-owning loop to avoid interleaving frames. Oversized/failed copies point to native scrollback rather than claiming success.

Fullscreen `/scrollback` uses `Runtime.Suspend` on the event loop and its decoded key channel. Print the complete projected transcript with printable text, keep the draft untouched, and return on Enter/Escape. `raw` skips Markdown styling; it never bypasses terminal-control sanitization. Restore errors become notices or a clean exit if raw input cannot be restored. Events received from workers while suspended remain queued and drain after return.

## Approval and question boundaries

Each prompt receives a generation ID. End/answer events reference it, so a canceled old prompt cannot clear or answer a replacement. The screen stores the draft separately while a dialog is active. Reply channels are bounded and receive at most one response. Context cancellation releases a wait and restores focus without treating cancellation as an empty answer.

Use a dialog-specific viewport rather than growing the footer beyond the terminal. Content has the full printable request and choice descriptions; actions remain outside its scroll region. No navigation key is an implicit allow. Denial opens a reason input with an explicit submit/cancel decision. Escape interrupts the pending call through the existing controller cancellation path.

Populate printable `ApprovalRequest.Arguments` from raw JSON without retaining raw Args in the display copy. It has no authority. Add `ApprovalRequest.RememberScope` as optional descriptive text produced by `workspace.Policy`: owned writes/edits cover their tool, Bash covers its exact command, and external tools cover their exact arguments. The CLI labels the action `allow for this Dashi run` because its policy instance survives session changes. Other hosts still own their policy lifetime. Do not change `ruleKey`, remembered rules, or grant persistence. Tests show the displayed scope agrees with actual matching.

Extend `workspace.Question` with `Multiple bool`; extend `workspace.Answer` with `Selections []string`. Both zero values preserve existing callers. The AskUser schema gains optional `multiple`. Multiple-mode validation requires distinct offered labels and exactly one of nonempty Selections or nonempty Text. Selected labels must belong to the offered choices and may not repeat. Invalid host answers become an explicit failed tool outcome. The model-visible result is JSON with `selections` or `text`; ordinary single/free-text results retain the existing plain text. No change to tool authority.

Propagate structured answers through the private CLI UI/controller/wiring signatures. Fullscreen and inline use the same question state: arrows focus options, Space toggles in multiple mode, Enter submits a nonempty selection, and an own-words alternative replaces selection. Line mode accepts `1,2` or `other: TEXT` and reprompts on invalid input. Print mode retains its existing refusal. A public Example pins the additive contract and JSON result.

## Inline runtime

Use `tui.InlineApp`, not fullscreen with the alternate screen disabled. A small screen adapter shares event handling and footer/dialog views. It prints a prefix of completed blocks once, in transcript order; the unfinished suffix remains in `LiveView`. Record-derived reopened history prints once. When an unfinished block changes, redraw the live suffix; do not print every delta as a new completed message. Reset starts a new display segment without deleting records or reprinting old blocks.

Inline native scrollback supplies ordinary terminal navigation/selection. Full inspection and `/scrollback` print bounded plain or styled transcript output while retaining the draft; the return prompt consumes its own key. Shared copy commands still use original source. Inline dialogs use the same bounded content and action semantics. Mouse capture stays off by default in inline mode. Ended/resumed compaction display follows #103's origin markers.

`--inline` and `--plain` are mutually exclusive. Fullscreen remains default on terminals. Nonterminal streams keep line mode. Both runtimes restore terminal state and cancel/wait for active work on exit. Tests inject runtime events and fake clients; no provider credentials or real clipboard writes.

## Alternatives and consequences

- Porting Dive's whole App would also port its execution reconstruction. Reuse focused UI helpers and preserve Dashi Engine/Controller instead.
- Disabling alternate screen avoids another runtime but does not deliver native once-only scrollback. Use Wonton InlineApp and accept a small display adapter.
- Keeping text-only multiselect avoids an additive API but loses exact selected labels and own-words distinction. Add the two minimal fields instead of a general dialog protocol.
- Changing approval policy to exact-file/session rules would make the label simpler but change authority and recovery expectations. Disclose the existing policy instead.

## Verification and rollout

Run root and CLI formatting, vet, build, and race tests. Public examples cover structured questions; keyless runtime fixtures cover focused navigation, stream anchoring, source copy, stale dialog cancellation, full previews, selection, line answers, and once-only inline completion/reopen. Fake clipboard tools cover timeout/fallback/OSC52 limits. Policy tests cover different file paths, session changes, and exact Bash commands. Existing recovery, approvals, queue, and compaction tests must pass.

Capture actual Wonton empty/loading/error/success/approval/question frames and an interactive recording outside git. Independent general, security, and taste reviews inspect the final implementation. Ship additive library fields and opt-in inline mode; no data migration. Document clipboard receipt and cross-terminal qualification limits. No unresolved architectural question blocks a reviewed implementation.
