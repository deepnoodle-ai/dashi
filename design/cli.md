# Dashi CLI: an interactive terminal coding agent

| Field | Content |
| --- | --- |
| Status | Implemented through the stand-in tools, 2026-09-28 |
| Author | CLI track lead |
| Roadmap | Consumer of [item 14](remaining-work.md); see Decision 1 |
| Reviewers | Curtis (sign-off pending) |
| Demo | Wednesday 2026-09-30. First slice ships Tuesday night. |

## Problem

Dashi has no CLI. Dive has an experimental CLI, but it runs on the old
library. A developer cannot try Dashi as a coding agent, and cannot see turns,
hooks, steering, and the recorder work together in one real program.

A developer needs three things from a coding agent.

- **Trust.** See every tool call. Approve risky ones.
- **Inspection.** See why a turn stopped and what it cost.
- **Continuity.** Close the terminal and pick the work up later.

Without this work, the demo has no product. Dashi's features stay in examples.

## Goals and metrics

The user is a developer who wants a terminal coding agent they can trust and
inspect. Primary metric: demo steps 1 to 4, 6, and 7 run end to end in a scratch Git
repository with the real coding tools. Step 5 (MCP) passes when the `mcp`
track wires in. The CLI PR does not wait for it.

Secondary metrics:

- Every event kind has a rendering test (text, tool call, progress, approval,
  each stop kind, usage).
- A reopened session shows full scrollback, and a follow-up uses prior
  history. A test proves it.
- One live run against `gpt-6-luna` is recorded in the PR.

Guardrails: the library module gains no CLI dependency. The CLI never loses a
committed step. A zero-value setup works: `OPENAI_API_KEY` set, no settings.

## The screens and commands the user meets

```text
$ dashi
dashi  gpt-6-luna  ~/git/scratch                     (new session)

> why does TestParse fail?

I will read the test first.
  read_file  parse_test.go                              ok  0.0s
  grep       "func Parse"                               ok  0.1s
The test expects "1,2" to give two items. Parse drops the last one.

  edit_file  parse.go                        allow once / always / deny?
  --- parse.go
  +++ parse.go
  @@ -12 +12 @@
  -	for i := 0; i < len(parts)-1; i++ {
  +	for i := 0; i < len(parts); i++ {
[turn 1 | 4 model calls | 3,210 tokens | $0.012]
```

Commands typed at the prompt:

| Input | Result |
| --- | --- |
| a line of text | Starts a turn. While a turn runs, the line is a steer. |
| `!` then text | An urgent steer. It cuts the model call in flight. |
| `/model` | Opens a picker of catalog models. |
| `/model gpt-6-luna` | Sets the model for the next turn. |
| `/new` | Starts a new session. |
| `/sessions` | Lists sessions for this directory. Pick one to reopen. |
| `/continue` | After a model-call limit stop, continues that turn. |
| `/quit` | Saves and exits. Ctrl-D does the same. |

Flags: `--resume` or `-c` reopens the last session for this directory.
`--resume ID` reopens session `ID`. `-p "prompt"` runs one turn without a UI
and prints the answer. `--model NAME` sets the model.

Model choice order: flag, `DASHI_MODEL`, settings file, then `gpt-6-luna`. The
settings file is `~/.dashi/settings.json`, overridden by
`.dashi/settings.json` in the repository.

**Input modes.** The prompt is always in one of three modes.

| Mode | Typed text | Ctrl-C |
| --- | --- | --- |
| Idle | A command or a new prompt | Exits |
| Turn running | A steer | Cancels the model call or the running tool; the turn stays resumable |
| Approval pending | An answer: `y` once, `a` always, `n` deny | Same as quitting (see Approval) |

A hung `go test` stops on Ctrl-C: the tool kills its process group and the
turn records the interruption.

## Main flows

**Turn.** The CLI calls `Runner.Stream` and draws each event as it arrives.
Assistant text streams into the scrollback. Each tool call shows one line
with name, main argument, state, and time, updated from tool progress. The
`bash` line shows its last output lines. A settled turn prints a footer with
calls, tokens, and cost from `out.Spend`. After a `bash` call settles, the CLI
runs `git diff --stat` and shows the diff, capped at 40 lines.

**Approval.** The CLI passes `workspace.NewPolicy(approve).Hooks()` as the
runner's hooks. `approve` is the CLI's `workspace.ApprovalFunc`. It blocks
the hook while a modal prompt shows the tool, its summary, and `Preview` (a
diff for file edits) or the exact command for `bash`. Answers map to
`workspace.Approval`: `y` allows once, `a` sets `Remember`, `n` denies with
the reason "The user declined this call." Remembered rules last as long as the
process, so a reopened session asks again. The `ask_user` tool uses the same
modal through the CLI's `workspace.AskFunc`.

