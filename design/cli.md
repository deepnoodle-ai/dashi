# Dashi CLI: an interactive terminal coding agent

| Field | Content |
| --- | --- |
| Status | Draft, 2026-09-28 |
| Author | CLI track lead |
| Roadmap | Consumer of [item 14](remaining-work.md); see Decision 1 |
| Reviewers | Curtis (sign-off pending) |
| Demo | Wednesday 2026-09-30. First slice ships Tuesday night. |

## Problem

Dashi has no CLI. Dive has an experimental CLI, but it runs on the old
library. A developer cannot try Dashi as a coding agent. A developer also
cannot see how Dashi's turns, hooks, steering, and recorder work together in
one real program.

Three things are hard for a developer today.

- **Trust.** The developer must see every tool call and approve risky ones.
- **Inspection.** The developer must see why a turn stopped and what it cost.
- **Continuity.** The developer must close the terminal and pick the work up
  later, with the same history.

If we do nothing, the demo has no product to show. Dashi's features stay
visible only in examples.

## Goals and metrics

Primary metric: the seven-step demo scenario in `PROGRAM.md` runs end to end
in a scratch Git repository, with stand-in tools, before Wednesday.

Secondary metrics:

- Every event kind has a rendering test (text, tool call, progress, approval,
  each stop kind, usage).
- Reopening a session shows the full scrollback and a follow-up uses prior
  history. A test proves it.
- One live run against `gpt-6-luna` is recorded in the PR.

Guardrails:

- The library module gains no CLI dependency.
- The CLI never loses a committed step, even on a crash.
- A zero-value setup works: `OPENAI_API_KEY` set, no settings file.

## Users

Primary: a developer who wants a terminal coding agent they can trust and
inspect. Secondary: a Go developer who reads the CLI as the reference
application for Dashi.

## The screens and commands the user meets

```text
$ dashi
dashi  gpt-6-luna  ~/git/scratch                     (new session)

> why does TestParse fail?

I will read the test first.
  read_file  parse_test.go                              ok  0.0s
  grep       "func Parse"                               ok  0.1s
  read_file  parse.go                                   ok  0.0s
The test expects "1,2" to give two items. Parse drops the last one.

  run_shell  go test ./...            [allow once / always / deny]
> _
[turn 1 | 4 model calls | 3,210 tokens | $0.012]
```

Commands typed at the prompt:

| Input | Result |
| --- | --- |
| a line of text | Starts a turn. While a turn runs, the line is a steer. |
| `!` then text | An urgent steer. It cuts the model call in flight. |
| `/model` | Opens a picker of catalog models. |
| `/model gpt-6-luna` | Switches the model for the next turn. |
| `/new` | Starts a new session. |
| `/sessions` | Lists sessions for this directory. Pick one to reopen. |
| `/continue` | Continues after a limit stop or an interrupted turn. |
| `/quit` | Saves and exits. Ctrl-D does the same. |

Command-line flags:

| Flag | Result |
| --- | --- |
| `--resume`, `-c` | Reopens the last session for this directory. |
| `--resume ID` | Reopens session `ID`. |
| `-p "prompt"` | Runs one turn without a UI. Prints the answer to stdout. |
| `--model NAME` | Sets the model. |

Model choice order: flag, then `DASHI_MODEL`, then settings file, then
`gpt-6-luna`. The settings file is `~/.dashi/settings.json`, overridden by
`.dashi/settings.json` in the repository.

Ctrl-C once cancels the running model call and returns to the prompt. The turn
stays resumable. Ctrl-C at an idle prompt exits.

## Main flows

**Turn.** The CLI calls `Runner.Stream` and draws each event as it arrives.
Assistant text streams into the scrollback. Each tool call shows one line with
its name, main argument, state, and time. The line updates from tool progress.
A settled turn prints a footer with calls, tokens, and cost from `out.Spend`.

**Approval.** A `BeforeTool` hook asks the user for each call that policy does
not allow. The prompt shows the tool name and arguments. Answers: allow once,
allow always (this tool, this session), deny. A denial goes to the model as
the reason "The user declined this call."

