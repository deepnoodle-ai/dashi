# Make Veo videos on Vertex AI and bill them by the video produced

Status: Draft\
PRD PR: TBD  Implementation PR: TBD  Updated: 2026-11-02\
Spec: [design/vertex-veo.md](../design/vertex-veo.md)

## Problem

Mobius makes Veo videos on Vertex AI in its own Google Cloud project. It
has no Gemini API key. `gemini.NewMedia` speaks only the Gemini API-key
route, so Mobius cannot move `MediaService.CreateVideo` from Dive to
Dashi. Mobius bills its customers per second of video, priced by
resolution and by audio. Dive returns the requested duration, not the
measured one, and has no audio flag.

The Gemini API route is also ending. Google shut down Veo 3.1 Preview
in the Gemini API on 2026-10-22 and points users to the GA models on
Vertex (now "Gemini Enterprise Agent Platform"). Without this work,
Dashi has no working Veo 3.1 route.

## Context

**Dashi today.** `gemini.MediaClient` implements `media.VideoJobClient`
(Prepare, Submit, Poll, Fetch) for the Gemini API: API-key header,
`v1beta/models/{model}:predictLongRunning`, a GET on the operation, and a
Files API download. It enforces "1080p and 4k need 8 s" for every
request. `media.Result` keeps provider usage only as `RawUsage` JSON;
Veo reports none. `gemini.Config` already has a Vertex path (`Vertex`,
`Project`, `Location`, `CredentialScope`, ADC with the `cloud-platform`
scope, quota-project header) for model calls. The media client does not
use it.

**Dive** calls Vertex through the genai SDK, sends `generateAudio: true`
for every model but Veo 2 (`veoGeneratesAudio`), and takes the inline
bytes (the SDK will not download Vertex files). It reports the requested
duration and a fixed 720p size.

**Vertex Veo, from Google's docs (2026-11-02).**

- Submit with `…/publishers/google/models/{model}:predictLongRunning`;
  poll with `…:fetchPredictOperation`. Without `storageUri`, the done
  operation holds `response.videos[].bytesBase64Encoded`.
- **The response does not state duration, resolution, or audio.** The
  reference says "the actual duration … may vary slightly". Usage must
  come from the MP4 file.
- `generateAudio` "Defaults to true"; older pages said Veo 3 requires
  it.
- Models (us-central1): `veo-3.1-generate-001` (GA; 720p, 1080p, 4k),
  `veo-3.1-fast-generate-001` (GA; model page says 720p and 1080p, but
  the price list has a 4k price), `veo-3.1-lite-generate-001` (Preview;
  720p, 1080p). All: 4, 6, or 8 s; 16:9 or 9:16; video/mp4; image to
  video for generate and fast. No doc ties 1080p or 4k to 8 s on
  Vertex.
- Prices per second, with audio / without: Fast 720p $0.10 / $0.08,
  1080p $0.12 / $0.10, 4k $0.30 / $0.25.

Doc links are in the spec.

## Users and job

**Primary:** a Go developer of an app like Mobius. Job: make a Veo video
on Vertex under the app's own project and credentials, get the bytes,
and bill the customer for the video that was actually made.

## Concepts

- **Vertex video client:** a Veo job client for one Vertex project and
  location. It uses the same credentials as a Vertex `gemini.Config`.
- **Audio off:** a video setting. By default a video has audio when the
  model can make it. The caller turns audio off to get a silent, cheaper
  video.
- **Video usage:** for each video in a result, its measured length,
  frame size, resolution tier ("720p", "1080p", "4k"), and whether it has
  an audio track. Dashi reads these from the file.

## Use cases

### UC-1: Make a video on Vertex

```go
c, err := gemini.NewVertexVideoJobs(gemini.VertexVideoConfig{
    Project: "mobius-prod", // or GOOGLE_CLOUD_PROJECT
})
req, err := c.PrepareVideo(ctx, media.VideoInput{
    Model:    "veo-3.1-fast-generate-001",
    Prompt:   []content.Block{content.TextBlock("A cup turns on a table")},
    Settings: media.VideoSettings{Duration: 8 * time.Second, Resolution: "1080p"},
})
sub, err := c.SubmitVideo(ctx, req)  // then PollVideo until done
res, err := c.FetchVideo(ctx, poll)
```

- [ ] With ADC (user or service account) and no API key, the job runs.
      Location defaults to us-central1.
- [ ] The request body has `generateAudio: true` and `sampleCount: 1`.
- [ ] `res.Content[0]` holds the MP4 bytes. FetchVideo makes no network
      call.
- [ ] A caller-supplied `Client` supplies its own credentials; Dashi
      adds no token.

### UC-2: Bill the video

