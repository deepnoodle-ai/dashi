# Receive late records and convert external MCP results

Status: Approved\
PRD PR: [#249](https://github.com/deepnoodle-ai/dashi/pull/249) · Implementation PR: Pending · Updated: 2026-10-11

## Problem and context

Applications run tools outside Dashi while keeping their own journals, permissions,
and workers. Two demonstrated bridges lose useful Dashi behavior.
[R22](../../design/swarm-integration/examples/r22-updates-after-finish.md) shows
`Follow` ending at cancellation before a late tool result. A mirror must switch
to manual reads. [R25](../../design/swarm-integration/examples/r25-mcp-images.md)
shows a journaled MCP result becoming JSON text instead of typed image content.
Swarm recognizes an array, but its MCP client saves the complete result object.

`dashi.Follow` already checks cursors, pages records, and waits without controlling
execution. The separate MCP module already converts SDK results privately inside
Toolset. The [observation review](../../design/swarm-integration/reviews/observation-embedding.md)
and [tools review](../../design/swarm-integration/reviews/accounting-tools.md)
accept small additions with explicit lifetime, format, and validation boundaries.
Further external discovery is unnecessary for these demonstrated problems.

## Users and concepts

The primary user builds an application-owned durable runtime. They need every
committed record and reusable model-facing MCP content while retaining execution
and storage authority.

**Tail** follows committed records until its reader context ends, including
records after execution ends. **Result conversion** turns a completed MCP SDK
result into a validated `tool.Outcome`. Conversion grants no access to resources.

## Use cases and acceptance

### UC-1: Mirror a result saved after cancellation

The application calls `feed.Tail(ctx, store, turnID, cursor)`, applies each Step,
and persists its cursor. It cancels this reader when its own job finishes.

- [ ] Receive the ending stop and later committed tool results in sequence.
- [ ] Resume from the ending stop or a later record without repeating the cursor.
- [ ] Normalize a negative cursor to zero. Reject an ahead cursor, wrong turn,
  repeated sequence, or missing sequence with the existing error identities.
- [ ] Drain pages before waiting. Waiter failures and missed wakes still lead to reads.
- [ ] Cancellation or a range break ends only this reader and leaves no reader work.
- [ ] Independent readers keep independent cursors and cancellation contexts.
- [ ] Both existing Follow functions retain their ending-stop and reconnect behavior.

### UC-2: Return a journaled MCP screenshot to the model

The application selects MCP conversion using the admitted tool's output format.
It calls `mcp.ConvertResult(result)` for an SDK result or
`mcp.ConvertResultJSON(raw)` for a saved complete result envelope. It reports
warnings, combines any journal failure with the outcome, then uses `Pending.Result`.
If conversion fails for completed work, it keeps the journal and records a safe
failed outcome through `Pending.Result`. It never repeats completed execution.
`mcp.ErrInputRequired` identifies a separate host interaction path. The host keeps
the request pending while supplying requested input under its existing policy.
Without an input handler, it settles a safe failure that explains unsupported input.

- [ ] Typed conversion requires no Toolset, connection, or running server.
- [ ] Text and guaranteed media retain order, kinds, and copied bytes. Image MIME:
  `image/png`, `image/jpeg`, `image/gif`, `image/webp`. Audio MIME: `audio/wav` and
  `audio/mpeg`. Normalize `image/jpg` to `image/jpeg`,
  `audio/wave` and `audio/x-wav` to `audio/wav`, and `audio/mp3` to `audio/mpeg`.
- [ ] Other syntactically valid image/audio types become omission notes and warnings
  at their original positions. A valid SVG fixture demonstrates this rule.
- [ ] Raw structured values retain exact JSON numbers. `9007199254740993` and
  `0.1234567890123456789` each appear correctly once, alone and beside equivalent
  JSON text. Typed input preserves its supplied value. Marshal failures return errors.
- [ ] MCP `IsError` produces `tool.StateFailed`. Examples preserve journal failure too.
- [ ] Nil results, malformed envelopes, invalid media, and incomplete input requests
  return useful errors. No partial outcome escapes on error.
- [ ] A runnable example retains an unusable completed result's journal, records
  a safe failed outcome through `Pending.Result`, and makes no repeated tool call.
- [ ] Input-required responses match `ErrInputRequired`, preserve the host's pending
  work/input state, and follow its input policy. They never become automatic success.
- [ ] Empty completed results succeed. Resources become descriptions or explicit
  omission notes and warnings. No resource is fetched or executed.
- [ ] Conversion validates MIME and content. Diagnostics identify the block without
  including media bytes, structured payloads, or server metadata.

## Decisions and boundaries

- Add `feed.Tail` outside the engine. Share the validated reader loop internally.
  Reject a second hand-written tail and a new root option for this integration job.
- Export typed conversion first. Add the JSON wrapper because Swarm journals the
  complete SDK envelope. Use SDK polymorphic decoding with exact structured numbers.
  Reject an array bridge and the SDK's lossy float64 structured decoding.
- Return warnings as `[]string` beside the outcome and error. Reuse Toolset conversion
  rather than establish a second conversion policy.
- The host stores raw output, admitted tool/version, format, and journal failure.
  JSON shape never establishes MCP provenance. Provider media support remains separate.
- No durable tool executor, worker ownership, permissions, network changes, billing
  watcher, record schema change, dependency upgrade, or module-version change.

## Success and risks

Runnable `Example*` tests demonstrate late-result tailing, standalone screenshot
conversion, and unusable-result settlement. Focused tests, `make check`, independent
implementation review, and security review pass. Guides and the changelog describe both APIs.
Untrusted MIME, resource names, and structured content can mislead readers or models.
Validation and bounded diagnostic text reduce that risk without treating output as
trusted instructions. The host must bound input sizes and authorize each reader.
No design question blocks implementation after independent PRD review.
