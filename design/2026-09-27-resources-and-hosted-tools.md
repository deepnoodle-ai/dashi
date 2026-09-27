# Resource references and provider-hosted tools

**Status:** In progress · **Date:** 2026-09-27

## Context and Dive baseline

At the start of this work, Kernel could represent a `content.Resource` URI
and mark a tool definition `provider_hosted`, but no adapter encoded either.
`tool.Definition.Validate` required a JSON Schema even for a hosted tool.
Anthropic Messages, OpenAI Responses, and Gemini rejected hosted definitions;
generic resource blocks still fail provider encoding by design. Kernel already encoded some media references:
OpenAI Responses accepts image/file URLs and file IDs; Anthropic accepts image
and document URLs/IDs; Chat Completions accepts image URLs and document IDs.
Gemini then accepted inline media only.

Dive has no general resource block. Its media `ContentSource` supports inline
data, URL, and provider file ID; the Google adapter converts image/document URLs
to GenAI `fileData` URIs. Its hosted-tool coverage is broader: Anthropic web
search and code execution, server-tool result replay, and remote MCP; OpenAI
Responses accepts native hosted-tool params and decodes web/file search, code
interpreter, image generation, and remote MCP activity/approvals (its dedicated
tool constructor covers web search); Grok web/X/collections search and code
execution; Meta web search. Dive's Anthropic computer tools are
**client-executed** and belong to a
separate port. Dive does not enable Gemini built-ins. It sometimes identifies
hosted history by ID prefix and drops foreign blocks without a recorded
decision. Sources: Dive `llm/content.go`, `providers/{anthropic,openai,grok,meta,google}`;
[Anthropic server tools](https://platform.claude.com/docs/en/agents-and-tools/tool-use/server-tools),
[OpenAI Responses tools](https://platform.openai.com/docs/api-reference/responses-streaming/response/web_search_call),
[Gemini file inputs](https://ai.google.dev/gemini-api/docs/generate-content/file-input-methods),
[xAI tool usage](https://docs.x.ai/developers/tools/tool-usage-details).

## Target contract

1. **Resource reference is a location, not fetched content.** Keep inline
   data, public URL, and provider file ID as distinct `content.Source` forms.
   Add a mutually exclusive provider-managed URI source with required MIME
   type and origin; add optional expiry to references. Keep `Resource` for
   display/output links with provenance, not as a generic model input. A host
   resolves private or application URIs *before* `Prepare`; Kernel never fetches
   them implicitly. An adapter may encode a supported URI as a native reference
   (Gemini `fileData`, for example). Otherwise `Prepare` rejects it, including
   in tool output. Never turn a resource URI into text silently.
2. **Hosted definitions are configured, never locally executable.** Extend
   `tool.Definition` with a hosted variant: provider/API identity, wire tool
   type, and immutable JSON options. Local/external definitions require Schema;
   hosted definitions require the hosted variant and no Schema. Provider
   packages offer typed constructors and validate allowed options; adapters
   reject a foreign tool or unsupported combination. Model evidence records
   support for each hosted tool type, beyond the current coarse `tools` flag.
3. **Hosted activity is observable and replayable.** Add distinct hosted-use,
   hosted-result, and approval content kinds in `llm`, with provider origin,
   status, stable IDs, display fields, and original structural `Replay` data.
   Never convert them to `tool.Call`: callers must not execute them. Preserve
   the provider's item/block order; a Responses item may carry call and result
   in one item, whereas Anthropic emits separate blocks. Unknown variants stay
   opaque. Complete and streamed decoding must yield the same final content.
4. **Continuation is a state rule.** Completed hosted work replays only to its
   original destination. A pending Anthropic server call may remain at the
   tail of a `pause_turn` or mixed client/server turn; it is not treated as an
   unanswered client call. Foreign hosted activity is rejected by default.
   An explicit history policy may omit *completed* foreign activity with a
   recorded `omit_hosted_activity` decision; it must not omit pending calls,
   approvals, or citations that depend on private replay. No ID-prefix
   inference or silent omission.
5. **Credentials stay outside recordable intent.** Remote MCP authorization
   may appear in the JSON body. Before enabling authenticated MCP, split the
   in-memory exact request from a safe recordable snapshot (redacted body plus
   digest and named secret bindings), and bind those secrets to a credential
   scope. `Prepare` and `Send` must still compare exact in-memory bytes. Do not
   put MCP tokens into `Input`, `RequestSnapshot`, `Raw`, or errors. Approval
   requests/responses are explicit content and require an application decision;
   Kernel never auto-approves.

## Delivery order and acceptance

| Step | Work | Acceptance |
| --- | --- | --- |
| 1. Lock the contract | Add hosted definition/activity types, clone/validation, capability evidence, replay and stop rules. Update `providercontent` to distinguish client calls from hosted activity. | Schema-free hosted definition validates; no hosted activity becomes executable; complete, paused, mixed, and interrupted history each has a deterministic result. |
| 2. Finish references | Implement Gemini URL/file URI inputs with MIME and scope checks. Audit media URL/ID paths in all adapters; add resource provenance/expiry and explicit unsupported errors for arbitrary URIs. Preserve resource links in hosted/MCP outputs without implicit fetch. | A reference either reaches the intended wire field unchanged or fails before dispatch; foreign provider IDs/URIs fail; fixture tests cover input and tool output. |
| 3. Port primary hosted tools | Start with Anthropic web search, web fetch replay, code execution and MCP; OpenAI Responses web search, file search, code interpreter, image generation and MCP. Copy useful Dive files with `cp`, then adapt to Kernel's contracts. | Request configuration, blocking/stream results, citations, error results, `pause_turn`, mixed client/hosted calls, and same-origin replay work in local fixtures. |
| 4. Port destination tools | Add Grok web/X/collections search and code execution, Meta web search. Add Gemini Google Search, URL Context, file search and code execution where model/API evidence supports them, exceeding Dive's Gemini coverage. Select v1beta explicitly if a tool requires it; changing API version changes replay scope. | Typed options preserve each vendor's distinct wire names and limits; includes expose requested search/code results; unsupported models/tools fail in `Prepare`. |
| 5. Qualify | Compare every Dive fixture and feature to the Kernel matrix, then run opt-in live tests by exact provider, model, endpoint, and tool version. | `go test -race ./...` and local wire/stream parity pass; live calls verify a real hosted invocation, continuation/replay, references, citations, and usage. Unverified combinations remain labeled unverified. |

## Tradeoffs and decision points

Provider-specific tool options stay at adapter boundaries; a single portable
`web_search` option set would hide incompatible controls. The hosted activity
types add public API surface but prevent an agent from mistaking provider work
for executable client calls. Resource resolution remains application-owned so
offline preparation and exact request capture stay intact. Remote MCP needs a
change to the current exact-body snapshot contract; implement that change before
MCP auth, and document which bytes the durable record can safely retain.

The first fixture spike must verify that a provider-managed URI in `Source`
covers Gemini file inputs while `Resource` remains a display/output block for
MCP links. If that split fails, revise the contract before step 2.

## Progress and remaining gates

The shared types, scoped URI references, recorded foreign-hosted omission,
Anthropic web search/web fetch/code execution and unauthenticated MCP,
Responses web/file/code/image and unauthenticated MCP, Grok and Meta hosted
definitions, and Gemini built-ins are implemented with offline fixtures.
Anthropic history and OpenAI approval fixtures cover continuation; Anthropic
stream and complete fixtures retain private result data. Gemini file search
requires an explicit v1beta endpoint. Authenticated MCP remains disabled until
request snapshots can redact tokens while retaining exact in-memory dispatch
bytes. Live provider qualification and a full Dive fixture comparison remain
open; local fixtures do not establish service acceptance.
