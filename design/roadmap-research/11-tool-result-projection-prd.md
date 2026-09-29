# 11 — Shorten large tool results: PRD

| Field | Value |
| --- | --- |
| Status | Implemented on branch `tools/result-bounds`, 2026-09-28. PR pending. |
| Roadmap | [Item 11](../remaining-work.md), "bound tool results". Size M, difficulty high. |
| Research | [Findings](11-tool-result-projection-research.md), [adjacent libraries](../gaps/C-adjacent-libraries.md), [after-tool hooks](../hooks-after-tool.md) |
| Ship target | First slice by Tuesday night, 2026-09-29, for the Wednesday demo |

Words used here: the *limit* is `Limits.ToolResultBytes`. A result is
*shortened* when its text exceeds the limit. The *model view* is what the
model reads of a tool result. The file name still says "projection", from the
first draft.

## Problem

A tool can return a large result: a shell log, a search, a file. Today Dashi
sends that result to the model unchanged, on every later model call. One
result can fill the context window and stop the turn. A coding agent meets
this often. A test run, `grep` on a large repository, or an MCP tool can print
megabytes.

A developer can cut the output inside the tool. Then the record loses what the
tool returned. Audit and recovery need that original.

Without this feature, every application writes its own clipping code and
decides alone how the model learns that text is missing.

## Goals

- Limit the text the model sees for each tool result. This works by default.
- Keep the original result in the turn record.
- Record the exact model view and the facts of the shortening. Replay never
  recomputes them.
- Tell the model that text is missing, without inviting it to repeat a call.
- Keep the API small. Add no reference type and no artifact store.

**Success measures.**

1. A tool prints 2 MB. The next model request holds at most 64 KiB of it.
   The record holds all 2 MB.
2. A zero `Agent` needs no configuration.
3. Replay, `Project`, and streaming give the same bytes for the model view.
4. Guardrail: a result under the limit is byte-identical to today's result.

The primary user is the application developer who writes tools and runs a
`Runner`. The `dashi` CLI is the first consumer. A host or UI that shows full
output to a human is secondary.

## Design

### One concept: the model view

A settled tool step has two things. `Outcome` is what happened. The model view
is what the model reads. An `AfterTool` hook can change the model view, and
`ToolFact.Result` records the change. `ToolView.Result()` returns the view.

Shortening uses this mechanism. It is one more step that changes the model
view. It does not replace `AfterTool`, and it adds no second view field.
Order at settlement:

1. The tool returns its `Outcome`. The record keeps it.
2. `AfterTool` hooks run, in order. A hook sees the full text, so a redactor
   sees every secret.
3. The shortening step runs last. A hook cannot bring the flood back.
4. Dashi commits one settlement with the outcome, the final view, and the
   shortening facts.

`examples/redact` stays as it is. The redactor changes meaning. The limit
changes size.

### API

```go
type Limits struct {
    // ToolResultBytes limits the text of one tool result in the model view.
    // Zero means 64 KiB. A negative value turns the limit off. A value
    // below 1 KiB is raised to 1 KiB, with a warning at Start.
    ToolResultBytes int `json:"tool_result_bytes"`
    // ...existing fields
}

// ToolFact gains one field. Dashi sets it, with Result, when it shortens.
type ToolFact struct {
    // ...existing fields
    Shortened *Shortened `json:"shortened,omitempty"`
}

// Shortened records how the limit changed the model view of one call.
// Sizes count text bytes after the AfterTool hooks ran. A redactor can make
// them differ from the tool's own output.
type Shortened struct {
    OriginalBytes int `json:"original_bytes"` // text before shortening
    ShownBytes    int `json:"shown_bytes"`    // text in the model view
    Limit         int `json:"limit"`          // the limit in force
}

// Shortened returns the recorded facts, or nil when the view is complete.
// It reads the record and never current policy.
func (v ToolView) Shortened() *Shortened
```

