# Compose and recall requests

Fullscreen and inline Dashi use the same composer. Enter submits the displayed
text. Shift+Enter adds a newline. Bracketed paste retains Unicode, tabs, and
line breaks without sending the request. Large text remains directly editable.
Pasted commands and aliases, such as `/status` and `/q`, stay in the composer
until Enter; they do not become file attachments. F2 enables native terminal
selection and copying without changing the draft. In fullscreen, press F2
again to restore application mouse capture. Inline already uses native selection.

## Recall and completion

Up/Down first move through visual rows. At the first or last row, they recall
submitted input. Moving beyond the newest entry restores the draft you were
writing. Editing a recalled entry does not change its stored text.

Dashi keeps up to your latest 1,000 submitted inputs across sessions and launches in
`~/.dashi/history.json`, within 64 MiB of serialized JSON. It removes oldest
whole entries to fit both limits. An entry that cannot fit by itself warns
and remains available only in local recall. Supported entries can still save.
Retained text is never truncated. Failed file writes preserve the previous file.
This includes slash commands, steers, and urgent input.
It preserves their original whitespace. Consecutive identical inputs appear
once. Empty submissions, dialog answers, and automatic queue replay are omitted.
The directory is private and the history file has mode 0600. Storage failures
warn while in-memory recall remains usable. Concurrent writers preserve each
other's additions under an advisory lock. Unsupported locks warn rather than
silently overwriting shared history.

Input history is for recall. It does not save unsent drafts or recover pending
or queued input. Recorded sessions remain the source for execution and recovery.

Type `/` and press Tab to discover available commands, including aliases and
user-invocable skills. Exact and prefix matches rank before fuzzy matches.
Tab/Down advance candidates. Shift+Tab/Up reverse them. Escape restores the
original draft. Enter submits the displayed input. Command arguments remain
unchanged. `/help` and `/?` list implemented commands, their availability, and
composer controls. Unknown slash input reports an error. Start a request with
ordinary text when referring to a path such as `/etc/hosts`. With `dashi run`, `/help`
and quit aliases run without provider work. Other slash commands require
`--plain` or interactive mode and report that they were not sent.

A trailing `@fragment` completes a workspace path, including spaces and Unicode.
For example, `Inspect @docs/req` may become `Inspect @docs/requirements.md`.
Completion inserts text only. It does not read file contents, attach files, or
grant access. Suggestions exclude `.git`, dependency/build directories, and
symlinks outside the workspace. The workspace index is built at launch. It
retains at most 50,000 names and displays at most 20 matches, with notices when
results are limited or incomplete. Relaunch to refresh newly created paths.

## Edit externally

Press Ctrl+G while idle to edit the current draft, including an empty draft.
Dashi uses `$VISUAL`, then `$EDITOR`, then `vi`. Quoted executable names and
arguments are supported. Dashi does not invoke a shell or expand environment
variables, substitutions, or globs. The editor runs in the workspace directory.

The editor receives sole terminal input and output on supported macOS/Linux
terminals. Its draft file is private and removed after return. A successful exit
loads the saved UTF-8 text without submitting it. Leaving without saving keeps
the original. Launch, exit, or read errors retain the original and show the cause.
Invalid UTF-8, NUL characters, or edited drafts larger than 16 MiB are rejected.
A terminal restoration failure stops interaction and reports the error.

During work, Ctrl+G preserves the draft and asks you to press it again after the
turn ends. It never schedules an editor action. Dialogs retain their own controls.

## Exit safely

When transcript text is selected and the main composer is focused, Ctrl-C
copies it. Escape interrupts running work even with a selection. A confirmed
local copy clears the selection; an unconfirmed terminal request preserves it.

Without selection, the first idle Ctrl-C preserves the draft and identifies
any unsent, pending, or queued input that exit would lose. Press Ctrl-C again within two seconds to
confirm. Editing, another action, expiry, or changed queued input resets the
confirmation. While work runs, Ctrl-C interrupts it as before.

Ctrl-D deletes the character at the cursor in nonempty input. At the end, it
leaves the draft intact. With empty input, Ctrl-D requests exit and retains
pending/queued-loss protection. `/q` and `/exit` are aliases of `/quit`.
