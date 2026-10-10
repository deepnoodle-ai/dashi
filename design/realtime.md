# Native realtime voice

A `realtime.Session` is a live model connection, separate from a durable Dashi
session and Runner. It accepts audio, text and caller-executed tool results.
It emits audio, transcript updates, tool calls, interruption and usage evidence.
Applications own devices, playback, tool authority, persistence and spending.

Construct `geminilive.New` or `voice.New` with explicit destination credentials,
then `Connect(ctx, realtime.Config{Instructions: ..., Tools: ...})`. Connect
waits for setup acknowledgment. Its context owns the connection lifetime.
Read with one Receive loop; Send serializes concurrent writers. Close releases
blocked reads. Setup is limited to 20 seconds. No operation reconnects or retries.

The initial qualified formats are PCM signed 16-bit little endian, mono:
Gemini input 16000 Hz/output 24000 Hz; Grok input/output 24000 Hz. Info reports
those effective formats. Unsupported formats, native provider tools, non-object
schemas and invalid commands fail before dispatch. Audio commands are limited to
64 KiB; wire messages to 2 MiB; tool JSON to 64 KiB; pending calls to 32. These
bounds are library limits, not provider capability claims.

Completed tool calls retain native call IDs. Results must match a pending ID and
name. The application executes them and returns a JSON object. Grok requires a
separate CommandRespond after all tools finish; Info.NeedsResponseAfterTools
exposes this difference. Applications should wait until queued playback ends
before requesting another response. Gemini schedules tool replies WHEN_IDLE.

Transcript delta appends; replace replaces the current item, including final
transcripts. Grok cumulative input captions are replacements. Gemini IDs prefixed
`local-` identify adapter-local responses, not native provider objects.
Interruption clears local playback. Gemini ignores old audio through turnComplete
before beginning a new response identity. Native tool cancellation prevents a
late result from being accepted. Playback truncation is unqualified and rejected;
generated output must not be interpreted as proof of what someone heard.

Errors omit provider payloads and authentication URLs. Usage is unpriced evidence;
missing modality detail stays explicitly incomplete. A disconnect ends the session.
The app must start a new connection explicitly and reconcile unknown tool effects.

Adapters target `gemini-3.8-live` and `grok-voice-think-fast-2.0`. Gemini manual
interruption and explicit Respond are unsupported. Grok CommandAudioEnd is
unsupported with server VAD: mute by ceasing capture and input frames.

Sources: [Gemini model](https://ai.google.dev/gemini-api/docs/models/gemini-3.8-live),
[Live capabilities](https://ai.google.dev/gemini-api/docs/live-api/capabilities),
[Grok voice wire reference](https://docs.x.ai/developers/rest-api-reference/inference/voice),
[Grok speech guide](https://docs.x.ai/developers/model-capabilities/audio/speech-to-speech).
Fake-wire tests establish protocol handling, not paid-provider or hardware quality.
