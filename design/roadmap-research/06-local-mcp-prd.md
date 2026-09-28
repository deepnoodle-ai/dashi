# PRD: local MCP client

| Field | Value |
| --- | --- |
| Status | Draft for discussion |
| Date | 2026-09-28 |
| Roadmap | [Item 6](../remaining-work.md), size M / high difficulty |
| Evidence | [Source review](06-local-mcp-research.md), [example-port gap](../gaps/F-example-ports.md) |

## User problem

A Dashi application can define a Go tool, yet cannot connect its own MCP server and give the model that server's tools. The developer currently has to build transport, discovery, naming, call/result conversion and cleanup, then decide how the bridge interacts with Dashi approvals and recovery. A tool that changes name or schema between presentation and execution can fail the turn; a silent conversion loss or automatic retry can be worse.

## Goals and non-goals

Provide one optional package that turns a caller-configured MCP server into a Dashi `tool.Toolset`. A developer should be able to connect, inspect the exposed names, hand the set to `Runner.Tools`, use a `BeforeTool` hook for approval, and close the connection. Preserve the important MCP result distinctions and make failures actionable. Use the official Go MCP SDK transport boundary so applications can choose stdio or HTTP and supply their own scoped authentication. Keep remote-server credentials out of persisted turns and tool definitions.

This slice does not build a tenant connection registry, OAuth browser flow, MCP server implementation, provider-hosted MCP authentication ([item 7](../remaining-work.md)), automatic server installation, or a broad JSON config loader. A Dashi package must not infer that a server's `readOnly`/`idempotent` hint grants permission or makes an uncertain call safe to replay.

## Proposed smallest API and flow

Add optional `mcpclient` with a single `Connect(ctx, Config{Name, Transport, AllowedTools}) (*Toolset, error)`. `Transport` is an `mcp.Transport`; `Name` is the developer-chosen stable server identity; `AllowedTools` is an optional list of **remote** names (nil means all). The returned value implements `tool.Toolset` and `Close() error`. `Connect` completes initialization and all discovery pages before returning; it exposes an immutable tool snapshot and a small `Catalog()` view of qualified name, remote name and annotations for configuration/diagnostics. It maps each remote `search` from server `docs` to `mcp__docs__search`, retaining `search` for RPC. Reject invalid/sanitization-colliding names and duplicate remote tools with a message naming both originals. Do not silently omit a requested allowlisted tool.

```go
set, err := mcpclient.Connect(ctx, mcpclient.Config{
    Name:      "docs",
    Transport: &mcp.CommandTransport{Command: exec.Command("docs-mcp")},
    AllowedTools: []string{"search"},
})
if err != nil { return err }
defer set.Close()
runner := dashi.Runner{Tools: set, Hooks: appHooks}
```

For HTTP, the application supplies a transport with its own HTTP client/auth mechanism. This keeps per-user or per-tenant credentials in the host's connection scope, not in a model-facing definition, `tool.Call`, or recorder. One set represents one server in the first slice; applications can combine its `Tools(ctx)` result with their own `tool.List` for a fixed loadout. A later small `tool.Combine` helper may make multiple live sets ergonomic without embedding tenant configuration in Dashi.

Discovery is a connection-time snapshot. MCP `tools/list_changed` can be reported as “refresh available,” but this slice does not mutate the active snapshot mid-turn. An explicit replacement connection or refresh between turns is the safe first behavior: Dashi's runner matches the recorded definition before dispatch. Calls validate object arguments and use the exact remote name. Protocol `IsError` becomes a model-visible `tool.StateFailed` outcome. A proven pre-dispatch connection/credential failure may use `tool.Fatal`; once a request may have reached a server, a lost response is an **unknown effect**. Dashi currently settles returned executor errors as failed (`tools_loop.go:435-479`), so delivery requires a narrow executor/runner mechanism to record this uncertainty and stop for host reconciliation. The client never retries such a call automatically. Supported text, image and audio content preserve order. Structured content is encoded as explicit JSON model content when present; unsupported embedded/blob/resource forms fail explicitly rather than disappearing. Exact conversion details and duplication rules should be pinned by contract tests before implementation.

## Acceptance criteria

1. A developer can connect a stdio server and a streamable HTTP server through the same package, list their projected tools, run one through `Runner`, and close the sessions. Setup errors identify the server and phase (connect, list, convert, call).
2. Discovery consumes all pages, preserves raw input schema and advisory annotations, applies allowlists, and rejects ambiguous or invalid projected names before a model call. Model-facing names remain stable for a connection snapshot and dispatch uses original remote names.
3. An application's `BeforeTool` hook can allow, deny or defer a projected MCP call. With no hook opinion, bound local tools run by default. Annotations do not bypass a configured hook.
4. Tool results preserve supported block order and distinguish successful, protocol-error, transport-error and unsupported-content outcomes. Structured content is visible. No unknown block is silently dropped. Provider-route tests prove the chosen content mapping can be sent or reports a clear unsupported error.
5. Credentials stay in the caller-owned transport. Tests inspect recorded definitions, calls and turn facts for secret leakage. A session is closed on discovery failure and `Close` is idempotent.
6. A changed catalog/definition never dispatches under a stale binding. No automatic replay of a tool call after connection loss or an uncertain effect; a lost response after server execution records an uncertain call and stops for host reconciliation, rather than recording a known failed outcome.

## Risks and open decisions

- **Lifecycle vs. dynamic discovery:** OpenHands updates live tools on notifications, while Dashi records the offered definition. Decide whether a future refresh API swaps the snapshot only when no turn uses it, or whether a new set must be connected for each turn. First release keeps the snapshot fixed.
- **Result representation:** Dashi has content blocks but no native MCP `StructuredContent`/embedded-resource envelope. Choose the least surprising JSON text representation and define ordering when both structured and ordinary content are present. Avoid double-presenting the same value.
- **Credential scope:** A reusable set must not accidentally carry one user's HTTP credentials into another user's turn. Document one set per credential scope; a host-managed transport callback may be needed if the same server is shared across users.
- **Error boundary:** Protocol errors are ordinary tool failures; transport failure after egress may have changed external state. The necessary narrow `tool`/runner uncertainty marker must not turn every timeout into “not executed.” Host reconciliation remains application-owned.
