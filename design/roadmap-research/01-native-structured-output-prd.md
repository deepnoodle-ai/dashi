# PRD: native structured output for direct model calls

| Field | Value |
| --- | --- |
| Status | Draft for independent design review; no implementation yet |
| Date | 2026-09-28 |
| Roadmap | [Item 1](../remaining-work.md), M / medium |
| Evidence | [Source review](01-native-structured-output-research.md), [turn output contract](../structured-output.md), Dashi `16700d1` |

## User problem and outcome

A Go developer extracting a small object with `llm.Generate` cannot request a provider's JSON Schema response mode or receive a locally validated value. They either assemble provider-specific HTTP requests or run an agent turn with a synthetic final tool. A successful direct call should give them one checked JSON object, while a refusal, output limit or failed validation remains explicit with its usage and provider evidence.

This first slice covers **direct calls through the OpenAI Responses adapter**. It does not change agent turns. The turn's `OutputSchema` and recorded `Outcome.Output` already form a durable contract; using native formatting inside the runner would require separate proof for tools, recovery and output provenance.

## Proposed API and developer flow

Add `llm.Settings.OutputSchema json.RawMessage` and `llm.Result.Output json.RawMessage`. A nonempty schema requests a **strict, object-root JSON Schema final answer**. The caller's schema is the local validation authority. `Result.Output` is non-nil only after a finished answer passes local JSON parsing and schema validation; the `Message` and `Raw` response remain available. An empty setting preserves current behavior. Use a fixed provider-safe format name (`dashi_output`) on the wire; callers need no new name or format-type vocabulary.

```go
schema := json.RawMessage(`{
  "type":"object", "additionalProperties":false,
  "properties":{"label":{"type":"string"}}, "required":["label"]
}`)
result, err := llm.Generate(ctx, client, llm.Input{
    Model: model,
    Messages: []llm.Message{llm.UserMessage(llm.Text(input))},
    Settings: llm.Settings{OutputSchema: schema},
}, nil)
if err != nil { /* handle failure; result may contain usage and content */ }
if result.Stop.Kind != llm.StopFinished { /* handle refusal or limit */ }
var value struct{ Label string `json:"label"` }
if err := json.Unmarshal(result.Output, &value); err != nil { /* Go type mismatch */ }
```

The example schema is intentionally strict-compatible. Existing `MustOutputSchema[T]` targets the broader turn contract; a type with `omitempty` may generate an optional property that OpenAI strict mode rejects. Document the error and the required/nullable alternative rather than silently changing the caller's schema. A typed decode helper can follow if use shows repeated friction.

## First route and correctness rules

1. `Input.Validate` checks that a requested schema is a self-contained, bounded JSON Schema object with `type:"object"`; share the existing turn compiler and value limits through an internal package so the two contracts do not drift. The OpenAI Responses `Prepare` additionally checks the documented strict subset, including required property sets, `additionalProperties:false` on each object, supported keywords and structural limits. Report the schema path and reason. Malformed schemas are `ErrorInvalidRequest`; valid JSON Schema outside this route's strict subset is `ErrorUnsupported`. Do not add required fields, nullable unions, or `additionalProperties` on the caller's behalf.
2. The adapter sends `text.format` with `type:"json_schema"`, `name:"dashi_output"`, `strict:true` and the **unchanged** requested schema. `RequestSnapshot.Input`, `Effective` and exact `Body` show what was requested and sent. No schema drop is allowed under `CapabilityAdjust`.
3. Register `FeatureStructuredOutput` in model evidence. Known unsupported models fail in `Prepare`; unknown support follows existing policy: `CapabilityStrict` rejects, `CapabilityAdjust` sends and records `Unverified`. Catalog support claims require official model evidence; a custom Responses-compatible endpoint needs endpoint-scoped `ModelInfo` for a known claim. Other adapters reject `OutputSchema` in `Prepare` until their route is implemented. The first live qualification targets the public OpenAI Responses endpoint, not all compatible proxies.
4. For this slice, current-call `Tools`, hosted tools, `ToolChoice` and `ParallelTools` with `OutputSchema` fail before dispatch. Prior tool history may be replayed normally. Combining native formatting with offered tools is a later route-specific qualification task.
5. Both blocking and streaming send paths validate the same terminal `llm.Result`. A finished response must contain exactly one assistant output-text JSON object, with no additional answer text; reasoning/opaque replay content can coexist. Validate the raw text as one JSON value, enforce the value-size limit, then schema-check and copy it into `Output`. Stream deltas stay provisional.
6. Refusal, output limit, tool use, and other non-finished stops return their existing result/stop with `Output=nil`; they are not relabeled as invalid JSON. A finished response with missing, malformed, multi-part, or schema-invalid answer returns the accumulated `Result` plus `llm.Error{Category: ErrorProtocol}` whose cause names the output failure. Under `Send`'s current contract, an error makes `Stop.Kind=StopIncomplete`; `Stop.Raw`, `Raw`, message, usage and cost retain provider evidence. Do not auto-retry a paid request.

## Acceptance and qualification

- The API example works with a strict-compatible schema on OpenAI Responses. `Prepare` and restored request snapshots preserve the exact `text.format` bytes. Empty `OutputSchema` leaves existing payloads and results unchanged.
- Offline fixtures exercise valid blocking and terminal streaming output, malformed JSON, schema mismatch, multiple text parts, refusal, output limit, unexpected tool call, failed provider response, and interrupted stream. All non-success cases keep `Output=nil`; usage and cost survive when reported. Stream and blocking results agree for equivalent terminal responses.
- Invalid root, oversized schema/value, optional object fields, missing `additionalProperties:false`, unsupported keywords, unsupported model/route, and current-call tools fail at the specified boundary with useful paths. A fixture proves the input schema is never rewritten, and another proves `CapabilityAdjust` never omits it.
- Add a short direct-call guide covering preparation errors, checking `Stop`, decoding `Output`, and why the turn schema helper may need stricter fields. Existing turn output tests pass unchanged. A dated live probe records endpoint, model snapshot, schema and observed result separately from fixture coverage; a failed or unavailable probe must not be reported as qualified.

## Alternatives, risks and follow-up

`Settings.OutputSchema` and a fixed wire name avoid Dive's three-mode `ResponseFormat` surface while matching Dashi's turn vocabulary. A provider-neutral schema normalizer could accept more caller schemas, but silently making optional fields required or nullable changes application meaning; reject in this slice. A result-only `Message.Text()` design would force every caller to duplicate validation and could splice unrelated text parts. `Result.Output` makes the validated value discoverable without changing the original response.

The strict subset and model support can drift. Keep route-specific preflight conservative, source its constraints from the current OpenAI guide, and treat live qualification as evidence for named models only. Follow-up work may add other provider routes, optional typed decoding, and native formatting in a turn. That turn path must still record the existing `StructuredOutput` fact and attempt/source provenance, and must prove native/tool coexistence before switching from `dashi_submit_output`.
