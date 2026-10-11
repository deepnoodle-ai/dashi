# Connecting application-run tools with less conversion code

See the [main recommendations](README.md). Swarm should keep tool execution,
approvals, and sandbox rules. Dashi should make it easy to identify the requested
tool and give the model its result.

## R23 — carry existing CallRef through application envelopes

[Before and after](examples/r23-tool-call-identity.md).

**Problem.** [Swarm][ids] hashes Turn/call IDs, then [scans the view][results]
to recover the native call ID when results return.

**Action now.** Store Dashi's existing CallRef in the job/result envelope, keeping
native IDs beside any opaque key. Validate membership and duplicate completed
results. Use existing Pending.Result to build Resolve commands.

**Benefit:** remove reverse-hash scans without new Dashi concepts. Optional key
encoding can wait until a real remaining duplication is demonstrated.

## R24 — defer a batch result planner

[Before and after](examples/r24-several-tool-results.md).

[Swarm][results] saves one Resolve per result, then wakes the agent. Existing
Pending.Result removes command-ID construction. A PlanResults API adds types
while keeping the application's loop, partial-save recovery and wake decisions.

**Defer it.** First adopt the smaller existing builder and CallRef envelopes.
Any future batch helper must handle completed exact retries, conflicting results,
and lost acknowledgments; several writes are not automatically atomic.

Keep outside tools concurrent. A blocking inbox wait must not prevent a sibling
post; moving execution inside the model loop can change that behavior.

## R25 — let Swarm reuse Dashi's MCP result converter

[Before and after](examples/r25-mcp-images.md).

**Problem.** [Swarm's converter][content] recognizes a top-level array of
text/image blocks. [Its MCP client][mcpclient] returns the complete
`CallToolResult` object, and [the manager][mcphandler] passes it onward. The
converter therefore sends that MCP object to the model as JSON text. An image
inside `content` does not become a model image. The screenshot-array test does
not cover this representation.

Dashi's private mcp.convertResult maps ordered text, images, audio, structured
content and resources, with notes for content it cannot represent. Resource
descriptions/omissions and MIME validation still need explicit handling.

**Change.** Expose typed SDK-result conversion independently of Toolset execution first.
A raw JSON entry point should use the SDK's polymorphic decoding. Return a `tool.Outcome` and warnings.
Preserve order/structure, validate MIME/content, and combine journal/MCP error
status so neither failure becomes success. Select the converter from the
admitted tool's explicit format; never infer MCP provenance from JSON shape. Swarm can retain the
original result in its journal and use the converter for model-facing content.

**Benefit:** a smaller bridge and an actual multimodal correctness fix. Keep it
in the separate MCP module. Swarm should not have to transfer tool execution
to Dashi just to convert a result. Plain application JSON should remain a
distinct input format rather than relying on ambiguous shape guessing.

Dashi already accepts a custom HTTP client for MCP. Swarm's restricted dialing,
origin tracking, and tool-version tracking remain application responsibilities.

## R26 — report delivered input directly to the application

[Before and after](examples/r26-delivered-input.md).

**Problem.** [Swarm][steering] converts stored messages into Dashi Steers, then
scans saved records to acknowledge their delivery. It also consumes their keys
in the same transaction as the record. [NudgeKeyed][nudge] handles mail-specific
correlation separately.

**Change.** Include delivered input IDs and sequence in R09's save changes.
Delivery means saved into the transcript, not dispatched to a model. A hook
or budget check can stop before dispatch. Begin with receipts, not a generalized
mailbox adapter. The required Steering.Delivered callback idempotently confirms
the receipt; notifications follow durable visibility.

**Benefit:** remove whole-Turn scanning. Swarm keeps mail identity
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