The comment on `ToolFact.Result` changes to: set when a hook or the limit
changed what the model sees.

`Agent.resolved` writes the default into the recorded agent at `Start`, as it
does for other limits. A resumed turn keeps its limit if the default changes.

### What the model sees

The limit counts the text bytes of all text blocks of one result, errors
included, plus up to 2 bytes for each gap between text blocks, because
providers join blocks with `\n` or `\n\n`. The view, marker included, holds at
most `ToolResultBytes` bytes of text. A shortened view has one text block.

- **Under the limit:** the view equals the original. Nothing is recorded.
- **Over the limit:** the view keeps the first half and the last half of the
  budget, with the marker between them. Shell and test output often ends with
  the error, so the tail matters.
  - Text blocks are one sequence and become one block in the view. Non-text blocks (images, resources) stay in
    place and are not counted.
  - Cuts move inward to a rune boundary. The text is valid UTF-8. Invalid
    bytes in the kept parts become U+FFFD.
  - `IsError`, `CallID`, and block order do not change.

The marker:

```text
[Tool output shortened: 2097152 bytes, 1998848 omitted. To see omitted text,
ask for a smaller part: a path, a pattern, or a line range. Do not repeat a
command that changes state only to see its output.]
```

The marker never tells the model to run the call again. Many tools have side
effects, and a second run of a slow suite costs time.

### Where it applies

The limit applies to the results `AfterTool` covers: executor results
(success, failure, `tool.Fatal`, `not_executed`) and `Settle` results. Text
the runner writes (denied, invalid, unknown calls) is short and is not
shortened. Provider-hosted results are not touched.

### Streaming, replay, and hosts

Dashi computes the view once, at settlement, and records it. `Project`,
`Prepare`, the stream path, and cold replay read the recorded view. No path
recomputes it. `Observer` and `Stream` events carry the raw output, so a live
reader shows the full result to the human. A host that wants to say "the model
saw less" reads `ToolView.Shortened()` after settlement. The events do not
change in this slice.

## User stories

**US-1: Default protection.** As a developer, I run a zero-value `Agent`. A
tool prints 2 MB.
- [ ] The next `Prepare` request holds at most 64 KiB of that result.
- [ ] `ToolView.Outcome` and the record hold all 2 MB.
- [ ] `ToolView.Shortened()` reports original, shown, and limit.

**US-2: Lower limit.** As a developer with a small-context model, I set
`Limits.ToolResultBytes` to 16 KiB.
- [ ] The view fits 16 KiB, marker included.
- [ ] A value of 100 becomes 1 KiB, with a warning at `Start`.
- [ ] A negative value gives the full result.

**US-3: Safe text.** As a developer, my tool prints multi-byte text.
- [ ] Cuts inside 2-, 3-, and 4-byte runes give valid UTF-8.
- [ ] A result of exactly the limit, and of the limit plus one byte, behave as
  stated above.
- [ ] Multi-block, error, and empty results work.

**US-4: Parity.** As an operator, I recover a turn.
- [ ] `Project`, Runner continuation, streaming, and cold replay give
  byte-identical views.
- [ ] A changed default changes neither a recorded view nor `Shortened()`.
- [ ] Recovery does not run the tool again.

**US-5: Live reader.** As a host, I show the full output to the human.
- [ ] The `Stream` and `Observer` events carry the full result.

**US-6: Hooks.** As a developer, I use a redactor and the limit.
- [ ] The hook sees the full text. The limit shortens the hook's result.
- [ ] `Shortened.OriginalBytes` counts the text after the hook.

## Also required

- `docs/guides/tools.md` gains a section on the limit. It says the limit
  applies to each result, so parallel calls add up: set a lower limit with
  `ToolConcurrency`. It says shortening is not redaction. `docs/guides/hooks.md`
  states the order of hooks and limit.
- Example `examples/bigresult` shows a large-log tool, the record, and the
  model view.
