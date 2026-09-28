# Dashi

Dashi is a Go foundation for building agents that run as durable, recoverable
turns across LLM providers. It is the successor to Dive, Deep Noodle's Go
library for cross-provider LLM support and agents. Dashi (pronounced
"DAH-shee") is named for the Japanese stock that other dishes are built on.

## Why Dashi

Dashi fixes, in the library, what our production applications had to work
around when building durable agent runtimes on Dive.

- **Durable turns.** Every turn keeps a recorded truth. Recovery, accounting,
  and live presentation read recorded facts instead of reconstructing them.
- **Reliable records.** Transcripts need no repair, errors have one meaning,
  and identities stay stable for billing.
- **One clear API around the Turn.** Types and concepts form one coherent
  vocabulary, designed to be clearer than Dive's.
- **Iterator streaming.** Model calls and agent turns stream with Go
  range-over-func iterators, with backpressure.
- **Excellent cross-provider support.** One model layer across providers, with
  thorough coverage of provider-specific behavior.
- **Cost tracking and budgets.** Results carry usage and cost, and per-turn
  budgets enforce token and model-cost caps.
- **Small, idiomatic Go.** Clear package boundaries, with shared content and
  model packages that depend only on the standard library.

## Quick look

```go
client, err := responses.New(responses.Config{})
request, err := client.Prepare(ctx, llm.Input{
    Model:  model,
    Stream: true,
    Messages: []llm.Message{
        llm.UserMessage(llm.Text("Say hello in one sentence.")),
    },
})
var result llm.Result
for event, err := range llm.Stream(ctx, client, llm.Call{Request: request}) {
    if event.Delta != nil { /* show progress */ }
    if event.Result != nil { result = *event.Result }
    if err != nil { log.Fatal(err) }
}
```

Requires Go 1.26 and an API key. Set `OPENAI_API_KEY`, then
run `go run ./examples/basic` for one model call or `go run ./examples/agent`
for a recorded agent turn. Both make paid requests. See
[examples](examples/) for tools, approvals, resuming a turn, and more.

## Providers

- Anthropic Messages
- OpenAI Responses, Chat Completions, and Images
- Google Gemini API and Vertex (a separate module:
  `go get github.com/deepnoodle-ai/dashi/providers/google/gemini`)
- xAI (Grok) conversations, images, and video
- Configured destinations: DeepInfra, Mistral, OpenRouter, Meta, and Ollama

## Status

Dashi is experimental; public interfaces will evolve as the agent layer takes
shape. Selected Anthropic, OpenAI, and Gemini routes are tested live; see
[live tests](docs/dev/live-tests.md) for the dated record.

## Documentation

- [Guides](docs/guides/README.md): packages, model calls, and running turns.
- [Agent turns and recovery](docs/agent-turns.md): the turn contract.
- [Design notes](design/): decisions, proposals, and terminology.
- [Go style guide](docs/dev/go-style.md): conventions for contributors.

## Verify

Run `go test -race ./...`; paid provider checks are described in
[live tests](docs/dev/live-tests.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
