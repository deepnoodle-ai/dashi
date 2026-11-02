# Make Veo videos on Vertex AI and bill them by the video produced

Status: Draft\
PRD PR: #371  Implementation PR: TBD  Updated: 2026-11-02\
Spec: [design/vertex-veo.md](../design/vertex-veo.md)

## Problem

Mobius makes Veo videos on Vertex AI in its own Google Cloud project. It
has no Gemini API key. `gemini.NewMedia` speaks only the Gemini API-key
route, so Mobius cannot move `MediaService.CreateVideo` from Dive to
Dashi. Mobius bills per second of video, priced by resolution and audio.

The Gemini API route is also ending. Google shut down Veo 3.1 Preview
in the Gemini API on 2026-10-22 and points users to the GA models on
Vertex (now "Gemini Enterprise Agent Platform"). Without this work,
Dashi has no working Veo 3.1 route.

## Context

**Dashi today.** `gemini.MediaClient` implements `media.VideoJobClient`
for the Gemini API with an API key and enforces "1080p and 4k need 8 s"
everywhere. `media.Result` keeps usage only as `RawUsage` JSON.
`gemini.Config` already has a Vertex credential path for model calls;
the media client does not use it.

**Dive** calls Vertex through the genai SDK, sends `generateAudio: true`
for every model but Veo 2, takes the inline bytes, and reports the
requested duration and a fixed 720p size.

**Vertex Veo, from Google's docs (2026-11-02).**

- Submit with `…/publishers/google/models/{model}:predictLongRunning`;
  poll with `…:fetchPredictOperation`. Without `storageUri`, the done
  operation holds `response.videos[].bytesBase64Encoded`. Google's Veo
  page shows only `gcsUri`; the inline form comes from the genai SDK
  (`videoFromVertex`) and Dive in production.
- **The response does not state duration, resolution, or audio.** The
  reference says "the actual duration … may vary slightly". Usage must
  come from the MP4 file.
- `generateAudio` "Defaults to true"; older pages said Veo 3 requires
  it.
