# Request public reasoning summaries

Status: Approved\
PRD PR: [#134](https://github.com/deepnoodle-ai/dashi/pull/134) · Implementation PR: Pending · Updated: 2026-09-30

## Problem and context

An application can choose reasoning effort or budget, but cannot request a public reasoning summary through Dashi's API. CLI [UC-3/I07](cli-providers.md) depends on that request. Displaying unsolicited reasoning does not finish the job.

`llm.ReasoningSettings` controls effort or budget. `llm.Content.Reasoning.Text` already holds public display text; `Content.Replay` retains separate private provider data. Gemini currently sends `includeThoughts: true` whenever a budget is set. OpenAI Responses retains summaries but never requests them. Preparation already records requested/effective settings, adjustments, and support evidence.

[OpenAI](https://developers.openai.com/api/docs/guides/reasoning#reasoning-summaries) exposes `reasoning.summary`; [Gemini](https://ai.google.dev/gemini-api/docs/generate-content/thinking#thought-summaries) exposes `includeThoughts`; [Anthropic](https://platform.claude.com/docs/en/build-with-claude/thinking#controlling-thinking-display) exposes `thinking.display`. These controls describe public output separately from reasoning spend. Support varies by model and route.

## Users and concepts

The primary user is a Go application developer presenting a recorded agent turn and inspecting the controls sent to its provider.

- **Public reasoning summary:** provider-designated display text about reasoning. It is separate from answer text and private replay data.
- **Summary request:** an optional request for that text. It does not promise text, change reasoning effort, or authorize private-data display.

## Use cases and acceptance

### UC-1: Request and inspect a public summary

Use the selected configured client. Model evidence qualifies this fixture route.

```go
input := llm.Input{
    Model: "fixture-model",
    Settings: llm.Settings{ReasoningSummary: new(true)},
    Messages: []llm.Message{llm.UserMessage(llm.Text("Explain the result."))},
}
request, err := client.Prepare(ctx, input)
if err != nil { return err }
effective := request.Effective().ReasoningSummary
result, err := client.Send(ctx, llm.Call{Request: request})
// Read public text from result.Message.Content[i].Reasoning.Text.
```

For Anthropic, first set `input.Settings.Reasoning` to a supported effort or budget. For example, a qualified effort route uses `&llm.ReasoningSettings{Effort: "low"}` before `Prepare`.

- [ ] `Settings.ReasoningSummary *bool`, JSON `reasoning_summary`, provides one independent control. `nil` adds no opt-in; `true` requests a native public summary; `false` stops requesting summaries for new calls.
- [ ] Qualified OpenAI Responses and Gemini summary-only calls are valid without `Settings.Reasoning`. Existing effort/budget validation remains unchanged. Preparation never invents an effort or budget to satisfy a summary request.
- [ ] Keyless fixtures prove native request encoding on qualified OpenAI Responses and Gemini routes, plus Anthropic with an existing compatible reasoning configuration.
- [ ] Input, effective settings, exact dispatched bytes, and recorded attempts agree. Caller mutation after preparation cannot change the request.
- [ ] A successful response with no public summary remains successful. Empty reasoning blocks and private replay data do not count as returned public text.

### UC-2: Adjust or reject an unsupported request

Prepare the same input on a known unsupported route, then an unknown compatible route. Inspect the decision before dispatch.

- [ ] `FeatureReasoningSummary` describes native opt-in support, independently of effort/budget support, with supported/unsupported/unknown facts scoped to model, API, and endpoint.
- [ ] Under `CapabilityAdjust`, known unsupported `true` becomes omission with `SettingReasoningSummary`, `AdjustmentOmit`, and a warning. Under `CapabilityStrict`, it fails before dispatch with an actionable unsupported error.
- [ ] Unknown support on a wire-capable route stays requested and appears in `Unverified` under adjust. Strict rejects it. A route with no supported wire representation cannot silently retain an effective `true`.
- [ ] Missing provider prerequisites fail under both policies: Anthropic without configured reasoning must explain how to add a supported effort/budget or choose another route. It must not enable reasoning implicitly.
- [ ] Receiving unsolicited text does not establish summary-request support. A custom endpoint does not inherit a native endpoint's evidence.

### UC-3: Record, reopen, and stop requesting summaries

Run a turn with summary previews and a settled summary. Reopen its record, then set the next turn's summary preference to `new(false)`.

- [ ] Public previews use `DeltaReasoning`; settled text uses `ContentReasoning`. Both remain distinct from answer text. Their existing attempt/content identity lets consumers reconcile previews with settled content.
- [ ] Recorded settled messages restore public text after reopen. Signed, encrypted, redacted, opaque, and unmodeled reasoning data never become public text by inspection or decoding assumptions.
- [ ] Editing display text does not alter replay bytes. Compatible replay preserves original provider artifacts; existing incompatible/partial replay rejection and explicit omission policy remain intact.
- [ ] Explicit false overrides inherited true for a new turn, including after a model switch. It does not delete prior summaries or alter an open turn's recorded controls.
- [ ] False sends native suppression only with supported model/API/endpoint evidence and a documented mapping. Unsupported and unknown routes omit summary fields and retain the explicit false preference, including under strict policy. Fixtures cover both cases after inherited true.
- [ ] False governs requests, not provider compliance or unsolicited-output filtering. Applications control display separately.

## Decisions and boundaries

- Use a top-level pointer boolean. It distinguishes omission from explicit off and avoids empty `ReasoningSettings` or provider-specific verbosity modes.
- Use native controls only. Reject prompt-based summaries, invented budgets, and treating unsolicited output as opt-in support.
- Preserve the current public text/private replay split. Reject a new summary content kind or exposing `Replay.Raw` to finish presentation.
- Allow warned omission of unsupported opt-in under adjust. Summaries are optional presentation; strict callers can require support before dispatch.
- Require configured Anthropic reasoning in this slice, including models with default adaptive thinking. Reject silently adding a thinking mode; CLI must report the prerequisite and remedy without inventing effort/budget.

CLI rendering, commands, and persistence belong to `cli-providers`. Summary verbosity, enabling/disabling reasoning itself, new provider APIs, catalog-wide refreshes, migrations, and paid provider qualification are out of scope.

## Success and verification

A runnable keyless `Example*` test requests a summary, inspects effective settings, reads public text, and shows unsupported omission/rejection. Fixtures cover complete, streaming, absent-summary, off, and recorded-reopen cases. Independent implementation and security reviews verify the public/private boundary. The [technical spec](../design/reasoning-summary.md) defines the mappings and evidence limits. This foundation enables CLI I07; CLI acceptance remains separate.
