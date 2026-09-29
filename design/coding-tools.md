# PRD: coding tools and approval policy

| Field | Value |
| --- | --- |
| Status | Draft for review |
| Date | 2026-09-28 |
| Roadmap | None. Track of the terminal coding agent program |
| Package | `github.com/deepnoodle-ai/dashi/workspace` |
| Reference | Dive `toolkit` and the Dive CLI `workspace_scope.go` |

## Problem

A developer who builds a coding agent on Dashi must write the same tools first: read, search, edit, and run commands. Each tool needs many small details to work well with a model. The developer must also keep the model inside one directory and ask a person before risky actions. Dive has these tools, but they use Dive's tool types. Dashi has no equivalent. The `dashi` terminal agent cannot ship its demo without them.

Without this work, every coding agent on Dashi writes its own file tools. Each one gets path checks, edit errors, and shell cleanup wrong in its own way.

## Goal

One package makes a Dashi agent a capable and safe coding agent in one directory. Registering the tools takes one line. Approval takes one more.

Success means:

- The `dashi` CLI registers the tools and the policy with no extra tool code.
- A live agent fixes a failing test in a temporary Git repository, using only these tools.
- No test finds a read or write outside the workspace, including through symlinks.
- Every model-fixable failure names the problem and the next step in its error text.

## User

The first user is a developer who builds a coding agent, starting with the `dashi` CLI. The second user is a developer who builds a server-side agent that works on a checked-out repository. This person defers approvals instead of asking inline.

## Public API

Package name: `workspace`. It says where the tools work. The name `toolkit` would say nothing.

```go
// Tools returns the coding tools for dir. dir must be an existing directory.
func Tools(dir string, opts ...Option) (tool.List, error)

// Options. The zero value of each works.
func WithAsker(ask AskFunc) Option      // adds ask_user; without it the tool is absent
func WithShell(cfg ShellConfig) Option  // default timeout, maximum timeout, shell path
func WithoutTools(names ...string) Option

type AskFunc func(ctx context.Context, q Question) (Answer, error)
type Question struct {
	Prompt  string
	Options []Option // empty means free text
}
type Answer struct{ Text string }

// Approval policy for Runner.Hooks.
func NewPolicy(ask ApprovalFunc) *Policy
func (p *Policy) Hooks() dashi.Hooks

type ApprovalFunc func(ctx context.Context, r Request) (Verdict, error)
type Request struct {
	Tool    string
	Summary string          // "edit main.go", "run: go test ./..."
	Args    json.RawMessage // the exact arguments
}
type Verdict struct {
	Allow  bool
	Always bool   // remember for the rest of this session
	Reason string // shown to the model when Allow is false
}
```

The agent uses it like this:

```go
tools, err := workspace.Tools(dir, workspace.WithAsker(cliAsk))
runner := dashi.Runner{Agent: agent, Client: client, Tools: tools,
	Hooks: workspace.NewPolicy(cliApprove).Hooks()}
```

### The tools

| Name | Kind | Behavior |
| --- | --- | --- |
| `read_file` | read-only | Returns numbered lines. Takes `offset` and `limit`. Default limit is 2000 lines. Refuses binary files. |
| `list_directory` | read-only | Lists one directory. Marks directories. |
| `glob` | read-only | Matches a pattern such as `**/*.go`. Sorts by modification time. |
| `grep` | read-only | Runs `rg` when found. Otherwise uses a Go search. Supports content, file-list, and count modes. |
| `write_file` | mutating | Writes a whole file. Creates parent directories. |
| `edit_file` | mutating | Replaces `old_string` with `new_string`. Fails unless `old_string` is unique or `replace_all` is true. |
| `bash` | mutating | Runs a command in the workspace. Reports progress. |
| `ask_user` | read-only | Asks the person a question through `AskFunc`. |

Each tool sets `Annotations.ReadOnly`. `ask_user` counts as read-only because the question is the prompt.

## Behavior

**Paths.** A relative path resolves against the workspace. An absolute path must lie inside it. The check resolves symlinks in the deepest existing part of the path, so a link that points outside is refused. For a new file, the check applies to the parent directory. The error says: `path "../secret" is outside the workspace /repo. Use a path inside it.`

**Edit errors.** A missing string gives: `old_string not found in main.go`. A repeated string gives the count and the line numbers: `old_string appears 3 times (lines 12, 40, 88). Add surrounding lines to make it unique, or set replace_all.` Both return as tool errors, so the model sees them and retries.

