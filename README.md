# Kernel

The foundation of a Go agent library. Public interfaces are experimental and
will evolve as the agent layer takes shape.

## Foundation packages

- `content`: Shared text, media, resources, citations, and replay origins.
- `model`: Shared model identity, capability support, and evidence.
- `tool`: Definitions, calls, outputs, execution outcomes, and typed functions.
- `llm`: Model references, requests, preparation, results, streaming, and usage.
- `media`: Image, video, and non-speech audio values, preparation, results, and video jobs.
- `speech`: Recognition and synthesis values and capability descriptions.
- `providers/anthropic/messages`: Anthropic Messages API format.
- `providers/openai/responses`: OpenAI Responses API format.
- `providers/openai/completions`: Chat Completions format.
- `providers/openai/images`: OpenAI image generation and editing.
- `providers/google/gemini`: Gemini API and Vertex GenerateContent via Google's Go SDK.
- `providers/grok/imagine`: xAI image generation and editing, and video jobs.
- `providers/deepinfra`, `mistral`, `openrouter`: configured Chat Completions destinations.
- `providers/grok`, `meta`: configured Responses destinations.
- `providers/ollama`: configured Anthropic-compatible Messages destination.

Dependency direction: providers → `llm` → `tool` → `content`; `llm`, `media`,
and `speech` share `model` identity and evidence. Shared content and model
depend only on the standard library. Agent and session execution come later.
The Google adapter depends on `google.golang.org/genai`; the core packages do not.
Media has provider clients for Gemini image generation/editing and Veo video
jobs, xAI image generation/editing and video jobs, and OpenAI image
generation/editing. The clients prepare exact JSON or multipart request bodies
before dispatch. A video submission records accepted, rejected, or unknown
outcome; an unknown outcome needs provider reconciliation before any retry.
`speech` still defines values and capabilities; speech stream transports await
adapter qualification. See [media implementation](design/media-provider-implementation.md)
for supported routes and current limits.

## Try one model call

Requires Go 1.26. Set `OPENAI_API_KEY` and `OPENAI_MODEL`, then run:

```sh
go run ./examples/basic
```

The [example](examples/basic/main.go) uses the Responses client:

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
result, err := client.Send(ctx, request, sink)
```

Running the example makes a paid request. It explicitly lets the API validate
unknown model capabilities. Strict validation is the default: supply dated
`llm.ModelInfo` entries in `Config.Models` or select `llm.UnknownAllow`. Each
entry's `ModelEvidence` must match the exact model, API, and endpoint and identify
its source, owner, date, and method. Local fixtures do not imply live
qualification. Known unsupported features are always rejected. Explicitly allowed
unknown features are recorded in the preparation. No built-in model catalog is
supplied yet.

`Prepare` validates and encodes without network access. Its immutable `Request`
and recordable `RequestSnapshot` preserve the original input, effective settings,
typed adjustments, content changes, model evidence, and one exact body.
Preparation is not proof of dispatch or receipt.
`Input.Stream` selects streaming; a sink observes deltas without changing the
request. A nil sink still collects a streaming result. Each `Send` makes at most
one HTTP dispatch; the caller handles recording, admission, and any retry.
`llm.Generate` combines preparation and sending for simple use.

Custom endpoints require `Config.Provider`, the application's stable name for
the destination. That identity and the model form `llm.ModelRef`.
Private replay also requires matching API, endpoint, and credential scope;
changing credentials conservatively invalidates old private artifacts.
Recognized portable history can be re-encoded for another model with each
replay strip recorded. Message source remains intact and file IDs require origin.
`Input.ContentPolicy.UnreplayableReasoning` can explicitly permit recorded
reasoning omission; other unsupported content losses are rejected. See [content replay](design/provider-content-replay.md)
for supported conversions and stored-record changes.

Preparation deterministically remaps unsupported tool IDs and their paired
results, recording both original and outgoing IDs in `ContentChanges`. Saved
history and execution IDs remain unchanged; unsafe private replay edits fail.

Tool definitions use raw JSON Schema. `tool.Func` generates and validates a
schema before executing the callback. Known tool failures are outcomes; Go
execution errors represent uncertainty. Deltas are provisional: inspect the
final result and stop kind before acting on tool calls. Missing usage is nil.
`Result.Cost` holds a provider-reported amount when available; list-price
estimates require an explicitly selected `llm.Pricing` sheet and complete usage.

See [foundation](design/foundation.md) for decisions, provenance, and limits,
and [terminology](design/terminology-proposal.md) for API and record changes.

## Verify

```sh
go test -race ./...
go vet ./...
go build ./...
```

Tests use local servers and fixtures, without credentials. They do not establish
live API compatibility.
