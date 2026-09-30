# Use and inspect MCP tools safely in the terminal

Status: Approved\
PRD PR: [#132](https://github.com/deepnoodle-ai/dashi/pull/132) · Implementation PR: Not opened · Updated: 2026-09-30

## Problem and context

A developer moving from Dive cannot use MCP tools in Dashi's terminal agent. They must build their own configuration, approval, inspection, and cleanup integration. This delivers I12/I14 in the [CLI parity matrix](../research/cli-parity-integrations.md). I13 remains a separate remote-MCP foundation dependency.

Dive loads user, project, and explicit MCP files, isolates failed servers, and closes connections. Its project approvals can persist by server name. Dashi's [MCP module](../guides/mcp.md) already supplies stdio/streamable HTTP, raw configuration fingerprints, protected merging, qualified names, snapshots, and cancellation. The CLI only wires workspace tools. [Claude Code](https://code.claude.com/docs/en/mcp) makes project approval and connection status visible. [Google ADK Go](https://github.com/google/adk-go/blob/main/tool/mcptoolset/set.go) separates transport configuration from tool-call confirmation. Reuse Dashi's module and existing call policy.

## Users and concepts

The primary user codes with one terminal agent and wants approved MCP tools beside workspace tools.

**Server configuration** names a command or HTTP endpoint and its source. **Startup approval** permits a project server to start for this Dashi run. It grants no tool-call permission. **Tool-call approval** permits the exact action under the existing external-tool policy. **MCP inventory** shows selected sources, connection state, and exposed tools. MCP access is external-server access, not a workspace read/write grant.

## Use cases and acceptance

### UC-1: Select configuration and approve project startup

Put `demo` in `<workspace>/.mcp.json`. Run `dashi --mcp-config extra.json`. Review the project startup prompt before sending work.

- [ ] Load `mcpServers` from `~/.dashi/settings.json`, workspace `.mcp.json`, then repeatable `--mcp-config FILE` arguments in order. User/explicit files are trusted. Project entries are untrusted. Use the selected workspace root, without ancestor discovery or automatic `.dive` imports.
- [ ] Later entries win except that a project entry never replaces a trusted entry. Inspection identifies the selected source and ignored conflicting source. Explicitly naming a project file makes that explicit file trusted.
- [ ] Missing default files produce an empty state. Unreadable or malformed files warn with the source and contribute no entries. A missing explicit file warns. Other valid files remain usable. An empty flag value is an argument error.
- [ ] Prompt once per eligible project server, in sorted name order, before any server starts. Show name, source, transport, raw command/arguments or HTTP scheme/destination, and consent scope. Offer “Start for this Dashi run” and “Do not start”. Disabled, rejected, and unsupported entries never prompt or start.
- [ ] Show configured environment/header binding names and their unexpanded variable references. Replace literal values and variable defaults with a redacted indication. Show the canonical workspace directory for stdio, and state that the child inherits the process environment. A header binding such as `Authorization: ${OPENAI_API_KEY}` must be reviewed before any project connection.
- [ ] Start every stdio server in the canonical selected workspace, regardless of configuration source or invocation directory. A relative server path works when Dashi launches from elsewhere. Startup approval and inventory show that directory.
- [ ] No project process or HTTP request occurs before consent. Denial, cancelled approval, or an unavailable unattended prompt leaves the server blocked. Tool-call approvals and auto-approval flags cannot supply startup consent.
- [ ] Consent uses the name, `Server.Fingerprint()`, and selected workspace, and never persists. Changed command, arguments, environment, URL, headers, type, source, or workspace require new consent. Rotated variable values do not change the raw fingerprint. A new Dashi process asks again.
- [ ] `${VAR}` and `${VAR:-default}` expand only through the MCP module. Missing variables produce name-only warnings. Config loading and approval execute no shell expansion or credential helper.
- [ ] Reject URL userinfo in every HTTP entry, regardless of configured headers, before environment access/expansion, client creation, or network requests. A fixture with `https://user:secret@host` and empty headers asserts zero credential lookups, clients, and requests. Startup consent cannot bypass rejection.
- [ ] Any configured HTTP header makes an entry credential-bearing, even with empty/unset values. Require HTTPS, except HTTP with a literal IP in `127.0.0.0/8` or exactly `::1`. Require literal scheme/authority for these entries. Reject unsafe entries before variable expansion, credential access, or network requests. DNS names, including `localhost`, do not qualify. Approval cannot bypass this rule. Header-free HTTP remains usable.
- [ ] Follow no HTTP redirects. A 3xx fails the connection without requesting its target, including HTTPS downgrades and other HTTPS origins. Fixtures prove zero initial requests for rejected entries and zero target requests for redirects.

### UC-2: Call a connected tool under normal policy

Approve `demo`, then ask the agent to call its `echo` tool. Approve or deny the requested call. Repeat with a tool that advertises a read-only hint.

- [ ] The model sees the module's deterministic `mcp__<server>__<tool>` names. Inspection maps each exposed name to its server and original remote name. Long names retain the module's stable shortening.
- [ ] Startup consent and a trusted configuration source never bypass call approval. The prompt shows the qualified tool name and exact call arguments. Read-only/idempotent server claims never widen permission or authorize repetition.
- [ ] Allow once covers one call. Existing remembered external-call approval covers matching tool name and arguments for this policy/process only. Denial, cancellation, and unanswered unattended approval dispatch no call.
- [ ] Collisions retain the module's deterministic winner and explain the dropped tool. No external tool replaces a workspace tool. Unknown names return a clear normal failed tool outcome without dispatching a different remote tool.
- [ ] Tool results and approvals use normal Turn records. Inspection is not evidence that a tool ran. Cancellation reaches the MCP call. The CLI introduces no retry of a call with a lost response and no transcript repair.

### UC-3: Inspect partial startup and remote compatibility gaps

Configure one stdio server, one HTTP server, one broken server, one denied project server, and one SSE entry. Enter `/mcp` before and during a turn.

- [ ] Startup gives a compact summary. `/mcp` lists servers in name order with source, trusted/project scope, transport, stdio directory, state, tool count/names, and safe reasons/warnings. The resource inventory consumes these same MCP source/status facts.
- [ ] Distinguish empty, starting, connected, blocked, failed, disabled, and unsupported states. Connected with zero tools is explicit. One server's error or timeout does not prevent usable servers or workspace tools from operating.
- [ ] A server's start/discovery has the module's 10-second default. `MCP_TIMEOUT` supplies positive milliseconds. An invalid value warns and uses the default. Help and inventory show the effective timeout.
- [ ] Legacy SSE and recognized OAuth-only configuration show unsupported with the I13 dependency and migration guidance: use streamable HTTP with caller-supplied headers where possible. Authentication-required HTTP failures identify the gap when known. No unsupported authentication setting is silently ignored or replaced with anonymous success.
- [ ] Inspection never connects, restarts, refreshes tools, starts a turn, or changes approval. It remains available during work. Tool-change notifications can show “catalog changed; restart between turns”. This slice keeps one catalog/configuration snapshot per run.
- [ ] Screen and line modes expose the same facts. Print-mode startup notices use stderr. Empty/loading/error/success states remain readable at 80×24. Server-controlled text renders as printable text.
- [ ] Inventory shows no environment/header values, resolved variables, credentials, or raw server stderr. Endpoint display excludes URL userinfo/query. Approval preserves raw variable references. Diagnostics never dump configuration or raw connection exceptions that can contain credential material.

### UC-4: Stop without leaving a child process

Start multiple fixture servers, interrupt startup or a running call, then exit. Repeat with an app/session initialization failure after connections open.

- [ ] Every exit path closes owned connections and stdio child processes, including cancellation, partial startup, and later initialization failure. Cleanup completes before CLI exit and repeated close is safe.
- [ ] Interrupting a call preserves ordinary recorded stop/recovery behavior. Session switches and `/new` do not restart MCP servers or grant permission. Reopening a process establishes fresh connections and startup consent.
- [ ] Changed or missing tool definitions on recorded continuation use the runner's existing checks and clear stop behavior. The CLI never substitutes another tool or repeats a recorded effect to recover availability.

## Decisions, scope, and success

- Keep trust-aware `mcp.Merge`, not Dive's ordinary tier overwrite. A repository must not take over a trusted server name.
- Keep startup consent separate and process-scoped. Reject blanket enable-all settings, name-only persisted grants, and treating resource instructions as permission.
- Require safe credential transport and refuse redirects. Reject DNS-based loopback exemptions and arbitrary insecure opt-ins. Trust does not make an insecure credential route safe.
- Use the module's 10-second default and `MCP_TIMEOUT` override. Reject a second timeout implementation or Dive's 30-second default.
- Keep `/mcp` read-only and startup snapshots fixed. Reject live config reload, reconnect controls, or mid-turn catalog mutation in this slice.
- Use the separate MCP module and existing hooks. Reject CLI-local transport/authentication, token storage, browser OAuth, and a second tool policy system.

Remote OAuth/SSE foundation work belongs to slice 14. MCP prompts, resources, sampling, server installation/editing, marketplaces, and broad access grants are outside scope. Shared options/settings/wiring/app integration requires scheduled ownership with provider and resource slices. The [technical spec](../design/cli-mcp.md) includes the small MCP working-directory prerequisite.

Success requires keyless stdio/HTTP fixtures for every acceptance group, a runnable transcript and `Example*` for the working-directory API, and Wonton recordings. Independent implementation/acceptance, security, and taste reviews must pass. Existing coding, recovery, queue, and approval tests must pass. No material question blocks the checkpoint.
