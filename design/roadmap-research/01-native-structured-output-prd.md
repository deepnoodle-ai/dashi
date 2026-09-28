# PRD: native structured output for model calls

| Field | Value |
| --- | --- |
| Status | Draft for discussion |
| Date | 2026-09-28 |
| Roadmap | [Item 1](../remaining-work.md), size M / medium difficulty |
| Evidence | [Source review](01-native-structured-output-research.md), [current turn contract](../structured-output.md) |

## User problem

A Go developer using `llm.Generate` for classification or extraction cannot ask Dashi to use a provider's native JSON Schema response mode. They must hand-build provider requests or run a whole agent turn with a synthetic output tool. Turn users already get a validated `Outcome.Output`, but that path never uses native formatting where it could reduce malformed responses. The smallest useful addition should let both kinds of caller express the **same intended object schema** while keeping the difference between provider guidance and locally verified output clear.

## Goals and non-goals

**Goals:** One discoverable schema setting for a direct model call; a validated object on successful final responses; accurate prepared-request evidence of the selected native route; and optional native use by the turn runner without changing its `Outcome.Output` contract. The usable first slice must include at least one qualified provider route and a clear unsupported-path error.

**Non-goals:** A universal JSON Schema dialect converter, automatic Go struct-to-schema generation, arbitrary scalar-root turn output, a new retry loop, and a switch that makes every tool-using turn native. Do not replace the current reserved output tool until native/tool combinations are qualified separately.

## Proposed smallest API and flow

Add `llm.Settings.Output *llm.OutputFormat`, initially `{Schema json.RawMessage, Name string, Description string}`. A non-nil value requests a JSON object satisfying `Schema`; `Name` is optional and Dashi supplies a stable provider-safe default when one is needed. Reuse the turn schema preflight rules and limits where possible. One call then reads as:

```go
result, err := llm.Generate(ctx, client, llm.Input{
    Model: "...",
    Messages: messages,
    Settings: llm.Settings{Output: &llm.OutputFormat{Schema: schema}},
}, nil)
if err != nil { /* includes partial result, usage, and cause where available */ }
var value Classification
if err := json.Unmarshal(result.Output, &value); err != nil { /* application type mismatch */ }
```

`Result.Output` is set only after Dashi validates a finished model response against the requested schema. Keep `Result.Message`, stop, raw response, usage and cost; streamed text remains provisional until `Send` completes. If the model calls tools, refuses, reaches an output limit, or returns invalid JSON/schema, `Output` stays absent and the caller gets the existing stop or a typed validation error, with accounting preserved. A helper for decoding into a Go value can follow if repeated use shows friction; the schema remains the validation authority.

`Client.Prepare` chooses a provider-native format only when the route supports it, records the effective format and exact encoded body in the existing request snapshot, and returns an actionable error for unsupported schema keywords or incompatible tools. It must **not** silently omit the schema under `CapabilityAdjust`. For a turn, the runner may opt into a qualified native route for a final model call, validate the returned object, then record the existing `StepOutputRecorded` / `StructuredOutput` with a new native source and attempt ID. Otherwise it keeps today's submission tool. The public `Agent`/`Start.OutputSchema` and `Outcome.Output` shape stay the same.

## Acceptance criteria

1. A direct `llm.Generate` caller can supply a self-contained object JSON Schema and obtain a locally validated `Result.Output` from at least one supported native route; complete and streaming calls agree.
2. `Prepare` shows the selected route, effective settings and exact provider body. Unsupported route or schema features fail before dispatch with a field-specific error. Unknown support is not advertised as guaranteed.
3. A malformed, truncated or refused final response cannot produce `Result.Output`; its result/error preserves usage, cost and the provider stop. Tool-call responses are not misclassified as malformed final output.
4. Existing turn output tests pass without changes to caller-visible output. A native turn path, when enabled and qualified, writes the same replayable output fact and provenance as the tool path.
5. Provider fixtures cover accepted JSON, unsupported schema, refusal, output limit, tool-call interaction, and streaming. A dated live probe records route/model/version separately from fixture coverage.

## Risks and open decisions

- **Provider/tool combinations:** ADK uses a tool fallback for a reason. Qualify native mode with tools per route; default to the established turn tool path until evidence supports a switch. Whether the first direct-call release allows `Output` with tools remains open.
- **Schema adaptation:** OpenAI strict mode can require schema normalization. Any transformation must be visible in prepared evidence and must preserve the caller's local validation contract. If it cannot, reject it with a useful explanation.
- **Surface naming:** `Settings.Output` matches the existing model-control direction; `OutputSchema` matches `Start.OutputSchema`. Settle one name before implementation, and document that it is a *model-call format request*, while `Outcome.Output` is a *validated turn result*.
- **Rollout:** Start with an OpenAI Responses route because Dive has a precedent. Add Chat Completions, Anthropic, and Gemini only with adapter fixtures and live qualification for each exact route. No claim of universal support follows from a shared interface.
