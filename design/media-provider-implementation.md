# Image and video provider routes

Status: adapter, model review, and selected live Gemini routes, 2026-09-27.
This implements dedicated and provider tool routes from
[media and speech](media-and-speech-proposal.md).

## Scope and shape

`media.ImageClient` separates offline `PrepareImage` from one `SendImage` call.
`media.VideoCallClient` does the same for synchronous video output.
`media.VideoJobClient` separates offline `PrepareVideo`, one `SubmitVideo`
attempt, `PollVideo`, and `FetchVideo`. Each provider has a separately
constructed media client; existing `llm.Client` implementations stay scoped to
model calls. Current documented routes include:

| Provider | Image | Video |
| --- | --- | --- |
| OpenAI | [Images API](https://developers.openai.com/api/docs/guides/image-generation) with GPT Image 2.5 Sunburst/Flare; Responses image generation provider tool | [Videos API shut down](https://developers.openai.com/api/docs/deprecations) September 24, 2026 |
| Grok | [Imagine 2.0](https://docs.x.ai/developers/model-capabilities/images/generation) generate/edit; Responses [image generation provider tool](https://docs.x.ai/developers/tools/image-generation) | [Imagine Video 1.5](https://docs.x.ai/developers/model-capabilities/video/generation) generation, edit, and extension jobs |
| Gemini | [Nano Banana](https://ai.google.dev/gemini-api/docs/image-generation) via Interactions, plus GenerateContent | [Omni Flash](https://ai.google.dev/gemini-api/docs/omni) inline calls and URI-delivered jobs; [Veo 3.1](https://ai.google.dev/gemini-api/docs/veo) jobs |

Gemini conversational editing still needs a qualified history and video-source
contract. Veo supports the basic generation job; frame-role controls are not
yet represented. Grok supports scoped Files IDs as inputs and optional
[provider-side output storage](https://docs.x.ai/developers/model-capabilities/imagine/files/outputs).
OpenAI's retired DALL-E models are rejected.

Preparation snapshots the caller's input, effective settings, exact JSON or
multipart body, content type, destination scope, model evidence, typed provider
options, and any unknown capability claims explicitly allowed. It may hold
substantial bytes. Adapters validate supported input forms and bound large
local payloads. They never fetch an input URL or provider file implicitly.
The host records the snapshot before dispatch and decides durable storage for
generated output.
Provider-stored files and public URLs are recorded per generated block, with
their own lifetimes and partial failures; they do not become host artifacts.
Provider usage is retained as raw JSON until shared billing units are qualified.

Video submission returns accepted, rejected, or unknown. A transport failure
or ambiguous server response is unknown unless the provider proves rejection.
Neither a retry hint nor a local timeout authorizes resubmission. Polling is a
read-only observation of a known handle; fetching output is a separate step
that can fail and be retried independently. Provider-specific checks of an
unknown submission against provider state, or idempotency, can be added only
when documented and qualified.

## Boundaries and tradeoffs

Common `media` values express settings, video operations, and lifecycle facts;
adapters own endpoint paths, wire formats, and model-specific restrictions. A
setting is rejected when the adapter cannot encode it. Model capability
evidence is scoped to one configured API and base endpoint. A missing claim
requires an explicit unknown-support policy, matching `llm` preparation.

The common request currently retains exact wire bytes as well as the original
input. This makes one request reviewable at the cost of memory and record size;
streaming upload plans can replace that representation when a qualified API
requires larger inputs. No client internally polls to completion or retries a
submission. Local-server tests verify wire shape, destination binding,
unknown results, poll/fetch separation, and content conversion. Live
provider interoperability remains a separate qualification step.

## Live Gemini qualification

On 2026-09-27, API-key calls passed for `gemini-3.1-flash-lite-image`
generation and editing through both GenerateContent and Interactions. Each
returned a decodable 1024×1024 JPEG, and the edits visibly applied the prompt.
`veo-3.1-lite-generate-preview` completed a four-second 720p job through
submit, poll, and Files download. `gemini-omni-1.1-flash` returned valid 360p
MP4 video through both inline and URI-delivered paths. URI delivery requires
`store=true` and creates provider-owned state; the host still owns downloaded
artifacts. The opt-in tests are in `providers/google/gemini/*_live_test.go`.
Other models, higher resolutions, Vertex credentials, and conversational
editing remain unqualified.
