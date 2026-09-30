# MCP tools in the CLI

Dashi can use project MCP tools beside its repository tools. Configure servers
under `mcpServers` in `~/.dashi/settings.json`, workspace `.mcp.json`, or a
file named with repeatable `--mcp-config FILE` flags. Explicit paths resolve
from the invocation directory. Stdio children run in the selected canonical
workspace, including when you launch with `--workspace` from elsewhere.

```json
{"mcpServers":{"docs":{"command":"documentation-server","args":["serve"],
  "env":{"TOKEN":"${DOCS_TOKEN}"}}}}
```

The CLI reads configuration files once. Later definitions replace earlier
ones, except a project entry cannot replace a trusted user/explicit entry.
Explicitly naming the project file makes that selection trusted. Missing
default files are quiet. Broken files warn and leave other files usable.

## Startup and calls

Project servers require startup consent before any server starts. Review the
source, command/arguments or HTTP destination, stdio directory, and variable
binding names. Literal credentials and variable defaults are redacted. Choose
“Start for this Dashi run” or “Do not start”. Consent lasts for this process;
new processes ask again. The stdio child inherits the process environment.

Trusted user/explicit entries need no startup prompt. Print mode and piped
input cannot approve project startup. `--yes` approves tool calls only; it
never supplies startup consent. New requests and continuations wait until
startup finishes. Input submitted before then asks you to resubmit it.

Tool calls still need approval of the qualified name and exact arguments.
Startup consent, trusted configuration, and remote read-only claims grant no
call or workspace permission. Cancellation reaches the server. A lost response
never triggers an automatic tool retry.

## Inspect and stop

`/mcp` shows source selection, ignored conflicts, connection state, timeout,
and the qualified/original tool names. It remains available during work and
opens no connection. `/resources` uses the same inventory. Failed or declined
servers do not prevent work with usable servers or repository tools.

The catalog is fixed for the run. If a server advertises changed tools, finish
work and restart Dashi. Session changes retain the existing runtime. Exit,
canceled startup, and failed initialization close owned connections and stdio
children. Recovery uses the runner's ordinary recorded-definition checks.

`MCP_TIMEOUT` accepts positive milliseconds for each server's startup and tool
discovery. The default is 10 seconds. Invalid or overflowing values warn and
use that default. Diagnostics never display raw server stderr or credentials.

## HTTP and compatibility

Stdio and streamable HTTP are supported. HTTP URLs require a literal scheme
and authority; URL userinfo and authority substitutions are rejected before
expansion or requests. Environment references may appear in the path.

Any configured HTTP header requires HTTPS, except literal `127.0.0.0/8` or
exactly `::1` loopback addresses. `localhost` and mapped/abbreviated IPs do not
qualify. Redirects are refused without requests to their targets.

OAuth, credential helpers, and legacy SSE remain unsupported. Inventory
identifies these gaps. Use streamable HTTP with caller-supplied headers where
possible. No reconnect, refresh, server installation, or browser login is
provided by this slice.