- Models (us-central1 only, per the model page): `veo-3.1-generate-001` (GA; 720p, 1080p, 4k),
  `veo-3.1-fast-generate-001` (GA; model page says 720p and 1080p; only
  copies of the price list show 4k), `veo-3.1-lite-generate-001` (Preview;
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
- **Video usage:** for each video in a result, the length of its video
  track (edit list applied, so an audio track that runs longer does not
  count), frame size, resolution tier ("720p", "1080p", "4k"), and
  whether it has an audio track. Dashi reads these from the file.

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
// Persist req, then submit once. Persist *sub.Job: it is the handle a
// restarted host polls with. Never resubmit an unknown submission.
sub, err := c.SubmitVideo(ctx, req)
poll, err := c.PollVideo(ctx, *sub.Job)
for poll.State == media.JobPending {
    time.Sleep(10 * time.Second)
    poll, err = c.PollVideo(ctx, *sub.Job)
}
// JobFailed: report poll.Failure. JobSucceeded: the poll holds the video
// bytes, so do not persist its body; store the bytes once, from res.
res, err := c.FetchVideo(ctx, poll)
```

- [ ] With ADC (user or service account) and no API key, the job runs.
      Location defaults to us-central1. A location that resolves to
      `global` (for example `GOOGLE_CLOUD_LOCATION` shared with model
      calls) is adjusted to us-central1, and the doc comment says so.
- [ ] The request body has `generateAudio: true` and `sampleCount: 1`.
- [ ] `res.Content[0]` holds the MP4 bytes. FetchVideo makes no network
      call.
- [ ] A caller-supplied `Client` supplies its own credentials; Dashi
      adds no token.

### UC-2: Bill the video

- [ ] For an 8 s request, `res.VideoUsage[0].Duration` is within one
      frame of 8 s, taken from the video track with its edit list
      applied. `Width`×`Height` is 1920×1080, `Resolution` "1080p", and
      `Audio` true, all read from the MP4. The doc comment says the host
      chooses how to round.
- [ ] If the file cannot be read, the video is still returned, with no
      usage entry for it. The doc comment tells the host to bill from its
      recorded request in that case.

### UC-3: A silent video

- [ ] `Settings.DisableAudio: true` sends `generateAudio: false`, and
      usage shows `Audio` false.
- [ ] On the Gemini API Veo route, `DisableAudio` fails in PrepareVideo
      with `ErrorUnsupported`, because that route always makes audio.
- [ ] Grok maps `DisableAudio` to `generate_audio: false`.
      `imagine.VideoOptions.GenerateAudio` is removed.

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
  quota-project header. It adds no credential source. Only the location
  default differs: us-central1, and `global` adjusts to it.
- R-3: Audio is on by default, can be turned off, and is always sent
  explicitly on Vertex for Veo 3.x.
- R-4: Built-in model info covers the three Vertex IDs.
- R-5: Each fetched Vertex video carries usage measured from the file;
  `Duration` is the video track's length.
- R-6: Contract tests cover the request body, polling, decoding, and
  usage. One live test makes an 8 s 1080p Fast video with audio.

## Not in scope

- `storageUri` output to Cloud Storage. Veo makes at most 8 s, so inline
  bytes fit. Add it later, when a caller needs longer videos.
- Video edit, extend, reference images, last frame, Imagen, and Vertex
  images. Images stay on the current routes.
- Prices or cost in Dashi. Dashi reports usage; the host prices it.
  Pricing can come later for the cost-tracking goal.
- Changes to the Gemini API Veo route beyond rejecting `DisableAudio`
  and pointing the `NewMedia` doc comment to `NewVertexVideoJobs`.

## Decisions

- **A separate constructor, `NewVertexVideoJobs`**, not new fields on
  `MediaConfig`. `MediaConfig` also makes images, which stay on the API
  key route; a Vertex flag there would make half the client fail. It
  follows `NewInteractionsVideoJobs`. Rejected: `MediaConfig.Vertex`.
- **`DisableAudio bool` in the shared `media.VideoSettings`** replaces
  Grok's `imagine.VideoOptions.GenerateAudio`: one control for one
  meaning, no shim. Zero means the model's default (audio). Rejected:
  `Audio *bool` (three states, and nil reads as "no audio"); keeping
  both.
- **Changes a documented decision: the poll carries the bytes.**
  `design/media-provider-implementation.md` said a poll only observes
  and a separate fetch gets the output. The done Vertex poll already
  holds the bytes (up to about 100 MB of base64 at 4k), so the
  succeeded `JobPoll` carries them and FetchVideo only checks and
  measures. Hosts persist the job handle, not succeeded poll bodies.
  Rejected: poll again in Fetch (downloads the video twice).
- **Changes a documented decision: `VideoUsage` is the first shared
  billing unit.** That doc kept usage as raw JSON "until shared billing
  units are qualified". `media.Result.VideoUsage` has one entry per
  video, measured from the MP4, because Google states nothing about the
  output and the length can differ from the request. Rejected: copying
  the request; `RawUsage`. The design doc is updated in this PR.
- **Location default is us-central1**, the only region Google documents
  for Veo. A resolved `global` adjusts to us-central1 rather than
  failing; an explicit other location is used as given.
- **Fast allows 4k, marked untested.** Its model page omits 4k; its
  evidence is `EvidenceInference`. A wrong claim costs one rejected,
  unbilled submit.
- **`CredentialScope` defaults to the project**, because Vertex jobs
  belong to the project. (`gemini.Config` requires it for private model
  content.)

## Success

Mobius moves `CreateVideo` to Dashi and bills from `VideoUsage`; the
live test passes; Gemini API clients behave as before.

## Risks and open questions

- Operation names may hold the project number, not the ID. The poll
  check matches location, model, and operation ID only, and always calls
  the configured endpoint. The live test shows the real form.
- An audio track can be present but silent, or Google may bill audio
  that a safety filter removed. Usage reports the track. The live test
  shows a track for an audio request.
- A 4k inline response can be near 100 MB of base64. Dashi caps it at
  the existing 512 MiB video limit.
- If Vertex returns only `gcsUri` without `storageUri`, the poll fails
  with `ErrorProtocol`, and the live test fails.

No open questions remain: `storageUri` is deferred, and Dashi reports
usage only, without prices.
