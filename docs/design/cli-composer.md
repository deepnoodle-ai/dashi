# CLI composer and command discovery

Status: Accepted · Author: Codex · Date: 2026-09-30\
Workflow: spec, orchestrator checkpoint, then build. Independent PRD review is ready.
Implementation: [#142](https://github.com/deepnoodle-ai/dashi/pull/142).

## Context and boundaries

The [approved PRD](../prds/cli-composer.md) adds persistent recall, completion, external editing, and safe idle exit. Keep Wonton as the input/runtime owner. Turn records and Engine own delivery, recovery, pending prompts, steers, and queues. Composer state holds only the editable draft, recall entries, completion presentation, and exit confirmation. No new core API, record format, CLI flag, or attachment interpretation.

Private helpers live in `cmd/dashi`: `composer.go`, `input_history.go`, `completion.go`, `external_editor.go`, and `commands.go`. `screen.go` binds them to focused input; `control.go` uses the command inventory and exit guard. `main.go` initializes history and runtime integration. Slice 1 owns viewport/dialog precedence. Integrate shared files only under the orchestrator's schedule after #103; preserve its pending Start identity, FIFO, and loss warnings.

## Input and history

Keep `InputField` with its stable input ID, multiline mode, `History`, `OnKey`, `OnComplete`, and `OnChange`. OnChange resets idle exit confirmation when draft text changes. OnKey claims composer and viewport shortcuts while preserving slice 1's dialog controls. Leave ordinary cursor movement, paste, recall, and completion cycling to Wonton. Completion uses whole-draft replacements because OnComplete supplies text without a cursor position.

Main-composer OnKey always returns false for Escape. Wonton first restores and consumes Escape during completion cycling. Without active cycling, Escape reaches `screen.HandleEvent`, which applies slice 1's running-work, selection, then transcript-bottom actions. Dialog fields retain their cancellation hooks and do not enable composer completion. Wonton v0.2.1 already supports this route: `inputState.HandleKeyEvent` returns false for unused Escape, and both Runtime and InlineApp propagate it. No completion-state mirror or new Wonton hook is needed.

Capture the raw submitted value before the Controller trims or interprets it. Append only nonblank main-composer submissions, suppress consecutive exact duplicates, and retain the newest 1,000 in memory. Persist the newest whole entries that fit both that count and 64 MiB of serialized JSON. Loading uses the same byte limit. An entry that cannot fit alone fails persistence explicitly and remains in local recall. Remove that permanent rejection from the persistence retry list so later supported submissions can save. Filesystem failures retain supported unsaved additions for retry, preserving the previous valid file. Dialog answers and internally dispatched queued inputs never append. Submission history is not a delivery acknowledgment. Resetting/reopening a session preserves the global recall list.

Store JSON `{ "entries": ["..."] }` at `~/.dashi/history.json`. Load before screen initialization. Missing files return empty history; parse/read errors return a warning and usable empty recall. Use a 0700 parent and 0600 same-directory temporary file, close then rename. Failure leaves the previous valid file intact. Never log entry contents in an error. Serialize persistence tasks in submission order and post errors to the screen event loop; event-loop rendering never mutates files.

Concurrent CLI writers serialize read-append-truncate-rename with a user-history advisory lock. Append only this process's unsaved submissions to the latest stored list, never overwrite from a stale startup snapshot. A lock/save failure keeps those submissions available in memory, warns, and permits retry at the next submission or graceful exit. This adds coordination for shared global storage without changing session locks or creating a recovery store. Corrupt-history replacement is allowed only after a parse warning; valid-file read failures never become an empty overwrite.

## Commands and completion

Use one private inventory of canonical names, aliases, usage, description, availability, and dispatch action. Register only implemented handlers. Derive command help, completion, unknown-command suggestions, and dispatch from it. UI-dependent transcript commands appear only when their implemented UI handler is available. Future slices add their real handlers here; no placeholder commands.

`/help` and `/?` also render a separate keyboard-controls section from private help entries containing key, action, focus/context, and availability. These entries are help content, not command handlers or completion candidates. Render the following controls and their conditions. Plain/print help labels them as fullscreen/inline controls; unavailable editor handoff reports its requirement.

| Keys | Action and context |
| --- | --- |
| Enter / Shift+Enter | Submit displayed main draft / insert newline, idle or running. |
| Ctrl+J | Insert newline in any terminal, including those without the Kitty keyboard protocol, which send Shift+Enter as Enter. |
| Up / Down | Move through visual rows, then recall history at the first/last row; restore the draft beyond newest history. |
| Tab / Shift+Tab / Down / Up | Tab starts completion; while cycling, Tab/Down advances and Shift+Tab/Up reverses. |
| Escape | Restore pre-completion draft while cycling; otherwise use existing running-work, selection, and transcript actions. Dialog focus retains cancellation. |
| Ctrl+G | Open external editor with idle main focus and supported handoff; while running, request an idle retry. |
| Ctrl+C | Interrupt running work; idle exit requires an unchanged repeat within two seconds. Dialog controls retain precedence. |
| Ctrl+D | Delete at cursor in nonempty main input; empty input requests exit with pending/queued-loss protection. |

Normalize aliases before dispatch: `/q` and `/exit` use `/quit`; `/?` uses `/help`. All leading slash submissions are commands in idle and running modes. Unknown names warn rather than entering Prompt or Steer. Running help/queue and implemented immediate transcript actions run now; quit cancels and lets running work finish under existing rules. Every other command reports “submit again when the turn ends.” Do not store a deferred command or claim one exists. Compaction remains a retry-only command while running. Update the guide's current promise that commands wait automatically.

Command completion preserves leading whitespace, arguments, and remaining lines. Match the first command-name token using Unicode-aware, case-insensitive exact/prefix ranking, then subsequence ranking with lexical tie-breaking. Include aliases without duplicating canonical help rows. A bare slash enumerates available names. Enter submits the displayed text, not an implicit top suggestion.

Path completion recognizes the last whitespace-delimited `@` marker and treats all following text as its fragment, including spaces. Replace that suffix with `@` plus the chosen workspace-relative path. Do not reinterpret earlier text or shell escapes. Slash-command completion takes precedence only for the command-name token. Enumerate paths without opening contents, never follow symlink directories, reject escapes/absolute fragments, and omit out-of-workspace symlink targets. Exclude `.git`, `node_modules`, `vendor`, `.venv`, and common build outputs. Apply stable exact/prefix/fuzzy ranking to relative Unicode paths. Bound candidate display to 20 and reuse a workspace index outside render calls; report incomplete enumeration rather than inventing no matches after a failure.

OnComplete returns candidate draft replacements. Wonton owns cycling and Escape restoration. Zero candidates preserve the draft and emit a bounded no-match notice. Listing/index errors report their cause. Completion never attaches bytes, changes permissions, or starts a model call.

## External editor and required Wonton contract

Ctrl+G opens an editor only with idle main-composer focus. During a running turn, consume Ctrl+G, preserve the draft exactly, and show “External editing is available after the turn ends; press Ctrl+G again then.” Do not launch a child, create a temporary file, queue an editor action, or claim one is pending. Dialog fields retain existing controls.

For idle editing, capture the draft, then schedule handoff at a runtime event-loop boundary. Use the first nonempty VISUAL/EDITOR value, otherwise `vi`. Parse executable and arguments with quoting support but no variable expansion, command substitution, globbing, pipes, or shell. Append a private temporary file containing the exact draft. The child inherits the current workspace as its directory. Prompt text and filenames are never interpolated into a command string.

The orchestrator owns the Wonton foundation contract and freezes its exported method before integration. It must:

- Quiesce the decoder and any outstanding terminal read before starting the editor. Give the child sole terminal stdin ownership and normal terminal output.
- Leave application rendering/input routing suspended while the child runs. Preserve screen mode, mouse mode, raw mode, cursor state, and the original input source.
- Wait for child termination before restarting decoding. Editor keystrokes cannot become later Dashi events. Preserve pre-handoff application events without sending them to the child.
- Restore prior terminal/input state and force a repaint on every child return, including launch error and cancellation. Reject nested or unsupported handoffs before invoking the child.
- Return callback and restoration failures separately. A restoration failure stops accepting interactive input and reports the cause on stderr; Dashi must not continue when it cannot tell who owns input.

Existing `Runtime.Suspend` retains stdin and remains the decoded-key transcript viewer API. Do not launch an editor through it, race os.Stdin, or add a private terminal reader. Qualify Wonton's Ctrl-D mapping as delete-at-cursor for nonempty composer text; at empty input the application owns exit.

After successful child exit and successful restoration, read valid UTF-8 from the file and replace the draft through the event loop. Unchanged contents preserve the original; edited empty text clears it intentionally. Nonzero exit, launch/read/validation failure, or unsupported handoff preserves the original and warns. Restore main-input focus and transcript position, then remove the temporary file on every path. Successful editing never calls Controller.Input or writes input history. A runtime restoration failure fails closed and preserves recorded work; it cannot claim the composer returned safely.

## Exit protection and failure ownership

A private exit guard tracks canonical action, confirmation time, and draft revision. Preserve slice 1's selection/dialog handling before composer exit. Running Ctrl-C remains Controller interruption and clears idle confirmation. First idle Ctrl-C emits one notice with draft presence, Engine.Queued count, pending status, and a two-second repeat instruction. Engine.Queued already includes the pending Start: do not count it twice. A second unchanged idle Ctrl-C within two seconds accepts all disclosed loss once, avoiding another keepQueued warning.

Editing, another action, expiry, or new volatile input invalidates confirmation. Ctrl-D on nonempty text reaches Wonton's delete behavior. Empty Ctrl-D and explicit quit/new/sessions retain existing repeat-action queue protection and canonicalize aliases. Never infer saved delivery from recall persistence. Keep cancellation, checks of unknown recorder writes, and session switching in their existing owners.

## Alternatives, rollout, and verification

Porting Dive's manual history/menu would duplicate Wonton state and lose visual-row draft restoration. A second composer widget could expose cursor completion but creates another input framework. A shell-based editor command supports more environment syntax but expands execution authority. Native input hooks, trailing suffix completion, and executable arguments keep the integration small. Global history needs writer coordination; bounded completion may omit lower-ranked matches and must disclose enumeration limits.

Ship after the Wonton foundation is released/pinned and shared-file integration is scheduled. No migration or feature flag. Update the CLI guide for history/privacy, completion, editor selection, exit confirmation, and retry-only command semantics.

Keyless tests must drive the focused Wonton field, not only screen.HandleEvent: multiline paste/Unicode recall/restoration, completion cycle/cancel, alias/help consistency, unknown running commands, and exit guard with compaction pending/FIFO input. In both runtimes, Escape during completion must restore the exact draft without interrupting work, clearing selection, or scrolling. With cycling inactive, test the existing application Escape actions and dialog cancellation. Help fixtures assert every control and condition above in idle/running modes, alias parity, and unsupported-editor labeling.

Running Ctrl+G fixtures use empty and multiline Unicode drafts. Assert the exact retry notice, unchanged text, and no child, temporary file, or queued action. After the turn ends, assert no automatic editor launch; a fresh Ctrl+G opens it. History fixtures cover relaunch, permissions, failed rename, corrupt load, and simultaneous writers. Editor fixtures cover argv without expansion, save/no-save/nonzero/invalid text, cleanup, and unavailable handoff.

A real terminal editor recording proves sole keyboard ownership and restored fullscreen/inline mode at 80×24. Capture empty/loading/error/success and timeout/reset exit states. Run formatter, vet, build, tests, and race tests in library and CLI modules; preserve existing queue, compaction, recovery, and transcript suites. Independent implementation/acceptance, security, and taste reviews remain required. No product decision remains open; Wonton contract qualification is a release dependency.
