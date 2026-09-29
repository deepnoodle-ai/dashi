# PRD: local MCP client

| Field | Value |
| --- | --- |
| Status | Draft, revised 2026-09-28 after independent review |
| Roadmap | [Item 6](../remaining-work.md), size M |
| Evidence | [Source review](06-local-mcp-research.md) |

## Problem

Developers keep MCP servers in `.mcp.json` or a user settings file. Dashi cannot
use them. An application must write its own transport, discovery, naming, result
conversion, and cleanup. It must also decide how the bridge meets approvals.

A project `.mcp.json` arrives with a repository. A stdio entry runs a command on
the user's machine. A naive client runs it at startup. That is a security hole,
so approval must be part of the design.

The user's job: "I listed my servers once. The agent uses their tools. I approve
what runs. A broken server does not stop my work."

## Goals

- One package turns configured MCP servers into a `tool.Toolset`.
- `.mcp.json` and settings files work as they do in Claude Code and Dive.
- A project server never starts without host approval.
- MCP tool calls pass the same `BeforeTool` hooks as every other tool.
- A failed server never stops the agent. The user sees why it failed.

Success: a scripted model calls a tool on a real test server over stdio and
streamable HTTP. The demo step "the agent calls a tool from a configured MCP
server" works in the `dashi` CLI.

## Non-goals

- OAuth flows and token storage. Dive's token store is 216 lines and adds
  keychain code. Record it as a follow-up.
- Legacy SSE, MCP prompts, resources, sampling, roots, elicitation, and building
  MCP servers.

- Result-size bounds (roadmap 11) and tool progress display.

## Public API

New Go module `github.com/deepnoodle-ai/dashi/mcp`. It uses the official Go SDK.
The library module gains no dependency.

```go
// Config is the merged set of servers. Keys are server names.
type Config struct{ Servers map[string]Server }

type Server struct {
    Type    string            // "stdio" (default when Command is set) or "http"
    Command string            // stdio: program to run
    Args    []string
    Env     map[string]string // added to the parent environment
    URL     string            // http: streamable HTTP endpoint
    Headers map[string]string
    Disabled bool
    Source   string           // file that defined it; set by the loaders
        Untrusted bool           // needs Options.Approve; set by LoadProjectFile
    Warnings []string        // parse problems, unset variables
}

// Fingerprint is a stable hash of the raw entry (before variable expansion):
// command, args, env, URL, headers, and the file path.
func (s Server) Fingerprint() string


func ParseConfig(data []byte) (Config, error)      // reads the "mcpServers" key
func LoadFile(path string) (Config, error)         // trusted: user or flag file
func LoadProjectFile(path string) (Config, error)  // servers are Untrusted
func Merge(cfgs ...Config) Config                  // later files win per name

type Options struct {
    // Approve runs once per Untrusted server, in sorted name order, one at a
    // time, before any server starts. It gets the raw entry and its
    // fingerprint. A false result skips the server. Nil means decline.
    Approve func(ctx context.Context, name string, s Server, fingerprint string) (bool, error)
    // ConnectTimeout bounds each server's start and discovery. Zero is 10s.
    ConnectTimeout time.Duration
    // HTTPClient lets the host add per-server auth to HTTP servers. Optional.
    HTTPClient func(name string, s Server) *http.Client
}

func Open(ctx context.Context, cfg Config, opts Options) (*Toolset, error)

func (t *Toolset) Tools(ctx context.Context) ([]tool.Tool, error)
func (t *Toolset) Status() []ServerStatus
func (t *Toolset) Info(toolName string) (ToolInfo, bool)
func (t *Toolset) Refresh(ctx context.Context, names ...string) error
func (t *Toolset) Close() error

type ServerStatus struct {
    Name, Source string
    State        State // Connected, Failed, Declined, Disabled
    Tools        int
        Err          error    // set when Failed
    Changed      bool     // server said its tools changed; call Refresh
    Warnings     []string // config problems, dropped or degraded tools
}
type ToolInfo struct{ Server, Remote string }
```

