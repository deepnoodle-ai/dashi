# Public reasoning summary requests

**Status:** Accepted · **Author:** Codex · **Date:** 2026-09-30\
**Workflow:** Spec → independent requirements review → build.

## Context and goals

The [PRD](../prds/reasoning-summary.md) adds a public-summary request for CLI I07 and Go applications. Keep the configured-client `Prepare`/`Send` contract and recorded Turn authority. A preparation decision describes encoded controls, not proof of provider compliance. Preserve existing `llm.Reasoning.Text` and private `Content.Replay`; no content schema migration is needed.

## API and preparation

Add `Settings.ReasoningSummary *bool` (`reasoning_summary,omitempty`), `FeatureReasoningSummary` (`reasoning_summary`), and `SettingReasoningSummary` (`reasoning_summary`). A pointer keeps explicit false distinct from absence during session inheritance. Do not nest the field in `ReasoningSettings`, whose effort/budget selection remains valid only with exactly one control.

`nil` leaves summary opt-in unset. `true` requests the provider's documented public summary with its native default detail. `false` clears inherited opt-in everywhere. False does not guarantee absence of unsolicited text and is not a display filter.

Extend feature validation, `requestedFeatures`, and `Fit` through the existing path. Only true requests `FeatureReasoningSummary`. Known unsupported true: adjust removes the pointer and records `AdjustmentOmit` from `true` to `null`; strict errors. The warning names `reasoning_summary` and tells the caller to remove the request or use a supporting route. Unknown true remains unverified under adjust and fails under strict.

Keep false in requested/effective settings without requiring positive support to clear the preference. Send a native suppression field only when exact model/API/endpoint evidence reports supported summary control and the adapter documents the off mapping. Otherwise omit all summary-control fields, under both policies. This preserves false without a setting adjustment or an unknown field on the wire. Effective false describes no opt-in, not proven provider suppression.

The adapter must qualify its wire mapping before retaining true. An unimplemented protocol is known unsupported, not unknown. A documented protocol on a custom destination can be encoded under adjust with unknown model support. Do not install native model facts for that endpoint. Existing `RequestSnapshot.Input`, `Effective`, `Adjustments`, `Unverified`, and model evidence carry the new field; `NewRequest` rejects unrecorded changes. Existing `ModelFact.Effective` and the recorded Agent carry it through run/reopen/continue. Do not add another request planner or summary-status record.

## Provider mappings and qualification

| Route | Requested true | Explicit false / nil |
| --- | --- | --- |
| OpenAI Responses | Add `reasoning.summary: "auto"`; merge with supplied effort. Summary-only calls do not add effort. | Both omit `summary`; retain a supplied effort. |
| Gemini GenerateContent | Add `generationConfig.thinkingConfig.includeThoughts: true`; merge with supplied budget. Summary-only calls do not add a budget. | Qualified false sends `includeThoughts: false`; other false and nil omit it. A budget alone no longer opts in. |
| Anthropic Messages with configured reasoning | Add `thinking.display: "summarized"` to the existing enabled/adaptive object. | Qualified false adds `display: "omitted"`; other false and nil omit it. |
| Anthropic without configured reasoning | Reject before dispatch with the missing prerequisite and remedy, even under adjust. | No thinking object is introduced. |
| Chat Completions and unqualified compatible dialects | Known unsupported unless this implementation establishes a documented native request mapping. | Send no summary opt-in. |

These are protocol mappings, not claims that every model supports them. Add dated documentation evidence only for exact catalog entries established by the sources. Fixture entries remain fixture evidence. Leave other models unknown rather than deriving support from names or effort support. API-wide inability can be declared unsupported. Keep Google SDK/Vertex preparation and dispatch aligned with the same frozen request bytes; Vertex gets no borrowed public-endpoint qualification.

Anthropic summary-only calls remain outside this slice even when a model defaults to adaptive thinking. Its runnable example first selects a supported effort/budget. CLI preparation must surface that prerequisite and remedy; it must not fill one implicitly.

[OpenAI's guide](https://developers.openai.com/api/docs/guides/reasoning#reasoning-summaries) documents opt-in and automatic detail. [Gemini's guide](https://ai.google.dev/gemini-api/docs/generate-content/thinking#thought-summaries) separates summaries from thought budgets; its [schema](https://ai.google.dev/api/generate-content#ThinkingConfig) returns summaries only when available. [Anthropic](https://platform.claude.com/docs/en/build-with-claude/thinking#controlling-thinking-display) accepts summarized/omitted display with enabled/adaptive thinking and preserves signatures when text is omitted. Sources checked 2026-09-30; no paid probe is required.

## Public text and replay security

The summary control changes requests, not classification of arbitrary output. Decode only documented public shapes into `Reasoning.Text`: Responses `summary_text`, Gemini thought-marked text, and recognized Anthropic thinking text. Empty/redacted blocks remain recordable with empty display text and private replay. Unknown shapes stay opaque. Consumers inspect public fields; full Turn JSON remains a durable record with private artifacts, not a safe public export.

Responses streaming currently combines `response.reasoning_summary_text.delta` and `response.reasoning_text.delta`. Separate their handling. An event name alone cannot establish public semantics. OpenAI's unmodeled reasoning content must not reach `DeltaReasoning` or be synthesized into a summary. [xAI documents](https://docs.x.ai/developers/model-capabilities/text/reasoning) certain `reasoning_text.delta` events as public summaries; preserve that distinction only with an explicit documented destination/model rule. Otherwise do not publish those deltas. Unsolicited xAI summaries do not establish a native request control.

Maintain stable existing delta block identity and authoritative final decoding. Test multiple summary parts, final replacement of provisional text, and failures after partial text. Never expose ciphertext, signatures, redacted payloads, or `Opaque.Raw` as preview text. A missing summary is ordinary absence, not a stop reason or synthesized explanation. Keep summary text out of `Message.Text()` and reply text.

Replay uses original artifacts regardless of edited display text. Summary-off affects only the new request's output preference; it must not strip earlier thinking blocks required for continuity. `ReasoningOmit` remains a separate explicit history policy. Partial and incompatible artifacts continue to fail under the established guards.

## Verification and rollout

Use keyless fixtures for native true/off/nil bodies and combinations with effort/budget, strict/adjust unsupported paths, unknown support, and prerequisite errors. Assert prepared bytes equal dispatched bytes. Test false overriding inherited true after a model switch: supported routes send their documented off field; known unsupported models and strict unknown endpoints omit summary fields. Keep false in each snapshot. Also test mutation isolation, continued-turn authority, stream/final absence, and recorded summary recovery. Private-marker fixtures prove zero leakage into public text and deltas while compatible replay preserves exact artifacts.

Commit a runnable `Example*` and update model-call documentation. Run root `go test -race ./...` and the separate Gemini module's tests. Independent implementation and security reviews cover the integrated boundary. CLI owns matching provisional and final display text, and reopening presentation acceptance. No live qualification claim follows from fixture success.

The new field is additive. Historical records decode with nil, preserving their recorded meaning. New Gemini calls with budget alone stop requesting summaries; document this deliberate separation. Summaries can add provider latency and vary in detail, so no timing or content guarantee is offered.

## Alternatives and decisions

Putting summary inside `ReasoningSettings` groups related controls but requires changing effort/budget validity and still needs an explicit-off representation. Provider verbosity enums offer precision but add modes CLI does not need. The pointer boolean is the smaller public contract.

Anthropic could inject adaptive thinking for a summary-only call. The accepted prerequisite rejects that approach because it changes reasoning behavior without an effective canonical control. Broader thinking-mode support is a separate change.
