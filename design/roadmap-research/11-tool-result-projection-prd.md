# 11 — Bound tool results: PRD

| Field | Value |
| --- | --- |
| Status | Draft, 2026-09-28. Awaiting review. |
| Roadmap | [Item 11](../remaining-work.md). Size M, difficulty high. |
| Research | [Findings](11-tool-result-projection-research.md), [adjacent libraries](../gaps/C-adjacent-libraries.md), [after-tool hooks](../hooks-after-tool.md) |
| Ship target | First slice by Tuesday night, 2026-09-29, for the Wednesday demo |

## Problem

A tool can return a large result: a shell log, a search, a file. Today Dashi
sends that result to the model unchanged, on every later model call. One
result can fill the context window and stop the turn. A coding agent meets
this often. A test run, `grep` on a large repository, or an MCP tool can print
megabytes.

A developer can cut the output inside the tool. Then the record loses what the
tool really returned. Audit and recovery need that original.

Without this feature, every application writes its own clipping code. Each one
also has to decide how the model learns that text is missing.

## Goals

- Bound the text the model sees for each tool result. This works by default.
- Keep the original result in the turn record.
- Record the exact view the model saw. Replay never recomputes it.
- Tell the model that text is missing and how to get it.
- Keep the API small. Add no artifact store.

**Success measures.**

1. A tool prints 2 MB. The next model request holds at most 64 KiB of it,
   plus a marker. The record holds all 2 MB.
2. The default needs no configuration. A zero `Agent` protects the turn.
3. Replay, `Project`, and streaming give the same bytes for the model view.
4. Guardrail: a result under the limit is byte-identical to today's result.

## Design

### One concept: the model view

A settled tool step already has two things. `Outcome` is what happened. The
*model view* is what the model reads. An `AfterTool` hook can change the model
view, and `ToolFact.Result` records the change. `ToolView.Result()` returns the
view.

Bounding uses this same mechanism. The bound is one more step that changes
the model view. It does not replace `AfterTool`. It does not add a second
record field for the view.

Order of steps at settlement:

1. The tool returns its `Outcome`. The record keeps it.
2. `AfterTool` hooks run, in order. A hook sees the full text. A redactor
   therefore sees every secret, not a clipped part.
3. The bound runs last. It clips the text the hooks left. A hook cannot
   bring the flood back.
4. Dashi commits one settlement with the outcome and the final view.

`examples/redact` stays as it is. The redactor changes meaning. The bound
changes size. Both write the same view.

### API

```go
type Limits struct {
    // ToolResultBytes caps the text of one tool result in the model view.
    // Zero means 64 KiB. A negative value turns the cap off. A value below
    // 1 KiB is raised to 1 KiB.
    ToolResultBytes int `json:"tool_result_bytes"`
    // ...existing fields
}

// Bound reports how the cap changed the model view of a settled call.
// It is nil when the view is the full result.
func (v ToolView) Bound() *Bound

type Bound struct {
    OriginalBytes int // text bytes in the original result
    OmittedBytes  int // text bytes not in the model view
    Limit         int // the cap in force
}

// In package tool. A tool or a host executor sets it to point at the
// full output.
type Output struct {
    // ...existing fields
    Ref *Reference `json:"ref,omitempty"`
}

type Reference struct {
    URI     string    // where the host can read the full output
    Expires time.Time // zero means no expiry is stated
}
```

`Agent.resolved` writes the default into the recorded agent at `Start`, as it
does for other limits. A resumed turn keeps its cap even if the default
changes later.

### What the model sees

The cap counts the text bytes of all text blocks in one result, errors
included. The view, marker included, holds at most `ToolResultBytes` bytes of
text.

- **Under the cap:** the view equals the original. Nothing is recorded in
  `ToolFact.Result` for the cap.
- **Over the cap:** the view keeps the first half and the last half of the
  budget, and puts a marker between them. Shell and test output often ends
  with the error, so the tail matters.
  - Text blocks are treated as one sequence. Non-text blocks (images,
    resources) stay in place and are not counted.
  - Cuts move inward to a rune boundary. The clipped text is valid UTF-8. If
    the original had invalid bytes in the kept parts, they become U+FFFD.
  - `IsError` does not change. A successful tool stays successful.

Marker, with no reference:

```text
[Tool output shortened: 2097152 bytes, 1998848 omitted. Narrow the query
(for example with a path, a pattern, or a line range) and run the tool again.]
```

Marker, with a valid reference:

```text
[Tool output shortened: 2097152 bytes, 1998848 omitted. The full output is at
file:///scratch/run-42.log until 2026-10-01T12:00:00Z. Read it with a tool
that can open that address, or narrow the query and run the tool again.]
```

Dashi validates a reference: a non-empty URI, no control characters, at most
2048 bytes, and an expiry later than the settlement time. An invalid reference
is dropped and the marker uses the first form. Dashi does not add a fetch
tool. The host owns storage and access checks. A local temporary path may not
survive recovery, so the host chooses a URI that will.