Use in an application:

```go
user, _ := mcp.LoadFile(userSettings)
proj, _ := mcp.LoadProjectFile(".mcp.json")
ts, err := mcp.Open(ctx, mcp.Merge(user, proj), mcp.Options{Approve: ask})
defer ts.Close()
runner := dashi.Runner{Tools: tool.Join(localTools, ts), Hooks: hooks}
```

`Open` returns an error only if the context ends. A bad entry, a missing
program, or a timeout becomes a `Failed` status. Approved servers start in
parallel. Invalid JSON in a whole file makes `ParseConfig` and `LoadFile` return
an error. The CLI warns and continues without that file.


## Flows

**Config.** The file format is `{"mcpServers": {"github": {"command": "gh-mcp",
"args": ["serve"], "env": {"TOKEN": "${GH_TOKEN}"}}}}`. The loader expands
`${VAR}` and `${VAR:-default}` in command, args, env, URL, and headers. An unset
variable expands to empty text and adds a warning on the `Server`. `Open` copies
it into `Status`. The parser ignores other keys, so it reads a settings file
that has more content.

**Approval.** `Open` calls `Approve` once per Untrusted server, one at a time,
in sorted name order, before it starts any project server. The callback gets the
raw entry, so the prompt shows the exact command or URL and the variable names
it reads. It also gets the fingerprint. The CLI asks "Start server 'github' from
.mcp.json?" and may remember the answer, keyed on the fingerprint. A changed
command, args, env, URL, or headers gives a new fingerprint, so the CLI asks
again. The package stores no answer. `Merge` keeps the user entry when a project
entry has the same name, and adds a warning to that server's status.

**Discovery and names.** After connect, the client reads every page of
`tools/list`. The model sees `mcp__<server>__<tool>`, as in Claude Code. The
name depends only on the server name and the remote tool name. The client
replaces characters outside `[A-Za-z0-9_-]`. It cuts names longer than 64
characters to 57 and appends `_` and 6 hex characters of a hash of the full
name. If two tools still get the same name, the client sorts servers by name,
keeps the first tool, drops the later one, and reports the drop in `Status`.
Start order never changes a name. Dispatch uses the original remote name. The
definition keeps the raw input schema and hints as annotations.

**Calls.** `BeforeTool` hooks run first. A hook can use `Info` to find the
server. The executor then calls the server with the run context. Context cancel
sends MCP cancellation and returns.

**Results.** Text, image, and audio blocks keep their order. If a server returns
`IsError`, the outcome is `StateFailed` with its content. Structured content
becomes one JSON text block, only when no text block already carries it. A
resource link becomes a text line with name and URI. Embedded text resource
becomes text with its URI. A binary resource becomes a text note with its type
and size. The note adds a status warning. Nothing vanishes silently.

**Failure and lifecycle.** A failed server has `Failed` status, and its tools
are absent. If a stdio process exits later, its calls fail with a clear message
and the status becomes `Failed`. `Refresh` re-lists tools on live servers and
reconnects only `Failed` servers. It never restarts a live server. A
`tools/list_changed` notice sets `Changed` on that server's status, so the CLI
refreshes only when a server changed or failed. The CLI calls `Refresh` between
turns, never during one, because the runner requires the offered definition to
match at dispatch. The client never retries a call. A transport error after send
returns a failed outcome whose text says the call may have run. Close stops all
servers and is safe to call twice.

## Acceptance criteria

1. `ParseConfig` reads stdio and http entries, expands variables, and rejects
   nothing fatally. An invalid entry appears as a `Failed` status.
2. `Merge` lets later files win. An untrusted entry never replaces a trusted
   one, and the user entry gets a warning.
