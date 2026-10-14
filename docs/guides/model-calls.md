# Model calls

This guide shows how to prepare, send, and stream one model call without an
agent or session.

## Try it

Requires Go 1.27. Set `OPENAI_API_KEY` (and optionally `OPENAI_MODEL`), then
run:

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

For independent public reasoning-summary requests, see
[reasoning summaries](reasoning-summaries.md).

## Native structured output on OpenAI

For a direct call that needs one JSON object, set `llm.Input.OutputSchema`.
The Responses and Chat Completions clients send OpenAI's strict JSON Schema
format, then check the finished reply locally. `Result.Output` contains the
validated JSON object; `Result.Message` and `Result.Raw` still describe the
provider response.

```go
schema := json.RawMessage(`{
    "type": "object",
    "properties": {"label": {"type": "string"}},
    "required": ["label"],
    "additionalProperties": false
}`)
result, err := llm.Generate(ctx, client, llm.Input{
    Model:        model,
    Messages:     []llm.Message{llm.UserMessage(llm.Text("Classify this item"))},
    OutputSchema: schema,
}, nil)
if err != nil {
    return err // result may still contain usage or response evidence
}
if result.Stop.Kind != llm.StopFinished {
    return fmt.Errorf("model stopped: %s", result.Stop.Kind)
}
var value struct {
    Label string `json:"label"`
}
if err := json.Unmarshal(result.Output, &value); err != nil {
    return err
}
```

Preparation rejects schemas that the first strict route cannot express. Each
object must name all its properties in `required` and set
`additionalProperties:false`; nested types may pair one type with `null`.
The subset allows `type`, `properties`, `required`, `additionalProperties`,
`items`, `enum`, and `description`, with at most five nesting levels, 100
properties, 250 enum values, and 32 KiB of schema. The validated reply is
limited to 256 KiB. `MustOutputSchema[T]` was designed for agent turns and can
emit optional fields that this route rejects; use a strict-compatible schema
here.
An empty `OutputSchema` keeps ordinary model-call behavior.

This route does not combine a schema with current-call tools or an agent turn.
For other providers, native output is not yet qualified and `Prepare` rejects
the request. A custom Responses or Chat Completions endpoint, including the
OpenRouter, Mistral, and DeepInfra clients, requires matching
`Config.Models` evidence that explicitly supports structured output. Unknown
model support on the public OpenAI endpoint follows the capability policy;
check `RequestSnapshot.Unverified` when that matters. Refusals and output
limits retain their stop reason and leave `Output` empty. Failed local
validation returns an error plus the partial result and usage, when reported.

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

`llm.Input.Model` is always required; the llm layer never picks a model.
`catalog.Default(provider)` names a provider's general-purpose model, which a
`dashi.Runner` uses when a turn's agent names none, and
`catalog.Small(provider)` names an inexpensive one for examples and tests
(false where the catalog names none, as for Meta).

## Clients and keys

Each provider's `New` reads its key from the environment when `Config.APIKey`
is empty. Construction and `Prepare` work without a key, so tests, dry runs,
and cost estimates need none. `Send` then fails before dispatch with an
authentication `llm.Error` matching `llm.ErrNoAPIKey`, naming the variable to
set. A custom endpoint or `Config.Client` skips the check, since a proxy or
transport may supply credentials; Ollama and Gemini on Vertex need no key.

Constructors return concrete types. To choose a provider at runtime, assign
the result to an `llm.Client` variable and return it only when `err` is nil,
so a failed constructor's nil pointer never becomes a non-nil interface:

```go
var client llm.Client
var err error
switch name {
case "anthropic":
	client, err = messages.New(messages.Config{})
case "openai":
	client, err = responses.New(responses.Config{})
default:
	return nil, fmt.Errorf("unknown provider %q", name)
}
if err != nil {
	return nil, err
}
return client, nil
```