*Quit during a prompt.* Exit cancels the run context. `approve` returns the
context error, the hook stops, and Dashi records the turn as interrupted
(the default `ContextInterrupt`). The call had not started, so no effect is
lost. On reopen the screen says "Turn 3 was interrupted." An empty Enter
continues it with `dashi.Continue`; the same call is asked again. A test
proves this path. If Dashi records a failure instead, the fix goes in the
library in this PR.

**Steering.** Each line typed during a turn goes to a `dashi.Inbox` passed
with `WithSteering`. The screen marks it "steer queued", then "steer
delivered" when the record holds it. A steer that misses its turn becomes the
next turn's input (`Inbox.Leftovers`).

**Limit stop.** The CLI sets `Limits.ModelAttempts` to 40. At the limit, the
screen says: "Stopped after 40 model calls. Type /continue to go on."
`/continue` continues that same turn with a raised limit
(`dashi.Continue{ModelAttempts: 80}`, doubling each time). It adds no user
line and no note. Decision 13 records the library change that allows it.

**Resume.** `dashi -c` loads the session, replays every turn into the
scrollback, and waits at the prompt. If the last turn did not end, the
screen says so. A new prompt at that point cancels the unfinished turn and
shows "Turn 3 canceled." before the new turn starts.

## Tools and wiring

The `codetools` track (package `workspace`) merges first. The CLI uses it
directly:

```go
ws, err := workspace.New(dir)
tools, err := ws.Tools(workspace.WithAsker(ask))
hooks := ws.Policy(approve).Hooks()
```

Library packages export plain `tool.Toolset` or `tool.List` and
`dashi.Hooks`. They know nothing about the CLI. `cmd/dashi/wire.go` holds
one short adapter for each track. It returns tools, hooks, and an optional
close function. The CLI joins the results with `tool.Join` and
`dashi.JoinHooks`. Wiring the `mcp` or `skills` track is one added adapter.

```go
// cmd/dashi/wire.go
type wiring struct {
	Tools        tool.Toolset
	Hooks        dashi.Hooks
	Instructions string       // appended to the system prompt
	Close        func() error // may be nil
}
var adapters = []func(Env) (wiring, error){workspaceWiring /*, mcpWiring */}
```

Until `workspace` merges, a fallback adapter registers two stand-in tools,
`read_file` and `list_directory`, with the same names. It is removed when the
merge lands. The CLI owns no approval policy.

**Session store.** One directory per session under `~/.dashi/sessions`.

```go
// cmd/dashi/session
type Session struct {
	ID    string
	Dir   string   // repository path
	Title string   // first prompt, shortened
	Model string
	Turns []string // turn IDs, in order
	Created, Updated time.Time
}
func (s *Store) Create(dir, model string) (*Session, error)
func (s *Store) Save(*Session) error
func (s *Store) Latest(dir string) (*Session, error)
func (s *Store) List(dir string) ([]Session, error) // this directory only
func (s *Store) Recorder(*Session) (*filerecorder.Recorder, error)
func (s *Store) Lock(*Session) (unlock func(), err error)
func (s *Store) History(ctx, rec, sess) (History, error)
func (s *Store) HistoryBefore(ctx, rec, sess, n) (History, error)

type History struct {
	Messages []llm.Message
	Notices  []string // turns skipped, with the reason
}
```

`History` is the only place that builds model history. It joins
`dashi.Project(turn)` in order. A turn that cannot be read or projected is
skipped and named in `Notices`, and the session goes on. The screen shows each
notice as a warning. The `compaction` track later hands the CLI a summary
record. It applies inside `History`, over the projected messages, so that
wiring is one change. Turn IDs are `<session ID>-<n>`. A `session.json` file
holds the fields above. Turn records live in a `filerecorder` under
`<session>/turns`. `HistoryBefore` builds the history for a `Continue`, which
must exclude the turn it continues.

## Requirements

- FR-1: The screen shows streamed text, each tool call (name, argument,
  state, duration), each stop reason, and a footer with model, calls, tokens,
  and cost.
- FR-5: The screen, line mode, and `-p` use one event-to-text renderer.
- FR-6: The store saves sessions and lists them by directory.
- FR-7: `-c` and `--resume` reopen a session with full scrollback.
- FR-8: A follow-up sends the history built by `History`.
- FR-9: An unreadable turn gives a warning, is skipped, and does not stop
  the session. A test covers it.
- FR-10: A second `dashi` on one session shows a notice and exits. The store
  locks the session directory.
- FR-11: Lines typed during a turn are steers. `!` makes one urgent.
- FR-12: Approval blocks the call until the user answers. The prompt shows
  the diff or command.
- FR-13: Quit during approval leaves the turn interrupted and continuable.
- FR-14: `Limits.ModelAttempts` is 40. The limit stop offers `/continue`.
- FR-15: `/model` with no argument opens a picker. An unknown name gives a
  warning and keeps the old model.
