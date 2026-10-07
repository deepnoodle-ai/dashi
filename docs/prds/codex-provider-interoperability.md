# Run empty tools and continue portable history across providers

Status: Approved\
PRD PR: [#228](https://github.com/deepnoodle-ai/dashi/pull/228) · Implementation PR: [#233](https://github.com/deepnoodle-ai/dashi/pull/233) · Updated: 2026-10-07

## Problem and context

Application developers need a conversation to continue without manual transcript repair.
Issue [#213](https://github.com/deepnoodle-ai/dashi/issues/213) reports that an Anthropic call to an MCP tool with schema `{"type":"object"}` fails with `incomplete tool arguments`.
Issue [#219](https://github.com/deepnoodle-ai/dashi/issues/219) reports rejected Anthropic history after selective compaction and an OpenAI provider switch.
The stock compactor retains the same replay attachments in its tail. Uncompacted conversations can encounter the same boundary.

The [accepted replay contract](../../design/provider-content-replay.md) already assigns conversion to offline provider preparation.
`internal/providercontent` clones history, checks replay meaning, pairs tools, and records `strip_replay` changes.
Existing fixtures accept recognized Anthropic text and completed tool exchanges across providers.
The retained corpus adds `caller: {"type":"direct"}` to tool blocks, which the replay guard treats as unfamiliar.
Anthropic [defines this shape](https://platform.claude.com/docs/en/agents-and-tools/tool-use/programmatic-tool-calling#the-caller-field-in-responses) as ordinary direct invocation.
Use a sanitized regression to recognize this shape. Keeping replay data in saved history is intentional.

Anthropic stream parsing currently rejects an empty accumulated argument string at block stop.
Its [streaming contract](https://platform.claude.com/docs/en/agents-and-tools/tool-use/fine-grained-tool-streaming) separates the initial input object from streamed JSON fragments.
A completed empty tool must not become indistinguishable from malformed or interrupted input.

## Users and job

The primary user is a Go application developer running local tools and retaining conversations across model choices.
The job is to run a tool with no arguments, then continue supported saved history on another configured provider.
No new public concept, setting, or API is needed.

## Use cases and acceptance

### UC-1: Run an MCP tool with no arguments

The developer opens an MCP toolset and supplies it to `dashi.Runner` with an Anthropic client.
The provider returns a completed `tool_use` block whose input is `{}`.

- [ ] A stream with no argument fragments or only empty fragments produces one tool call with `{}` arguments.
- [ ] Empty fragments before or between nonempty fragments do not change the final JSON arguments.
- [ ] A fixture using the actual Anthropic client and MCP server runs the empty-schema tool once, records its result, and completes the turn.
- [ ] Complete and streamed responses agree on canonical arguments, call identity, and complete replay data.
- [ ] Nonempty malformed JSON still fails with a protocol error. Interrupted blocks retain partial evidence and never execute a tool.
- [ ] The parser does not fabricate `{}` for absent or invalid initial input. Existing block lifecycle checks remain enforced.

### UC-2: Continue retained history on another provider

The developer keeps original generated messages, including their replay attachments, and prepares a request on the selected destination:

```go
input := llm.Input{Model: "destination-model", Messages: savedHistory}
request, err := destination.Prepare(ctx, input)
if err != nil { return err }
changes := request.ContentChanges()
result, err := destination.Send(ctx, llm.Call{Request: request})
```

- [ ] A sanitized fixture reproduces #219's direct-caller replay rejection before the fix and demonstrates supported preparation afterward.
- [ ] Recognized canonical text and completed tool exchanges prepare across Anthropic, OpenAI Responses, and Chat Completions destinations.
- [ ] This holds for unchanged conversation history and retained messages from either compaction strategy. Compactors need no stripping rule.
- [ ] Each stripped attachment records `strip_replay`, a reason, and its original message and content indices.
- [ ] Text, tool names, argument values, and call/result pairing survive conversion. Existing deterministic ID remapping stays intact.
- [ ] Preparation makes no network request. Saved messages and the original request input retain replay data and provenance.
- [ ] Snapshot restoration and dispatch preserve the exact prepared bytes for complete and streamed requests.

### UC-3: Refuse content whose meaning cannot be preserved

- [ ] Foreign unfamiliar fields, citations, signatures, opaque content, partial wrappers, and unsupported wrapper versions remain rejected before dispatch.
- [ ] A source field gains portability only when its semantics are modeled and canonical content preserves those semantics.
- [ ] Anthropic `caller` is portable only as exactly `{"type":"direct"}` or when absent. Programmatic callers and additional nested fields remain unsupported for conversion.
- [ ] Unchanged compatible replay retains unfamiliar provider fields. Editing content with unknown dependencies remains rejected.
- [ ] Existing explicit reasoning omission stays narrow. This work does not authorize reasoning, citation, or opaque omission.
- [ ] Unsafe #219 content receives a documented safe rejection outcome rather than blanket replay removal.

## Decisions and scope

- Keep adaptation in provider preparation. Every retained conversation uses this boundary. Reject compactor-specific replay deletion.
- Distinguish empty argument fragments from malformed nonempty input in the Anthropic parser. Reject general JSON repair or schema-based argument invention.
- Model only verified portable source shapes. Reject accepting every attachment on a known content kind.
- Preserve saved provenance and record preparation changes. Reject rewriting stored history to suit the latest model.
- Use keyless local fixtures for evidence. Reject paid requests for these deterministic defects.

Implementation owns Anthropic parsing, shared replay guards, MCP regression fixtures, provider preparation tests, focused guidance, and the changelog.
Selective ranking, compaction policies, provider-hosted tool protocols, and unrelated provider features are outside this batch.

## Success and remaining uncertainty

The empty-schema tool completes a recorded turn. Supported retained history needs no manual replay removal when the developer changes provider.
Unsafe content still fails before dispatch. Required independent review and `make check` pass.
Fixtures establish library behavior, not live acceptance by every provider model or endpoint.

Structural inspection of the original corpus resolved the #219 shape without exposing session content.
Keep private recordings outside Git and replace all session text with synthetic values in regression fixtures.
