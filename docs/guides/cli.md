# The dashi terminal coding agent

`dashi` is a terminal coding agent built on this library. It reads and edits
files in your repository and runs shell commands, with your approval. The
source is in `cmd/dashi`. It is a separate Go module, so the library gains no
terminal dependencies. The design is in [design/cli.md](../../design/cli.md).

## Install and run

```sh
cd cmd/dashi
go install .
export OPENAI_API_KEY=...
cd ~/your/repo
dashi
```

Use `dashi models --available` to list configured destinations and exact model
IDs. Choose a pair with `--provider ID --model NAME`, or use `/model` for the
next turn. With no saved selection, Dashi detects configured vendor keys; its
fallback is OpenAI's `gpt-6-luna`. See [providers and next-turn settings](cli-providers.md)
for registration, precedence, generation controls, environment files, and trust.
`--workspace DIR` selects an existing workspace without changing your shell's
directory.

## What you can do

Type a request, such as "why does TestParse fail?". The screen shows the
answer as it streams. Each tool call shows one line with its name, its main
argument, and its state. Paths show relative to the repository. After a shell
command, the screen shows the `git diff`. At the end of a turn, a footer shows
the model, the number of model calls, the tokens, and the cost.

While a turn runs, type to send a steer. The model reads it at its next step.
Start the line with `!` to cut the model call in progress. A steer that
arrives too late for the turn is not lost: it runs as the next turn. Type
`/queue text` to hold a prompt for when the turn ends; it then runs as the
next turn. Press Ctrl-C to stop the model call or the tool. The turn stays
open, and Enter continues it; queued prompts wait and go ahead of your next
prompt, and the screen says how many wait. Queued prompts live only while
dashi runs, so `/quit`, Ctrl-C, Ctrl-D, `/new`, and `/sessions` first warn that
they would drop them; repeat the action to go ahead.

The CLI summarizes history before new work when the last model call uses
80% of the selected model's context window. `/compact` requests a summary
while idle, without sending queued prompts. The transcript shows compaction
states and its usage footer; it hides the summary request and text. Unknown
windows use 128,000 tokens and warn once per selected model.

Input submitted during compaction waits in order, including `!text`. The
urgent prefix cannot interrupt a summary; use Ctrl-C. If automatic compaction
is interrupted before your prompt starts, the screen keeps your original
prompt pending. Enter continues the summary, then sends that prompt once.
Previously queued input goes before it; later submitted lines go after it.
Pending and queued input live only in memory. Exit or session changes warn
before discarding them. Recorded summaries can be reopened and continued.

## Commands

| Input | Result |
| --- | --- |
| `/copy [N|all]` | Copy selection, list code blocks, or copy their original source |
| `/scrollback [raw]` | Inspect complete output in native terminal scrollback |
| `/mouse` | Toggle application mouse capture for native selection |
| `/compact` | Summarize history while idle; leave queued input waiting |
| `/model [destination model\|model]` | Pick or set a destination/model for the next turn |
| `/settings [--json]` | Inspect safe next-turn settings and a preparation preview |
| `/effort LEVEL\|omit` | Set or omit next-turn reasoning effort |
| `/thinking on\|off` | Request and display public summaries for new turns |
| `/temperature N\|omit` | Set or omit next-turn temperature |
| `/max-tokens N\|default` | Set the next-turn output limit |
| `/new` | Start a new session |
| `/sessions` | List the sessions of this directory and reopen one |
| `/continue` | Go on after a model-call limit stop (Enter does the same) |
| `/queue text` | Hold a prompt for when the running turn ends |
| `/done` | Mark a command done that was running when dashi stopped |
| `/discard` | Discard the open turn |
| `/quit` | Save and exit (Ctrl-D does the same) |
| `/help` | Show the commands |

Display commands work during a turn. Mutating commands may request that you retry after the turn ends.

## Sessions

Each session is a folder in `~/.dashi/sessions`. It holds one record for each
turn, and a `session.json` with the title, directory, and next-turn preferences. The
turns are a [`dashi.Session`](sessions.md). A session is saved when its first
turn records, so a launch that you close at once leaves nothing behind.

- `dashi -c` (or `--resume`) reopens the newest session of this directory
  that has a turn. It shows the full scrollback.
- `dashi --resume ID` reopens one session.
- If the last turn did not finish, the screen says so. Press Enter to
  continue it, or type a new prompt to cancel it.
- If dashi exited while a command ran, the command may still be running:
  a new prompt is refused until you check it. Type `/done` once it has
  finished, or `/discard` to discard the turn; the record keeps the cancel.
- A second `dashi` cannot open a session that is open in another terminal.

`dashi -p "prompt"` runs one turn without a screen and prints only the answer
to stdout. Steps go to stderr. The session is saved. Use `dashi -c -p "..."`
to add a turn to the last session, for example
`dashi -c -p "what did you change?"`.

## Read and copy output

Page Up/Down and the wheel scroll without following new output. End returns
an empty draft to live output; Ctrl+Home/End navigate with a draft. The
indicator shows lines below. Drag selects; double/triple clicks select a word
or original logical line, including its whitespace and Markdown syntax. Copying
selection excludes role labels, display indentation, and wrapped row breaks.
Escape clears a selection or returns to live output when idle.
Ctrl+L repaints. Click a tool header to expand its full retained output.

`/copy` copies selected text or lists fenced blocks in the latest answer.
`/copy N` copies one code block; `/copy all` copies all, without fences or
prose. Code copying retains original source lines, even when wrapped on screen.
A newer prompt ends the previous answer's copy window. OSC52 and tmux
terminal requests report unconfirmed receipt; local clipboard process success
is reported separately. `/scrollback raw` provides complete printable text.
Enter/Escape return from inspection with the draft intact.

Start `dashi --inline` to keep completed output in native scrollback and
interactive streaming, prompts, and dialogs below it. `--plain` and `--inline`
cannot be combined. Nonterminal input/output still uses line mode.

## Approvals

The tools that read files run at once. A tool that changes files or runs a
command asks first. The bounded pane shows the full command, directory,
arguments, and diff. Arrow/Page keys scroll the details; decisions stay visible.

| Key | Result |
| --- | --- |
| `y` | Allow this call once |
| `a` | Allow for this Dashi run; the displayed rule survives session switches |
| `n` | Enter an optional denial reason, then press Enter |
| Esc | Interrupt without granting permission; Enter can resume |

Remembered rules cover all calls to the owned write/edit tool, the exact Bash
command, or exact external-tool arguments. They last until the process exits;
they are not permanent grants.

Questions support choices or own words. Multiple questions use checkboxes:
arrows move, Space selects, Tab moves to Submit, and Enter submits. Press `o`
for own words. Escape interrupts without submitting an empty answer. Dialogs
restore the draft when they end.

If you quit during a prompt, the call does not run. After `dashi -c`, press
Enter to continue the turn. The prompt shows again.

Text from the model is shown safely. Control characters, such as ESC and
carriage return, appear as visible marks.

In `-p` mode nobody can answer, so every call that needs approval is refused.
Add `--yes` to approve all of them. Use `--yes` only in a repository that you
can restore.

## Line mode

Line mode runs when stdin or stdout is not a terminal, or with `--plain`. It
prints plain text and reads approval answers as lines.

For multiple-choice questions, line mode accepts `1,2` or `other: YOUR ANSWER`
and reprompts on invalid input. `n REASON` supplies an approval denial reason.
