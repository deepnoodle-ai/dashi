# PRD: local MCP client

| Field | Value |
| --- | --- |
| Status | Draft, revised 2026-09-28 for the demo slice |
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

Success: an integration test has a scripted model call a tool on a real test
server over stdio and streamable HTTP. The demo step "the agent calls a tool
from a configured MCP server" works in the `dashi` CLI.

## Non-goals

- OAuth flows and token storage. Dive's token store is 216 lines and adds
  keychain code. Record it as a follow-up.
- Legacy SSE, MCP prompts, resources, sampling, roots, elicitation, and building
  MCP servers.
- Approval policy. The host decides. MCP hints never grant permission.
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
}

func ParseConfig(data []byte) (Config, error)      // reads the "mcpServers" key
func LoadFile(path string) (Config, error)         // trusted: user or flag file
func LoadProjectFile(path string) (Config, error)  // servers are Untrusted
func Merge(cfgs ...Config) Config                  // later files win per name

type Options struct {
    // Approve runs once per Untrusted server, before it starts. A false
    // result skips the server. Nil means decline. Zero value is safe.
    Approve func(ctx context.Context, name string, s Server) (bool, error)
    // ConnectTimeout bounds each server's start and discovery. Zero is 30s.
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
    Warnings     []string // renamed tools, degraded content
}
type ToolInfo struct{ Server, Remote string; ReadOnlyHint bool }
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
program, or a timeout becomes a `Failed` status. Servers start in parallel.

## Flows

**Config.** The file format is `{"mcpServers": {"github": {"command": "gh-mcp",
"args": ["serve"], "env": {"TOKEN": "${GH_TOKEN}"}}}}`. The loader expands
`${VAR}` and `${VAR:-default}` in command, args, env, URL, and headers. An unset
variable expands to empty text and adds a warning. The parser ignores other
keys, so it reads a settings file that has more content.

**Approval.** `Open` calls `Approve` for each Untrusted server. The CLI asks
"Start server 'github' from .mcp.json?" and may remember the answer. The package
stores no answer. `Merge` keeps the trust flag of the winning entry. A user file
entry overrides a project entry of the same name, so the project cannot replace
a trusted server.

**Discovery and names.** After connect, the client reads every page of
`tools/list`. The model sees `mcp__<server>__<tool>`, as in Claude Code. The
client replaces characters outside `[A-Za-z0-9_-]` and limits names to 64
characters. When two names collide, the client adds a short hash to the later
one and records a warning. It does not fail. Dispatch uses the original remote
name. The definition keeps the raw input schema and hints as annotations.

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
and the status becomes `Failed`. `Refresh` restarts named servers (all if none
named) and re-lists tools. The CLI calls it between turns, never during one,
because the runner requires the offered definition to match at dispatch. The
client never retries a call. A transport error after send returns a failed
outcome whose text says the call may have run. Close stops all servers and is
safe to call twice.

## Acceptance criteria

1. `ParseConfig` reads stdio and http entries, expands variables, and rejects
   nothing fatally. An invalid entry appears as a `Failed` status.
2. `Merge` lets later files win. An untrusted entry never replaces a trusted
   one.
3. An Untrusted server does not start when `Approve` is nil or returns false.
   The test server records that it never ran.
4. A scripted model calls `mcp__test__echo` on the test server over stdio and
   sees the result in its next request. The same test passes over HTTP.
5. A test hook that denies `mcp__test__echo` blocks the call. The server sees no
   request.
6. A broken server (bad command) gives a `Failed` status with the error. The
   other server still works. The agent still runs.
7. Results: image, structured, error, and binary-resource cases each map as
   described. A test covers each.
8. Cancel during a slow call returns quickly. The test server sees the cancel.
9. Two tools with colliding names both appear, one with a hash suffix, and both
   dispatch to the right remote name.
10. `Refresh` picks up a tool that the test server adds. `Close` ends child
    processes.

## Slice for Tuesday night

In: everything above, the test server (`mcp/internal/testserver`: `echo`, `add`,
`fail`, `image`, `structured`, `slow`), the runnable example (`mcp/example`),
the guide `docs/guides/mcp.md`, and the `packages.md` entry. Tests run under the
scripted provider with no network.

Later: `tools/list_changed` notifications with an `OnChange` callback; OAuth and
token store; a runner marker for uncertain effect, so a lost response stops for
host reconciliation ([draft](06-local-mcp-research.md)); SSE; resources and
prompts; a bound on result size once roadmap 11 lands. After the CLI PR merges,
the CLI track wires `Open`, the approval prompt, and a `/mcp` status screen
(below).

```
/mcp
  github  connected  12 tools  .mcp.json
  docs    failed     exec "docs-mcp": not found in $PATH
  files   declined   not approved
```

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| SDK API changes | Rework | Pin v1.7.0. Keep SDK types out of the public API. |
| Orphan server processes | Leaked processes | Close stops each child. Test it. |
| Uncertain effect after a lost response | Model may repeat a mutating call | Text says the call may have run. Runner marker is follow-up. |
| Provider rejects a result block | Turn fails | Convert audio and binary to text when the provider lacks support. Test per route later. |

## Decisions

1. **Own module.** The SDK brings an HTTP and OAuth stack. The core module stays
   lean. Gemini already sets the pattern.
2. **Approval is a callback in `Open`.** The library owns the rule "untrusted
   never starts". The host owns the prompt and its memory. This makes the safe
   path the short path.
3. **Server failure is status, not error.** Program rule: warn and adjust. The
   agent must run when one server breaks.
4. **Names use `mcp__server__tool`.** This matches Claude Code, so users keep
   their permission rules. Collisions get a hash, not a failure.
5. **Fixed snapshot with explicit `Refresh`.** The runner matches definitions at
   dispatch. A live swap could break a pending call.
6. **OAuth deferred.** Static headers, env expansion, and the `HTTPClient` hook
   cover the demo and host-owned auth.
7. **Uncertain effect is text for now.** The correct fix needs a runner change.
   That change does not block the demo.
