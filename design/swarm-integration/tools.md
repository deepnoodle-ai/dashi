# Connecting application-run tools with less conversion code

See the [main recommendations](README.md). Swarm should keep tool execution,
approvals, and sandbox rules. Dashi should make it easy to identify the requested
tool and give the model its result.

## R23 — carry the complete tool-call identity through the API

**Problem.** A provider's tool-call ID is scoped to a Turn. [Swarm][ids] hashes
both IDs to create a journal key, then [scans the view][results] to find the
native ID when the result comes back.

**Change.** Carry Dashi's existing `CallRef` (Turn ID plus call ID) through pending
work and result APIs. Offer a stable single-key encoding where storage needs it;
keep the original IDs beside the key. Validate duplicate results as well as
currently pending calls.

**Benefit:** remove key conventions and result-time scans. Swarm must also update
its own tool-result envelope to carry both IDs. It can use `Pending.Result`
already to construct a Resolve command instead of repeating that construction.

## R24 — make submitting several tool results a small operation

**Problem.** [Swarm][results] creates and saves a Resolve command per result,
then wakes the agent. This repeats command-ID and partial-failure handling.

**Change.** Start with a helper that produces the existing Resolve commands from
a batch of typed call identities and results. A submission helper could then
report exactly which results were saved and what remains pending.

Several writes are not automatically an atomic batch. Preserve each result's
retry/conflict rules when only some writes succeed or acknowledgments are lost.
Continuing the agent should remain an explicit choice after results settle.

**Benefit:** less result-submission code. Keep external tools concurrent: Swarm
can run a blocking inbox wait beside a sibling post. Running all tools inline
inside the model loop could change that behavior or deadlock it.

## R25 — let Swarm reuse Dashi's MCP result converter

**Problem.** [Swarm's converter][content] recognizes a top-level array of
text/image blocks. [Its MCP client][mcpclient] returns the complete
`CallToolResult` object, and [the manager][mcphandler] passes it onward. The
converter therefore sends that MCP object to the model as JSON text. An image
inside `content` does not become a model image. The screenshot-array test does
not cover this representation.

Dashi's private `mcp.convertResult` already preserves ordered text, images,
audio, structured content, and resources, and reports content it cannot fully
represent.

**Change.** Export that conversion independently of Toolset execution, including
an entry point for the raw MCP result JSON. Return a `tool.Outcome` and warnings.
Preserve error status, order, structure, and MIME checks. Swarm can retain the
original result in its journal and use the converter for model-facing content.

**Benefit:** a smaller bridge and an actual multimodal correctness fix. Keep it
in the separate MCP module. Swarm should not have to transfer tool execution
to Dashi just to convert a result. Plain application JSON should remain a
distinct input format rather than relying on ambiguous shape guessing.

Dashi already accepts a custom HTTP client for MCP. Swarm's restricted dialing,
origin tracking, and tool-version tracking remain application responsibilities.

## R26 — report delivered input directly to the application

**Problem.** [Swarm][steering] converts stored messages into Dashi Steers, then
scans saved records to acknowledge their delivery. It also consumes their keys
in the same transaction as the record. [NudgeKeyed][nudge] handles mail-specific
correlation separately.

**Change.** Include delivered input IDs and sequence in R09's save changes.
Provide a small adapter for listing pending inputs, acknowledging delivery, and
notifying a running agent. A notification should follow durable visibility;
a repeated acknowledgment must be safe.

**Benefit:** remove scanning and repeated wake setup. Swarm keeps mail identity
and decides whether input is urgent. Saving the execution record and consuming
input together still belongs in its transaction.

## Where I would stop

Keep a universal durable tool executor as a future possibility, not a current
recommendation. Moving Swarm's whole executor into Dashi would pull in approvals,
uncertain side effects, sandbox ownership, and concurrency policy. The evidence
here supports better identities, results, and delivery reporting. A second
application with the same execution needs would strengthen the case for more.

[ids]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L182
[results]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L918
[content]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L956
[mcpclient]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/mcpclient/client.go#L144
[mcphandler]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/manager/mcp.go#L38
[steering]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/recording.go#L140
[nudge]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L999