- FR-16: Ctrl-C behaves as the input-mode table says.

## First slice (ships Tuesday night)

Demo step coverage:

| Step | Covered by | Completes in |
| --- | --- | --- |
| 1 Launch | CLI | this PR |
| 2 Ask for a fix | CLI loop | this PR |
| 3 Read, search, edit | `workspace` tools | `codetools` PR |
| 4 Approve shell, see result and diff | `workspace` policy, CLI diff | this PR |
| 5 MCP tool | `mcp` adapter | `mcp` PR |
| 6 Steering | CLI inbox | this PR |
| 7 Exit, reopen, follow up | session store | this PR |

Tests: a fake client drives each screen line, each approval answer, quit
during approval then continue, a steer found in the recorded turn, a reopened
store with the same history, a skipped bad turn, and `-p` printing only the
answer.

Order of work: store, `History`, and renderer with tests; then the
`Runner.Stream` loop and `-p`; then the Wonton screen, approval, and
steering; then `/model`, `/sessions`, and resume. If the screen slips, line
mode and `-p` still run the loop.

## Later

- Wire in `mcp`, `skills`, delegation, compaction, and attachments.
- Persist remembered approvals in a settings file.
- Session search, rename, and delete.

## Non-goals

- A library `Session` type. The CLI proves the need first (item 14).
- A shell sandbox. Approval is the only gate.
- Themes, key remapping, and IDE integration.
- Two writers on one session.
- Listing sessions from other directories.

## Risks and dependencies

| Risk | Impact | Mitigation |
| --- | --- | --- |
| `workspace` merges late | Stand-ins stay in the demo | Fallback adapter covers steps 1, 2, and 7 |
| Wonton screen takes longer | Demo has no UI | Line mode on the same renderer |
| Quit during approval records a failure | Turn not continuable | Test first; fix the library in this PR |
| Live `gpt-6-luna` differs from fakes | Demo surprise | Live check on Tuesday |
| Steering costs extra calls | Limit surprise | Footer shows calls; limit is 40 |

## Decisions

1. **New PRD, not a revision of the item 14 draft.** That draft is a library
   `HistoryRef` for hosts with their own log. The CLI owns history through
   turn records and needs no ref.
2. **Turn records are the only history store**, built in one function
   (`History`). No second copy can drift, and compaction wires in there.
3. **Model-attempt limit is 40.** Coding reads many files. Eight stops early.
4. **Use `workspace` directly.** The coordinator directed it. Stand-ins are a
   fallback only.
5. **Review 1 accepted.** The PRD now maps each demo step to a tool and a
   track. Steps 3 and 5 finish in other PRs.
6. **Review 2 accepted.** Libraries export plain tools and hooks. The CLI
   has a small adapter. `Verdict` and `Feature` are gone. Approval is the
   `workspace` policy with a CLI `ApprovalFunc` that shows `Preview`.
7. **Review 3 accepted in part.** Approval stays inline (the coordinator
   chose it). The PRD defines three input modes, Ctrl-C, and the quit path.
   `Defer` is not used in the CLI.
8. **Review 4 accepted.** `/continue` means only "after the limit stop". Cancel of an unfinished turn is shown.
   Interrupted turns continue with an empty Enter after `dashi -c`.
9. **Review 5 accepted.** A bad turn is skipped with a notice.
10. **Review 6 accepted by delegation.** The CLI stores no approval rules.
    The `workspace` policy remembers a shell approval for the exact command
    only, and only while the process lives.
11. **Plain Enter during a turn is a steer; `!` is urgent.** It matches
    `examples/steering`.
12. **`/sessions` lists this directory only.** The `--all` flag is cut.
13. **Fix the limit gap in the library now** (orchestrator ruling; Dashi has
    no users). `Continue.ModelAttempts` raises the limit and the same turn
    goes on (library decision 34). `/continue` needs no synthetic input.

## Open questions

None block the first slice.
14. **Controller and UI split.** `Controller` holds the input modes, slash
    commands, and run goroutine. The Wonton screen, line mode, and `-p` are
    thin `UI` values, so tests drive every mode without a terminal.
15. **Line mode and `--plain`.** Line mode runs when stdout or stdin is not a
    terminal, or with `--plain`. Piped input runs one line at a time, so a
    script does not steer its own turns. `-p` denies approvals unless
    `--yes` is set.
16. **The CLI defines `ApprovalRequest`, `Approval`, and `Question`.** Their
    fields match `workspace`, so its adapter converts field by field. The
    `workspace` API changed to `New`, `Tools`, and `Policy` before merge.
17. **`/continue` doubles the limit** (40, 80, 160). The full screen prints
    the plain transcript and the resume line on exit, since the alternate
    screen leaves no scrollback.
