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

## Approve project servers

A `.mcp.json` file arrives with a repository, and a stdio entry runs a program.
`LoadProjectFile` marks each server `Untrusted`. `Open` starts an untrusted
server only when `Options.Approve` returns true. A nil callback declines.

`Approve` runs once for each untrusted server, one at a time, in name order,
before any server starts. It receives the raw entry, so the prompt can show the
exact command and the variable names. It also receives `Server.Fingerprint()`,
a hash of the raw entry and its file path. To remember an answer, key it on the
fingerprint. A changed command, argument, environment entry, URL, header, type,
or file path gives a new fingerprint, so the host asks again. This includes a
changed variable name in the text, such as `${A}` to `${B}`. The value of a
variable is not part of the fingerprint. A rotated secret does not ask again.

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

Text, image, and audio blocks keep their order. A result with `IsError` is a
failed outcome that the model sees. Structured content becomes one JSON text
block, unless a text block already holds it. A resource link and a text
resource become text. A binary resource becomes a text note with its type and
size, and the server status gets a warning.

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

The `mcp/example` program asks before it starts each server and before each
call. See its package comment for a test server that you can run without a
server of your own. The package tests run a small test server in
`mcp/internal/testserver`, with a scripted model and no network.
