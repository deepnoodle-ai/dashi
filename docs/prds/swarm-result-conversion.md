# Convert external MCP results into model input

Status: Draft\
PRD PR: [#249](https://github.com/deepnoodle-ai/dashi/pull/249) · Implementation PR: [#253](https://github.com/deepnoodle-ai/dashi/pull/253) · Updated: 2026-10-11

## Problem and context

The user connects an MCP screenshot tool and selects an image-capable model.
They ask why the checkout button is hidden. The agent calls the tool, whose
server returns a complete object containing PNG content. Swarm saves that object.
Its converter expects a standalone content array, so the next model request
contains JSON text instead of an image. [R25](../../design/swarm-integration/examples/r25-mcp-images.md)
shows the saved-object mismatch and the before-and-after input.

The MCP module already converts SDK results privately inside Toolset. Share that
conversion with applications that own execution and journals. They can preserve
images, audio, text, and failure without adopting Toolset's execution ownership.
A local envelope fix in Swarm is the smaller alternative for the screenshot alone.

The earlier Tail proposal is withdrawn. Stopping the agent is a reasonable point
to stop displaying pending tool results. No current Swarm consumer needs later
delivery; the proposed activity panel was an invented requirement. Existing Follow
behavior serves that sequence. [R22](../../design/swarm-integration/examples/r22-updates-after-finish.md)
retains the reasoning. The previously independent-reviewed MCP requirements remain;
this revision narrows the scope and makes no new adoption claim from passing tests.

## Users and concepts

The developer runs tools through an application-owned runtime and retains workers,
journals, permissions, and recovery. **Result conversion** turns a completed MCP
SDK result into a validated `tool.Outcome`. Conversion grants no resource access.

## UC-1: Return a journaled MCP screenshot to the model

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

- Export typed conversion first. Add the JSON wrapper because Swarm journals the
  complete SDK envelope. Use SDK polymorphic decoding with exact structured numbers.
  Reject an array-only bridge and the SDK's lossy float64 structured decoding.
- Return warnings as `[]string` beside the outcome and error. Reuse Toolset conversion
  rather than establish a second conversion policy.
- The host stores raw output, admitted tool/version, format, and journal failure.
  JSON shape never establishes MCP provenance. Provider media support remains separate.
- No Tail, generic reader extraction, durable tool executor, worker ownership,
  permissions, network changes, billing watcher, record schema change, dependency
  upgrade, or module-version change.

## Success and risks

Runnable examples demonstrate screenshot conversion and unusable-result settlement.
Acceptance requires focused tests, `make check`, and the existing independent
implementation/security findings to remain addressed. Guides and changelog cover MCP.
Untrusted MIME, resource names, and structured content can mislead readers or models.
Validation and bounded diagnostics reduce that risk. The host must bound input sizes.
The screenshot proof is typed model input, not a guarantee of diagnosis quality.
