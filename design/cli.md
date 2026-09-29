# Dashi CLI: an interactive terminal coding agent

| Field | Content |
| --- | --- |
| Status | Implemented with the real coding tools, 2026-09-28 |
| Author | CLI track lead |
| Roadmap | Consumer of [item 14](remaining-work.md); see Decision 1 |
| Reviewers | Curtis (sign-off pending) |
| Demo | Wednesday 2026-09-30 |
| Guide | [docs/guides/cli.md](../docs/guides/cli.md) |

## Problem

Dashi has no CLI. Dive has an experimental CLI, but it runs on the old
library. A developer cannot try Dashi as a coding agent. The developer cannot
see turns, hooks, steering, and the recorder work together in one program.

A developer needs three things from a coding agent.

- **Trust.** See every tool call. Approve risky ones.
- **Inspection.** See why a turn stopped and what it cost.
- **Continuity.** Close the terminal and pick the work up later.

## Goals and metrics

The user is a developer who wants a terminal coding agent they can trust and
inspect. Primary metric: demo steps 1 to 4, 6, and 7 run end to end in a
scratch Git repository with the real coding tools. Step 5 (MCP) passes when
the `mcp` track wires in. The CLI PR does not wait for it.

Secondary metrics:

- Every event kind has a rendering test.
- A reopened session shows full scrollback, and a follow-up uses prior
  history. A test proves it.
- One live run against `gpt-6-luna` is recorded in the PR.

Guardrails: the library module gains no CLI dependency. The CLI never loses a
committed step. A zero-value setup works: `OPENAI_API_KEY` set, no settings.

## What the user meets

```text
$ dashi
dashi  gpt-6-luna  ~/git/scratch

> why does TestParse fail?
  read_file  parse_test.go                              ok  0.0s
The test expects "1,2" to give two items. Parse drops the last one.
  edit_file  parse.go                        allow once / always / deny?
[turn 1 | gpt-6-luna | 4 model calls | 3,210 tokens | $0.012]
```

| Input | Result |
| --- | --- |
| a line of text | Starts a turn. While a turn runs, the line is a steer. |
| `!` then text | An urgent steer. It cuts the model call in flight. |
| empty Enter | Continues a turn that was interrupted or hit the call limit. |
| `/model [name]` | Opens a picker, or sets the model for the next turn. |
| `/new`, `/sessions` | Starts a session, or lists this directory's sessions. |
| `/continue` | Same as Enter after a model-call limit stop. |
| `/quit` | Saves and exits. Ctrl-D does the same. |

A slash command typed during a turn is not a steer. It waits, and the screen
says so. Flags: `-c` or `--resume` reopens the last session of this
directory that has a turn. `--resume ID` reopens session `ID`. `-p "prompt"`
runs one turn without a UI and prints the answer. `--model NAME` sets the
model. The order for the model is flag, `DASHI_MODEL`, settings file, then
`gpt-6-luna`. Settings are in `~/.dashi/settings.json`, overridden by
`.dashi/settings.json` in the repository.

| Mode | Typed text | Ctrl-C |
| --- | --- | --- |
| Idle | A command or a new prompt | Exits |
| Turn running | A steer | Cancels the model call or the tool; the turn stays resumable |
| Approval pending | `y` once, `a` always, `n` deny | Quits (see Approval) |

## Main flows

**Turn.** The CLI calls `Runner.Stream` and draws each event. Text streams
into the scrollback. Each tool call shows one line: name, main argument, state,
time. Paths inside the repository show relative to it, and a long path loses
its left end. The `bash` line shows the last output lines from
`workspace.ShellProgress`. A settled turn prints a footer with the model that
ran it, calls, tokens, and cost. After `bash` settles, the CLI shows
`git diff`, capped at 40 lines.

**Safe text.** The model controls much of what the screen shows. Every block
goes through `workspace.PrintableText` before a UI sees it. Approval requests
go through `ApprovalRequest.Printable()`. A test puts ESC and CR in a command.

**Approval.** `ws.Policy(approve).Hooks()` is the runner's hook. `approve`
blocks while a modal prompt shows the summary and the diff or exact command.
`y` allows once, `a` sets `Remember`, `n` denies with a reason the model
reads. Remembered rules live as long as the process. Quit during a prompt
cancels the run. Dashi records the turn as interrupted, and the call had not
started. After `dashi -c`, Enter continues the turn and asks again.

