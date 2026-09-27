# Model content and provider replay

Status: accepted foundation contract, 2026-09-27. Supports portable text,
completed tool exchanges with deterministic ID remapping, and explicit omission
of unreplayable reasoning. Other loss policies remain deferred.

## Purpose

A saved conversation may continue with another selected model, including a
model on the same provider. Kernel must explain what preparation preserves,
changes, or refuses. This is an intended library capability, not a claim that
an application already requires every conversion.

Saved messages retain their original content and provenance. The configured
client's single offline `Prepare` path derives a destination-specific
representation, records changes, and freezes the exact body. `Send` makes at
most one dispatch. Preparation and entering `Send` prove neither dispatch nor
receipt; the caller owns attempts, retries, and execution records.

## Content and provenance

- `Message.Source` retains requested and reported model references, API, and
  response ID when a generated message leaves its `Result`. Unknown reported
  facts stay empty. Partial output has source identity; failure without a
  provider message does not manufacture it. `Result` keeps its own call facts.
- `Content.Replay` holds private data on known content, including reasoning.
  `Content.Opaque` holds content whose semantics Kernel does not understand.
  Unknown provider output never becomes an executable tool call by inference.
- `Opaque.Unit` identifies an item, content block, or message part. `Partial`
  prevents replay. `Version` identifies Kernel's wrapper: only version 1 is
  replayable. Other versions remain inspectable, without migration by guesswork.
- `content.Source.Origin` is required for provider file IDs, including files in
  tool outputs. Inline bytes and URLs have no file origin. Kernel fetches neither.

Origins retain known provenance. Compatibility rules decide which fields must
match; they never erase an originating model to express broader compatibility.
Private replay and citations require exact provider/API/model/scope matches.
Files require provider/API/endpoint-and-credential scope, independently of model.
The scope includes credentials, so credential rotation conservatively requires
requalifying file references. A matching origin does not prove API acceptance.

## Preparation rules

The shared internal history step owns traversal, reference checks, call/result
pairing, and decision recording. Source API guards recognize replay shapes;
provider encoders own wire conversion. The original `Input` is never mutated.

| Content | Initial behavior |
| --- | --- |
| Recognized text or tool call with incompatible replay | Strip the attachment and record `strip_replay` only when its canonical representation preserves meaning. |
| Unfamiliar private fields | Preserve unchanged at the original destination; reject stripping or edits with unknown dependencies. |
| Compatible reasoning or unknown opaque content | Replay complete, supported structural units. Display reasoning does not replace signed or encrypted data. |
| Unreplayable reasoning | Reject by default; an explicit call policy may omit recognized reasoning and record `omit_reasoning`. |
| Incompatible opaque content or citations | Reject. The reasoning policy cannot authorize their omission. |
| Provider file ID | Check its origin, including inside tool results; reject missing or incompatible provenance. |
| Tool exchanges | Require unique calls and exactly one later result per call. Reject unresolved calls, orphan results, or ambiguous pairing. |

The initial tool-ID subset is 1–64 ASCII letters, digits, underscores, or hyphens.
This is Kernel's conservative supported subset, not every ID an API may accept.
Nonempty UTF-8 IDs outside it are deterministically hashed into supported IDs.
Preparation reserves existing supported IDs first and resolves collisions without
changing them. Each changed call and its result get matching `remap_tool_id`
decisions with `from_id` and `to_id` at their original positions. Empty or invalid
UTF-8 IDs fail. Saved execution IDs stay unchanged.

Only canonical correlation IDs change. Arguments, opaque data, and independent
provider item IDs retain their identity. Remapping runs before replay edit checks;
unfamiliar private fields prevent an edit whose dependencies are unknown.
Gemini also uses this shared step; signed tool calls reject ID additions or
changes. Only recognized unsigned Gemini function-call replay permits remapping,
and foreign Gemini private replay remains unsupported. A signed call without a
provider ID keeps Kernel's internal call/result correlation while its wire
function response omits the ID.

A known content kind alone does not establish portability. Unrecognized fields,
nonempty output-phase metadata, and unmodeled semantics prevent automatic
stripping. Edits overlay canonical fields only when their dependencies are
understood. Editing cited text while retaining dependent citation data fails.
Unchanged replay preserves provider data semantically; it does not promise the
original JSON formatting on a subsequent request.

`RequestSnapshot.ContentChanges` records typed actions, reasons, and original
message/content indices. `Adjustment` remains the settings vocabulary. Existing
representations of developer instructions as system content and tool error flags
as visible text are also recorded. The snapshot retains original input and the
exact outgoing body, rather than another mutable effective-message collection.

## Reasoning policy and evidence

The call selects its loss policy explicitly:

```go
input.ContentPolicy = llm.ContentPolicy{
    UnreplayableReasoning: llm.ReasoningOmit,
}
```

The zero value, `ReasoningReject`, preserves strict rejection. `ReasoningOmit`
omits only reasoning the target cannot replay; compatible reasoning stays intact.
It never turns reasoning into ordinary text. The original input retains the
policy and omitted payloads, with each decision using the original indices.
Messages emptied by omission leave the wire history; unusable history is rejected.

Omission requires deliberate classification of known reasoning. Anthropic
`thinking` and `redacted_thinking`, and Responses `reasoning`, carry private replay
as `ContentReasoning`. Before omission, source shape, unit, and wrapper version
must be recognized. Unfamiliar fields, malformed data, partial payloads, and
unsupported versions remain rejected. Replayless canonical reasoning may be
omitted when the target cannot encode it. Unknown opaque blocks stay opaque.

Plain-reasoning support is an explicit internal encoder capability, independent
of model capability evidence. Reasoning-to-text conversion, citation omission,
file replacement, and general opaque omission remain deferred.

Fixtures check serialization, source retention, strict rejection, paired history,
change records, immutable input, and equality of prepared and dispatched bytes in
complete and streaming calls. Live provider qualification is separate; local
success cannot establish remote acceptance or qualify another endpoint or model.

## Stored record changes

This extends the already implemented [terminology decision](terminology-proposal.md):
`Content.ProviderData` and `Reasoning.Replay` become `Content.Replay`; messages gain
`source`, file sources gain `origin`, opaque wrappers gain `version`, and request
snapshots gain `content_changes`. Result identity fields remain intact.

The reasoning policy adds `input.content_policy` and `omit_reasoning` decisions.
Tool-ID mapping adds `remap_tool_id` decisions with `from_id` and `to_id`.
Previously recorded `Opaque` redacted-thinking blocks remain opaque; migrating
them to known reasoning requires explicit source-aware conversion.

Legacy replay field names are rejected during decoding. Unversioned wrappers
cannot be prepared. Migrate prototype records explicitly, retaining their known
origin and structural unit; never invent missing file provenance. This wrapper
version does not version the entire message or request-snapshot schema.
