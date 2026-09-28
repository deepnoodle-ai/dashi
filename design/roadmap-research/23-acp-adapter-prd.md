# 23. ACP adapter — draft PRD

**Status:** Draft proposal, 2026-09-28. **Roadmap:** [item 23](../remaining-work.md). **Basis:** [source research](23-acp-adapter-research.md), updated for merged hooks and `Follow`. **Decision sought:** Ship an optional ACP **v1 agent/server** adapter for one real editor client, Zed, after its session and delivery mapping are qualified.

## User problem

A Go application that builds an agent on Dashi should be able to offer it in an editor without writing JSON-RPC, ACP session recovery, tool cards and permission handling itself. The editor user needs to see current work, approve the exact call shown, cancel safely and reopen the conversation after restart. The application developer needs one Dashi Turn record for recovery/accounting; ACP updates should be a presentation of those facts, not a second execution log.

## Goals and non-goals

**Goals:** Zed can start a session, submit text, see streamed answer and tool lifecycle, choose allow/reject once, cancel, and load the saved conversation after process restart. Client-provided stdio MCP servers become available as Dashi tools, as ACP v1 requires. The adapter advertises only capabilities it actually implements and preserves Dashi's Turn IDs, approval binding and uncertainty. It lives in an optional `adapters/acp` package with no ACP types in core Dashi.

**Non-goals:** ACP client/hosting of external agents; v2 on the wire; remote HTTP/WebSocket transport; a general protocol framework; replacing Dashi Session design; implicit approval or permanent grants; model/budget authority from ACP `usage_update`; claiming exact replay of ephemeral previews. Client filesystem/terminal integration and Zed's Keep/Reject review controls are follow-on work after one real Dashi file tool can use that seam.

## Smallest user flow and API direction

The adapter has three injected responsibilities, shaped around existing Dashi boundaries: a factory that binds a `Runner` and tools to an ACP session's absolute `cwd`; a host-owned store for ACP session metadata, ordered Turn IDs and caller-owned prior history; and a Turn recorder/`StepReader` that loads and follows acknowledged steps. Sketch, not a committed signature:

```go
server := acp.Server{
    Open:     openRunnerForSession, // cwd -> configured dashi.Runner
    Sessions: sessionStore,         // ID, cwd, ordered Turn IDs/history
    Turns:    turnStore,            // Recorder + StepReader
}
err := server.ServeStdio(ctx, os.Stdin, os.Stdout)
```

The host keeps credentials, workspace confinement and tool implementation. The adapter validates `cwd`, creates a stable ACP session ID, pins its workspace, and serializes one foreground prompt per session. It connects client-provided stdio MCP servers through the local MCP client from [item 6](06-local-mcp-prd.md), scoped to this session; unsupported optional transports fail clearly and are not advertised. `session/prompt` creates a Dashi Turn with a stable ID and commits `Start` before any success/update is exposed; subsequent model/tool steps remain Dashi authority. Use the implemented `Follow` and a host `StepReader` for committed updates and reconnection; `Replay` builds `session/load`. An owner `Runner.Stream` may add provisional previews while connected, but they are not recovery facts. The adapter must not rely on asynchronous `Observer` callbacks for ACP wire order. A follower disconnect does not cancel the Turn; breaking the owner stream still invokes Dashi's interruption/cancel rule. ACP v1 `session/load` is not an exact event cursor.

Map acknowledged model/tool facts to v1 `session/update`; use provisional deltas only while their owner stream is continuous. Derive session-unique message/tool IDs from recorded Turn/attempt/call identities, stable across `load`; replay committed full content and terminal tool statuses, not old previews. Announce a call with title/kind before permission, then show `in_progress` only after the recorded tool start. If a `BeforeTool` hook defers a local call, keep the ACP prompt open, issue `session/request_permission` with `allow_once`/`reject_once`, and settle using Dashi's `ArgsDigest` and the hook verdict revision. A cancelled or disconnected request fails closed; no tool dispatch follows an unanswered permission. A later `allow_always` option requires a separate persisted host grant and is not offered here. Provider-hosted approval needs its own mapping and is not silently treated as a local tool grant.

