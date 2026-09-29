# PRD: coding tools and approval policy

| Field | Value |
| --- | --- |
| Status | Implemented (first slice) |
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
	Choices []Choice // empty means free text
}
type Choice struct{ Label, Description string }
type Answer struct{ Text string }

// Approval policy for Runner.Hooks.
func NewPolicy(ask ApprovalFunc, opts ...PolicyOption) *Policy
func (p *Policy) Hooks() dashi.Hooks
func TrustTools(names ...string) PolicyOption // tools that skip the prompt

type ApprovalFunc func(ctx context.Context, r ApprovalRequest) (Approval, error)
type ApprovalRequest struct {
	Tool    string
	Summary string          // "edit main.go", "run: go test ./..."
	Preview string          // unified diff for write_file and edit_file
	Dir     string          // working directory, for bash
	Command string          // exact command text, for bash
	Args    json.RawMessage // the exact arguments
}
type Approval struct {
	Allow    bool
	Remember bool   // remember this answer while the Policy exists
	Reason   string // shown to the model when Allow is false
}

// ShellProgress is the Data of each bash progress report.
type ShellProgress struct{ Lines []string } // last 20 output lines
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

Each tool sets `Annotations.ReadOnly`. `ask_user` counts as read-only because the question is the prompt. `bash` takes an optional `timeout` argument in milliseconds. The tool clamps it to the maximum and adds a note to the result when it does.

## Behavior

**Paths.** A relative path resolves against the workspace. An absolute path must lie inside it. The check resolves symlinks in the deepest existing part of the path, so a link that points outside is refused. For a new file, the check applies to the parent directory. The error says: `path "../secret" is outside the workspace /repo. Use a path inside it.`

**Edit errors.** A missing string gives: `old_string not found in main.go`. A repeated string gives the count and the line numbers: `old_string appears 3 times (lines 12, 40, 88). Add surrounding lines to make it unique, or set replace_all.` Both return as tool errors, so the model sees them and retries.

**Shell.** The command runs under `bash -c`, or `sh -c` when bash is absent. The working directory is the workspace. Standard input is empty. The tool merges standard output and error in order. The default timeout is 2 minutes. The maximum is 10 minutes. On timeout or cancel, the tool kills the whole process group and returns the output so far. A non-zero exit returns a failed outcome with the output and the exit code, so the screen shows a failing test as a failure.

**Progress.** While `bash` runs, it calls `tool.Report` at most four times a second. `Message` is the last output line. `Data` holds a `ShellProgress` value. The report is live only, as `tool/progress.go` defines.

**Policy.** For each call, the policy decides as follows:

1. A read-only tool from this package runs with no prompt. So does a tool that the host names with `TrustTools`.
2. A remembered rule for the call allows it.
3. Any other tool calls `ApprovalFunc`. This includes MCP tools, even when they carry a `ReadOnly` annotation. An annotation is a hint from the tool's author, so the policy does not trust it.
4. `Approval.Allow` returns `dashi.Allow`. A refusal returns `dashi.Deny` with the reason. The default reason tells the model not to retry the same call.
5. When `ask` is nil, the policy returns `dashi.Defer`. This supports the durable approval flow with no extra code.

The request for `write_file` and `edit_file` carries a unified diff in `Preview`. For `edit_file` the diff comes from the file and the replacement. For `write_file` it compares the old file, or an empty file, with the new content. The request for `bash` carries the exact command and the working directory. The diff that the demo shows after a command is `git diff`, run by the CLI. It is not a library feature.

`Remember` stores a rule in the `Policy` value. The rule lives as long as the `Policy` lives, and the host decides that. A new `Policy` starts with no rules, so a reopened CLI session forgets them. The rule for `write_file` and `edit_file` is the tool name. The rule for `bash` is the exact command text. A prefix rule for `bash` is unsafe, because `go test ./... && rm -rf ~` starts with `go test`. The policy asks one person at a time: a mutex serializes prompts from parallel calls.

## Decisions

