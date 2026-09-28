# Batch speech transport — draft PRD and lightweight spec

| Field | Value |
| --- | --- |
| Status | Draft for independent review, 2026-09-28 |
| Roadmap | [Item 8](../remaining-work.md), size M, difficulty Medium |
| Workflow | Research → spec → independent feedback → implementation |
| References | [Source survey](08-speech-transport-research.md), [media and speech proposal](../media-and-speech-proposal.md), `speech/`, `providers/openai/images/` |

## User problem

A Go application can carry an audio block in Dashi, but cannot ask a provider to transcribe it or synthesize spoken audio through a Dashi client. Developers must call a provider SDK and recreate validation, provenance, and failure handling. Dive offers convenient batch calls but hides provider selection in a global registry and mixes speech with other media settings. The first useful Dashi experience is: choose a destination explicitly, prepare and inspect one call, send it, then receive a typed result. A silent recording may produce valid empty text. Generated audio is data for the application to store or play.

## Goals

- Ship small **batch** recognition and synthesis contracts grounded in the existing `speech` inputs/results and an explicit provider client.
- Show destination, effective settings, unsupported options, and safe intent metadata during preparation. Preserve provenance and valid silence semantics.
- Cover one real OpenAI transcription route and one real OpenAI synthesis route, with exact wire fixtures and named opt-in live probes.
- Keep streaming and realtime voice outside this slice; they have different lifecycle contracts.

Success: one documented Go example completes each operation through the same `speech` vocabulary, with no hidden fetch, dropped setting, automatic retry, or device/session concern. This is a contract and adapter milestone, not a production quality claim about either model.

## Smallest proposed API and flow

Add two narrow interfaces in `speech`, parallel to the existing `media` prepared-call style:

```go
type Recognizer interface {
    PrepareTranscription(context.Context, TranscribeInput) (TranscriptionRequest, error)
    SendTranscription(context.Context, TranscriptionRequest) (Transcription, error)
}
type Synthesizer interface {
    PrepareSynthesis(context.Context, SynthesizeInput) (SynthesisRequest, error)
    SendSynthesis(context.Context, SynthesisRequest) (Synthesis, error)
}
```

Conceptual use: construct `providers/openai/speech.Client` with an explicit destination and key; build an existing `speech.TranscribeInput` or `SynthesizeInput`; Prepare, inspect **safe metadata**, then Send with a context. Prepare makes no network call. It checks source, model, settings, and known capability evidence and fixes the route, effective settings, content type, and wire body. Unsupported or unencodable controls fail before dispatch. Unknown catalog support follows Dashi's `model.CapabilityPolicy` and is marked unverified; it never overrides a known wire incompatibility. Send accepts only a request bound to that client's destination and credential scope and makes one attempt.

| Operation | First route and model | Accepted values |
| --- | --- | --- |
| Transcribe | `POST /v1/audio/transcriptions`, `gpt-4o-mini-transcribe`, multipart upload, JSON result | Inline MP3 or WAV up to 25 MB; optional language and prompt. Reject `Granularity` and `Diarization`, which this model/response shape cannot fulfill. |
| Synthesize | `POST /v1/audio/speech`, `gpt-4o-mini-tts`, JSON request, MP3 result | Required text up to 4096 characters; voice, instructions, speed 0.25–4.0. Empty voice becomes documented `alloy`, visible in preparation. Empty `OutputMIMEType` becomes `audio/mpeg`; reject other output types and `Language`/`Format`. |

