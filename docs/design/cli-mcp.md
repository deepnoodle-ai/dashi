# CLI MCP startup and inventory

Status: Accepted · Author: Codex · Updated: 2026-09-30\
Workflow: Spec → root checkpoint → build, after addressing the named [PRD](../prds/cli-mcp.md) review.

## Context and boundaries

The CLI lacks MCP composition. The separate `mcp` module already owns parsing, protected merging, connections, snapshots, qualified names, and cleanup. Workspace policy already approves external tools. Keep those responsibilities. Add a flat CLI host for configuration selection, preflight, startup consent, and safe inventory. Turn records remain authoritative. OAuth/SSE remain I13.

| Location | Responsibility |
| --- | --- |
| `cmd/dashi/mcp.go` | Concrete runtime, source selection, preflight, consent projection, status/tool inventory, and module composition. |
| `options.go`, `app.go`, `wire.go`, `main.go` | Scheduled flags/configuration and two-phase startup/ownership. |
| `control.go`, `screen.go`, `ui_line.go` | `/mcp`, startup interaction, and printable presentation. |
| `mcp/toolset.go` | Small `Options.Dir` prerequisite; no CLI dependencies. |
| CLI/module guides and `go.mod` | User contract, separate-module dependency, and examples. |

Use concrete structs/functions, not a resource framework or transport implementation. The resource slice consumes the MCP inventory projection beside its own facts.

## Configuration and preflight

Parse repeatable `--mcp-config FILE`; reject empty values. Resolve explicit paths against invocation cwd before selecting the workspace. Select user settings, workspace `.mcp.json`, and explicit files in order. Read each configuration file exactly once into immutable bytes. Both raw preflight and `mcp.ParseConfig` consume those same bytes. Do not reread paths through `LoadFile`/`LoadProjectFile` or before `Open`.

Attach the absolute source path as `Server.Source`. Set `Server.Untrusted` true for the default project file and false for user/explicit selections. An explicit selection remains trusted when it names the project file. Keep raw per-source metadata beside `mcp.Merge` for selected/ignored-source inspection. Do not treat project `.dashi/settings.json` as a trusted MCP source.

Default absence is quiet; other file failures warn and exclude the whole file. Preserve valid files and servers.

Inspect the same input bytes for unsupported entry metadata before the module parser discards unknown fields. A non-null `oauth` entry and recognized `headersHelper` credential helpers are unsupported, not anonymous configurations. Legacy `type: sse` is unsupported. Unknown transport/missing command or URL gets a bounded failed status. Disabled entries stay disabled without expansion or startup. Carry these decisions through source selection so a later trusted definition can replace an unavailable candidate correctly.

Every HTTP entry requires a literal scheme and authority, regardless of configured headers. Reject scheme/authority substitutions, raw URL userinfo, and malformed URLs before environment lookup/expansion, client creation, or requests. Validate the raw URL before `mcp.Open`. Path substitution occurs only after validation fixes the authority. Expansion cannot change the scheme or authority. No trust answer or flag bypasses rejection.

Use header bindings for credentials. Every nonempty header map makes an HTTP entry credential-bearing. Require `https`, or `http` with a parsed literal IPv4 in `127.0.0.0/8` or exactly IPv6 `::1`. Do not resolve DNS or accept `localhost`, abbreviated IPs, or IPv4-mapped IPv6 as exemptions.

Exclude rejected entries from `Open` and retain their safe failure reason in inventory. This prevents the module from expanding their headers or creating clients. Header-free HTTP remains usable with a literal scheme/authority. Parse `MCP_TIMEOUT` with an overflow-safe positive-millisecond check; invalid values warn and select 10 seconds.

Supply an owned `http.Client` through `Options.HTTPClient`. Set `CheckRedirect` to return `http.ErrUseLastResponse` for all endpoints. The module clones this client when wrapping configured headers, preserving redirect refusal. Do not follow `Location` manually. A response outside the configured endpoint remains a failed connection/call under existing module semantics. This also blocks credential transfer on same-origin redirects and HTTP downgrades.

## Startup consent and UI ordering

Build the runtime with its frozen raw configuration and an initially empty forwarding Toolset. Building adapters opens no servers. Construct the Engine/Controller, bind the UI, and open the selected session before starting resource initialization. Start the initialization worker only after the screen event loop or line input loop can service prompts. The controller gates new prompts, queue admission, and continuation until initialization completes. Cancellation exits without admitting a turn. Print/unattended mode uses a startup callback that declines project entries without reading stdin.

Use a distinct startup request/presentation, not a model question or a remembered tool-call grant. Its two choices are “Start for this Dashi run” and “Do not start”. The module calls it serially in sorted name order before starting any eligible server. The callback's consent key is server name, raw `Server.Fingerprint()`, and canonical workspace root. Keep it in memory only. Denials remain blocked for this runtime. `/new` and session changes retain the runtime; process reopening creates fresh consent and connections.

Project review shows raw command/argv, source, canonical stdio directory, HTTP scheme/destination, and configured environment/header binding names. For each binding, show variable tokens such as `${OPENAI_API_KEY}` without resolving them. Replace literal fragments and `:-default` contents with a redacted indication. Show bindings containing only literals as `configured literal value (redacted)`. Redact configured literal credential fragments if repeated in command/argv. State that stdio inherits the process environment. Never enumerate inherited values. URL presentation shows only scheme/host/port and never expands references.