### Where it applies

The bound runs for the same results as `AfterTool`: executor results (success,
failure, `tool.Fatal`, `not_executed`) and `Settle` results. Text the runner
writes (denied, invalid, unknown calls) is short and is not bounded. Hosted
results from a provider are not touched.

### Streaming and replay

Dashi computes the view once, at settlement, and records it. `Project`,
`Prepare`, the stream path, and cold replay read the recorded view. No path
recomputes it. `Observer` and `Stream` events keep the raw output, so a live
reader shows the full result to the human.

## User stories

**US-1: Default protection.** As a developer, I run a zero-value `Agent`. A
tool prints 2 MB. The model gets about 64 KiB and a marker.
- [ ] The next `Prepare` request holds at most 64 KiB of that result.
- [ ] `ToolView.Outcome` and the record hold all 2 MB.
- [ ] `ToolView.Bound()` reports the sizes.

**US-2: Lower limit.** As a developer with a small-context model, I set
`Limits.ToolResultBytes` to 16 KiB.
- [ ] The view fits 16 KiB, marker included.
- [ ] A value of 100 becomes 1 KiB.
- [ ] A negative value gives the full result.

**US-3: Safe text.** As a developer, my tool prints multi-byte text.
- [ ] Clipped text is valid UTF-8 for cuts inside 2-, 3-, and 4-byte runes.
- [ ] A result of exactly the cap, and of the cap plus one byte, behave as
  stated above.
- [ ] Multi-block results, error results, and empty results work.

**US-4: Parity.** As an operator, I recover a turn.
- [ ] `Project`, Runner continuation, streaming, and cold replay give
  byte-identical views.
- [ ] A changed default does not change a recorded turn.
- [ ] Recovery does not run the tool again.

**US-5: Live reader.** As a host, I show the full output to the human.
- [ ] The `Stream` and `Observer` events carry the full result.

**US-6: Reference.** As a tool author, I set `Output.Ref`.
- [ ] A valid reference appears in the marker with its expiry.
- [ ] An expired or malformed reference is left out of the marker.

**US-7: Works with hooks.** As a developer, I use a redactor and the cap.
- [ ] The hook sees the full text. The bound clips the hook's result.

## Also required

- Guide `docs/guides/tools.md` gains a section on limits and references.
  `docs/guides/hooks.md` states the order of hooks and cap.
- Example `examples/bigresult` shows a large-log tool, the record, and the
  model view.
- Tests cover UTF-8 edges, errors, replay parity, and streaming parity.
- `design/remaining-work.md` item 11 is updated when the PR is ready.

## Decisions

| Decision | Reason |
| --- | --- |
| Reuse the `AfterTool` model-view mechanism. Add no second view field. | One concept, one record field. Replay code stays the same. The redact example stays valid. |
| The bound runs after hooks. | A redactor sees all text. A hook cannot exceed the cap. |
| On by default at 64 KiB. | A zero `Agent` must work. The number comes from pi (50 KiB) and app experience. A test with a real consumer can change it. |
| Half head, half tail. | Test and shell output ends with the error. Search output starts with the best match. |
| Record no digest and no projection version. | The recorded view holds the exact bytes. `Outcome` holds the original. A digest adds nothing that replay needs. |
| Cut the typed "projection required" error for non-text blocks. | No host needs it yet. Later work can add a request-byte ceiling. |
| Raise a cap below 1 KiB to 1 KiB. Do not fail. | The marker needs room. Zero-value config rules say adjust and do not fail. |
| No per-tool limit. | One knob is enough for the demo. Tools that need more return a reference. |
| Keep the full result in the record. | The record grows as it does today. The guide says how to keep large bytes out by returning a reference instead. |

## Out of scope

- Summaries made by a model.
- An artifact store, or a fetch tool.
- Token-exact budgets. The cap protects one result, not the whole request. A
  request too large for the model still ends with the normal context error.
- Bounds for images, documents, JSON structure, or provider-hosted results.
- Session compaction (roadmap item 15).

## Later work

- Per-tool limits on `tool.Definition`.
- A request-byte ceiling with a typed error for content that cannot be
  clipped.
- A host artifact reference in place of large inline record bytes.
- A `dashi` CLI flag for the cap, and CLI display of the marker.

## Risks

| Risk | Mitigation |
| --- | --- |
| 64 KiB does not fit every model. | The knob exists. The guide tells developers to set it. |
| The model needs the middle of the text. | The marker tells it to narrow the query. Coding tools offer paging. |
| Large errors hold secrets. | The bound is not redaction. The guide says so, as for `AfterTool`. |
| The record stays large. | Known and stated. Measured in the example. |
| Time: due Tuesday night. | Cut `Output.Ref` and the example first. The default cap and recording are the demo path. |
