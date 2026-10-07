# Empty tool streams and portable direct-call replay

**Status:** In Review · **Author:** Codex · **Date:** 2026-10-07\
**Workflow:** Spec → build in parallel, following the independent PRD review.

## Context and goals

The [PRD](../prds/codex-provider-interoperability.md) covers #213 and #219.
Empty Anthropic argument deltas currently create an invalid empty JSON accumulator.
Retained Anthropic tool blocks contain `caller: {"type":"direct"}`, which shared preparation rejects as unfamiliar.
Fix both defects without rewriting history, inventing arguments, or accepting unmodeled dependencies.

## Proposal

Keep the existing flow: provider stream decoder produces a saved message, then shared offline preparation derives the destination request.
The parser owns stream completion. `internal/providercontent` owns replay portability and original-position change records.
MCP and compaction continue to supply ordinary tool definitions and retained messages.

### Empty stream fragments

In `providers/anthropic/messages/stream.go`, do not create or change an argument accumulator for an empty `input_json_delta` string.
This preserves the validated initial input when the completed block has no argument bytes.
Nonempty fragments continue to accumulate exactly, including whitespace. They replace the placeholder only when they form valid JSON.
Block-stop checks, final reconstruction, and partial replay marking use the same distinction.
Missing or malformed initial input still fails decoding. Nonempty malformed JSON and interrupted lifecycles still fail.
Do not consult a tool schema or synthesize an empty object.

### Direct caller metadata

In `internal/providercontent/replay.go`, extend the Anthropic `tool_use` recognized field set to include `caller`.
The object is understood only when it has exactly one key, `type`, with the string value `direct`.
An absent caller remains recognized for older saved messages. Null, malformed, programmatic, or extended caller objects are not recognized.
Anthropic [defines direct and programmatic callers separately](https://platform.claude.com/docs/en/agents-and-tools/tool-use/programmatic-tool-calling#the-caller-field-in-responses).
Canonical `tool.Call` already represents direct invocation through its name, arguments, and call/result correlation.
Programmatic callers reference a code-execution run, which canonical direct calls cannot preserve.

Shared preparation can strip a foreign recognized direct-call attachment and record `strip_replay`.
It still rejects unfamiliar foreign replay and edits whose dependent fields are unknown.
Compatible unchanged replay continues to retain every original provider field.
No public API, serialized record, module dependency, or compactor changes are needed.

## Evidence and failure boundaries

- Sanitized parser fixtures cover no deltas, empty deltas, fragmented arguments, malformed nonempty fragments, invalid initial input, and interrupted blocks.
- An MCP runner fixture uses the actual Anthropic client and empty-schema test server. It proves one execution, a recorded result, and successful continuation.
- A provider fixture decodes direct-caller Anthropic history, retains text and paired tool exchanges, then prepares and dispatches frozen requests on supported destinations.
- The same shape fails before the guard change. Negative fixtures cover programmatic callers, unfamiliar nested fields, malformed caller values, unknown outer fields, partial replay, and unsafe edits.
- Existing citation, opaque, reasoning, pairing, source identity, and tool-ID remapping guards remain covered.
- Run focused tests, race tests for the changed boundaries, and the repository's `make check` before the implementation review.

Fixtures use synthetic text and values. They establish local parser and preparation behavior, not live provider acceptance.
This batch makes no paid request. Private corpus files remain outside Git and PR descriptions.

## Alternatives and consequences

Strip replay in each compactor: simpler locally, but it loses saved provenance and leaves uncompacted history exposed. Shared preparation serves every caller.

Accept any caller field on a known tool block: simpler code, but it discards code-execution dependencies and future semantics. Use a closed source shape.

Replace invalid or empty JSON with `{}` at block stop: a small patch, but it can execute incomplete or malformed requests. Ignore only empty fragments.

The closed guard requires deliberate updates for future Anthropic caller shapes. This cost preserves safe conversion and clear ownership.
Ship as a compatible defect fix with a changelog entry and focused provider guidance. No migration or feature flag is required.
