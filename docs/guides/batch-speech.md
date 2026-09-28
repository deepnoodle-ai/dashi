# Batch speech

The `speech` package describes recognition and synthesis. The first batch
transport is `providers/openai/speech` at OpenAI's public API endpoint. It
supports one exact model for each operation; other providers and streaming
speech need their own adapters. A runnable example is in
`examples/batch_speech`; running it makes two billable calls.

```go
client, err := openaispeech.New(openaispeech.Config{}) // OPENAI_API_KEY
if err != nil { return err }

clip, err := os.ReadFile("recording.mp3")
if err != nil { return err }
recognition, err := client.PrepareTranscription(ctx, speech.TranscribeInput{
    Model: "gpt-4o-mini-transcribe",
    Audio: content.AudioBlock(content.Source{
        Data: clip, MIMEType: "audio/mpeg",
    }),
})
if err != nil { return err }
recordIntent(recognition.Summary()) // metadata only; does not include the clip
transcript, err := client.SendTranscription(ctx, recognition)
if err != nil { return err }
_ = transcript.Text // empty text can mean valid silence

synthesis, err := client.PrepareSynthesis(ctx, speech.SynthesizeInput{
    Model: "gpt-4o-mini-tts", Text: "Hello there.", Voice: "alloy",
})
if err != nil { return err }
recordIntent(synthesis.Summary()) // no spoken text or instructions
audio, err := client.SendSynthesis(ctx, synthesis)
if err != nil { return err }
_ = audio.Audio // inline MP3 content; the application owns storage/playback
```

`recordIntent` stands for an application function; the snippet assumes imports
for `context`, `os`, `content`, `speech`, and `openaispeech`.

Prepare does no network work. The request keeps its exact wire body privately;
`Summary()` returns detached metadata and cannot reconstruct or replay it.
Send makes one attempt. If a `*speech.Error` has `RemoteOutcomeUnknown`, a lost
or unusable response does not establish whether the provider processed the request.
Do not automatically retry it.

Transcription accepts inline MP3 or WAV up to 25,000,000 file bytes. Dashi
identifies the bytes, checks any declared MIME type, and makes an upload
filename. URLs and provider file references are not fetched. This slice
accepts an optional lowercase two-letter language hint and an 8192-byte prompt;
it checks the hint's shape but does not verify code membership. Timestamps and
diarization require a later model/response shape. Synthesis returns MP3 only:
text is at most 4096 Unicode code points, instructions at most 8192 bytes,
speed 0.25–4.0, and voice is one of the documented built-in names. An empty
voice selects Dashi's `alloy` default. `Served` and `ResponseID` remain empty
when OpenAI does not report them.

Wire fixtures run with `go test ./providers/openai/speech`. To exercise both
provider endpoints with a billable opt-in probe, set `OPENAI_API_KEY` and
`DASHI_LIVE_SPEECH=1`, then run
`go test ./providers/openai/speech -run TestLiveBatchSpeechEndpoints -count=1`.
The probe synthesizes a short clip and transcribes it. Fixture success does
not prove remote acceptance, transcription accuracy, or output quality.
