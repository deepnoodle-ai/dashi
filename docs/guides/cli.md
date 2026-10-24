# The dashi terminal coding agent

`dashi` is a terminal coding agent built on this library. It reads and edits
files in your repository and runs shell commands, with your approval. The
source is in `cmd/dashi`. It is a separate Go module, so the library gains no
terminal dependencies. The design is in [design/cli.md](../../design/cli.md).

## Install and run

```sh
brew install deepnoodle-ai/tap/dashi
export OPENAI_API_KEY=...
cd ~/your/repo
dashi
```

Each release also attaches binaries for macOS, Linux, and Windows, and
`go install github.com/deepnoodle-ai/dashi/cmd/dashi@latest` builds it with
Go. From a checkout, `go install ./cmd/dashi` builds the working tree instead;
the repository's `go.work` points it at the local library modules.

The command line has three commands; `dashi --help` and `dashi COMMAND --help`
describe every flag, and `dashi completion bash|zsh|fish` prints a completion
script.

```text
dashi [flags]               start an interactive session
dashi run [flags] [prompt]  send one request and print the reply
dashi models [--available]  list destinations and their exact model IDs
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
reply as it streams. Each tool call shows one line with its name, its main
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
20% of the selected model's context window, or the token count that
`--compaction-threshold N` (or `DASHI_COMPACTION_THRESHOLD`) names. A
threshold above the model's window counts as the window, with a warning.
`/compact` requests a summary while idle, without sending queued prompts. The transcript shows compaction
states and its usage footer; it hides the summary request and text. If a
summary fails, the history is shortened without one, and the CLI warns once.
Unknown windows use 200,000 tokens for this between-Turn history workflow
and warn once per selected model.

Active-Turn protection checks text-only requests against their recorded model window;
`/settings` shows that window. Media requests show one “protection unavailable for media”
marker per session, including after reopen.
Provider content preparation and actual window acceptance govern these requests; a known
window does not prove media fit. Cost limits still apply. See
[active context](compaction.md#protect-one-long-request).

Input submitted during compaction waits in order, including `!text`. The
urgent prefix cannot interrupt a summary; use Ctrl-C. If automatic compaction
is interrupted before your prompt starts, the screen keeps your original
prompt pending. Enter continues the summary, then sends that prompt once.
Previously queued input goes before it; later submitted lines go after it.
Pending and queued input live only in memory. Exit or session changes warn
before discarding them. Recorded summaries can be reopened and continued.

Compose multiline requests, recall input across launches, complete commands
and workspace paths, or press Ctrl+G to edit a draft externally. Idle Ctrl-C
requires an unchanged repeat within two seconds. See [composer controls](cli-composer.md)
for keyboard behavior, private history, editor selection, and exit protection.

Attach a screenshot, PDF, or text file with `/attach PATH` or `@path`.
Explicit mentions show a preview on the first Enter and send on the second.
Use repeatable `--attach PATH` with `dashi run`. See [attachments](attachments.md) for
selection limits, failure recovery, and immutable snapshots.

## Commands

| Input | Result |
| --- | --- |
| `/copy [N|all]` | Copy selection, list code blocks, or copy their original source |
| `/scrollback [raw]` | Inspect complete output in native terminal scrollback |
| `/mouse` | Toggle application mouse capture for native selection |
| `/attach PATH` | Select a file for the next idle request |
| `/attachments` | Inspect numbered selections |
| `/detach N` | Remove a selection and its mention spans |
| `/compact` | Summarize history while idle; leave queued input waiting |
| `/model [destination model\|model]` | Pick or set a destination/model for the next turn |
| `/settings [--json]` | Inspect safe next-turn settings and a preparation preview |
| `/resources` | Inspect selected instructions, skills, and tool access scopes |
| `/mcp` | Inspect MCP server sources, status, and exposed tools |
| `/effort LEVEL\|omit` | Set or omit next-turn reasoning effort |
| `/thinking on\|off` | Request and display public summaries for new turns |
| `/temperature N\|omit` | Set or omit next-turn temperature |
| `/max-tokens N\|default` | Set the next-turn output limit |
| `/new` | Start a new session |
| `/sessions [QUERY]` | Find recorded workspace sessions and reopen one |
| `/rename TITLE` | Name the current saved session while idle |
| `/status [ID]` | Inspect recorded stop, destination, and own-session spend |
| `/continue` | Go on after a model-call limit stop (Enter does the same) |
| `/queue text` | Hold a prompt for when the running turn ends |
| `/done [CALL_ID]` | After verifying its outcome, mark the current unresolved command done |
| `/discard` | Discard the open turn |
| `/quit`, `/q`, `/exit` | Save and exit (Ctrl-D requests exit with empty input) |
| `/help`, `/?` | Show available commands, aliases, and composer controls |

See [find and inspect sessions](cli-sessions.md) for search, names, recorded accounting,
and targeted recovery guidance. Status is read-only and also works during a Turn.

Help, queueing, resource inspection, and implemented transcript actions work
during a turn. Other commands require explicit resubmission after it ends.
Dashi does not queue command execution from a retry notice.

Workspace AGENTS.md takes precedence over CLAUDE.md. Project and home skills
support `/skill arguments` and progressive model loading. See
[instructions and skills](cli-resources.md) for discovery, approval, and recovery.

Configure MCP servers in user settings, workspace `.mcp.json`, or repeatable
`--mcp-config FILE` selections. Project servers require startup consent before
work; each tool call still uses normal approval. `/mcp` and `/resources` show
the same safe inventory. See [MCP tools](cli-mcp.md) for trust, HTTP,
unsupported authentication, and cleanup.

## Sessions

Each session is a folder in `~/.dashi/sessions`. It holds one record for each
turn, and a `session.json` with the title, directory, and next-turn preferences. The
turns are a [`session.Session`](sessions.md). A session is saved when its first
turn records, so an attachment-free launch that you close at once leaves
nothing behind. Attachment publication can retain snapshots and a discovery
header before Start. That header is not proof of a recorded request or completed
work; ordinary session lists still require a Turn.

- `dashi -c` (or `--continue`) reopens the newest session of this directory
  that has a turn. It shows the full scrollback.
- `dashi -r ID` (or `--resume ID`) reopens one session.
- If the last turn did not finish, the screen says so. Press Enter to
  continue it, or type a new prompt to cancel it.
- If dashi exited while a command ran, the command may still be running:
  a new prompt is refused until you check it. Type `/done` once it has
  finished, or `/discard` to discard the turn; the record keeps the cancel.
- A second `dashi` cannot open a session that is open in another terminal.

`dashi run "prompt"` runs one request and prints its last parent reply.
After `compaction_failed` or `context_window`, `dashi run -c "prompt"`
summarizes older history before the new request. Pending tools, approvals,
and unfinished child tasks still block it.
Redirected stdin supplies one literal payload. `--output-format
text|json|jsonl` (`-o`) selects the output format. See [script input, response formats,
resuming, and exit codes](cli-automation.md), including a CI example that keeps
the build's exit status.

## Drive dashi from another program

A program that types prompts into a dashi terminal, such as a terminal
multiplexer, uses two flags of the interactive session. They work in the
full screen, `--inline`, and `--plain`. `dashi run` takes neither.

- `--session-id UUID` starts a new session with that ID, so the program knows
  it before dashi starts. Letters may be either case; dashi uses the
  lowercase form. An ID that already names a session is refused. Reopen
  that session with `--resume UUID`. `--session-id` cannot be combined with
  `-c` or `-r`.
- `--turn-events PATH` appends one JSON line to `PATH` (mode 0600) for each
  event. dashi sets an existing file to mode 0600 and refuses a symlink or a
  file that is not a regular file. Redirected input without `--plain` makes
  one request, so dashi refuses the flag there; add `--plain`. The lines
  use the field names of Claude Code hook inputs, so a reader of those hooks
  reads dashi too:

| `hook_event_name` | When | Other fields |
|---|---|---|
| `UserPromptSubmit` | a turn starts, or text reaches the running turn as a steer | `prompt_id` (the turn ID), `prompt` |
| `Notification` | a tool call waits for approval, or the model asks you a question | `notification_type` (`permission_prompt`), `message` |
| `Stop` | a turn completes | `prompt_id`, `last_assistant_message` |
| `StopFailure` | a turn stops any other way, or dashi refuses the prompt | `prompt_id` (empty for a refusal), `error_type` (the stop kind, such as `failed` or `interrupted`), `error_message` |

Every line also has `session_id` and `cwd`. dashi writes and fsyncs the end
of a turn before it takes the next prompt. Text fields keep their first 32
KiB. A compaction turn writes nothing, and nothing marks the end of a wait:
a child task's approval answered while dashi is idle is followed by no
record until the next turn. Escape interrupts a turn in the full screen; in
`--plain`, Ctrl-C does. Typed `@word` is a file mention, so a sender that
means a name writes `\@word`.

## Read and copy output

In fullscreen, Page Up/Down and the wheel scroll without following new output.
End returns an empty draft to live output; Ctrl+Home/End navigate with a draft. The
indicator shows lines below. Drag selects; double/triple clicks select a word
or original logical line, including its whitespace and Markdown syntax. Copying
selection excludes role labels, display indentation, and wrapped row breaks.
With the main composer focused, Ctrl+C copies selected source without changing
the draft. A confirmed local clipboard write clears the selection; failed or
unconfirmed writes keep it for retry. Without selection, Ctrl+C interrupts work
or uses the idle exit confirmation. Dialogs keep their cancellation controls.
Escape interrupts running work, or clears a selection when idle.
Ctrl+L repaints. Click a tool header to expand its full retained output.

`/copy` copies selected text or lists fenced blocks in the latest reply.
`/copy N` copies one code block; `/copy all` copies all, without fences or
prose. Code copying retains original source lines, even when wrapped on screen.
A newer prompt ends the previous reply's copy window. OSC52 and tmux
terminal requests report unconfirmed receipt; local clipboard process success
is reported separately. F2 toggles fullscreen mouse capture without changing
the draft. With capture off, use your terminal's selection and copy controls
for transcript or composer text; F2 restores application selection. `/mouse`
also toggles capture. Inline always uses your terminal's selection and copy
controls. Ctrl+C interrupts work or requests exit, even with a native selection.
`/scrollback raw` provides complete printable text.
Enter/Escape return from inspection with the draft intact.

Start `dashi --inline` to keep completed output in native scrollback and
interactive streaming, prompts, and dialogs below it. `--plain` and `--inline`
cannot be combined. Nonterminal input/output still uses line mode.

## Approvals

The tools that read files run at once. A tool that changes files or runs a
command asks first. The prompt names the tool and what the call does, then
shows the command and directory, the diff, or the arguments as `key: value`
lines. A call from a delegated task names that task. Long argument values show
their first lines; `e` shows them all. Arrow/Page keys scroll the details; the
choices stay visible.

| Key | Result |
| --- | --- |
| `y` | Allow this call once |
| `a` | Allow for this Dashi run; the displayed rule survives session switches |
| `n` | Enter an optional denial reason, then press Enter |
| `e` | Show or clip long argument values |
| `/` | Run an inspection command, such as `/task ID`, without answering |
| Esc | Interrupt without granting permission; Enter can resume |

Remembered rules cover all calls to the owned write/edit tool, the exact Bash
command, or exact external-tool arguments. They last until the process exits;
they are not permanent grants.

`--dangerously-skip-permissions` approves every tool call without asking,
including calls from delegated tasks and MCP servers. Use it only where
something outside Dashi, such as a container, limits what the agent can do.

`--decide command_risk` adds a decide check to shell commands: the prompt
shows the check's answer, and in bypass mode a command it flags still asks.
The check sends each checked command to TypeSafe or Cloudflare with your key.
Bypass mode still runs edits, writes, and MCP calls unasked and unchecked.
`--decide prompt_injection` warns the agent about an MCP result or `gh`,
`curl`, `wget`, or `http` output that tries to take it over, and sends that
text out. `--decide turn_watcher` reminds the agent when it repeats a failing
call, and sends the turn's recent calls out when it does.
Every decide feature is off by default. See [decide checks](cli-decide.md).

Questions support choices or own words. Multiple questions use checkboxes:
arrows move, Space selects, Tab moves to Submit, and Enter submits. Press `o`
for own words. Escape interrupts without submitting an empty answer. Dialogs
restore the draft when they end.

In an own-words answer or denial-reason field, Ctrl-S opens a separate
`/status [ID]` field without editing the pending draft. Enter inspects;
Escape returns to the exact answer or reason. You can also submit `/status`
from an empty dialog field. Ordinary slash/path text stays editable; replacing
a draft with a command does not preserve the text you deleted.

If you quit during a prompt, the call does not run. After `dashi -c`, press
Enter to continue the turn. The prompt shows again.

Text from the model is shown safely. Control characters, such as ESC and
carriage return, appear as visible marks.

In `dashi run` nobody can answer, so every call that needs approval is refused.
Add `--yes` (`-y`) to approve all of them. Use `--yes` only in a repository that you
can restore.

## Line mode

Use `--plain` for the line/command/approval stream when stdin is redirected.
Redirected stdin without `--plain` selects one-shot text. With terminal stdin,
nonterminal stdout retains the line fallback. Line mode reads approval answers as lines.

For multiple-choice questions, line mode accepts `1,2` or `other: YOUR ANSWER`
and reprompts on invalid input. `n REASON` supplies an approval denial reason.
