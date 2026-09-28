# Model calls

This guide shows how to prepare, send, and stream one model call without an
agent or session.

## Try it

Requires Go 1.26. Set `OPENAI_API_KEY` and `OPENAI_MODEL`, then run:

```sh
go run ./examples/basic
```

The [example](../../examples/basic/main.go) uses the Responses client:

```go
client, err := responses.New(responses.Config{})
// Handle err, then:
request, err := client.Prepare(ctx, llm.Input{
    Model:  model,
    Stream: true,
    Messages: []llm.Message{
        llm.UserMessage(llm.Text("Say hello in one sentence.")),
    },
})
// Handle err and record request.Snapshot() if needed, then:
var result llm.Result
for event, err := range llm.Stream(ctx, client, llm.Call{Request: request}) {
    if event.Delta != nil { /* show provisional progress */ }
    if event.Result != nil { result = *event.Result }
    if err != nil { log.Fatal(err) }
}
// Inspect result.Stop before acting on tool calls; pass result.Warnings on.
```

Running the example makes a paid request.

## Validation and model evidence

Requests work by default. With no `llm.ModelInfo` for a model, preparation
sends the request and records the features whose support is unknown in
`RequestSnapshot.Unverified`. When a value exceeds a known limit, such as an
effort level the API lacks, a temperature above its range, or `MaxTokens` above
the model's output limit, preparation sends the nearest accepted value instead.
An effort is never lowered to `none`, and `extra-high`, `extra_high`, and
`extra high` are treated as `xhigh` when fitting. An unset `MaxTokens` becomes
`llm.DefaultMaxTokens` (32,768), or the model's documented output limit when
smaller, for every provider; this default is recorded as an `Adjustment` but is
not a warning.
Each such change is a typed `Adjustment` and appears as an `llm.Warning` on the
`Request`, its snapshot, and the `Result`, so an application can pass warnings
on in its own responses. Requests fail only where no safe substitute exists,
such as image input to a model known to lack vision; the error names the fix.

Set `Config.CapabilityPolicy` to `llm.CapabilityStrict` to reject unknown
support and out-of-range values instead. Dated `llm.ModelInfo` entries in
`Config.Models` supply known limits; each entry's `ModelEvidence` must match the
exact model, API, and endpoint and identify its source, owner, date, and method.
Local fixtures do not imply live qualification. See
[decision 4](../../design/decisions.md) for the rationale.

## Model catalog

The `catalog` package documents known models at public endpoints: context
window, output limit, reasoning efforts, temperature support, and a dated
list-price sheet. Clients for Anthropic, OpenAI (Responses and Chat
Completions), the Gemini API, xAI, Meta, and Mistral use it when
`Config.Models` has no entry for the model, so known limits are fitted with
warnings without setup. Its evidence uses the `documentation` method; an entry
in `Config.Models` replaces it. Custom endpoints, Vertex, Ollama, OpenRouter,
and DeepInfra get no catalog facts by default; bind an entry to your endpoint
with `catalog.Find` and `Entry.Bind` to adopt one. See the
[catalog design](../../design/model-catalog.md).

## Prepare, Request, and RequestSnapshot

`Prepare` validates and encodes without network access. Its immutable `Request`
and recordable `RequestSnapshot` preserve the original input, effective
settings, typed adjustments and their warnings, content changes, model
evidence, and one exact body. Preparation is not proof of dispatch or receipt.
The snapshot always records the resolved `capability_policy`, `adjust` or
`strict`.

## Send, Stream, and Generate

`Send` takes an `llm.Call`: the prepared `Request`, an optional `Sink`, and an
`Attempt` that a recording caller such as a Dashi turn sets and a direct call
leaves zero. `Input.Stream` selects streaming; a sink observes deltas without
changing the request. A nil sink still collects a streaming result. Each `Send`
makes at most one HTTP dispatch; the caller handles recording, admission, and
any retry.

`llm.Stream` starts that dispatch when ranged, yields deltas with backpressure,
then yields one final result. Breaking the range cancels the request. The
sequence is single-use; ranging it again returns `llm.ErrStreamConsumed`.
`Send` remains available when one collected result is simpler.

`llm.Generate` combines preparation and sending for simple use.

## Custom endpoints and ModelRef

Custom endpoints require `Config.Provider`, the application's stable name for
the destination. That identity and the model form `llm.ModelRef`.

## Replay

Private replay also requires matching API, endpoint, and credential scope;
changing credentials conservatively invalidates old private artifacts.
Recognized portable history can be re-encoded for another model with each
replay strip recorded. Message source remains intact and file IDs require
origin. `Input.ContentPolicy.UnreplayableReasoning` can explicitly permit
recorded reasoning omission; other unsupported content losses are rejected.
See [content replay](../../design/provider-content-replay.md) for supported
conversions and stored-record changes.

## Tool ID remapping

Preparation deterministically remaps unsupported tool IDs and their paired
results, recording both original and outgoing IDs in `ContentChanges`. Saved
history and execution IDs remain unchanged; unsafe private replay edits fail.

## Tools, outcomes, and errors

Tool definitions use raw JSON Schema. `tool.Func` generates and validates a
schema before executing the callback; see [tools](tools.md) for its rules. A
tool's Go error is a failure the model sees; wrap it with `tool.Fatal` to stop
the turn instead. Only a panic or a lost result makes a call uncertain. Deltas are provisional: inspect the final
result and stop kind before acting on tool calls.

## Usage and cost

Missing usage is nil. `Result.Cost` holds a provider-reported amount when
available; list-price estimates require an explicitly selected `llm.Pricing`
sheet and complete usage. A catalog sheet applies when `Entry.Serves` accepts
`Result.Served`. Prices and costs are exact `money.Amount` values: rates per
million tokens and charges in nano-units, never binary floats.

## Further reading

See [foundation](../../design/foundation.md) for decisions, provenance, and
limits, and [terminology](../../design/terminology-proposal.md) for API and
record changes.