**Steering.** The prompt stays live while a turn runs. Each line goes to a
`dashi.Inbox` passed with `WithSteering`. The screen marks it "steer queued",
then "steer delivered" when the record holds it. A steer that misses its turn
becomes the next turn's input (`Inbox.Leftovers`).

**Limit stop.** The CLI sets `Limits.ModelAttempts` to 40. At the limit, the
screen says: "Stopped after 40 model calls. Type /continue to go on." A turn
that hits this limit is terminal in Dashi, so `/continue` starts a new turn
with the input "Continue the previous task." History carries over.

**Resume.** On start with `-c`, the CLI loads the session, replays every turn
into the scrollback, and waits at the prompt. If the last turn did not end
(the process died), the screen says so and offers `/continue`. A new prompt
first cancels the unfinished turn, then starts the new one.

## Public seams

The CLI is an application, not a library. Its seams serve the other tracks.

**Feature registration.** Other tracks add tools with one call each.

```go
// cmd/dashi/feature.go
type Feature struct {
	Name         string
	Tools        tool.Toolset  // may be nil
	Hooks        dashi.Hooks   // extra hooks, may be zero
	Instructions string        // appended to the system prompt
	Policy       Policy        // approval policy, may be nil
	Close        func() error  // called at exit, may be nil
}

type Env struct {
	Workdir  string
	Settings Settings
}

// features.go lists constructors. Wiring a track is one line:
var features = []func(Env) (Feature, error){
	standIn, // read_file, list_files
	// codetools.Feature, mcp.Feature, skills.Feature, ...
}
```

**Approval policy.** The CLI supplies the prompt. A feature supplies rules.

```go
type Verdict int // Ask (zero), Allow, Deny

type Policy func(dashi.ToolPlan) Verdict
```

The zero verdict is `Ask`. The default policy allows tools annotated
read-only and asks for all others. When several policies answer, the most
restrictive answer wins (`Deny`, then `Ask`, then `Allow`).

**Session store.** The store owns one directory per session.

```go
// cmd/dashi/session
type Session struct {
	ID      string
	Dir     string   // repository path
	Title   string   // first prompt, shortened
	Model   string
	Turns   []string // turn IDs, in order
	Allowed []string // tools approved with "allow always"
	Created, Updated time.Time
}

type Store struct{ Root string } // default ~/.dashi/sessions

func (s *Store) Create(dir, model string) (*Session, error)
func (s *Store) Save(*Session) error
func (s *Store) Latest(dir string) (*Session, error)
func (s *Store) List(dir string) ([]Session, error)
func (s *Store) Recorder() dashi.Recorder // a filerecorder
func (s *Store) History(ctx context.Context, sess *Session) ([]llm.Message, error)
```

`History` joins `dashi.Project(turn)` for each turn in order. The CLI keeps no
second copy of messages, so the record is the only source of truth. Turn IDs
are `<session ID>-<n>`. A `session.json` file holds the fields above. Turn
records live in `filerecorder` files beside it.

## Requirements

Screen

- FR-1: Assistant text appears as it streams.
- FR-2: Each tool call shows name, main argument, state, and duration.
- FR-3: Each stop kind (completed, waiting, limited, refused, canceled,
  interrupted, failed, uncertain) shows its reason in plain words.
- FR-4: The footer shows model, calls, tokens, and cost for each turn.
- FR-5: The renderer works without a UI. `-p` and non-terminal output use the
  same event-to-text code.

Session

- FR-6: The store saves each session and lists them by directory.
- FR-7: `-c` and `--resume` reopen a session with full scrollback.
- FR-8: A follow-up prompt sends the joined history of all earlier turns.
- FR-9: `/new` starts a session. The old one stays listed.
- FR-10: A second `dashi` process on one session shows a notice and exits.
  The store locks the session directory to enforce this.

Control

- FR-11: Lines typed during a turn go to the inbox as steers.
- FR-12: A `!` prefix sends an urgent steer.
- FR-13: Approval asks allow once, allow always, or deny, and blocks the call
  until the user answers.
- FR-14: `Limits.ModelAttempts` is 40. The limit stop offers `/continue`.
- FR-15: `/model` with no argument opens a picker. With an argument, it sets
  the model. An unknown name gives a warning and keeps the old model.
- FR-16: Ctrl-C cancels the model call in flight and keeps the turn resumable.

