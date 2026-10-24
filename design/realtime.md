# Native realtime voice

A `realtime.Session` is a live model connection, separate from a durable Dashi
session and Runner. It accepts audio, text and caller-executed tool results.
It emits audio, transcript updates, tool calls, interruption and usage evidence.
Applications own devices, playback, tool authority, persistence and spending.

Construct `geminilive.New` or `voice.New` with explicit destination credentials,
then `Connect(ctx, realtime.Config{Instructions: ..., Tools: ..., History: ...})`.
Connect waits for setup acknowledgment, then sends History. Its context owns
the connection lifetime.
Read with one Receive loop; Send serializes concurrent writers. Close releases
blocked reads. Setup is limited to 20 seconds. No operation reconnects or retries.

The initial qualified formats are PCM signed 16-bit little endian, mono:
Gemini input 16000 Hz/output 24000 Hz; Grok input/output 24000 Hz. Info reports
those effective formats. Unsupported formats, native provider tools, non-object
schemas and invalid commands fail before dispatch. Audio commands are limited to
256 KiB; wire messages to 2 MiB; tool JSON and text commands to 64 KiB; pending
calls to 32. Declared tools default to 128 and instructions to 256 KiB
(`Config.MaxTools`, `MaxInstructionBytes`); History text to 256 KiB in total.
A setup at the defaults with ordinary tools fits one message, since History is
sent separately and JSON is written without HTML escaping. These bounds are
library limits, not provider capability claims; 121 tools were accepted live
by Gemini on Vertex.

History seeds earlier text, user or assistant, without asking for a response.
Gemini setup sets `historyConfig.initialHistoryInClientContent`, then one
`clientContent` carries the turns with `turnComplete`; live, the model did not
reply and then used the history. Grok receives one `conversation.item.create`
per line: user `input_text`, assistant `text` (xAI's `output_text` belongs to
`force_message`, which speaks), and no `response.create`.

CommandText reaches Gemini as `realtimeInput.text`; `clientContent` would
interrupt generation and is reserved for seeding. Grok uses a user item.

`geminilive.Config.Vertex` selects Vertex AI: the
`{location}-aiplatform.googleapis.com` LlmBidiService endpoint
(`aiplatform.googleapis.com` for `global`), a bearer token taken from the
`oauth2.TokenSource` on every Connect, and the model
`projects/{p}/locations/{l}/publishers/google/models/{m}`. Token errors are not
wrapped. Model availability varies by location: `gemini-3.8-live` connected at
`us-central1` and was not found at `global` or `us-east4` (2026-10-23).

Completed tool calls retain native call IDs. Results must match a pending ID and
name. The application executes them and returns a JSON object. Grok requires a
separate CommandRespond after all tools finish; Info.NeedsResponseAfterTools
exposes this difference. Applications should wait until queued playback ends
before requesting another response. Gemini schedules tool replies WHEN_IDLE. Gemini completes the turn right
after a tool call and answers the result in a new response; the pending call
outlives the first.

Transcript delta appends; replace replaces the current item, including final
transcripts. Grok cumulative input captions are replacements. Gemini IDs prefixed
`local-` identify adapter-local responses, not native provider objects.
Assistant output establishes a Gemini response ID even when a transcript or tool
call arrives before audio. Accumulated output transcripts finalize at generation
completion, interruption or turn completion.

Interruption clears local playback. Gemini ignores old audio through turnComplete
before beginning a new response identity. Native tool cancellation prevents a
late result from being accepted. Grok canceled response completion cancels only
tools owned by that response. Speech activity flushes playback immediately but
does not cancel tools from an already completed response awaiting results. Playback truncation is unqualified and rejected;
generated output must not be interpreted as proof of what someone heard.

Errors omit provider payloads and authentication URLs. A provider close
keeps its status and reason as an ErrorProvider. Usage is unpriced evidence
with per-modality tokens: Grok's text, audio and grok details (grok kept as
its own modality), Gemini's prompt, cache and response details, with thoughts
as text output as in the batch Gemini provider. Detail that does not add up
to the totals stays explicitly incomplete. A disconnect ends the session.
The app must start a new connection explicitly and reconcile unknown tool effects.

Adapters target `gemini-3.8-live` and `grok-voice-3.0`. xAI serves a different
model for an id it does not know, so Grok's Info.Model is the model that
session.updated names. Gemini manual
interruption and explicit Respond are unsupported. Grok CommandAudioEnd is
unsupported with server VAD: mute by ceasing capture and input frames.

Sources: [Gemini model](https://ai.google.dev/gemini-api/docs/models/gemini-3.8-live),
[Live capabilities](https://ai.google.dev/gemini-api/docs/live-api/capabilities),
[Vertex Live API](https://cloud.google.com/vertex-ai/generative-ai/docs/live-api),
[Grok voice wire reference](https://docs.x.ai/developers/rest-api-reference/inference/voice),
[Grok speech guide](https://docs.x.ai/developers/model-capabilities/audio/speech-to-speech).
Fake-wire tests establish protocol handling, not paid-provider or hardware quality.
