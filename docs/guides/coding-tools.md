# Coding tools

The `workspace` package gives an agent tools to work in one directory. It also
gives an approval policy for those tools. See [tools](tools.md) for how tools
work and [approvals](approvals.md) for hooks that approve calls.

## Try it

Set `OPENAI_API_KEY`, then run
`go run ./examples/coding -dir /path/to/repo "Fix the failing test."`
([example](../../examples/coding/main.go)).

```go
tools, err := workspace.Tools(dir, workspace.WithAsker(ask))
// Handle err, then:
policy := workspace.NewPolicy(approve, workspace.InDir(dir))
runner := dashi.Runner{
	Agent: agent, Client: client,
	Tools: tools, Hooks: policy.Hooks(),
}
```

`Tools` returns an error when `dir` is not an existing directory.

## The tools

| Tool             | Kind      | What it does                                             |
| ---------------- | --------- | -------------------------------------------------------- |
| `read_file`      | read-only | Returns numbered lines. Takes `offset` and `limit`.      |
| `list_directory` | read-only | Lists one directory. A name that ends in `/` is a directory. |
| `glob`           | read-only | Finds files by pattern, such as `**/*.go`. Newest first. |
| `grep`           | read-only | Searches contents. Uses `rg` when found, else Go.        |
| `write_file`     | mutating  | Writes a whole file. Creates parent directories.         |
| `edit_file`      | mutating  | Replaces one exact string. Fails when it is not unique.  |
| `bash`           | mutating  | Runs a command in the directory. Reports live output.    |
| `ask_user`       | read-only | Asks a person through `WithAsker`. Absent without it.    |

Each tool sets `Annotations.ReadOnly`. Use `WithoutTools` to remove tools.
The package does not bound the size of results, except `read_file` (2000
lines) and `grep` and `glob` (a result count). Bound results in the host.

Failures that the model can fix return as failed outcomes. The text names the
problem and the next step. For example, `edit_file` gives the line numbers of
each match when `old_string` is not unique.

## Path checks

A relative path resolves against the directory. An absolute path must lie
inside it. The file tools resolve symlinks before they check a path, and they
open the resolved path. A link that points outside the directory is refused,
also when the target file does not exist yet.

A small gap remains. Another process can replace a directory with a link
between the check and the open. Do not run the agent next to a process that
you do not trust.

## The shell has no sandbox

The path checks cover the file tools only. A shell command can reach any path
that your user can reach. Approval is the guard. Use a container or a separate
account when the risk is too high.

`bash` runs `bash -c`, or `sh -c` when bash is absent. It gives the command no
input and no terminal. The result merges output and error output in order.
A non-zero exit code returns a failed outcome with the output and the code.

The default timeout is 2 minutes and the maximum is 10 minutes. The model may
pass `timeout` in milliseconds. The tool reduces a longer value and says so.
On timeout or cancel, the tool kills the shell and its child processes on
Unix, and returns the output so far. `ShellConfig` changes these values.

While a command runs, `bash` calls `tool.Report` up to four times a second.
`Progress.Message` is the last output line. `Progress.Data` decodes to
`ShellProgress`, which holds the last 20 lines.

## Approval policy

`NewPolicy(ask).Hooks()` returns a `BeforeTool` hook. For each call:

1. A read-only tool of this package runs with no prompt. So does a tool that
   you name with `TrustTools`.
2. A remembered answer for the call allows it.
3. Any other tool calls your `ApprovalFunc`. This includes MCP tools, also when
   they carry a `ReadOnly` annotation. The tool's author sets that annotation,
   so the policy does not trust it.
4. `Allow` runs the call. A refusal denies it, and the model sees `Reason`.
5. A nil `ApprovalFunc` defers the call. The [durable
   flow](approvals.md#deferring-for-later) then applies.

`ApprovalRequest` carries a one-line `Summary`. For `write_file` and
`edit_file` it carries a unified diff in `Preview`. For `bash` it carries the
exact `Command` and the `Dir`. Pass `InDir` so that the policy can read the
files for previews.

`Remember: true` on an allow stores a rule in the `Policy`. The rule lives as
long as the `Policy`. A new `Policy` starts with none. For `write_file` and
`edit_file`, the rule covers the whole tool. For `bash`, it covers the exact
command text only. A prefix rule is unsafe: `go test ./... && rm -rf ~` starts
with `go test`. A refusal is never remembered.

The policy asks one question at a time. A prompt holds up the other calls of
its batch.
