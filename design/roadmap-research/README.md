# Dashi roadmap research and PRDs

Snapshot: 2026-09-28. This packet expands the [24-item roadmap](../remaining-work.md).
Each item has a source review and a Dashi PRD. Some first slices have merged;
the [roadmap](../remaining-work.md) records current status and remaining scope.
Unimplemented PRDs are proposals for discussion, not implementation commitments.

The comparative reviews inspected Dashi at `015b0ec`; their source paths and
line references belong to that revision unless a review states a later commit.
Item 20 was rechecked at `018af90`. Hooks and committed-step `Follow`
landed afterward, on `main` at `108cb52`. The roadmap records later merged
slices and follow-ups through PR #70. Use `git show 015b0ec:<path>` for
historical source citations such as `policy.go`.

The reviews inspect Dashi, Dive, Nvoken Cloud, Mobius Cloud, pi, Eino, Google
ADK Go, Microsoft Agent Framework Go, OpenHands Software Agent SDK, and
OpenHands Canvas at the pinned revisions named in the notes. A comparison
project's capability may be marked partial, absent in the inspected surface,
or unknown; those labels are not claims about every extension or newer release.
Source and fixture evidence is distinct from paid live-provider qualification.
No paid live probes were run for this packet.

| # | Roadmap item | Source comparison | Dashi PRD |
| --- | --- | --- | --- |
| 1 | [Native structured output for model calls](../remaining-work.md#1-native-structured-output-for-model-calls) | [Research](01-native-structured-output-research.md) | [First-slice PRD](01-native-structured-output-prd.md) |
| 2 | [Generation controls and provider options](../remaining-work.md#2-generation-controls-and-provider-options) | [Research](02-generation-controls-research.md) | [Draft PRD](02-generation-controls-prd.md) |
| 3 | [Prompt-cache controls](../remaining-work.md#3-prompt-cache-controls) | [Research](03-prompt-cache-research.md) | [PRD](03-prompt-cache-prd.md) |
| 4 | [Provider dialect conformance](../remaining-work.md#4-provider-dialect-conformance) | [Research](04-provider-dialect-research.md) | [First-slice PRD](04-provider-dialect-prd.md) |
| 5 | [Live provider qualification](../remaining-work.md#5-live-provider-qualification) | [Research](05-live-qualification-research.md) | [Draft PRD](05-live-qualification-prd.md) |
| 6 | [Local MCP client](../remaining-work.md#6-local-mcp-client) | [Research](06-local-mcp-research.md) | [Draft PRD](06-local-mcp-prd.md) |
| 7 | [Authenticated provider-hosted MCP](../remaining-work.md#7-authenticated-provider-hosted-mcp) | [Research](07-hosted-mcp-auth-research.md) | [Draft PRD](07-hosted-mcp-auth-prd.md) |
| 8 | [Speech transport](../remaining-work.md#8-speech-transport) | [Research](08-speech-transport-research.md) | [First-slice PRD](08-speech-transport-prd.md) |
| 9 | [Nullable typed-tool arguments](../remaining-work.md#9-nullable-typed-tool-arguments) | [Research](09-nullable-tool-args-research.md) | [Implemented PRD](09-nullable-tool-args-prd.md) |
| 10 | [Tolerance of extra tool arguments](../remaining-work.md#10-tolerance-of-extra-tool-arguments) | [Research](10-extra-tool-args-research.md) | [Implemented PRD](10-extra-tool-args-prd.md) |
| 11 | [Tool-result size and projection](../remaining-work.md#11-tool-result-size-and-projection) | [Research](11-tool-result-projection-research.md) | [Draft PRD](11-tool-result-projection-prd.md) |
| 12 | [Skills](../remaining-work.md#12-skills) | [Research](12-skills-research.md) | [First-slice PRD](12-skills-prd.md) |
| 13 | [Child turns and delegation](../remaining-work.md#13-child-turns-and-delegation) | [Research](13-child-turns-research.md) | [First-slice PRD](13-child-turns-prd.md); [messages PRD](13-subagent-messages-prd.md) |
| 14 | [Session continuity](../remaining-work.md#14-session-continuity) | [Research](14-session-continuity-research.md); studies of [pi](14-session-study-pi.md) and [Codex](14-session-study-codex.md) | [PRD](14-session-continuity-prd.md) |
| 15 | [Context compaction](../remaining-work.md#15-context-compaction) | [Research](15-context-compaction-research.md) | [Draft PRD](15-context-compaction-prd.md) |
| 16 | [Turn-owned attachment references](../remaining-work.md#16-turn-owned-attachment-references) | [Research](16-attachment-references-research.md) | [Draft PRD](16-attachment-references-prd.md) |
| 17 | [Incremental projection](../remaining-work.md#17-incremental-projection) | [Research](17-incremental-projection-research.md) | [Draft PRD](17-incremental-projection-prd.md) |
| 18 | [Inspecting newer record versions](../remaining-work.md#18-inspecting-newer-record-versions) | [Research](18-newer-record-versions-research.md) | [Draft PRD](18-newer-record-versions-prd.md) |
| 19 | [Durable retries after proven rejection](../remaining-work.md#19-durable-retries-after-proven-rejection) | [Research](19-durable-retries-research.md) | [Draft PRD](19-durable-retries-prd.md) |
| 20 | [Connection and idle-stream timeouts](../remaining-work.md#20-connection-and-idle-stream-timeouts) | [Research](20-stream-timeouts-research.md) | [First-slice PRD](20-stream-timeouts-prd.md) |
| 21 | [Hosted tools under a cost cap](../remaining-work.md#21-hosted-tools-under-a-cost-cap) | [Research](21-hosted-tools-cost-cap-research.md) | [Draft PRD](21-hosted-tools-cost-cap-prd.md) |
| 22 | [Remaining provider price gaps](../remaining-work.md#22-remaining-provider-price-gaps) | [Research](22-provider-price-gaps-research.md) | [First-slice PRD](22-provider-price-gaps-prd.md) |
| 23 | [ACP adapter](../remaining-work.md#23-acp-adapter) | [Research](23-acp-adapter-research.md) | [Draft PRD](23-acp-adapter-prd.md) |
| 24 | [A2A and OpenTelemetry adapters](../remaining-work.md#24-a2a-and-opentelemetry-adapters) | [Research](24-a2a-otel-adapters-research.md) | [Draft PRD](24-a2a-otel-adapters-prd.md) |
