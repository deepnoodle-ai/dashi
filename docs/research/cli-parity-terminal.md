# Terminal workflow coverage

The baseline is Dashi `95ec475`, Dive `da15e91` (v1.34.0, experimental CLI 0.1.0), and Wonton v0.2.1. Inspection date: 2026-09-29. **Supported** means implemented CLI behavior. **Partial** means a narrower path. **Missing** means no CLI path, even when a library package exists. These labels do not establish live-provider or cross-terminal qualification.

**V** names files in [Dive's pinned CLI](https://github.com/deepnoodle-ai/dive/tree/da15e916148fcc93f248c08648d22dfab1a4b73a/experimental/cmd/dive). **D** names files in [Dashi's CLI](../../cmd/dashi/). Functions provide the code evidence. Slice numbers refer to the [umbrella PRD](../prds/cli-parity.md). The Dashi CLI and session keyless tests pass at this baseline.

| ID | Dive workflow | Dashi baseline | Evidence and disposition |
|---|---|---|---|
| T01 | Interactive coding, streaming Markdown | Supported | V `app.go:runAgent`, `render.go`; D `engine.go:run`, `screen.go:blockView`. |
| T02 | Read/list/glob/grep, write/edit, Bash | Supported | V `main.go:createTools`; D `wire.go:workspaceWiring`, library `workspace.Tools`. |
| T03 | Exact command/diff approval, allow once/session, deny | Partial | V `main.go:tuiDialog.showConfirm`; D `screen.go:approvalView`, `workspace.Policy`. D preview truncates, denial cannot collect custom feedback. 1. |
| T04 | Ask text, select, multiselect, “other” | Partial | V `main.go:tuiDialog.Show`; D `screen.go:askView/submit` returns text only. 1. |
| T05 | Fullscreen and inline terminal modes | Partial | V `app.go:Run`, `main.go:--inline`; D fullscreen or `--plain` line loop. Interactive inline absent. 1. |
| T06 | PgUp/PgDn, Home/End, follow output | Partial | V `screen.go:handleScreenKey`; D `screen.go:key`. Home/End consumed by focused Wonton input. 1. |
| T07 | Wheel scroll, new-lines jump, Escape to bottom | Missing | V `screen.go:handleScreenMouse/handleScreenClick`, `app.go:handleKeyEvent`; D no mouse handler. 1. |
| T08 | Drag, double-word/triple-line selection, auto-scroll | Missing | V `screen.go:handleScreenMouse`; D does not call Wonton selection APIs. 1. |
| T09 | Native/tmux/OSC52 clipboard, honest fallback | Missing | V `clipboard.go:newClipboardCopier`; D no clipboard path. 1. |
| T10 | `/copy [N\|all]`, fenced source without wrapping | Missing | V `copy.go:handleCopyCommand`; D `control.go:command` has no copy. 1. |
| T11 | `/mouse`, terminal-native selection escape | Missing | V `screen.go:toggleMouse`; D no mouse control. 1. |
| T12 | `/scrollback [raw]`, return without losing draft | Missing | V `scrollback.go:handleScrollbackCommand`; D dumps transcript only on exit. 1. |
| T13 | Expand/collapse tool results and reports | Missing | V `screen.go:handleScreenClick`; D `screen.go:toolView` has fixed displayed lines. 1. |
| T14 | Transcript in terminal after exit | Supported | V `screen.go:printExitTranscript`; D `main.go:runScreen`, `screen.go:dumpTranscript`. |
| T15 | Ctrl+L repaint | Missing | V `app.go:handleKeyEvent`; D no repaint binding. 1. |
| T16 | Multiline input, Shift+Enter, bracketed paste | Supported | V `app.go:inputAreaView`; D `screen.go:inputView`, `main.go:WithBracketedPaste`. Terminal qualification needed. |
| T17 | Up/Down input history, draft restoration | Partial | V `history.go`, `app.go:handleInputNavKey`; D `InputField.History` with in-memory entries. 2. |
| T18 | Persist input history across launches | Missing | V `history.go`, `main.go:defaultInputHistoryStore`; D history never saved. 2. |
| T19 | Slash/file fuzzy autocomplete | Missing | V `app.go:updateAutocomplete/getFileMatches`; D no completion binding. 2/6. |
| T20 | Interrupt running work, Escape cancel | Partial | V `app.go:handleKeyEvent`; D `control.go:Interrupt` supports Ctrl-C, not Escape. Preserve resumability. 1. |
| T21 | Double Ctrl-C exit and draft protection | Partial | V `app.go:handleKeyEvent`; D idle Ctrl-C exits immediately except queued-work warning. 2. |
| T22 | `--resume` picker, title/ID/workspace filter | Partial | V `session_picker.go:RunSessionPicker`; D `-c` latest, exact `--resume ID`, `/sessions` unfiltered directory picker. 7. |
| T23 | Resume prior history, no empty session rows | Supported | V `session_picker.go:nonEmptySessions`, `screen.go:appendSessionHistory`; D `session.Store.List`, `Engine.Scrollback`. |
| T24 | `/clear` new conversation | Supported replacement | V `app.go:handleCommand` deletes old session. D `/new` preserves it. Keep D behavior. |
| T25 | `/quit`, `/exit`, `/q`, `/help`, `/?` | Partial | V `app.go:handleCommand`; D `control.go:command` lacks short aliases and command inventory omits `/help`. 2. |

## Required replacements

T24 uses `/new` to clear the active conversation without deleting the previous session. Command aliases may preserve Dive spelling, but must retain this behavior. Dashi's durable recovery, locked sessions, damaged-log warnings, steers, queued inputs, and exit transcript remain supported. Tests must exercise focused Wonton input, because consumed keys do not reach `screen.key`.
