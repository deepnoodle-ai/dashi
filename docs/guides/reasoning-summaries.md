# Public reasoning summaries

Set `llm.Settings.ReasoningSummary` to `new(true)` to request the provider's
public reasoning summary, independently of effort or budget:

```go
request, err := client.Prepare(ctx, llm.Input{
    Model: model,
    Messages: []llm.Message{llm.UserMessage(llm.Text("Explain the result."))},
    Settings: llm.Settings{ReasoningSummary: new(true)},
})
// Handle err, then inspect request.Effective(), Warnings(), and Snapshot().Unverified.
result, err := client.Send(ctx, llm.Call{Request: request})
// Handle err and inspect result.Stop. Public text is in ContentReasoning.Reasoning.Text.
```

Responses requests `reasoning.summary:"auto"`. Gemini requests
`thinkingConfig.includeThoughts:true`; a budget by itself no longer requests
summaries. Anthropic requests `thinking.display:"summarized"` and requires an
existing supported `Settings.Reasoning` effort or budget. Preparation does not
invent one. The [reasoning example](../../examples/reasoning/main.go) shows that
combination; running it calls Anthropic and requires a key. The keyless
`ExampleClient_Prepare_reasoningSummary` test uses an explicitly qualified fixture.

`nil` leaves opt-in unset. `new(false)` clears inherited opt-in for new turns,
including after a model switch. Responses then omits `summary`; qualified
Gemini and Anthropic routes send their documented off controls. Unknown and
unsupported routes send no summary field, retaining the explicit false
preference even under strict policy. False does not filter unsolicited text,
change effort/budget, or remove previous summaries and private replay artifacts.

Only true requests `FeatureReasoningSummary`. Known unsupported opt-in is
omitted with a typed `SettingReasoningSummary` warning under `CapabilityAdjust`;
`CapabilityStrict` rejects it. Unknown support on a compatible protocol stays
unverified under adjust and fails under strict. Chat Completions and the native
xAI/Meta Responses routes have no implemented opt-in mapping. Exact documented
catalog model/API/endpoint facts qualify native support; custom endpoints and
Vertex do not inherit public endpoint evidence.

Public previews use `DeltaReasoning`; settled text uses `ContentReasoning` and
stays separate from `Message.Text()` and agent answers. Previews are provisional:
use the final recorded message to reconcile them. A successful response can
contain no public summary. Empty or redacted blocks do not count as display text.

Signed, encrypted, redacted, opaque, and unmodeled provider data remain private
replay artifacts. Editing display text does not change original replay bytes.
Full recorded Turn JSON contains private artifacts and is not a public display
projection. Summary-off applies to new output requests; `ReasoningOmit` remains
the separate policy for deliberately dropping incompatible reasoning history.