`session/load` rebuilds the visible conversation from saved Turns and returns only after replay notifications drain. `session/resume` may be advertised only when restoring context without replay is verified against Zed; otherwise advertise load alone. The host's session store supplies prior history to each Dashi `Start`/`Continue`; ACP session ID never becomes a Dashi Turn ID by accident. If a prompt response was lost at process death, load shows the recorded Turn and any pending/uncertain state; it must not resubmit the user's prompt or auto-repeat an uncertain effect. A pending permission may be reissued only from its recorded call identity after client behavior is qualified; otherwise show an actionable wait, never an implicit allow. A v1 connection has no exact event cursor, so independent mid-turn reattachment and byte-for-byte preview replay are out of scope.

Map `StopCompleted` to `end_turn`, `StopCanceled` to `cancelled`, and only matching output/attempt limits to v1 `max_tokens`/`max_turn_requests`. A hook refusal is mapped to `refusal` only after Zed's deletion behavior is qualified. For Dashi `StopFailed`, `StopUncertain` or an unresolved external wait, send an actionable JSON-RPC error with structured Dashi stop data after all prior updates have drained; never claim `end_turn`. `session/cancel` must cancel an open permission, settle/cancel the Turn as Dashi requires, flush committed updates, then answer the outstanding prompt.

## Acceptance criteria

1. A Zed session starts with truthful negotiated capabilities. A text prompt produces one recorded Dashi Turn, ordered chunks/tool cards, and exactly one final v1 prompt response. No user content is acknowledged before `Start` is committed. A client-provided stdio MCP server supplies tools; unsupported HTTP/SSE descriptions fail explicitly unless implemented and advertised.
2. A deferred file or command tool appears before the permission request. Allow once executes only the call whose recorded argument digest and hook verdict revision match the display; reject/cancel never executes it. An `allow_always` choice is absent until a host-backed grant exists.
3. Cancellation during streaming and during permission yields `cancelled` after recorded updates drain, and a later prompt works in the same session. Failed, limited and uncertain Dashi stops are displayed truthfully and do not become successful `end_turn` responses.
4. Restart plus `session/load` recreates the same message/tool IDs and terminal statuses from acknowledged Turn records; it sends no stale provisional chunks and performs no model/tool work merely to replay. Pending approvals remain pending and are reissued only with the original call binding, if Zed accepts that flow. A saved `cwd` mismatch is rejected rather than loading a different workspace.
5. Golden v1 frames validate against the pinned official schema. A real Zed run covers new/prompt, tool card, permission allow/reject, cancel and load; acpx or an independent client exercises ordering and unsupported capability behavior. A separate fixture proves that v2 prompt/upsert frames are **not** emitted under v1 negotiation.

## Risks and open decisions

- **Session owner:** Dashi currently leaves cross-turn history with the caller. Align the store contract with [item 14](14-session-continuity-prd.md) before publishing a public adapter API; do not create competing Session concepts.
- **MCP baseline:** V1 agents must handle client-provided stdio MCP servers. The local MCP client in [item 6](06-local-mcp-prd.md) is a prerequisite; no-op acceptance of server descriptions is not conformant.
- **Live delivery:** `Follow` now reads committed steps independently of the owner and survives follower disconnects. `Runner.Stream` supplies optional continuous previews, but breaking its owner stream ends work. `Observer` never drops committed steps, yet can drop previews and delivers asynchronously. Test ACP ordering from the committed cursor and keep preview settlement explicit.
- **Protocol version:** V2 is alpha and changes prompt completion, lifecycle and reducer rules. Maintain a separate mapping design and fixtures, then implement after a concrete v2 client and release target justify it.
- **Go wire library:** The earlier Dive spike found schema drift and lost fields in the surveyed Go SDK. Recheck maintenance at implementation time; a small owned v1 transport/types layer remains the current candidate, with schema validation and a real client as gates.
