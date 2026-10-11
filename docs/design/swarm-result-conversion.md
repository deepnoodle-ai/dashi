# Standalone MCP conversion

Status: Scope narrowed · Author: Codex · Updated: 2026-10-11\
Workflow: Retain the previously reviewed MCP requirements; withdraw Tail and its reader extraction.\
PRD: [#249](https://github.com/deepnoodle-ai/dashi/pull/249) · [Requirements](../prds/swarm-result-conversion.md)

## Context and ownership

The user asks an image-capable agent to inspect an MCP screenshot. The tool returns
an object, Swarm journals it, and its array-only bridge sends JSON text to the model.
The shared converter reads the object and supplies an image block in the next request.

`mcp/result.go` already converts SDK results privately inside Toolset. Keep the
shared conversion in the separate MCP module. Workers, journals, permissions,
and recovery remain application-owned. No root reader implementation, execution
facts, storage schemas, module versions, or dependency versions change.

## APIs

```go
func mcp.ConvertResult(res *sdk.CallToolResult) (tool.Outcome, []string, error)
func mcp.ConvertResultJSON(raw json.RawMessage) (tool.Outcome, []string, error)
```

`sdk` denotes the already-required MCP Go SDK. Warnings describe deliberate
projection loss. Errors describe unusable input. On error, return a zero outcome.
An empty completed result is valid. A nil SDK result is invalid. Export the sentinel
`mcp.ErrInputRequired` so hosts distinguish an interaction from unusable completion
with `errors.Is`. Both converter entry points preserve that error identity.

## Completed MCP results

Make `ConvertResult` the single converter used by standalone callers and Toolset.
Toolset retains its connection errors and sends conversion warnings to server
status. Conversion errors return through the executor's existing error path.
Preserve `IsError` as a failed outcome. The standalone example sets failed state
when the journal also reports failure. Neither source can turn failure into success.

For known completed work, the host retains the original journal and settles a
conversion error with `pending.Result(tool.Failed("Tool completed, but its result could not be used."))`.
It saves that command through its existing storage path and retries only persistence
after an uncertain acknowledgment. It never reruns completed work. The runnable
example proves journal retention, a saved failed result, and zero repeated calls.

Detect `NeedsInput()` before completed-content validation and return `ErrInputRequired`.
The host journals the original input requests/request state and keeps Dashi work
pending while its existing permission/input flow fulfills them. It resumes that
same MCP request with the saved state and input responses, as the protocol requires.
This is an incomplete interaction, not a repeat of completed work. A host without
that flow settles a safe failed outcome explaining unsupported input, with no retry.
Toolset has no input handler and uses this safe failure path. Unknown transport
outcomes keep their existing recovery handling. Reject sampling-only tool-use/result
blocks and nil entries, including typed nil pointers, without panic or execution.

Convert blocks in their original order. Text remains text. Clone image/audio bytes
so later SDK mutation cannot alter an outcome. Marshal structured content before
returning anything. Append it after content unless existing JSON text represents
the same value. Use existing `internal/canonjson.Equal` for exact numeric comparison.
Typed callers can supply `json.Number` or `json.RawMessage`. Conversion preserves
that supplied value; it cannot restore numbers rounded before the call. Marshal failure
is an error, not omission. Validate the completed outcome before returning it.

For media, require nonempty bytes and syntactically valid MIME in the block's family.
Guarantee typed conversion for this fixed set, after signature validation:

| Canonical MIME | Accepted aliases | Header check |
| --- | --- | --- |
| `image/png` | None | PNG signature |
| `image/jpeg` | `image/jpg` | JPEG signature |
| `image/gif` | None | GIF87a/GIF89a |
| `image/webp` | None | RIFF/WEBP |
| `audio/wav` | `audio/wave`, `audio/x-wav` | RIFF/WAVE |
| `audio/mpeg` | `audio/mp3` | ID3 or MPEG audio frame header |

Normalize aliases to canonical MIME. Missing or mismatched headers for these types
are errors. Other valid image/audio MIME types become omission notes and warnings
in position, including valid `image/svg+xml`. Headers validate format identity,
not complete codec integrity. Provider acceptance remains separate. Never fetch
media, turn it into a URL, or add provider provenance.

Describe resource links with their name, URI, and available description/type.
These are descriptions, not fetched content. Text resources retain URI and text.
Binary resources become a note with URI, MIME, and byte count plus a warning.
Require a nonempty valid URI, valid optional MIME, and unambiguous text/blob fields.
Malformed resources are errors. Report discarded MCP metadata or annotations as
projection warnings without copying their values into diagnostics.

## Journal JSON and untrusted input

The raw wrapper removes Swarm's demonstrated complete-envelope/array mismatch.
Require one JSON object. For completed results, require a `content` array, including
an empty array. Reject null, arrays, missing completed content, wrong field types,
and trailing JSON. Input-required responses need no completed content.
Extract `structuredContent` as raw JSON first. Decode that value with `UseNumber`,
remove it from the envelope sent to SDK decoding, then set the exact value on the
decoded result. This avoids the SDK's `any`/float64 conversion and preserves large
integers and decimal fractions. Decode polymorphic content with the SDK's
`CallToolResult.UnmarshalJSON`, then call the typed converter. Unknown kinds produce
decoding errors. Validate optional resultType as complete or input-required. Detect
input-required before completed-content decoding and return `ErrInputRequired`.
Allow unrelated extension fields without granting them authority.

Format selection precedes conversion and comes from the admitted tool identity.
The helper cannot prove provenance from bytes. The host retains the original
journal and tool/version beside the result. It also enforces permissions and size
limits before decoding. Conversion performs no file access, URL fetch, server
connection, input fulfillment, or tool execution. Resource descriptions and text
remain untrusted model input. Error and warning templates use block index/kind and
safe reasons, never raw payloads or SDK exception text containing input values.

## Alternatives, tradeoffs, and verification

- Unwrap the envelope in Swarm: the smaller screenshot fix, but applications
  retain content-format conversion already implemented inside Dashi Toolset.
- Export typed conversion only: sufficient for fresh SDK calls, but leaves the
  demonstrated journal envelope bridge. The thin SDK-decoded wrapper removes it.
- Move external tools into Toolset: removes some conversion calls but transfers
  concurrency, journal, permissions, and recovery authority. Keep conversion pure.

Conversion rejects malformed MCP input more strictly. Media outside the fixed
set produce visible omissions. Tail and the generic reader extraction are removed:
Follow already ends with execution, and the user expects no result after stopping.

Test typed and JSON
MCP paths with ordered mixed blocks, both failure flags, safe unusable-result settlement,
empty/nil cases, incomplete results, invalid MIME/signatures, malformed envelopes,
resources, unsupported kinds, all guaranteed media/aliases, omitted valid SVG,
copied bytes, and safe diagnostics. Test `9007199254740993` and
`0.1234567890123456789` alone and beside equivalent JSON text, asserting one exact
representation. Keep screenshot and failed-settlement examples.
Update the MCP guide and Unreleased changelog.
Run `make check` and independent implementation/security reviews before publication.
No unresolved design question blocks the planning checkpoint.