Extension

- FR-17: `Feature` and `Policy` exist as shown. The stand-in tools use them.

## User stories

- **US-1.** As a developer, I ask for a fix and see each read, search, and edit
  as it happens. Done when a test drives a fake client and checks each line.
- **US-2.** As a developer, I approve a shell command once or always. Done when
  a test answers each choice and checks the hook result.
- **US-3.** As a developer, I send a correction mid-turn. Done when a test
  sends a steer and finds it in the recorded turn.
- **US-4.** As a developer, I quit and reopen the session. Done when a test
  reopens a store and finds the same scrollback and history.
- **US-5.** As a script author, I run `dashi -p`. Done when stdout holds only
  the answer and the exit code is 0 for a completed turn.

## First slice (ships Tuesday night)

In scope: everything above, with two read-only stand-in tools (`read_file`,
`list_files`) and one stand-in `run_shell` tool that always asks. The tools
are enough to run the demo path and to test the loop. The `codetools` track
replaces them.

Tuesday order of work: session store and renderer (with tests), then the
`Runner.Stream` loop and `-p`, then the Wonton screen, approval, and steering,
then `/model`, `/sessions`, and resume. If the Wonton screen slips, `-p` and a
line-mode renderer still run the loop. The same event-to-text code serves both.

## Later

- Wire in `codetools`, `mcp`, `skills`, and delegation through `Feature`.
- Persist "allow always" across sessions in `.dashi/settings.local.json`.
- Show the diff and test result after a shell call (needs `codetools`).
- Compaction of long sessions (needs the compaction track).
- Attachments and image paste.
- Session search, rename, and delete.

## Non-goals

- A library `Session` type. The CLI proves the need first (roadmap item 14).
- A sandbox for shell commands. Approval is the only gate in this slice.
- Multiple providers in the picker beyond the catalog's own list.
- Themes, key remapping, and IDE integration.
- Two writers on one session.

## Risks and dependencies

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Wonton screen takes longer than planned | Demo has no UI | Keep a line-mode renderer on the same event code |
| A terminal-limit turn cannot continue in Dashi | `/continue` starts a new turn, so the record shows two turns | Show them as one task in scrollback; record as a library gap |
| Live `gpt-6-luna` behavior differs from fake clients | Demo surprise | Run the live check on Tuesday, not Wednesday |
| Codetools merges late | Stand-ins stay in the demo | Stand-ins cover the scenario by design |
| Steering can cost extra model calls | Cost and limit surprise | Show calls in the footer; set 40 attempts |

Assumptions: Wonton `tui` gives streaming scrollback (Dive's screen shows it
does). `gpt-6-luna` supports tool use and streaming in Dashi's OpenAI route.

## Decisions

1. **Write a new PRD, not a revision of the item 14 draft.** That draft covers
   a library `HistoryRef` for hosts that own a log. The CLI owns its history
   through turn records, so it needs no ref. The CLI is a consumer and a test
   of that draft's premise.
2. **Turn records are the only history store.** `History` joins
   `dashi.Project` over turns. This avoids a second copy that can drift. It
   also uses the recorder's crash safety.
3. **Model attempt limit is 40.** Coding tasks read many files. Eight stops
   too early. Forty bounds cost and runaway loops.
4. **`/continue` starts a new turn at the limit.** Dashi ends a turn that used
   its last attempt. Raising a limit on an ended turn is a library change. We
   do not make it for the demo.
5. **Plain Enter during a turn is a steer; `!` is urgent.** This matches
   `examples/steering` and needs no new key model.
6. **"Allow always" lasts for the session.** It is stored in `session.json`.
   Persisting it across sessions is a policy choice for later.
7. **Default policy asks for anything not read-only.** The safe default fits
   "a terminal coding agent they can trust". Features may tighten it.
8. **One renderer serves the screen, line mode, and `-p`.** This makes each
   event kind testable without a terminal.
9. **A session is bound to a directory.** `-c` picks the latest session for
   the current directory. Sessions from other directories stay in `/sessions`
   behind a flag, `--all`.

## Open questions

None block the first slice. Later: should the library offer a command that
raises limits on a limited turn, so `/continue` stays one turn?