These exact models and options are implementation scope, not a universal speech contract. Other models get explicit unsupported errors until qualified. OpenAI's [transcription guide](https://developers.openai.com/api/docs/guides/speech-to-text) and [speech endpoint reference](https://developers.openai.com/api/reference/cli/resources/audio/subresources/speech/methods/create) ground the limits. A second provider is a follow-on qualification. Gemini's [dedicated Transcribe route](https://ai.google.dev/gemini-api/docs/transcribe) first uploads through Files API, then invokes Interactions. Hiding those two remote outcomes and file lifetime behind one Send would undermine prepared-call clarity; the earlier two-provider plan is too broad for this slice.

The prepared value owns the **bounded exact request bytes**, so changing caller data after Prepare cannot change dispatch. Transcription accepts inline `content.Source.Data` only; URL, provider ID, and URI are rejected rather than fetched or treated as uploadable. Require a supported, coherent MIME and filename mapping, and reject unknown or recognizable conflicting formats. Encode one multipart body of at most the provider's 25 MB file allowance plus small form overhead. Expose a safe summary with destination, non-content effective settings, upload byte count, capability evidence, and redacted adjustments. Do not put audio bytes, text, credentials, content digest, or raw wire body in a summary advertised as safe to log or persist. A raw-body accessor, if needed, must return a copy and be named/documented as sensitive. Request lifetime is process-local; persisting intent does not authorize replay.

On success, parse a bounded JSON transcription response with a present string `text`. Empty text means valid silence; missing or wrongly typed text is a protocol error. Preserve requested model and API. Set `Served` and `ResponseID` only if the provider actually supplies them; a request header ID is not a response ID. For synthesis, bound the audio response read (initial 64 MiB ceiling), reject empty or clearly conflicting output, and return an inline `content.AudioBlock` with `audio/mpeg` MIME. Storage and playback remain with the host. Report invalid input, unsupported settings, provider rejection, cancellation, transport uncertainty, and malformed or oversized responses distinctly enough for callers to decide what to show or retry. Never automatically retry a possibly accepted request.

## Acceptance criteria

1. A short Go example shows explicit client construction, Prepare → inspect safe metadata → Send for both operations. The same `speech` terms and result types work for transcription and synthesis.
2. Offline preparation rejects unsupported source/model/control, oversized upload, MIME mismatch, and wrong-client origin. Mutation after preparation leaves the exact outgoing bytes unchanged. Default voice and MP3 format are visible before Send; no unsupported hint silently disappears.
3. Deterministic HTTP fixtures assert method, URL, headers, multipart filename/MIME/body, JSON fields, and decoded results. Silence succeeds. Missing text, wrong media, empty/oversized output, cancellation, and provider rejection fail appropriately. Tests prove no hidden retry. Fixture evidence is labeled `fixture`, not `live_probe`.
4. Named opt-in live probes for both exact OpenAI routes/models run only with explicit credentials. Until run, provider acceptance, output validity, and model behavior remain **unverified live**; fixture success does not establish cross-provider qualification.
5. Streaming remains a separate design requiring frame format, partial/final events, input close ownership, backpressure, cancellation, and failure after partial output. Batch tests do not imply stream or live voice support.

## Non-goals

No microphone/speaker API, artifact store, speech-to-speech voice agent, realtime session, streaming transport, transcription chunking, diarization, timestamped segments, translation, PCM/WAV synthesis, or universal audio transcoder. No claim that every audio-capable chat model supports dedicated speech endpoints. No generic `Client[I,O]` abstraction or provider SDK types in common `speech` APIs.

## Risks and open decisions

- **Two providers now:** better cross-provider evidence, but Gemini's dedicated ASR path adds upload lifetime and two uncertain remote outcomes. Defer until the one-call contract is sound; this slice makes no cross-provider parity claim.
- **Dive-style single call:** shortest call site, but global routing and shared options hide the chosen operation. Prepared values cost an extra call for inspectability; add convenience only if real use proves repetition painful.
- **Stream the upload:** lower peak memory, but a caller-owned reader can change, close, or fail between Prepare and Send. The 25 MB provider cap makes one owned payload a reasonable first tradeoff. Revisit for larger provider limits or host-managed files.
- **Review question:** Should a prepared request expose a sensitive raw-body copy at all, or only a safe summary while the adapter keeps bytes private? Either choice must preserve immutable dispatch and destination binding.