**Steering.** Lines typed during a turn go to a `dashi.Inbox`. The screen
shows "steer queued", then "steer delivered". A steer that misses a finished
turn becomes the next turn's input. After Ctrl-C the steers stay queued, the
screen says how many, and the next run of the turn delivers them.

**Limit stop.** The CLI sets `Limits.ModelAttempts` to 40. Enter or
`/continue` continues the same turn with a limit twice as high. It adds no
user line. A new prompt cancels the limited turn instead.

**Sessions.** Each session is a directory in `~/.dashi/sessions` with a
`session.json` and one `filerecorder` log per turn. A session is written when
its first turn records. A launch that runs no turn leaves nothing behind.
`History` is the only code that builds model history. It joins
`dashi.Project(turn)` in order and skips a turn that cannot be read, with a
warning. Compaction applies there later. `-p` sessions are saved like any
other. `-c` picks the newest session with a turn, so `dashi -c -p "..."` adds
to the last session. `/sessions` lists this directory only. A lock file keeps
a second `dashi` out of an open session.

## Wiring

Library packages export plain tools and `dashi.Hooks`. `cmd/dashi/wire.go`
holds one short adapter for each. It returns tools, hooks, extra system
prompt text, and an optional close function. The CLI joins them with
`tool.Join` and `dashi.JoinHooks`. Wiring `mcp`, `skills`, or compaction is
one added entry in `adapters`. The workspace adapter is:

```go
ws, _ := workspace.New(dir)
tools := ws.Tools(workspace.WithAsker(ask))
hooks := ws.Policy(approve).Hooks()
```

The CLI uses the `workspace` approval types directly. It owns no policy.

## Requirements

- FR-1: The screen shows text, each tool call, each stop reason, and a footer.
- FR-2: The screen, line mode, and `-p` use one renderer.
- FR-3: `-c` and `--resume` reopen a session with full scrollback. A follow-up
  sends the history from `History`.
- FR-4: An unreadable turn gives a warning and does not stop the session.
- FR-5: Lines typed during a turn are steers. `!` makes one urgent.
- FR-6: Approval blocks the call until the user answers. Quit during approval
  leaves the turn continuable.
- FR-7: The limit stop offers Enter and `/continue`.
- FR-8: Model-controlled text never reaches the terminal raw.
- FR-9: An empty session is never saved or listed.

## Non-goals and risks

Non-goals: a library `Session` type, a shell sandbox (approval is the only
gate), themes, IDE integration, two writers on one session, and sessions from
other directories. Later: wire `mcp`, `skills`, delegation, compaction, and
attachments; persist remembered approvals; mouse-wheel scrolling; other model
providers.

| Risk | Mitigation |
| --- | --- |
| Live `gpt-6-luna` differs from fakes | A live pass is recorded in the PR |
| Screen bugs hide in a terminal | Line mode and `-p` run the same loop; the Controller has tests |

## Decisions

1. **New PRD, not the item 14 draft.** That draft is a library `HistoryRef`.
   The CLI owns history through turn records and needs no ref.
2. **Turn records are the only history store**, built in `History`.
3. **The model-call limit is 40.** Coding reads many files.
4. **Use `workspace` directly**, with its approval types.
5. **Libraries export plain tools and hooks.** The CLI has a small adapter.
6. **Approval is inline.** It has three input modes and a quit path.
7. **The CLI stores no approval rules.** The `workspace` policy remembers for
   the life of the process.
8. **Enter during a turn is a steer; `!` is urgent.** A slash command waits.
9. **`/sessions` lists this directory only.**
10. **The limit stop is not terminal** (library decision 34). The same turn
    continues with a raised limit, doubling each time (40, 80, 160).
11. **The `Controller` holds modes and commands.** The screen, line mode, and
    `-p` are thin `UI` values, so tests drive every mode without a terminal.
12. **Line mode** runs when stdin or stdout is not a terminal, or with
    `--plain`. Piped input runs one line at a time. `-p` denies approvals
    unless `--yes` is set.
13. **`-p` sessions are saved, and `-c` picks the newest session with a
    turn.** The launch that runs no turn is never saved, so it cannot hide
    real work. A `-p` run without `-c` starts its own session.
14. **The full screen prints the plain transcript on exit**, because the
    alternate screen leaves no scrollback.
