# Speech transport — draft PRD

| Field | Value |
| --- | --- |
| Status | Draft for discussion, 2026-09-28 |
| Roadmap | [Item 8](../remaining-work.md), size M, difficulty Medium |
| References | [Source survey](08-speech-transport-research.md), [media and speech proposal](../media-and-speech-proposal.md), `speech/speech.go`, `speech/capabilities.go` |

## User problem

A Go application can already carry an audio block in Dashi, but cannot ask a provider to transcribe it or synthesize spoken audio through a Dashi client. The developer must wire provider SDKs directly and invent validation, results, provenance, and errors for each route. Dive offers easy batch functions, yet its global routing and shared settings bag hide what will actually be sent. A developer needs a short, predictable path from an audio source to a transcript and from text to an audio block, with provider differences surfaced before a billable request. Streamed speech and a live voice agent are valuable, but have different lifecycle contracts.

## Goals

- Ship small **batch** recognition and synthesis contracts grounded in the existing `speech` inputs/results and explicit provider clients.
- Show the configured route, effective settings, and unsupported options during preparation; keep request/response provenance and honest silence semantics.
- Make one OpenAI-style upload endpoint and one differently shaped Gemini-style speech route usable and qualified, including distinct provider examples.
- Set a clear, reviewable contract for later one-way speech streams without making batch callers handle frames, sessions, or playback.

Success: an application can select a client, prepare one transcription or synthesis request, inspect the planned operation, send it, and consume a typed result. Unsupported hints fail before dispatch, and examples demonstrate the same vocabulary across two providers.

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

Conceptual use: construct a provider client explicitly; `PrepareTranscription` checks the model, endpoint, source origin/expiry, MIME and requested controls and returns a detached, origin-bound plan. The caller can record its **safe metadata** and knows whether a fallback/adjustment occurred before calling `SendTranscription`. `Transcription.Text == ""` is a valid completed result when no speech is recognized. `PrepareSynthesis` resolves voice/format/speed/instructions in the same way; `SendSynthesis` returns `Synthesis.Audio` as a `content.Block` the application can save or play. The library does not open a microphone, fetch remote URLs implicitly, or claim audio was heard.

The prepared request owns the exact dispatch bytes or a bounded, immutable upload source, including multipart boundaries and content type. It must not require copying arbitrarily large audio into several public snapshots. Expose origin, effective settings, model evidence, and recorded adjustments without making credentials or large media safe to persist by default. Provider options stay scoped to that provider API; a setting unsupported by the selected route is an error or an explicit, inspectable adjustment under the existing capability policy. Results identify requested and served model, API, response ID when supplied, and the actual output media type/format. Errors distinguish invalid input, unsupported capability, provider rejection, cancellation, and a possible unknown send outcome; the library must not silently replay a possibly accepted request.

The first implementation slice uses existing `TranscribeInput`, `SynthesizeInput`, `Transcription`, `Synthesis`, and `ModelInfo`. It adds one dedicated-upload transcription route and one synthesis route from **different providers**, plus local fixture tests and explicitly opt-in live tests. The [current proposal](../media-and-speech-proposal.md) suggests batch first. Before publishing streaming interfaces, write a small follow-up contract covering audio frame format, who closes input, ordered partial/final transcript events, synthesis chunk identity and finalization, backpressure, cancellation, and failure after partial output. Validate it against two genuinely different provider transports. `Listen` and incremental-text synthesis should remain separate capabilities; a one-way stream does not promise live turn-taking.

## Acceptance criteria

1. A documented example shows explicit construction, Prepare → inspect → Send for both operations, with the same `speech` concepts across two providers. No global registry or image/video settings bag is required.
2. Preparation rejects invalid audio sources, missing/unsupported models, expired or wrong-origin provider references, unsupported hints, and incompatible voice, language, timestamp, or output formats before any remote request. Unknown catalog evidence is reported as unknown rather than treated as support.
3. A prepared request pins destination, effective settings, content type, and dispatch payload; caller mutation after preparation cannot change the send. Large upload handling has an explicit size/memory bound and no implicit remote fetch. Sensitive bytes and credentials are excluded from any advertised safe record.
4. Successful transcription returns text, optional segments/language, and provenance. Empty text from a completed, well-formed provider response is valid. Successful synthesis returns a valid audio block with actual MIME/format and provenance; storage/playback remain caller-owned.
5. Tests cover the two selected real wire shapes with deterministic fixtures, option rejection, cancellation, malformed/oversized responses, and no automatic retry of uncertain sends. Named opt-in live probes distinguish actual provider acceptance from fixture proof and state exact routes/models.
6. A short reviewed stream contract records event ordering, finalization, flow control, and cancellation decisions. Streaming implementation and realtime voice are not implied by passing the batch tests.

## Non-goals

No microphone/speaker device API, artifact store, speech-to-speech voice agent, realtime session, or universal audio transcoder. No promise that every model accepting audio content supports a dedicated recognition endpoint. Batch clients do not adopt a generic `Client[I,O]` abstraction or expose provider SDK types in the common API.

## Risks and open decisions

- **Upload representation:** A byte-backed exact snapshot is simple, but too costly for large audio. Choose a bounded immutable source/spool representation after measuring the first two adapters; document its lifetime and replay behavior.
- **Gemini transcription semantics:** Prompted `GenerateContent` produces text that may include paraphrase or extra prose, unlike a dedicated ASR response. Decide whether this route meets the typed transcription contract under live qualification; if it does not, choose another provider rather than weakening `Transcription`.
- **Output verification:** Provider headers, inline MIME and actual bytes can disagree. Define which is authoritative, when to detect/reject a mismatch, and whether a documented conversion (as Dive does for Gemini WAV) counts as an adjustment.
- **Stream timing:** The proposal names `Listen` and streaming synthesis, but no transport is qualified yet. Keep public stream names/events provisional until provider probes demonstrate the necessary frame and finalization behavior.
