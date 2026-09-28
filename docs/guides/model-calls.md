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
client, err := responses.New(responses.Config{
    UnknownCapabilities: llm.UnknownAllow,
})
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
for event, err := range llm.Stream(ctx, client, request) {
    if event.Delta != nil { /* show provisional progress */ }
    if event.Result != nil { result = *event.Result }
    if err != nil { log.Fatal(err) }
}
// Inspect result.Stop before acting on tool calls.
```

Running the example makes a paid request. It explicitly lets the API validate
unknown model capabilities.

## Validation and model evidence

Strict validation is the default: supply dated `llm.ModelInfo` entries in
`Config.Models` or select `llm.UnknownAllow`. Each entry's `ModelEvidence` must
match the exact model, API, and endpoint and identify its source, owner, date,
and method. Local fixtures do not imply live qualification. Known unsupported
features are always rejected. Explicitly allowed unknown features are recorded
in the preparation. No built-in model catalog is supplied yet.

## Prepare, Request, and RequestSnapshot

`Prepare` validates and encodes without network access. Its immutable `Request`
and recordable `RequestSnapshot` preserve the original input, effective
settings, typed adjustments, content changes, model evidence, and one exact
body. Preparation is not proof of dispatch or receipt.

## Send, Stream, and Generate

`Input.Stream` selects streaming; a sink observes deltas without changing the
request. A nil sink still collects a streaming result. Each `Send` makes at
most one HTTP dispatch; the caller handles recording, admission, and any retry.

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
schema before executing the callback. Known tool failures are outcomes; Go
execution errors represent uncertainty. Deltas are provisional: inspect the
final result and stop kind before acting on tool calls.

## Usage and cost

Missing usage is nil. `Result.Cost` holds a provider-reported amount when
available; list-price estimates require an explicitly selected `llm.Pricing`
sheet and complete usage.

## Further reading

See [foundation](../../design/foundation.md) for decisions, provenance, and
limits, and [terminology](../../design/terminology-proposal.md) for API and
record changes.
