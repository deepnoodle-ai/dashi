# Media generation and speech

Status: proposal for team review, 2026-09-27; revised after first review.
Adds foundation concepts for image, video, and audio generation, speech
recognition and synthesis, and live voice. Related: [pricing](pricing-proposal.md),
[foundation](foundation.md).

## Context

Kernel abstracts over model providers for one model call. We also want it to
abstract over voice interaction, image generation, and audio generation. These
are foundational capabilities, and belong in this library.

Dive's `media` package defines interfaces for image generation, image editing,
video, text-to-speech, and transcription. They share one `Config`, and a
process-global registry selects providers. It works as a prototype but
conflicts with Kernel's accepted decisions:

- One settings bag serves every operation (`Voice`, `AspectRatio`,
  `Duration`, `TranscriptionPrompt`), and providers ignore what they do not
  use. Kernel refuses unsupported settings.
- There is no preparation, recordable snapshot, adjustments, or model evidence.
- Every operation is request/response. Speech cannot stream, there is no live
  voice, and video submission is hidden inside a call that polls for minutes.
- Results carry `Metadata map[string]any`, only images report usage, and
  outputs are not `content` blocks.

## Proposal: capability and execution shape

Two independent axes describe an operation. **Capability** decides the package:
what the operation does. **Execution shape** decides the lifecycle: how it runs.
An image operation may be a call or a job; speech may be a call or a stream.

| Shape | Examples | Lifecycle |
| --- | --- | --- |
| **Call** | Image generation and editing, batch synthesis and transcription | Prepare → Send → Result |
| **Job** | Video, music, some image APIs | Prepare → Submit → handle → Poll → Fetch |
| **Stream** | Streaming transcription or synthesis | Incremental input and output with ordered events |
| **Live** | OpenAI Realtime, Gemini Live | Long-lived two-way model connection with turn-taking and tools |

### Packages

```text
model      shared identity, evidence, and support (extracted when needed)
content    + video blocks, audio format, source expiry
llm        model calls, + native image and audio output
media      image, video, and non-speech audio generation: calls and jobs
speech     recognition and synthesis: calls and streams
realtime   live model sessions using llm messages and tool definitions
```

Providers add a sub-package per API, such as `providers/openai/realtime`.
Settings, results, and errors specific to a task stay in the package that owns
it. Extract `model` when the first sibling needs provider and API identity,
`ModelRef`, model evidence, and `Support`. Metered usage and pricing follow the
[pricing proposal](pricing-proposal.md) and stay task-specific until a second
operation proves a shared contract. Its package is not decided in advance.

### Preparation

Every operation follows the `llm` preparation principles: offline validation,
strict refusal of unsupported settings, effective settings, model evidence,
and recorded adjustments. It does not reuse `llm.Request`, which requires one
JSON body. Image edits and transcription upload multipart binary data, so each
operation prepares its own upload plan and bounds how it holds large bytes.

Capabilities pair `Support` with operation-specific constraints scoped by the
same evidence: valid voices, languages, formats, sample rates, image sizes, and
timestamp granularity (word, segment, or speaker labels).

### Jobs

A job separates facts that Dive hides inside one blocking call:

1. **Intent**, recorded before submission.
2. **Submission outcome**: accepted, rejected, or unknown when a response is
   lost.
3. **Provider handle**, recorded once known.
4. **Polling**, resumable from the handle after a restart.
5. **Output retrieval**, a separate step with its own failure.

An unknown submission outcome uses provider idempotency keys or reconciliation
where available; otherwise it stays uncertain rather than silently resubmitted,
matching an uncertain attempt in `llm`. `Job` stays local to `media` until
another package, such as LLM batch, needs the same lifecycle.

### Output and artifacts

Generated media are typed `content.Block` values, so they can enter transcripts
and tool outputs. The operation result carries requested and served model,
provider, API, and response identity where known; a detached block carries its
media source, not the complete operation record. A block is not a durable
artifact. The host preserves provenance with any artifact it creates,
materializes large or expiring output, and decides retention, access, and which
view the model sees. Host storage remains outside the foundation; if preparing
an operation needs host-held bytes, the consuming boundary gets a narrow
resolver rather than making `content` fetch implicitly.

Content gains:

- `KindVideo`.
- `AudioFormat`: encoding, sample rate, and channels where applicable. Streams
  may carry raw PCM, which a MIME type alone cannot describe.
- Source expiry for provider URLs and file IDs where known. `Source.Origin`
  remains the compatibility scope for provider file IDs; URL attribution stays
  with the operation result. Kernel never fetches implicitly.

