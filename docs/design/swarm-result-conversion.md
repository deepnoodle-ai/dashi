# Record tailing and standalone MCP conversion

Status: Draft · Author: Codex · Updated: 2026-10-11\
Workflow: Spec, independent PRD review, then implementation.\
PRD: [Receive late records and convert external MCP results](../prds/swarm-result-conversion.md)

## Context and ownership

`run_follow.go` contains the validated record iterator. `feed.Follow` adds Hub
previews but deliberately ends with execution. `mcp/result.go` privately converts
SDK results, silently drops structured marshal failures, and does not validate MIME.
Keep this work in `feed/` and the separate `mcp` module. A coordinated extraction
from `run_follow.go` is the only root implementation change. No execution facts,
storage schemas, module versions, or dependency versions change.

## APIs

```go
func feed.Tail(ctx context.Context, r dashi.StepReader,
    turnID string, after int64) iter.Seq2[dashi.Step, error]

func mcp.ConvertResult(res *sdk.CallToolResult) (tool.Outcome, []string, error)
func mcp.ConvertResultJSON(raw json.RawMessage) (tool.Outcome, []string, error)
```

`sdk` denotes the already-required MCP Go SDK. Warnings describe deliberate
projection loss. Errors describe unusable input. On error, return a zero outcome.
An empty completed result is valid. A nil SDK result is invalid.

## One record loop, two lifetimes

Extract the current loop into a small generic helper under `internal/`. It imports
no public Dashi types. Thin adapters supply Step identity, sequence, stop flags,
and existing error identities. Its private completion policy distinguishes ending
with execution from following all records. Keep intervals and paging private.
This is shared implementation, not a configurable reader framework.

`dashi.Follow` selects its existing completion policy. It still validates the
cursor's row, ends immediately when that row ended execution, and yields no later
records. `feed.Follow` continues to use it unchanged. `feed.Tail` uses the same
loop with ending-stop completion disabled. It yields the original committed Step
without altering stop flags or record meaning. Tail has no Hub or preview path.

Preserve negative-cursor normalization, contiguous sequence checks, primary-read
assumptions, page draining, stopped-turn polling, waiter fallback, missed-wake
backstop, and eager-waiter backoff. Errors yield once. Context cancellation ends
quietly, as Follow does. The loop runs on the consuming goroutine and starts no
background worker. StepReader and StepWaiter must honor cancellation promptly.
The caller authorizes access, persists cursors, applies idempotently across restart,
and chooses a finite reader lifetime. Host-only ledger changes need host notifications.

## Completed MCP results

Make `ConvertResult` the single converter used by standalone callers and Toolset.
Toolset retains its connection errors and sends conversion warnings to server
status. Conversion errors return through the executor's existing error path.
Preserve `IsError` as a failed outcome. The standalone example sets failed state
when the journal also reports failure. Neither source can turn failure into success.

Reject `NeedsInput()` results. The current SDK can represent an input-required
response, which is not a completed result. The host owns that interaction and retry
policy. Reject sampling-only tool-use/result blocks rather than execute them.
Reject nil content entries, including typed nil pointers, without panic.

Convert blocks in their original order. Text remains text. Clone image/audio bytes
so later SDK mutation cannot alter an outcome. Marshal structured content before
returning anything. Append it after content unless existing JSON text represents
the same value. Compare numbers without float64 precision loss. Marshal failure
is an error, not omission. Validate the completed outcome before returning it.

For media, require nonempty bytes and a syntactically valid MIME type in the block's
image or audio family. Check recognizable media signatures against the declared
type, with explicit common MIME aliases. A recognized mismatch is an error.
For a supported type, an absent signature is also an error. Other media encodings
become an omission note and warning at that position. This validates the bridge without
pretending to decode every codec or guarantee provider support. It never turns
media into a URL or adds provider provenance.

Describe resource links with their name, URI, and available description/type.
These are descriptions, not fetched content. Text resources retain URI and text.
Binary resources become a note with URI, MIME, and byte count plus a warning.
Require a nonempty valid URI, valid optional MIME, and unambiguous text/blob fields.
Malformed resources are errors. Report discarded MCP metadata or annotations as
projection warnings without copying their values into diagnostics.

## Journal JSON and untrusted input

The raw wrapper removes Swarm's demonstrated complete-envelope/array mismatch.
Require one JSON result object with a `content` array, including an empty array.
Reject null, arrays, missing content, wrong field types, and trailing JSON.
Decode content using the SDK's `CallToolResult.UnmarshalJSON`, then call the typed
converter. Unknown content kinds produce decoding errors. Validate an optional
resultType as complete or input-required; the latter cannot convert successfully.
Allow unrelated extension fields without granting them authority.

Format selection precedes conversion and comes from the admitted tool identity.
The helper cannot prove provenance from bytes. The host retains the original
journal and tool/version beside the result. It also enforces permissions and size
limits before decoding. Conversion performs no file access, URL fetch, server
connection, input fulfillment, or tool execution. Resource descriptions and text
remain untrusted model input. Error and warning templates use block index/kind and
safe reasons, never raw payloads or SDK exception text containing input values.

## Alternatives, tradeoffs, and verification

- Keep Follow plus manual StepsAfter: smallest public surface, but each adopter
  must repeat cursor validation, paging, waiting, and cancellation rules.
- Copy the loop into feed: avoids a generic helper, but fixes and guarantees can
  drift. Share only the existing loop with a private completion policy.
- Export typed conversion only: sufficient for fresh SDK calls, but leaves the
  demonstrated journal envelope bridge. The thin SDK-decoded wrapper removes it.
- Move external tools into Toolset: removes some conversion calls but transfers
  concurrency, journal, permissions, and recovery authority. Keep conversion pure.

Costs are a small generic extraction and stricter rejection of malformed MCP input.
Unrecognized codecs produce visible omissions. Tail remains open until its caller
ends it, so guides must discourage one permanent watcher per historical bill.

Test Tail with a real canceled external-tool turn and a later committed result,
resuming at and after the stop, multi-page history, invalid readers, independent
readers, waiter failure/missed wakes, cancellation during read/wait, and range break.
Run existing root and feed Follow tests to pin compatibility. Test typed and JSON
MCP paths with ordered mixed blocks, numeric structured values, both failure flags,
empty/nil cases, incomplete results, invalid MIME/signatures, malformed envelopes,
resources, unsupported kinds, copied bytes, and safe diagnostics. Commit runnable
late-result and screenshot examples. Update both guides and Unreleased changelog.
Run `make check` and independent implementation/security reviews before publication.
No unresolved design question blocks the planning checkpoint.
