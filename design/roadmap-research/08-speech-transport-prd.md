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

Conceptual use: construct `providers/openai/speech.Client` for the public OpenAI endpoint with an explicit API key (or the conventional environment key); build an existing `speech.TranscribeInput` or `SynthesizeInput`; Prepare, call `request.Summary()`, then Send with a context. Prepare makes no network call. It checks source, exact model, and settings, and fixes the route, effective settings, content type, and wire body. Unsupported or unencodable controls fail before dispatch. Send accepts only a request bound to that client's destination and credential scope and makes one attempt.

| Operation | First route and model | Accepted values |
| --- | --- | --- |
| Transcribe | `POST /v1/audio/transcriptions`, `gpt-4o-mini-transcribe`, multipart upload, JSON result | Inline MP3 or WAV up to **25,000,000 file bytes** (Dashi's conservative interpretation of the documented 25 MB limit); optional language and prompt. Reject `Granularity` and `Diarization`, which this model/response shape cannot fulfill. |
| Synthesize | `POST /v1/audio/speech`, `gpt-4o-mini-tts`, JSON request, MP3 result | Required text up to **4096 Unicode code points** (`utf8.RuneCountInString`, a conservative count for ambiguous “characters”); built-in voice, instructions, speed 0.25–4.0. Empty voice becomes Dashi's `alloy` default, visible in preparation. Empty `OutputMIMEType` becomes `audio/mpeg`; reject other output types and `Language`/`Format`. |

These exact models and options are implementation scope, not a universal speech contract. Other models get explicit unsupported errors until qualified. For the **fixed public endpoint only**, the adapter publishes embedded `speech.ModelInfo` for these two exact models, with `model.EvidenceDocumentation`, `QualifiedAt: 2026-09-28`, exact operation API, endpoint, and the official source URLs below. This dated documentation evidence is neither a live probe nor a general catalog. `CapabilityAdjust` cannot enable an unknown model or unencodable setting, so the first adapter does not expose a capability policy. A custom endpoint would require its own scoped evidence and is deferred. OpenAI's [transcription guide](https://developers.openai.com/api/docs/guides/speech-to-text) and [speech endpoint reference](https://developers.openai.com/api/reference/cli/resources/audio/subresources/speech/methods/create) ground the limits. A second provider is a follow-on qualification. Gemini's [dedicated Transcribe route](https://ai.google.dev/gemini-api/docs/transcribe) first uploads through Files API, then invokes Interactions. Hiding those two remote outcomes and file lifetime behind one Send would undermine prepared-call clarity; the earlier two-provider plan is too broad for this slice.

The first synthesis route accepts only documented built-in voices: `alloy`, `ash`, `ballad`, `coral`, `echo`, `fable`, `onyx`, `nova`, `sage`, `shimmer`, `verse`, `marin`, and `cedar`. It rejects custom voice IDs in this slice. The endpoint requires `voice`; choosing `alloy` when the caller leaves it empty is a **Dashi default**, not an OpenAI default. Preparation also caps transcription prompts and synthesis instructions at 8192 UTF-8 bytes so the prepared body remains bounded; this is a Dashi limit, not a claimed provider limit.

The prepared value owns the **bounded exact request bytes**, so changing caller data after Prepare cannot change dispatch. Transcription accepts inline `content.Source.Data` only; URL, provider ID, and URI are rejected rather than fetched or treated as uploadable. Because `content.AudioBlock` has no filename, identify WAV from RIFF/WAVE and MP3 from an ID3 tag **or a valid MPEG audio frame header**; `content.DetectMIMEType(data, "")` alone detects ID3 but misses tagless MP3. Normalize `audio/wave` and `audio/x-wav` to `audio/wav`. If `Source.MIMEType` is set, require `audio/mpeg` or a normalized WAV type and agreement with the identified bytes; an unknown or conflicting payload fails even with a declared MIME. Reject nonnil `Audio.Format` because these routes accept container files, not caller-described frames. Generate a deterministic multipart filename `audio.mp3` or `audio.wav`; Prepare freezes the multipart boundary and bytes. The **file bytes**, excluding form overhead, must fit 25,000,000; cap total prepared body separately at 25,000,000 plus a small fixed overhead.

Expose `TranscriptionRequest.Summary() TranscriptionSummary` and `SynthesisRequest.Summary() SynthesisSummary`. Each returns detached, safe metadata: `content.Origin` (operation API, model, opaque destination scope), MIME type, input byte or text-code-point count, and non-content effective controls (for example voice, speed) with documentation evidence. Prompt, spoken text, instructions, audio bytes, content digest, credentials, and raw wire body stay out. There is **no public raw Body/Snapshot accessor** in this slice; the provider adapter owns an unexported prepared implementation of each `speech` request interface, and Send verifies the originating client before using its private bytes. A summary can be logged or persisted as intent metadata, but it cannot reconstruct or authorize a send. Test that both summaries are inspectable before dispatch, contain effective defaults, and do not expose content, including through JSON serialization.

On success, parse a bounded JSON transcription response with a present string `text`. Empty text means valid silence; missing or wrongly typed text is a protocol error. Preserve requested model and API. Set `Served` and `ResponseID` only if the provider actually supplies them; an `X-Request-Id` response header identifies the HTTP request, not the model response. For synthesis, bound the audio response read (initial 64 MiB ceiling), reject empty or non-MP3 bytes, and return an inline `content.AudioBlock` with `audio/mpeg` MIME. Accept an absent or generic `application/octet-stream` response header **only when bytes identify as MP3**; reject an explicit conflicting media type. Storage and playback remain with the host. Report invalid input, unsupported settings, provider rejection, cancellation, transport uncertainty, and malformed or oversized responses distinctly enough for callers to decide what to show or retry. Never automatically retry a possibly accepted request.

## Acceptance criteria

1. A short Go example shows explicit client construction, Prepare → inspect `Summary()` → Send for both operations. Summaries are detached, serializable, content-free, and show effective defaults; the same `speech` terms and result types work for both operations.
2. Offline preparation rejects unsupported source/model/control, nonnil `Audio.Format`, unknown or mismatched detected MIME, oversized upload, invalid voice, overlong text, and wrong-client origin. Tests cover ID3 and tagless-frame MP3, RIFF/WAVE, declared MIME disagreement, `audio.mp3`/`audio.wav` filenames, 25,000,000-byte and 4096-rune boundaries, and mutation after preparation. No unsupported hint silently disappears.
3. Deterministic HTTP fixtures assert method, URL, headers, multipart boundary/filename/MIME/body, JSON fields, and decoded results. Silence succeeds. Missing text, empty/oversized or non-MP3 output, conflicting explicit output MIME, cancellation, and provider rejection fail appropriately; generic `application/octet-stream` with identifiable MP3 succeeds. Tests prove no hidden retry. Static documentation evidence and fixtures are not labeled `live_probe`.
4. Named opt-in live probes for both exact OpenAI routes/models run only with explicit credentials. Until run, provider acceptance, output validity, and model behavior remain **unverified live**; fixture success does not establish cross-provider qualification.
5. Streaming remains a separate design requiring frame format, partial/final events, input close ownership, backpressure, cancellation, and failure after partial output. Batch tests do not imply stream or live voice support.

## Non-goals

No microphone/speaker API, artifact store, speech-to-speech voice agent, realtime session, streaming transport, transcription chunking, diarization, timestamped segments, translation, PCM/WAV synthesis, custom endpoint, or universal audio transcoder. No claim that every audio-capable chat model supports dedicated speech endpoints. No generic `Client[I,O]` abstraction or provider SDK types in common `speech` APIs.

## Risks and open decisions

- **Two providers now:** better cross-provider evidence, but Gemini's dedicated ASR path adds upload lifetime and two uncertain remote outcomes. Defer until the one-call contract is sound; this slice makes no cross-provider parity claim.
- **Dive-style single call:** shortest call site, but global routing and shared options hide the chosen operation. Prepared values cost an extra call for inspectability; add convenience only if real use proves repetition painful.
- **Stream the upload:** lower peak memory, but a caller-owned reader can change, close, or fail between Prepare and Send. The 25 MB provider cap makes one owned payload a reasonable first tradeoff. Revisit for larger provider limits or host-managed files.
- **Private body:** omitting a public raw-body accessor makes it harder for a host to persist exact payloads, but keeps sensitive audio/text from accidental logging and avoids a second large copy. The safe summary records intent, not a replay capability.
