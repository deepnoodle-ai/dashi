# Using MCP servers

The `mcp` package connects an application to MCP servers. It gives their tools
to an agent as a `tool.Toolset`. It is a separate module:

```sh
go get github.com/deepnoodle-ai/dashi/mcp
```

The package supports stdio and streamable HTTP servers. It does not support
OAuth, legacy SSE, prompts, resources, or sampling.

## Load and open

A config file lists servers under the `mcpServers` key. This is the format of
`.mcp.json`. The parser ignores other keys, so it also reads a settings file.

```json
{"mcpServers": {"github": {"command": "gh-mcp", "args": ["serve"],
  "env": {"TOKEN": "${GH_TOKEN}"}}}}
```

```go
user, _ := mcp.LoadFile(userSettings)
proj, _ := mcp.LoadProjectFile(".mcp.json")
ts, err := mcp.Open(ctx, mcp.Merge(user, proj), mcp.Options{Approve: ask})
defer ts.Close()
runner := dashi.Runner{Tools: tool.Join(localTools, ts), Hooks: hooks}
```

`LoadFile` and `LoadProjectFile` return an error for a missing file (match it
with `fs.ErrNotExist`) and for invalid JSON. Warn and continue without that
file.

`Open` expands `${VAR}` and `${VAR:-default}` in the command, arguments,
environment, URL, and headers. It expands them when it starts the server. An
unset variable becomes empty text and adds a warning to the server status.

`Open` returns an error only when its context ends. A server that fails to
start gets the state `failed` in `Status`, and the agent runs without it.
`Options.ConnectTimeout` limits each server (default 10 seconds).

Set `Options.Dir` to the directory where stdio children should run. Relative
arguments resolve there. An empty value inherits the host process directory.
The option does not change the process directory or affect HTTP servers.
`ExampleOptions_Dir` proves the selected directory with a real child process.

## Approve project servers

A `.mcp.json` file arrives with a repository, and a stdio entry runs a program.
`LoadProjectFile` marks each server `Untrusted`. `Open` starts an untrusted
server only when `Options.Approve` returns true. A nil callback declines.

`Approve` runs once for each untrusted server, one at a time, in name order,
before any server starts. It receives the raw entry, so the prompt can show the
exact command and the variable names. It also receives `Server.Hash()`,
a hash of the raw entry and its file path. To remember a decision, key it on the
hash. A changed command, argument, environment entry, URL, header, type, or
file path gives a new hash, so the host asks again. This includes a changed
variable name in the text, such as `${A}` to `${B}`. The value of a variable
is not part of the hash. A rotated secret does not ask again.

`Open` declines a server when `Approve` returns false or an error. `Refresh`
never starts a declined server.

`Merge` never lets an untrusted entry replace a trusted entry of the same name.
The trusted entry stays, with a warning.

## Tool names

The model sees `mcp__<server>__<tool>`. Characters outside `A-Za-z0-9_-`
become `_`. A name longer than 64 characters is cut and gets a short hash. The
name depends only on the server name and the tool name, so it is the same on
every open. If two tools get the same name, the tool of the first server (by
sorted name) stays. The other is dropped and reported in `Status`.

`Toolset.Info(name)` returns the server and remote name of a tool. A
`BeforeTool` hook can use it to apply a rule for one server.

## Approve calls

MCP tool calls pass the same `BeforeTool` hooks as every other tool. The
package copies the destructive and open-world hints to
`Definition.Annotations`. It never sets the read-only or idempotent hint,
because a server cannot vouch for itself. After an interrupted run, the runner
does not repeat an MCP call whose result is unknown. A hook that trusts MCP
servers must say so on its own.

## Results

Text and supported media keep their order. `ConvertResult` converts a completed
MCP SDK result without a Toolset or connection. `ConvertResultJSON` converts a
saved complete result object with a `content` array. It uses the SDK to decode
polymorphic content and preserves exact structured JSON numbers. Structured
content becomes a JSON text block unless existing text carries the same exact
value. Typed conversion preserves its supplied value; it cannot restore numbers
rounded before the call.

Media bytes are copied. Header validation guarantees typed conversion for
`image/png`, `image/jpeg`, `image/gif`, `image/webp`, `audio/wav`, and `audio/mpeg`.
Aliases normalize: `image/jpg` to `image/jpeg`, `audio/wave` and `audio/x-wav`
to `audio/wav`, and `audio/mp3` to `audio/mpeg`. Header identity does not prove
complete codec integrity or provider support. Other valid image/audio types,
such as SVG, become ordered omission notes with warnings. Invalid MIME, empty
bytes, or a mismatched guaranteed-format header return an error.

Resource links become descriptions containing their URI and available metadata.
Text resources keep their URI and text. Binary resources become notes with URI,
type, and size plus an omission warning. Conversion never fetches resources.
Discarded MCP metadata and annotations produce warnings without their values.
Toolset records conversion warnings in server status and turns unusable completed
results into the runner's usual failed outcome.

Select conversion using the admitted tool's output format. JSON shape cannot
establish MCP provenance. Keep original output, tool/version, format, and journal
failure in the application journal. Either MCP `IsError` or journal failure must
remain failed. For a host without an additional-input flow:

```go
// The admitted tool format selected MCP conversion before reading JSON.
outcome, warnings, err := mcp.ConvertResultJSON(journal.Output)
switch {
case errors.Is(err, mcp.ErrInputRequired):
	outcome = tool.Failed("This host does not support additional MCP input.")
case err != nil:
	outcome = tool.Failed("Tool completed, but its result could not be used.")
}
if journal.IsError { outcome.State = tool.StateFailed }
reportWarnings(warnings)
command := pending.Result(outcome) // Save through the existing host command path.
```

Keep the journal when conversion fails. Retry persistence after uncertain save
acknowledgment and never repeat completed execution to fix its content.
`ErrInputRequired` identifies an incomplete interaction. A host that supports
input keeps work pending while its permission/input flow fulfills the original
requests, then resumes the same MCP request with saved request state and input
responses. Toolset has no input handler and records a safe unsupported-input
failure without retry. Bound input sizes before conversion; output remains
untrusted model input. `ExampleConvertResult`, `ExampleConvertResultJSON`, and
`ExampleConvertResultJSON_unusable` demonstrate screenshot conversion, exact
numbers, retained failure, and safe journal-preserving settlement without a server.

If the connection drops after a call is sent, the error text says the call may
have run. The client never retries a call. Cancelling the context sends an MCP
cancel and returns at once.

## Refresh and close

The tool list is a snapshot. The runner needs the definition of a tool to match
when it runs the call, so the list changes only when you call `Refresh`. Call
it between turns, not during one.

`Refresh` lists the tools again for live servers and reconnects failed servers.
It never restarts a live server and never asks for approval again.
`ServerStatus.Changed` is true when a server reported a change in its tools. If
a stdio process exits, its status becomes `failed`, and calls to it fail with a
clear message until `Refresh` reconnects it.

`Close` stops every server and its child process. It is safe to call twice.

## Try it

Run a complete example without an API key, config file, or separate server:

```sh
cd mcp
go run ./example/keyless
```

The program starts its own stdio MCP server, calls its `add` tool through a
Dashi agent, and closes the child process. `dashitest` supplies fixed model
replies. The MCP connection and tool result are real:

```text
MCP server demo: connected (1 tool)
MCP result: {"sum":5}
Scripted reply: 2 + 3 = 5.
```

The `mcp/example` program asks before it starts each server and before each
call. See its package comment for a test server that you can run without a
server of your own. The package tests run a small test server in
`mcp/internal/testserver`, with a scripted model and no network.
