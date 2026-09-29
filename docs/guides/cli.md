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

The default model is `gpt-6-luna`. To change it, use `--model NAME`, the
`DASHI_MODEL` variable, or `"model"` in `~/.dashi/settings.json`. A
`.dashi/settings.json` file in the repository overrides the user file.
`/model` changes the model for the next turn.

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

## Commands

| Input | Result |
| --- | --- |
| `/model [name]` | Pick a model, or set it for the next turn |
| `/new` | Start a new session |
| `/sessions` | List the sessions of this directory and reopen one |
| `/continue` | Go on after a model-call limit stop (Enter does the same) |
| `/queue text` | Hold a prompt for when the running turn ends |
| `/done` | Mark a command done that was running when dashi stopped |
| `/discard` | Discard the open turn |
| `/quit` | Save and exit (Ctrl-D does the same) |
| `/help` | Show the commands |

A command typed during a turn, other than `/queue` and `/help`, waits until
the turn ends.

## Sessions

Each session is a folder in `~/.dashi/sessions`. It holds one record for each
turn, and a `session.json` with only the title, directory, and model. The
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

## Approvals

The tools that read files run at once. A tool that changes files or runs a
command asks first. The prompt shows the diff, or the exact command.

| Key | Result |
| --- | --- |
| `y` | Allow this call once |
| `a` | Allow this call, and the same call again, until you exit |
| `n` or Esc | Refuse. The model reads the refusal and tries another way |

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
