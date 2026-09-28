# Packages

This guide lists Kernel's foundation packages and how they depend on each
other.

## Foundation packages

- `kernel`: Agent definitions, identified turn records, an acknowledged
  recorder, and the execution loop.
- `content`: Shared text, media, resources, citations, and replay origins.
- `model`: Shared model identity, capability support, and evidence.
- `tool`: Definitions, calls, outputs, execution outcomes, and typed functions.
- `llm`: Model references, requests, preparation, results, streaming, and
  usage.
- `media`: Image, video, and non-speech audio values, preparation, results, and
  video jobs.
- `speech`: Recognition and synthesis values and capability descriptions.

## Provider packages

- `providers/anthropic/messages`: Anthropic Messages API format.
- `providers/openai/responses`: OpenAI Responses API format.
- `providers/openai/completions`: Chat Completions format.
- `providers/openai/images`: OpenAI image generation and editing.
- `providers/google/gemini`: Gemini API and Vertex GenerateContent via Google's
  Go SDK.
- `providers/grok/imagine`: xAI image generation and editing, and video jobs.
- `providers/deepinfra`, `mistral`, `openrouter`: configured Chat Completions
  destinations.
- `providers/grok`, `meta`: configured Responses destinations.
- `providers/ollama`: configured Anthropic-compatible Messages destination.

## Dependency direction

Providers → `llm` → `tool` → `content`; `llm`, `media`, and `speech` share
`model` identity and evidence. Shared content and model depend only on the
standard library. The root agent loop depends on `llm` and `tool`; session
storage remains an optional future adapter.

The Google adapter depends on `google.golang.org/genai`; the core packages do
not.

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
