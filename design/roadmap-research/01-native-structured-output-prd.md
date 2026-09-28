# PRD: native structured output for direct model calls

| Field | Value |
| --- | --- |
| Status | Implemented; [PR #64](https://github.com/deepnoodle-ai/dashi/pull/64) open, hosted review fix pending independent re-review, 2026-09-28 |
| Date | 2026-09-28 |
| Roadmap | [Item 1](../remaining-work.md), M / medium |
| Evidence | [Source review](01-native-structured-output-research.md), [turn output contract](../structured-output.md), Dashi `16700d1` |

## User problem and outcome

A Go developer extracting a small object with `llm.Generate` cannot request a provider's JSON Schema response mode or receive a locally validated value. They either assemble provider-specific HTTP requests or run an agent turn with a synthetic final tool. A successful direct call should give them one checked JSON object, while a refusal, output limit or failed validation remains explicit with its usage and provider evidence.

This first slice covers **direct calls through the OpenAI Responses adapter**. It does not change agent turns. The turn's `OutputSchema` and recorded `Outcome.Output` already form a durable contract; using native formatting inside the runner would require separate proof for tools, recovery and output provenance.

## Proposed API and developer flow

Add `llm.Input.OutputSchema json.RawMessage` and `llm.Result.Output json.RawMessage`. A nonempty schema requests a **strict, object-root JSON Schema final answer** for this call. `Agent.Settings` is copied into every runner call, so placing the schema on `Settings` would activate native formatting inside turns before that path is designed. The caller's schema is the local validation authority. `Result.Output` is non-nil only after the transport completes and a finished answer passes local JSON parsing and schema validation; the `Message` and `Raw` response remain available. An empty field preserves current behavior. Use a fixed provider-safe format name (`dashi_output`) on the wire; callers need no new name or format-type vocabulary.

```go
func classify(ctx context.Context, client llm.Client, model, input string) (string, error) {
    schema := json.RawMessage(`{
      "type":"object", "additionalProperties":false,
      "properties":{"label":{"type":"string"}}, "required":["label"]
    }`)
    result, err := llm.Generate(ctx, client, llm.Input{
        Model: model,
        Messages: []llm.Message{llm.UserMessage(llm.Text(input))},
        OutputSchema: schema,
    }, nil)
    if err != nil { return "", err } // result may still contain usage
    if result.Stop.Kind != llm.StopFinished {
        return "", fmt.Errorf("model stopped: %s", result.Stop.Kind)
    }
    var value struct{ Label string `json:"label"` }
    if err := json.Unmarshal(result.Output, &value); err != nil { return "", err }
    return value.Label, nil
}
```

The example schema is intentionally strict-compatible. Existing `MustOutputSchema[T]` targets the broader turn contract; a type with `omitempty` may generate an optional property that OpenAI strict mode rejects. Document the error and the required/nullable alternative rather than silently changing the caller's schema. A typed decode helper can follow if use shows repeated friction.

## First route and correctness rules

1. `Input.Validate` checks that a requested schema is a self-contained JSON Schema object with `type:"object"` and at most **32 KiB** of encoded schema; share the existing turn compiler through an internal package so the two contracts do not drift. The first OpenAI strict subset permits only `type`, `properties`, `required`, `additionalProperties`, `items`, `enum`, and `description`. Types are the JSON primitives, object or array, or a single non-null type plus `null` below the root. Every object has `additionalProperties:false` and `required` equal to its property names; every array has one `items` schema. Cap nesting at **5**, total properties at **100**, and total enum values at **250**. Reject all other keywords, boolean schemas, external or local references, tuple arrays, and root unions. These conservative caps sit within OpenAI's documented limits, including for fine-tuned models; they do not assert that a fine-tuned model accepts this route. Do not rewrite the caller's schema. Report the offending field path. Invalid JSON/root/size is `ErrorInvalidRequest`; valid JSON Schema outside this subset is `ErrorUnsupported`.
2. The adapter sends `text.format` with `type:"json_schema"`, `name:"dashi_output"`, `strict:true` and the **unchanged** requested schema. `RequestSnapshot.Input.OutputSchema` and exact `Body` show what was requested and sent; `Effective` remains generation settings only. No schema drop is allowed under `CapabilityAdjust`.
3. Register `FeatureStructuredOutput` in model evidence. Known unsupported models fail in `Prepare`; unknown support follows existing policy: `CapabilityStrict` rejects, `CapabilityAdjust` sends and records `Unverified`. Catalog support claims require official model evidence; a custom Responses-compatible endpoint needs endpoint-scoped `ModelInfo` for a known claim. Other adapters reject `OutputSchema` in `Prepare` until their route is implemented. The first live qualification targets the public OpenAI Responses endpoint, not all compatible proxies.
4. For this slice, current-call `Tools`, hosted tools, `ToolChoice` and `ParallelTools` with `OutputSchema` fail before dispatch. Prior tool history may be replayed normally. The runner must reject any nonempty `Input.OutputSchema` before `Prepare`, regardless of turn `OutputSchema`. Its current `BeforeModel` decision can replace `Settings` and hide tools, not set this input field; retain the guard if hooks evolve. Combining native formatting with offered tools or a turn is later route-specific work.
5. Both blocking and streaming send paths validate the same terminal `llm.Result` **after** `providerhttp.Complete` succeeds. A finished response must contain exactly one assistant output-text JSON object, with no additional answer text or hosted tool/approval activity; reasoning and unrelated opaque replay can coexist. Validate the raw text as one JSON value, enforce the existing **256 KiB** output-value limit, then schema-check and copy it into `Output`. Stream deltas stay provisional. Any transport or completion error leaves `Output=nil`, even if terminal bytes decoded as valid JSON.
6. Refusal, output limit, tool use, and other non-finished stops return their existing result/stop with `Output=nil`; they are not relabeled as invalid JSON. A finished response with missing, malformed, multi-part, or schema-invalid answer returns the accumulated `Result` plus `llm.Error{Category: ErrorProtocol}` whose cause names the output failure. Under `Send`'s current contract, an error makes `Stop.Kind=StopIncomplete`; `Stop.Raw`, `Raw`, message, usage and cost retain provider evidence. Do not auto-retry a paid request.

## Acceptance and qualification

- The API example works with a strict-compatible schema on OpenAI Responses. `Prepare` and restored request snapshots preserve the exact `text.format` bytes. Empty `OutputSchema` leaves existing payloads and results unchanged.
- Offline fixtures exercise valid blocking and terminal streaming output, malformed JSON, schema mismatch, multiple text parts, refusal, output limit, unexpected tool call, hosted approval/call activity mixed with valid JSON, failed provider response, and interrupted stream. Reasoning and unrelated opaque replay remain compatible with a valid answer. Include a streamed terminal valid JSON response followed by a total timeout from `providerhttp.Complete`: `Send` returns an error, `StopIncomplete` and `Output=nil`. Usage and cost survive when reported. Stream and blocking results agree for equivalent completed responses.
- Invalid root, oversized schema/value, optional object fields, missing `additionalProperties:false`, unsupported keyword/reference, too many properties, excessive nesting, unsupported model/route, and current-call tools fail at the specified boundary with useful paths and categories. A fixture proves the input schema is never rewritten and `CapabilityAdjust` never omits it. A runner regression proves no native format reaches `Prepare` through an agent call, including after `BeforeModel` hooks.
- Add a short direct-call guide covering preparation errors, checking `Stop`, decoding `Output`, and why the turn schema helper may need stricter fields. Existing turn output tests pass unchanged. A dated live probe records endpoint, model snapshot, schema and observed result separately from fixture coverage; a failed or unavailable probe must not be reported as qualified.

## Alternatives, risks and follow-up

`Input.OutputSchema` and a fixed wire name avoid Dive's three-mode `ResponseFormat` surface while matching Dashi's turn vocabulary without entering `Agent.Settings`. A provider-neutral schema normalizer could accept more caller schemas, but silently making optional fields required or nullable changes application meaning; reject in this slice. A result-only `Message.Text()` design would force every caller to duplicate validation and could splice unrelated text parts. `Result.Output` makes the validated value discoverable without changing the original response.

The strict subset and model support can drift. Keep route-specific preflight conservative, source its constraints from the current OpenAI guide, and treat live qualification as evidence for named models only. Follow-up work may add other provider routes, optional typed decoding, and native formatting in a turn. That turn path must still record the existing `StructuredOutput` fact and attempt/source provenance, and must prove native/tool coexistence before switching from `dashi_submit_output`.
