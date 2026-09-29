# Packages

This guide lists Dashi's foundation packages and how they depend on each
other.

## Foundation packages

- `dashi`: Agents, runners, identified turn records, an acknowledged
  recorder, and the execution loop.
- `content`: Shared text, media, resources, citations, and replay origins.
- `money`: Exact monetary amounts in nano-units, shared by prices, charges,
  and budgets.
- `model`: Shared model identity, capability support, and evidence.
- `tool`: Definitions, calls, outputs, execution outcomes, and typed functions.
- `llm`: Model references, requests, preparation, results, streaming, and
  usage.
- `media`: Image, video, and non-speech audio values, preparation, results, and
  video jobs.
- `speech`: Recognition and synthesis values and capability descriptions.
- `filerecorder`: A durable recorder that keeps each turn in an append-only
  file, shared safely by processes on one machine (Unix only).
- `dashitest`: A conformance suite for host recorders and a fault-injecting
  recorder for recovery tests.
- `feed`: Serves a turn to live readers: committed steps from the record with
  the runner's live previews merged in place.
- `compaction`: Shortens a host-owned history with a recorded summary, and
  rebuilds the model view from the record.
- `catalog`: Documented capabilities, limits, and prices for known models at
  public provider endpoints, as embedded JSON.

## Provider packages

- `providers/anthropic/messages`: Anthropic Messages API format.
- `providers/openai/responses`: OpenAI Responses API format.
- `providers/openai/completions`: Chat Completions format.
- `providers/openai/images`: OpenAI image generation and editing.
- `providers/google/gemini`: Gemini API and Vertex GenerateContent via Google's
  Go SDK. This is a separate module; add it with
  `go get github.com/deepnoodle-ai/dashi/providers/google/gemini`.
- `mcp`: Tools from local and remote MCP servers as a `tool.Toolset`, with
  approval for project servers. This is a separate module; add it with
  `go get github.com/deepnoodle-ai/dashi/mcp`. See [MCP](mcp.md).
- `providers/grok/imagine`: xAI image generation and editing, and video jobs.
- `providers/deepinfra`, `mistral`, `openrouter`: configured Chat Completions
  destinations.
- `providers/grok`, `meta`: configured Responses destinations.
- `providers/ollama`: configured Anthropic-compatible Messages destination.

## Dependency direction

Providers → `catalog` → `llm` → `tool` → `content`; `llm`, `media`, and
`speech` share `model` identity and evidence. Shared content, model, and money
depend only on the standard library; `money` is a leaf so that `tool` can
report exact charges without importing `llm`. The root agent loop depends on
`llm` and `tool`; `filerecorder`, `dashitest`, and `feed` depend only on the
root package and the standard library. Session storage remains an optional
future adapter.

The Google adapter depends on `google.golang.org/genai` and gRPC, so it is its
own module and the root module does not require them. The other providers use
only the standard library and stay in the root module.

## Media and speech status

Media has provider clients for Gemini image generation/editing and Veo video
jobs, xAI image generation/editing and video jobs, and OpenAI image
generation/editing. The clients prepare exact JSON or multipart request bodies
before dispatch. A video submission records accepted, rejected, or unknown
outcome; an unknown outcome needs provider reconciliation before any retry.

`speech` still defines values and capabilities; speech stream transports await
adapter qualification.

See [media implementation](../../design/media-provider-implementation.md) for
supported routes and current limits.