[`examples/providers`](../../examples/providers/main.go) covers every
provider this way.

## Prepare, Request, and RequestSnapshot

`Prepare` validates and encodes without network access. Its immutable `Request`
and recordable `RequestSnapshot` preserve the original input, effective
settings, typed adjustments and their warnings, content changes, model
evidence, and one exact body. Preparation does not prove the request was sent
or received.
The snapshot always records the resolved `capability_policy`, `adjust` or
`strict`.

## Images, documents, and citations

`llm.Image(source)` and `llm.Document(source, title)` build message content
from a `content.Source`: a URL the provider fetches, a provider file ID, or
inline bytes. Inline bytes may omit `MIMEType`; preparation detects it from a
document title's extension (`.pdf`, `.txt`, `.md`, `.csv`, and similar) and
then the bytes, and records a `detect_mime_type` content change. Bytes it
cannot identify are sent as `application/octet-stream` with a
`mime_type_unknown` warning. Anthropic receives text documents as text and
PDFs as base64, and rejects other document and image types at preparation.
Image and video generation detect the MIME type of inline inputs the same way.

Set `Settings.Citations` to have the model cite passages of the request's
documents. Citations arrive on the reply's text blocks, which a cited reply
splits one per passage; `Citation.Location` gives the page or character range.
Anthropic supports citations, and the catalog records that for its models, so
strict preparation accepts them; other APIs omit the setting with a warning
when the request carries a document. See the [documents
example](../../examples/documents/main.go) and
[document citations](../../design/document-citations.md).

## Send, Stream, and Generate

`Send` takes an `llm.Call`: the prepared `Request`, an optional `Sink`, and an
`Identity` that a recording caller such as a Dashi turn sets and a direct call
leaves zero. `Input.Stream` selects streaming; a sink observes deltas without
changing the request. A nil sink still collects a streaming result. Each `Send`
sends at most one HTTP request; the caller handles recording, spending checks,
and any retry.

`llm.Stream` sends that request when ranged, yields deltas with backpressure,
then yields one final result. Breaking the range cancels the request. The
sequence is single-use; ranging it again returns `llm.ErrStreamConsumed`.
`Send` remains available when one collected result is simpler.

For response-start, stream-idle, and total HTTP bounds, see
[model HTTP timeouts](model-http-timeouts.md).

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
the turn instead. Only a panic or a lost result makes a call's result unknown.
Deltas are provisional: inspect the final result and stop kind before acting on
tool calls.

## Usage and cost

Missing usage is nil. `Result.Cost` holds a provider-reported amount when
available; display its `Source` and check `Partial()` before presenting its
`Total` as a complete charge; `Cost.String` does both. List-price estimates
require an explicitly qualified `llm.Pricing` sheet and sufficient usage.
`catalog.Cost(client, result)` returns the reported cost, or else an estimate
from a built-in catalog sheet only when it provably applies: the client's
`ModelInfo` for the requested model is the unmodified `catalog.Lookup` entry,
the provider served that model (`Entry.Serves`), and `Result.ServiceTier` is
a tier the catalog prices. Otherwise its error says why there is no cost. The
[provider example](../../examples/providers/main.go) prints either. With a
custom transport, proxy, or other destination, `llm.Client` does not prove
where the request actually ran; qualify that routing and its price
independently before using a catalog sheet.
Provider tool work is counted in `Usage.Requests` by meter
key, such as `llm.MeterWebSearch`, and priced by `Pricing.Requests`. When the
sheet lacks a rate for a key, `Estimate` still prices the rest and lists those
counts in `Cost.UnpricedRequests`; `Cost.Partial` reports that `Total` is then
a lower bound. Prices and costs are exact `money.Amount` values: rates per
million tokens and charges in nano-units, never binary floats.

## Further reading

See [foundation](../../design/foundation.md) for decisions, provenance, and
limits, and [terminology](../../design/terminology-proposal.md) for API and
record changes.
