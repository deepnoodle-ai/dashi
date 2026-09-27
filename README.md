# Kernel

The foundation of a Go agent library. Public interfaces are experimental and
will evolve as the agent layer takes shape.

## Foundation packages

- `kernel`: Agent definitions, identified turn records, an acknowledged recorder, and the execution loop.
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
depend only on the standard library. The root agent loop depends on `llm` and
`tool`; session storage remains an optional future adapter.
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
var result llm.Result
for event, err := range llm.Stream(ctx, client, request) {
    if event.Delta != nil { /* show provisional progress */ }
    if event.Result != nil { result = *event.Result }
    if err != nil { log.Fatal(err) }
}
// Inspect result.Stop before acting on tool calls.
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
`llm.Stream` starts that dispatch when ranged, yields deltas with backpressure,
then yields one final result. Breaking the range cancels the request. The
sequence is single-use; ranging it again returns `llm.ErrStreamConsumed`. `Send`
remains available when one collected result is simpler.
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

## Run an agent turn

With the same environment, run `go run ./examples/agent`. Its
[source](examples/agent/main.go) shows the definition, recorder, command, and
outcome. Each turn requires a stable ID and each command requires an ID for
duplicate delivery handling. An acknowledged model or tool intent may already
have caused an external effect; recover it by identity before deciding whether
to retry. See [agent turns and recovery](docs/agent-turns.md) for the recorder,
pending-result, approval, and stop contracts.
For a validated object result, set `Definition.OutputSchema` or pass a
per-turn `Start.OutputSchema`, then read `Outcome.Output.Value` after
`StopCompleted`. The output fact includes tool-call or final-text provenance;
see [structured final results](docs/agent-turns.md#structured-final-results).
The [Dive comparison and open gates](docs/agent-loop-review.md) record what
remains to prove before stabilizing the loop API.

`Engine.Stream` is the iterator form of `Engine.Apply`. It enables provider
streaming, yields acknowledged steps and provisional deltas in order, then
one final `Outcome`. Breaking the range cancels the turn; reload its record
to inspect the committed result or pending uncertainty. The iterator is
backpressured; `Bindings.Observer` remains a separate, lossy display hook.
Ranging the same turn sequence again returns `kernel.ErrStreamConsumed`.

## Verify

```sh
go test -race ./...
go vet ./...
go build ./...
```

Ordinary tests use local servers and fixtures, without credentials. For selected
live final-result routes, set `KERNEL_LIVE_TEST=1` plus
`ANTHROPIC_API_KEY`/`ANTHROPIC_MODEL`, `OPENAI_API_KEY`/`OPENAI_MODEL`, and
`GEMINI_API_KEY`/`GEMINI_MODEL`, then run
`go test -tags live ./providers -run TestLiveAgentFinalFacts -v`. This makes
six paid requests. To test one provider, filter a subtest such as
`-run '^TestLiveAgentFinalFacts/gemini$'` and set only its key and model.
On 2026-09-27, blocking and streaming final-result calls passed for Anthropic
Messages (`claude-haiku-4-5-20251001`), OpenAI Responses (`gpt-4.1-mini`), and
Gemini API (`gemini-2.5-flash`). This qualifies those routes only.
On 2026-09-27, Anthropic Messages (`claude-haiku-4-5-20251001`), OpenAI
Responses (`gpt-4.1-mini`), and Gemini API (`gemini-2.5-flash`) agent turns
passed live using `Engine.Apply`: a streamed final answer and a local tool call
followed by a second model call. With the corresponding API keys set:

```sh
KERNEL_LIVE_AGENT_TEST=1 ANTHROPIC_MODEL=claude-haiku-4-5-20251001 \
  OPENAI_MODEL=gpt-4.1-mini GEMINI_MODEL=gemini-2.5-flash \
  go test -tags live . -run '^TestLive(Anthropic|OpenAI|Gemini)AgentTurn$' -v
```

This makes three paid calls per provider and checks the recorded steps, replay,
and duplicate start. Filter `-run` to one provider when needed.

On 2026-09-27, one-call structured-output turns also passed live on these
three routes with a per-turn schema and a recorded `kernel_submit_output` call.
Run `go test -tags live . -run '^TestLive(Anthropic|OpenAI|Gemini)StructuredOutput$' -v`
with `KERNEL_LIVE_AGENT_TEST=1` and the corresponding provider keys and model
names to repeat them.

On 2026-09-27, the reserved final-answer attempt passed live on the same three
routes: each turn used a local multiplication tool once, sent a second request
without application tools, and returned an answer with `StopLimited` and
`model_attempts`. With the same keys and model names, run
`KERNEL_LIVE_AGENT_TEST=1 go test -tags live . -run '^TestLiveReservedFinalAnswer$' -v`
to repeat the two-call check per provider.

On 2026-09-27, `Engine.Stream` also passed one live streamed turn per provider
on those models. Run `go test -tags live . -run '^TestLiveIteratorAgentTurns$' -v`
with `KERNEL_LIVE_AGENT_TEST=1`, keys, and model names to repeat the probe.

Gemini's conversational client defaults to `v1beta` because the tested
`v1` route rejected function calling. Pin `v1` explicitly only for turns that
must retain its earlier private replay scope; `Prepare` rejects client function
calls there before dispatch.
Gemini LLM image inputs passed live on 2026-09-27 with `gemini-2.5-flash`:
two inline PNGs in blocking and streaming calls, a public image URL, and a
Gemini Files URI. Each response identified image content. Run the opt-in checks
with `GEMINI_API_KEY` and:

```sh
KERNEL_LIVE_IMAGE_INPUT_TEST=1 GEMINI_MODEL=gemini-2.5-flash \
  go test -tags live ./providers/google/gemini -run '^TestLiveImageInput$' -v
```

The full test makes four paid LLM calls and temporarily uploads one PNG;
filter a subtest to run one route.

Selected Gemini image and video routes were also tested live; see
[media qualification](design/media-provider-implementation.md). Opt-in media
tests use `-tags live` with `KERNEL_LIVE_MEDIA_TEST=1` and
`GEMINI_IMAGE_MODEL`, or `KERNEL_LIVE_VIDEO_TEST=1` and the selected
`GEMINI_VIDEO_MODEL`/`GEMINI_OMNI_MODEL`. Filter one test to control paid calls.