Publish the opened module Toolset once, before turn admission; thereafter the forwarding set returns its fixed snapshot. No config reload or `Refresh` command exists. Notifications can mark a catalog changed for inspection without mutating it. MCP joins workspace/resource tools and existing hooks. Duplicate names cannot replace workspace tools. The workspace external policy approves exact qualified name/arguments, regardless of startup source or server annotations.

## Working-directory prerequisite and ownership

Add `Dir string` to module `Options`: the working directory of every stdio child. Empty preserves inherited process cwd for existing callers. Set `exec.Cmd.Dir` before constructing `CommandTransport`; do not change process cwd or resolve commands through a shell. The CLI always supplies the canonical selected workspace. Existing raw fingerprints retain source paths; host consent adds workspace identity. The field changes no HTTP behavior, tool schema, record format, or transport policy.

Add a runnable module `Example*` with a keyless self-hosted stdio fixture. Supply a temporary `Options.Dir`, call a tool that reports its cwd, and print a stable equality result. Include a relative server argument and an invocation directory outside the selected root in CLI acceptance. This small public module prerequisite stays in the reviewed MCP track.

The concrete runtime owns the module Toolset and an idempotent `Close`. Register its closer before initialization. On build failure, close already-built adapters in reverse order. On `Open` cancellation, the module closes any started sessions; host rollback remains safe. Later session/UI failures, normal exit, and startup interruption close the runtime before process exit. Preserve each run mode's orderly controller stop/wait so cancellation records settle before connections close.

## Safe inventory and recovery

`/mcp` reads host metadata plus `Status`, `Tools`, and `Info`; it never opens a connection or changes a tool snapshot. Render sorted rows with source/selection, trust scope, transport, stdio directory, timeout, state, and exposed/remote tool names. Merge host blocked/unsupported/preflight statuses with module connected/failed/disabled states. Show starting until initialization finishes; zero tools is an explicit connected result. Print notices use stderr. `/resources` consumes these same facts.

Use an allowlisted diagnostic projection. Never stringify raw config, `ServerStatus.Err`, or stderr. Classify known timeout/cancellation/authentication/status codes when available; otherwise show `connection failed; check server configuration`. Warnings use host-generated templates and printable server/tool/source names, not raw remote exception text. Startup and inventory never enter credentials into records. Keep ordinary tool-call/result recording unchanged; model-visible connection diagnostics must use the same safe projection, without a raw wrapped URL/config exception.

The forwarding Toolset clones snapshot tools and wraps each MCP executor's returned error with a safe `Error()` message. Retain the original cause through `Unwrap()` for cancellation/fatal classification; never include it in display text. Do not alter definitions, arguments, ordinary result content, outcomes, or retry decisions. This host boundary prevents failed-connection stderr and raw URL exceptions from becoming recorded/model-visible diagnostic text. Secret-marker fixtures cover both failed startup and later disconnected calls.

For continued work, retain existing runner definition matching, approvals, and unknown-effect handling. Do not invent execution facts, substitute unavailable tools, repair transcripts, or automatically retry calls. A process restart can change availability and therefore stop continuation under the runner's existing checks. Inventory reports current availability, not successful past execution.

## Alternatives, costs, and rollout

- Persistent grants reduce prompts but stale names can approve changed commands. Use raw fingerprints and process consent.
- Changing process cwd avoids a module field but changes path behavior for unrelated features and concurrent work. Use zero-compatible `Options.Dir`.
- A separate HTTP transport could own credentials but duplicates the module. Use its client hook and preflight exclusion.
- Reconnect/refresh improves availability but changes catalog and consent lifecycle. Keep fixed startup snapshots for this slice.

Costs include startup prompts, waiting for bounded discovery, and restarting after configuration/catalog changes. Literal scheme/authority requirements restrict all HTTP endpoint templates. No database migration, token store, paid probe, or new durable MCP metadata is introduced. Ship additive flags/module options and guides. Preserve explicit I13 gap labels; this track does not claim full remote MCP parity.

## Verification and integration

Keyless stdio/HTTP fixtures cover all PRD groups, sorted pre-start consent, binding redaction/defaults, fingerprint/source changes, trusted-source collisions, failed/unknown tools, denial/cancellation, cleanup rollback, and fixed catalogs. A fixture changes the file after its sole read. Assert preflight and `mcp.ParseConfig` receive identical captured bytes, and `Open` receives only eligible parsed entries with correct source/trust metadata.

Fixtures cover literal URL userinfo and `https://${MCP_AUTHORITY}/mcp` with `MCP_AUTHORITY=user:secret@host`, using empty and configured headers. Assert zero environment lookups/expansions, clients, and requests. Cover scheme substitutions with the same counters. Verify path substitution preserves a validated literal scheme/authority. Assert credential lookup/request counters stay zero for rejected remote HTTP and `localhost` entries, even with unset/empty header variables. Exercise literal `127/8`, `::1`, header-free HTTP, HTTPS, and redirects 301/302/303/307/308 to same/different origin or HTTP: one initial request, zero target requests. A project-header fixture verifies binding review precedes requests.

Run root, MCP, and CLI formatting/vet/build/race checks. Run the working-directory `Example*` and CLI transcript. Record actual Wonton startup and empty/loading/error/success inventory states at 80×24. Independent implementation/acceptance, security, and taste reviews remain implementation gates. Schedule shared integration with provider/resources and #103/#116. No unresolved design question blocks the root checkpoint.