### Three routes to generated media

- **Native output**: a model call returns image or audio content. Requested
  output modalities become an `llm` setting, checked against capabilities.
- **Provider-hosted tool**: a configured tool, such as image generation in
  OpenAI's Responses API. It produces observable tool activity as well as
  media. Selection belongs in the `llm` request contract, distinct from an
  application-executed `tool.Definition`. Adapters own API-specific configuration
  and validate it during preparation. Kernel records requested and effective
  configuration and reported activity or usage, but does not turn hidden
  provider actions into `tool.Call`s it claims to dispatch or retry. The current
  `KindProviderHosted` marker alone does not provide this contract.
- **Dedicated endpoint**: a `media` operation, such as Imagen generation.

### `speech`

- `Recognizer`: `Transcribe` for complete audio; `Listen` for a stream of
  audio frames that yields partial and final transcripts and speech start and
  end events.
- `Synthesizer`: `Synthesize` for complete text; a stream that accepts text
  incrementally and yields audio chunks.

`speech` owns spoken-language recognition and synthesis, whether the audio is
used in a conversation, voiceover, or another product. `media` owns generation
of non-speech audio such as music.

### `realtime`

A **live session** is a model conversation over a live connection. The name
keeps it distinct from a Kernel **session**, the durable conversation that
organizes turns. It uses `llm` messages and instructions and `tool`
definitions. Preparation applies when it opens and to each settings update.
The caller sends audio, text, tool results, interruptions, and updates. The
live session emits audio, transcripts for both sides, speech start and stop,
tool calls, response completion with usage, and errors.

- **Generated, emitted, and heard are separate facts.** The provider reports
  generated audio, the live session records the chunks it emitted, and the
  application reports playback acknowledgments with chunk identity and played
  position, because only the playback device knows what played. Missing
  acknowledgment means unknown playback, not known silence. Conversation
  history derives the heard portion as a projection; the record keeps
  everything generated.
- **Completed items project to messages.** Each completed item can become an
  `llm.Message` with audio and transcript text, so a conversation can move
  between voice and text. Seeding a new live session from those messages is a
  provider capability, not a universal promise.
- **Resumption is provider-specific continuation.** Some providers issue a
  handle that may reconnect a live session. A `realtime` resume handle is
  private data bound to a provider, API, endpoint/credential scope, and expiry
  where known. It is separate from `llm.Opaque`, which represents message
  content for replay. Reconnection does not imply that missed events replay;
  the caller must account for any gap.

### Voice agents

A voice agent is either a pipeline (recognition, then an `llm` agent, then
synthesis) or a native speech-to-speech live session. The foundation supplies
the parts for both. The agent layer, whose design remains open, should treat a
turn the same whether input arrives as text or audio. Endpointing, interruption
handling, and splitting text into sentences for synthesis belong to that
runtime, not to `speech`.

## Alternatives considered

- **Port Dive's `media` package.** Fastest, but its shared settings bag and
  global registry contradict strict preparation and explicit construction, and
  it has no place for streams, jobs, or live sessions.
- **One lifecycle per medium.** Simple to name, but providers run the same
  capability in different shapes; the two axes avoid forcing a lifecycle.
- **One generic operation interface** (`Client[I, O]`). The shapes differ in
  transport, not only in payload; generics would hide those differences.
- **Keep shared vocabulary in `llm`.** Avoids a move, but names non-LLM
  failures and usage after the wrong concept and grows `llm` without bound.
- **Fold live sessions into `llm.Client`.** Live sessions share messages and
  tools, but a stateful two-way connection does not fit one Send attempt.

## Sequencing

1. Content additions, and `llm` native output modalities.
2. A dedicated image call including editing with a reference image or mask,
   plus a video job. The edit tests multipart binary preparation; the job tests
   uncertain submission. Extract `model` when these siblings need it.
3. Batch speech, then streaming speech.
4. Live sessions.
5. The voice-agent runtime, designed with the agent layer.

Qualify each contract against providers with meaningfully different behavior.
Dive's adapters are useful evidence for calls, but they establish nothing about
streams or live sessions.

## Qualification gates

Video is in scope now so the early slice exercises a provider job. Qualify
providers per operation rather than per package: start with implementations
that differ in API shape, then add a third when it exposes a missing constraint.
Before public stream and live interfaces settle, test input finalization,
bounded buffering, cancellation, event ordering, and reconnection gaps.