3. An Untrusted server does not start when `Approve` is nil or returns false.
   The test server records that it never ran. `Approve` runs in sorted order,
   never concurrently. A changed command, args, or URL changes the fingerprint.
   An unchanged raw entry keeps it.
4. A scripted model calls `mcp__test__echo` on the test server over stdio and
   sees the result in its next request. The same test passes over HTTP.
5. A test hook that denies `mcp__test__echo` blocks the call. The server sees no
   request.
6. A broken server (bad command) gives a `Failed` status with the error. The
   other server still works. The agent still runs.
7. Results: image, structured, error, and binary-resource cases each map as
   described. A test covers each.
8. Cancel during a slow call returns quickly. The test server sees the cancel.
9. The same config gives the same tool names on every open, in any start order.
   A name collision keeps the tool of the first server by sorted name, drops the
   other, and reports it in `Status`. A name over 64 characters gets a stable
   short name.
10. `Refresh` picks up a tool that the test server adds. It does not restart a
    live server (the process ID stays). It reconnects a server that exited.
    `Close` ends child processes.

## Slice for Tuesday night

In: everything above, the test server (`mcp/internal/testserver`: `echo`, `add`,
`fail`, `image`, `structured`, `slow`), the runnable example (`mcp/example`),
the guide `docs/guides/mcp.md`, and the `packages.md` entry. Tests run under the
scripted provider with no network.

Later: an `OnChange` callback for `tools/list_changed` (the `Changed` flag ships
now); OAuth and token store; a runner marker for uncertain effect, so a lost
response stops for host reconciliation ([draft](06-local-mcp-research.md)); SSE;
resources and prompts; a bound on result size once roadmap 11 lands. After the
CLI PR merges, the CLI track wires `Open`, the approval prompt, and a `/mcp`
status screen (below).

```
/mcp
  github  connected  12 tools  .mcp.json
  docs    failed     exec "docs-mcp": not found in $PATH
  files   declined   not approved
```

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |

| `${VAR}` in a project entry sends a secret to a repo-chosen URL | Secret leak | The prompt shows variable names. Approval binds to the fingerprint. |
| Orphan server processes | Leaked processes | Close stops each child. Test it. |
| Uncertain effect after a lost response | Model may repeat a mutating call | Text says the call may have run. Runner marker is follow-up. |
| Provider rejects a result block | Turn fails | Convert audio and binary to text when the provider lacks support. Test per route later. |

## Decisions

1. **Own module.** The SDK brings an HTTP and OAuth stack. The core module stays
   lean. Gemini already sets the pattern.
2. **Approval is a callback in `Open`, keyed on a fingerprint.** The library
   owns the rule "untrusted never starts". The host owns the prompt and its
   memory. A remembered approval holds only for that fingerprint, so a
   repository cannot change a command after approval. Prompts run one at a time,
   in sorted order. Same name in user and project files: the user entry wins,
   with a warning. (Review 1, 4.)
3. **Server failure is status, not error.** Program rule: warn and adjust. The
   agent must run when one server breaks.
4. **Names use `mcp__server__tool`, from server and tool name only.** This
   matches Claude Code, so users keep permission rules and resumed sessions
   (demo step 7) keep working. A collision drops the later tool in sorted server
   order and reports it. Start order never decides. (Review 2.)
5. **Fixed snapshot with explicit `Refresh`.** The runner matches definitions at
   dispatch. A live swap could break a pending call. `Refresh` re-lists live
   servers and reconnects only failed ones. (Review 3.)
6. **OAuth deferred.** Static headers, env expansion, and the `HTTPClient` hook
   cover the demo and host-owned auth.
7. **Uncertain effect is text for now.** The correct fix needs a runner change.
   It does not block the demo.
8. **Review points 5, 6, 7 accepted.** Warnings live on `Server` and reach
   `Status`. `ToolInfo` drops the read-only hint (`Definition.Annotations` holds
   it). Connect timeout is 10 seconds. No background connect. MCP hints never
   grant permission.