**Shell.** The command runs under `bash -c`, or `sh -c` when bash is absent. The working directory is the workspace. Standard input is empty. The tool merges standard output and error in order. The default timeout is 2 minutes. The maximum is 10 minutes. On timeout or cancel, the tool kills the whole process group and returns the output so far. A non-zero exit returns a failed outcome with the output and the exit code, so the screen shows a failing test as a failure.

**Progress.** While `bash` runs, it calls `tool.Report` at most four times a second. `Message` is the last output line. `Data` holds the last 20 lines as JSON. The report is live only, as `tool/progress.go` defines.

**Policy.** For each call, the policy decides as follows:

1. A tool with `ReadOnly` true runs with no prompt.
2. A remembered rule for the call allows it.
3. Any other tool calls `ApprovalFunc`. This includes tools from other packages, such as MCP tools.
4. `Allow` returns `dashi.Allow`. A refusal returns `dashi.Deny` with the reason. The default reason tells the model not to retry the same call.
5. When `ask` is nil, the policy returns `dashi.Defer`. This supports the durable approval flow with no extra code.

`Always` stores a rule for the session. The rule for `write_file` and `edit_file` is the tool name. The rule for `bash` is the exact command text. A prefix rule for `bash` is unsafe, because `go test ./... && rm -rf ~` starts with `go test`. The policy asks one person at a time: a mutex serializes prompts from parallel calls.

## Decisions

1. **Package name `workspace`.** It names the boundary that the tools enforce. `Tools(dir)` reads well at the call site.
2. **`Tools` returns an error.** A missing directory is not safe to adjust. The brief showed one return value. The error is the cost of failing early.
3. **Policy uses the `ReadOnly` annotation, not tool names.** It then covers MCP and custom tools with no list to maintain. An unannotated tool asks. Unknown means risky.
4. **Snake-case tool names, `bash` for the shell.** Models know `read_file`, `edit_file`, and `bash`. The names carry no version or product prefix.
5. **Non-zero exit is a failed outcome.** The tool ran, but the command failed. The person and the model both need to see that.
6. **The shell is not sandboxed.** The path checks cover the file tools only. A shell command can reach any path the user can. The approval prompt is the guard, and the guide says so. An OS sandbox is later work.
7. **`allow always` for the shell is exact-match.** This trades convenience for safety. Prefix rules wait for a parser that rejects operators.
8. **The read limit is the only size bound.** The tool-output track owns generic bounding.
9. **No read-before-edit rule.** The unique-match check already stops blind edits. A staleness check adds state and gives little in a first slice.
10. **`ask_user` is absent without `WithAsker`.** A tool that cannot ask would only confuse the model. This warns and adjusts instead of failing.

## First slice (ship by Tuesday night)

- Package `workspace` with all eight tools, path scope, `Tools`, `NewPolicy`, and the options above.
- Unix process-group kill. Other platforms build with a plain kill of the child.
- Tests: each tool; a table of path-escape cases (`..`, absolute path, symlink to outside, symlink to a new file, symlinked parent); edit uniqueness; shell timeout and kill; progress; policy decisions, including `Always` and deferral.
- Example `examples/coding`. Guide `docs/guides/coding-tools.md`, linked from `tools.md` and `approvals.md`.
- One live test, skipped without a key, that fixes a failing test in a temporary repository.
- Update `design/remaining-work.md` when the PR is ready.

## Later

- `WithReadRoots(dirs...)`: extra read-only directories. The skills track needs this.
- Prefix rules for `bash` and rules stored in a settings file.
- Read-before-edit staleness check. Multi-file patch tool. Image reads.
- Warning when the workspace is a subdirectory of a Git repository.
- Windows process-tree kill. OS sandbox for the shell.

## Out of scope

- Web fetch, web search, and media tools. Dive keeps them in the same toolkit. They need credentials and belong in other packages.
- Output bounding. The tool-output track owns it.
- The approval screen and the question screen. The CLI owns them.
- Config files and flags. The CLI owns them.
- Network isolation for the shell.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Symlink race between check and write | A write escapes the workspace | Resolve at use time. Open files with the resolved path. Test the race case. Accept a remaining time-of-check gap and document it. |
| Shell reaches outside the workspace | File tools no longer set the boundary | Approve every command. State the limit in the guide. |
| `rg` output differs from the Go fallback | The model sees two formats | Share one formatter. Test both against the same fixture. |
| Prompt blocks a parallel batch | Slow turns | Document that the hook holds the batch. The CLI shows one prompt at a time. |
| Two days to build | Cut work at demo time | The first slice list is the cut line. Later items do not block the demo. |

## Open questions

None block the first slice. The CLI track may ask for a diff preview in `Request`. Add a `Preview string` field when it does.