- Tests cover UTF-8 edges, errors, replay parity, and streaming parity.
- `design/remaining-work.md` item 11 is updated when the PR is ready.

## Decisions

| Decision | Reason |
| --- | --- |
| Reuse the `AfterTool` model-view mechanism. Add no second view field. | One concept, one recorded view. Replay stays the same. `examples/redact` stays valid. |
| Shortening runs after hooks. | A redactor sees all text. A hook cannot exceed the limit. |
| On by default at 64 KiB. | A zero `Agent` must work. pi uses 50 KiB. The demo run checks it against the `gpt-6-luna` window. |
| Half head, half tail. | Test and shell output ends with the error. Search output starts with the best match. |
| Accept review 1: cut `Output.Ref` and add no reference type. | `content.Resource` exists and item 16 plans another. Later, the marker can point at a `content.Resource` a tool supplies. |
| Accept review 2: record `Shortened` on `ToolFact`. | `Result` holds only text. After a redactor, sizes cannot come from `Outcome`. A default change must not alter old turns. |
| Accept review 3: the marker never says to run the call again. | A repeat can repeat a side effect or waste time. The marker gives sizes and advises a smaller read. |
| Accept review 4: sizes count text after hooks. Use "limit" and "shortened" everywhere. Keep the old file name. | One quantity, one meaning. Roadmap links use the file name. |
| Accept review 5: a limit below 1 KiB is raised with a warning at `Start`. | Zero-value rule: warn and adjust. The marker needs room. |
| Review 6: reject event changes. Hosts read `ToolView.Shortened()`. | Smaller API. Add event fields when the CLI needs them. |
| Accept review 7: the guide explains parallel calls. | The limit is per result, so eight calls send eight times as much. |
| Implementation: `Shortened.ShownBytes` counts kept original text, not the marker. The marker fits inside the limit. A new `WarningLimitAdjusted` (Start steps only) reports a raised limit. | The PRD left both open. Replay rejects a `Shortened` record with no `Result`, no limit, or sizes that do not shrink. |
| Implementation review: accept the continuation and recovery test; the runner rejects a changed limit with `ErrAgentChanged`, so a new limit applies to new turns. | US-4 needed proof that recovery reads the record and does not run the tool again. |
| Implementation review: the shortened view drops `Output.Display`. | No provider sends `Display` to the model, and keeping it would store big text twice. |
| Implementation review: the marker says the omitted middle was not shown and the model must make no claims about it. | A live run showed the model claiming "no errors" in text it never saw. |
| Implementation review: skip the copy and compare when no hook ran and the text fits. | A large result under a disabled limit did needless work. |
| Record no digest and no version. | The view holds the exact bytes. `Outcome` holds the original. |
| Keep the full result in the record. | The record grows as it does today. |
| No per-tool limit and no typed error for non-text blocks. | One knob is enough for the demo. |

## Out of scope

- Summaries made by a model.
- An artifact store, a fetch tool, or a full-output reference.
- Token-exact budgets. A request too large for the model still ends with the
  normal context error.
- Limits for images, documents, JSON structure, or provider-hosted results.
- Session compaction (roadmap item 15).

## Later work

- Marker text that points at a `content.Resource` a tool supplies.
- Per-tool limits on `tool.Definition`.
- A request-byte ceiling with a typed error for content that cannot be cut.
- A `dashi` CLI flag for the limit, and CLI display of `Shortened`.
- `Shortened` on settlement events.

## Risks

| Risk | Mitigation |
| --- | --- |
| 64 KiB does not fit every model. | The knob exists. The guide tells developers to set it. |
| The model needs the middle of the text. | The marker advises a smaller read. Coding tools offer paging. |
| Large errors hold secrets. | Shortening is not redaction. The guide says so. |
| The record stays large. | Known and stated. |
| Time: due Tuesday night. | Cut the example first. The default limit and recording are the demo path. |