- [ ] `res.VideoUsage[0]` holds `Duration` 8 s, `Width`×`Height`
      1920×1080, `Resolution` "1080p", and `Audio` true, all read from
      the MP4.
- [ ] `res.Content[0].Video.Duration` equals the measured length.
- [ ] If the file cannot be read, the video is still returned, with no
      usage entry for it. The doc comment tells the host to bill from its
      recorded request in that case.

### UC-3: A silent video

- [ ] `Settings.DisableAudio: true` sends `generateAudio: false`, and
      usage shows `Audio` false.
- [ ] On the Gemini API Veo route, `DisableAudio` fails in PrepareVideo
      with `ErrorUnsupported`, because that route always makes audio.

### UC-4: Wrong settings and failures

- [ ] 4 s at 1080p is not rejected by Dashi on Vertex; Google decides.
      The Gemini API route keeps its 8 s rule.
- [ ] 4k on `veo-3.1-lite-generate-001`, a 5 s duration, or 1:1 fails
      in PrepareVideo with a message that names the value.
- [ ] A safety-filtered job polls as `JobFailed` with Google's reasons.
- [ ] Missing project: construction fails with "set Project or
      GOOGLE_CLOUD_PROJECT". No ADC: SubmitVideo returns
      `SubmissionRejected` and `ErrorAuthentication`, and sends nothing.

## Requirements

- R-1: A Vertex video client submits and polls with the documented
  endpoints, and gets the bytes from the done operation.
- R-2: It reuses the Vertex credential path of `gemini.Config`: same
  fields, same environment fallbacks, ADC with `cloud-platform` scope,
  quota-project header. It adds no credential source.
- R-3: Audio is on by default, can be turned off, and is always sent
  explicitly on Vertex for Veo 3.x.
- R-4: Built-in model info covers the three Vertex IDs.
- R-5: Each fetched Vertex video carries usage measured from the file.
- R-6: Contract tests cover the request body, polling, decoding, and
  usage. One live test makes an 8 s 1080p Fast video with audio.

## Not in scope

- `storageUri` output to Cloud Storage. Veo makes at most 8 s, so inline
  bytes fit (see open question 1).
- Video edit, extend, reference images, last frame, Imagen, and Vertex
  images. Images stay on the current routes.
- Prices or cost in Dashi. Mobius owns its price list (open question 2).
- Changes to the Gemini API Veo route beyond rejecting `DisableAudio`.

## Decisions

- **A separate constructor, `NewVertexVideoJobs`**, not new fields on
  `MediaConfig`. `MediaConfig` also makes images, which stay on the API
  key route; a Vertex flag there would make half the client fail. It
  follows `NewInteractionsVideoJobs`. Rejected: `MediaConfig.Vertex`.
- **`DisableAudio bool` in the shared `media.VideoSettings`.** Grok and
  Vertex both switch audio, so a second provider proves the shape. Zero
  means the model's default (audio). Grok maps it to
  `generate_audio: false`. Rejected: `Audio *bool` (three states, and a
  nil pointer reads as "no audio"); a Vertex-only option.
- **Bytes reach FetchVideo through the poll.** The done poll already
  holds the bytes, so the succeeded `JobPoll` carries them and
  FetchVideo only checks and measures. Rejected: poll again in Fetch
  (downloads the video twice).
- **Usage lives in `media.Result.VideoUsage`, one entry per video,
  measured from the MP4.** Google states nothing about the output, and
  the duration can differ from the request. Rejected: copying the
  request (bills what was asked, not made); `RawUsage` (no shared units).
- **No usage entry when measuring fails**, rather than a guess.
- **Location default is us-central1**, the only region on the model
  page. `gemini.Config` defaults to global; Google's SDK samples use
  global, so the live test may show global works too.
- **Fast lists 4k** although its model page does not: Google prices it,
  and a wrong claim costs one rejected, unbilled submit.
- **`CredentialScope` defaults to the project**, because Vertex jobs
  belong to the project. (`gemini.Config` requires it for private model
  content.)

## Success

Mobius moves `CreateVideo` to Dashi and bills from `VideoUsage`. The
live test passes on mobius-506918. Gemini API image and video clients
behave as before.

## Risks and open questions

- Operation names may hold the project number, not the ID. The poll
  check matches location, model, and operation ID only, and always calls
  the configured endpoint. The live test shows the real form.
- An audio track can be present but silent, or Google may bill audio
  that a safety filter removed. Usage reports the track. The live test
  shows a track for an audio request.
- A 4k inline response can be near 100 MB of base64. Dashi caps it at
  the existing 512 MiB video limit.
- **Open 1 (does not block):** ship `storageUri` now, or later?
  Recommended: later, when a caller needs videos longer than 8 s.
- **Open 2 (does not block):** should Dashi also price Veo, as it does
  tokens? Recommended: not in this track; usage is enough for Mobius.
