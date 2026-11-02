# Make Veo videos on Vertex AI and bill them by the video produced

Status: Approved\
PRD PR: #371  Implementation PR: #373  Updated: 2026-11-02\
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

**Dashi today.** `gemini.MediaClient` runs Veo on the Gemini API with an
API key. `media.Result` keeps usage only as `RawUsage` JSON.
`gemini.Config` has a Vertex credential path for model calls only.
**Dive** calls Vertex through the genai SDK, takes the inline bytes, and
reports the requested duration and a fixed 720p size.

**Vertex Veo (Google's docs, 2026-11-02).** Jobs submit with
`predictLongRunning` and poll with `fetchPredictOperation`; without
`storageUri`, the done operation holds the video inline. **The response
does not state duration, resolution, or audio**, and the reference says the
duration "may vary slightly", so usage must come from the MP4. Models, all
us-central1 only: `veo-3.1-generate-001` (720p, 1080p, 4k),
`veo-3.1-fast-generate-001` (720p, 1080p; only price-list copies show 4k),
and `veo-3.1-lite-generate-001` (Preview; 720p, 1080p); 4, 6, or 8 s; 16:9
or 9:16. Fast costs $0.08 to $0.30 per second by resolution and audio. Doc
links are in the spec.

## Users and job

**Primary:** a Go developer of an app like Mobius. Job: make a Veo video
on Vertex under the app's own project, get the bytes, and bill the
customer for the video actually made.

## Concepts

- **Vertex video client:** a Veo job client for one Vertex project and
  location. It uses the same credentials as a Vertex `gemini.Config`.
- **Audio off:** a video setting. By default a video has audio when the
  model can make it. The caller turns audio off to get a silent video.
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
if err != nil {
    return err // SubmissionUnknown: record it; do not resubmit
}
ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
defer cancel()
var poll media.JobPoll
for {
    poll, err = c.PollVideo(ctx, *sub.Job)
    if err != nil {
        return err // the job may still run: poll again later from *sub.Job
    }
    if poll.State != media.JobPending {
        break
    }
    select {
    case <-ctx.Done():
        return ctx.Err()
    case <-time.After(10 * time.Second):
    }
}
// JobFailed: report poll.Failure. JobSucceeded: the poll holds the video
// bytes, so do not persist its body; store the bytes once, from res.
res, err := c.FetchVideo(ctx, poll)
```

- [ ] With ADC (user or service account) and no API key, the job runs.
      Location defaults to us-central1. A location that resolves to
      `global` (for example `GOOGLE_CLOUD_LOCATION` shared with model
      calls) is adjusted to us-central1, and the prepared request records
      the adjustment. Vertex model calls through `gemini.New` keep their
      `global` default.
- [ ] The request body has `generateAudio: true` and `sampleCount: 1`.
- [ ] `res.Content[0]` holds the MP4 bytes. FetchVideo makes no network
      call.
- [ ] A caller-supplied `Client` supplies its own credentials; Dashi
      adds no token.

### UC-2: Bill the video

- [ ] For an 8 s 1080p request, `res.VideoUsage[0]` reads from the MP4:
      `Duration` within one frame of 8 s, 1920×1080, "1080p", `Audio`
      true. The host chooses how to round.
- [ ] If the file cannot be read, the video is still returned without a
      usage entry; the host bills from its recorded request.

### UC-3: A silent video

- [ ] `Settings.DisableAudio: true` sends `generateAudio: false`, and
      usage shows `Audio` false.
- [ ] The Gemini API Veo route rejects `DisableAudio` with
      `ErrorUnsupported`. Grok maps it to `generate_audio: false`.

### UC-4: Wrong settings and failures

- [ ] 4 s at 1080p is not rejected by Dashi on Vertex; Google decides.
      The Gemini API route keeps its 8 s rule.
- [ ] 4k on `veo-3.1-lite-generate-001`, a 5 s duration, or 1:1 fails
      in PrepareVideo with a message that names the value.
- [ ] A safety-filtered job polls as `JobFailed` with Google's reasons.
- [ ] Missing project fails construction. No ADC: SubmitVideo returns
      `SubmissionRejected` and sends nothing.

## Requirements

- R-1: A Vertex video client submits and polls with the documented
  endpoints, and gets the bytes from the done operation.
- R-2: It reuses the Vertex credentials of `gemini.Config` (ADC with
  `cloud-platform` scope, quota-project header) and adds no credential
  source. Only its location default differs, and only in the video client.
- R-3: Audio is on by default, can be turned off, and is always sent
  explicitly for Veo 3.x.
- R-4: Built-in model info covers the three Vertex IDs.
- R-5: Each fetched video carries usage measured from the file.
- R-6: Contract tests cover the wire and usage. One live test makes an
  8 s 1080p Fast video with audio.

## Not in scope

- `storageUri` output to Cloud Storage. Veo makes at most 8 s, so inline
  bytes fit.
- Video edit, extend, reference images, last frame, Imagen, and Vertex
  images.
- Prices in Dashi. Dashi reports usage; the host prices it.
- Changes to the Gemini API Veo route beyond rejecting `DisableAudio`.

## Decisions

- **A separate constructor, `NewVertexVideoJobs`**, not new fields on
  `MediaConfig`, whose images stay on the API-key route. Rejected:
  `MediaConfig.Vertex`, which would make half that client fail.
- **`DisableAudio bool` in `media.VideoSettings`** replaces Grok's
  `GenerateAudio`, with no shim. Zero means the model's default (audio).
  Rejected: `Audio *bool`; keeping both.
- **Changes a documented decision: the poll carries the bytes.** The done
  Vertex poll already holds them, so the succeeded `JobPoll` carries them
  and FetchVideo only checks and measures. Hosts persist the job handle,
  not succeeded poll bodies. Rejected: poll again in Fetch (downloads the
  video twice).
- **Changes a documented decision: `VideoUsage` is the first shared
  billing unit**, measured from the MP4. Rejected: copying the request;
  `RawUsage`. `design/media-provider-implementation.md` is updated.
- **Location default is us-central1**, the only region Google documents
  for Veo. A resolved `global` adjusts to us-central1, recorded on the
  prepared request, rather than failing.
- **Fast allows 4k, marked untested.** A wrong claim costs one rejected,
  unbilled submit.
- **`CredentialScope` defaults to the project**, because Vertex jobs
  belong to the project.

## Success

Mobius moves `CreateVideo` to Dashi and bills from `VideoUsage`; the
live test passes; Gemini API clients behave as before.

## Risks and open questions

- Operation names may hold the project number, not the ID. The check
  matches the location and checks the other segments by format only, and
  polls always call the configured endpoint. The live test (2026-11-02)
  returned the project ID.
- Google states no retention period for a done operation. Hosts fetch
  promptly after success and treat a not-found re-poll as lost output.
- An audio track can be present but silent, or Google may bill audio
  that a safety filter removed. Usage reports the track. The live test
  returned an AAC track for an audio request.
- A 4k inline response can be near 100 MB of base64, under the existing
  512 MiB video limit.
- If Vertex returns only `gcsUri` without `storageUri`, the poll fails
  with `ErrorProtocol`, and the live test fails.

No open questions remain: `storageUri` is deferred, and Dashi reports
usage only, without prices.