1. **Package name `workspace`.** It names the boundary that the tools enforce. `Tools(dir)` reads well at the call site.
2. **`Tools` returns an error.** A missing directory is not safe to adjust. The brief showed one return value. The error is the cost of failing early.
3. **The policy trusts only this package and a host list.** The review found that a `ReadOnly` hint from an MCP server could skip every prompt. The policy now skips the prompt for this package's read-only tools and for names in `TrustTools`. Every other tool asks. The MCP PRD must keep `ReadOnly` unset unless the host trusts the server.
4. **Snake-case tool names, `bash` for the shell.** Models know `read_file`, `edit_file`, and `bash`. The names carry no version or product prefix.
5. **Non-zero exit is a failed outcome.** The tool ran, but the command failed. The person and the model both need to see that.
6. **The shell is not sandboxed.** The path checks cover the file tools only. A shell command can reach any path the user can. The approval prompt is the guard, and the guide says so. An OS sandbox is later work.
7. **`allow always` for the shell is exact-match.** This trades convenience for safety. Prefix rules wait for a parser that rejects operators.
8. **The read limit is the only size bound.** The tool-output track owns generic bounding.
9. **No read-before-edit rule.** The unique-match check already stops blind edits. A staleness check adds state and gives little in a first slice.
10. **Approval requests carry a diff.** The person must see the change that they approve. `Preview` is in the first slice. The CLI shows it.
11. **"Remember", not "session".** Dashi keeps "session" for a future concept. The rule lives as long as the `Policy` value. Persistence waits for the settings-file work.
12. **Distinct type names.** Dashi already has `HookVerdict`, `ToolDecision`, `Allow` and `Deny`. This package uses `ApprovalRequest` and `Approval`, and the callback's `Allow` field maps onto `dashi.Allow`. `Approval` adds `Remember`, so it is not a copy of `ToolDecision`.
13. **`bash` takes a `timeout` argument.** Long test runs need it. The tool clamps it and says so.
14. **`ShellProgress` is an exported type.** The CLI must parse `Data` to show live lines.
15. **No test for the symlink check-then-write race.** The test is unreliable. The tool opens the resolved path, and the guide documents the remaining gap.
. **`InDir` policy option.** The policy reads files to build diffs and must know the workspace. The PRD API had no way to say so. `InDir(dir)` sets it; the default is the current directory. A path outside the workspace gets no preview.
18. **Only an allow is remembered.** A remembered refusal of `edit_file` would block the tool for the whole session. `Remember` on a refusal is ignored.
19. **Rules for other tools match the exact arguments.** A tool that is not `write_file`, `edit_file`, or `bash` is remembered by name and exact argument text.
20. **The shell runs in its own session.** It has no terminal, so a command such as `git push` cannot prompt on `/dev/tty`. The process-group kill still works.
21. **grep and glob limits.** `grep` returns at most 250 lines by default (`head_limit`). `glob` returns at most 1000 files. Both say when they cut the result. Both skip `.git`, `node_modules`, and similar directories. The Go search also skips hidden files, as `rg` does. The tool `grep` output mode is `files`, not `files_with_matches`.
22. **Glob patterns.** A pattern with no `/` matches at any depth. `{a,b}` groups work.

## First slice (ship by Tuesday night)

- Package `workspace` with all eight tools, path scope, `Tools`, `NewPolicy`, and the options above.
- Unix process-group kill. Other platforms build with a plain kill of the child.
- Tests: each tool; a table of path-escape cases (`..`, absolute path, symlink to outside, symlink to a new file, symlinked parent); edit uniqueness; shell timeout and kill; progress; policy decisions, including `Remember` and deferral.
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
| Symlink race between check and write | A write escapes the workspace | Resolve at use time. Open files with the resolved path. Accept a remaining time-of-check gap and document it. |
| Shell reaches outside the workspace | File tools no longer set the boundary | Approve every command. State the limit in the guide. |
| `rg` output differs from the Go fallback | The model sees two formats | Share one formatter. Test both against the same fixture. |
| Prompt blocks a parallel batch | Slow turns | Document that the hook holds the batch. The CLI shows one prompt at a time. |
| Two days to build | Cut work at demo time | The first slice list is the cut line. Later items do not block the demo. |

## Open questions

None block the first slice.

## Review outcome

The independent review found seven items. Accepted: 1 (trust), 2 (preview), 3 (lifetime and wording), 4 (type names), 5 (`ShellProgress`), 6 (`timeout`), 7 (drop the race test). Rejected: none. Decisions 3 and 10 to 15 record the reasons.
