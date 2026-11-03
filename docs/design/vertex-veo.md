# Vertex Veo video jobs

**Status:** Approved · **Author:** Claude · **Date:** 2026-11-02\
**PRD:** [vertex-veo](../prds/vertex-veo.md)

## Context

`gemini.MediaClient` runs Veo only on the Gemini API with an API key, and
Google shut that Veo 3.1 route down on 2026-10-22. Vertex returns the video
inline as base64 but states nothing about its length, size, or audio. Goal:
a Vertex video job client that reuses the Vertex credentials of
`gemini.Config`, returns the bytes, and reports usage measured from the MP4.
The PRD records the product decisions and rejected alternatives; this spec
does not repeat them.

## Proposal

### 1. API shape

Package `providers/google/gemini`, new file `media_vertex_video.go`:

```go
type VertexVideoConfig struct {
    Provider        string // "" means "google-vertex"
    Project         string // "" means GOOGLE_CLOUD_PROJECT
    Location        string // see section 2
    CredentialScope string // "" means "project:" + Project
    Endpoint        string // "" means the documented predictLongRunning URL
    Client          *http.Client // set: Dashi adds no token; nil: ADC
    VideoModels      map[string]media.ModelInfo // added to the built-in models
    CapabilityPolicy model.CapabilityPolicy
}

func NewVertexVideoJobs(config VertexVideoConfig) (*VertexVideoJobClient, error)
// Provider, VideoModelInfo, and the four media.VideoJobClient methods.
```

Shared changes:

- `media.VideoSettings.DisableAudio bool`: false means the model's default,
  which is audio for models that make it.
- `media.VideoUsage{ContentIndex, Duration, Width, Height, Resolution,
  Audio}` and `media.Result.VideoUsage []VideoUsage`. `Duration` is the
  video track's length with its edit list applied, not rounded. A video
  with no entry was not measured; the host bills it from the recorded
  request. `Result.Validate` checks index, block kind, duplicates, and
  signs; `Clone` copies the slice. `Resolution` is the short edge's tier:
  720 → "720p", 1080 → "1080p", 2160 → "4k", other → "".
- Comments that describe the old "poll observes, fetch gets the output"
  rule change to say a succeeded poll can carry the output:
  `media.JobSucceeded`, `media.JobPoll.Content`, and the
  `media.VideoJobClient` comment in `media/request.go`.
- Grok `imagine.VideoOptions.GenerateAudio` is removed; Grok maps
  `DisableAudio` to `generate_audio: false`. The Gemini API Veo client and
  the Interactions video clients reject `DisableAudio` with
  `ErrorUnsupported`. `gemini.NewMedia`'s comment points Veo callers to
  `NewVertexVideoJobs`.

### 2. Location and credentials

Location resolution is local to `NewVertexVideoJobs`; `gemini.New` is
unchanged and still defaults Vertex model calls to `global`. The video
client resolves `Location`, then `GOOGLE_CLOUD_LOCATION`, then
`GOOGLE_CLOUD_REGION`, then `us-central1`, the only region Google documents
for Veo. A value that resolves to `global` becomes `us-central1`. Any other
value is used as given. Following decision 4 in `design/decisions.md` (warn
and adjust), `PrepareVideo` records the change as a `media.Adjustment` on
the prepared request: setting `location`, requested `global`, effective
`us-central1`, with the reason. The host sees it in
`VideoRequest.Snapshot().Adjustments`.

Two small helpers in `vertex.go` are shared with `gemini.New` and
`newSDKClient` without changing their behavior: `vertexHost(location)` and
`vertexCredentials()` (`credentials.DetectDefault` with the
`cloud-platform` scope, the call genai makes today). The video client
detects credentials on its first send and keeps them; a failed detection is
not kept. Each request carries `Authorization: Bearer` and, when the
credential has a quota project, `X-Goog-User-Project`. Submit and poll get
the token before sending, so a token failure sends nothing.

Token safety: Dashi attaches the ADC token only when `Client` is nil and
the endpoint is `https` on a Vertex AI host (the forms `vertexHost`
returns). Any other host needs a `Client`, or construction fails.
Redirects are refused. The project fallback is env-only, as in
`gemini.Config`.

### 3. Endpoints and operation names

Submit: `https://{host}/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:predictLongRunning`.
Poll: the same URL with `:fetchPredictOperation` and body
`{"operationName": job.ID}`. A custom `Endpoint` is checked as
`validateMediaEndpoint` does, plus the `/projects/{p}/locations/{l}/publishers/google`
scope, and needs `Provider`.

An operation name must be
`projects/{project}/locations/{location}/publishers/google/models/{model}/operations/{id}`.
The location must equal the configured one. The project, model, and ID are
checked by format only: Vertex may name the project by number or the model
by the ID it resolved, and a stricter check would lose a paid job. A poll
reply whose `name` is set must pass the same check and carry the job's
operation ID. The name only goes into the poll body; the poll URL always
comes from the configuration, so a name cannot send the token elsewhere.

### 4. Request and model info

The body sends `sampleCount: 1` (one video, one charge), `generateAudio:
!DisableAudio` for every model not prefixed `veo-2`, and `durationSeconds`,
`resolution`, `aspectRatio`, and one inline JPEG or PNG first frame only
when set. `PrepareVideo` rejects, with `ErrorUnsupported` and a message
naming the value: an operation other than generate, `Size`, more than one
frame, aspect ratios other than 16:9 and 9:16, durations other than 4, 6,
or 8 s, resolutions other than 720p, 1080p, and 4k, and `DisableAudio` on
a `veo-2` model, which rejects `generateAudio`. It does not apply
the Gemini API rule "1080p and 4k need 8 s". Then the model info check runs.

| Model | Resolutions | Frames | Evidence |
| --- | --- | --- | --- |
| `veo-3.1-generate-001` | 720p, 1080p, 4k | supported | documentation |
| `veo-3.1-fast-generate-001` | 720p, 1080p, 4k | supported | inference; 4k untested |
| `veo-3.1-lite-generate-001` | 720p, 1080p | unknown | documentation |

`VideoModels` entries win over built-in ones.

### 5. Poll

| Reply | Result |
| --- | --- |
| `done` false | `JobPending` |
| `error` set | `JobFailed`, Google's message |
| videos with inline bytes | `JobSucceeded`; `Content` holds the videos |
| no videos, filtered count > 0 | `JobFailed`, "blocked by safety filters: …" |
| done, no error, no videos, no filtered count | `ErrorProtocol` |
| a video with only `gcsUri`, or a non-video MIME type | `ErrorProtocol` |
| undecodable body, or a `name` that fails section 3 | `ErrorProtocol` |

The succeeded poll carries the bytes. Hosts persist the job handle, not a
succeeded poll body, and store the bytes once, from the result.

**Retention.** Google's long-running operation docs give no retention
period for a done operation, and the Veo docs say nothing either. Hosts
should fetch promptly after `JobSucceeded` and treat a re-poll that returns
not found as lost output, not as a reason to resubmit.

### 6. Fetch and usage

`FetchVideo` checks that the poll belongs to this destination, copies the
video blocks, and sends nothing. For each video it calls `mp4.Probe` in a
new `internal/mp4` package, which reads `moov` box headers only. Duration
comes from the first `vide` track, not `mvhd`, because AAC audio often runs
20 to 50 ms past the video. With an edit list, it is the sum of every edit's
segment duration, empty edits (`media_time` -1) included, since an empty
edit is a gap in the track's timeline; without one, it is the `mdhd`
duration. `tkhd` gives width and height; any `soun` track sets `Audio`.
`Probe` bounds box count and returns an error for a missing `moov` or video
track, a zero timescale, a length that rounds to zero, or a missing or zero
frame size. On success, FetchVideo sets `Video.Duration` and adds a
`VideoUsage`; on error, it adds neither and still returns the video.

### 7. Errors

| Case | Result |
| --- | --- |
| No project | construction error naming `Project` and `GOOGLE_CLOUD_PROJECT` |
| No ADC, token fetch fails | `ErrorAuthentication`; submit is `SubmissionRejected` |
| HTTP 401, 403 | `ErrorAuthentication` |
| Other 4xx on submit (not 408, 409, 425, 429) | `SubmissionRejected` |
| 408, 409, 425, 429, 5xx, transport error on submit | `SubmissionUnknown` |
| 2xx submit with an undecodable body, no name, or a name that fails section 3 | `SubmissionUnknown`, `ErrorProtocol` |

A 2xx submit reply means Google may have accepted and billed the job, so an
unusable reply is unknown, never rejected. The `media.Error` keeps the raw
reply in `Body` and names the operation in its message, so the host can
reconcile the job by hand.

## Testing

Contract tests run against an httptest server with a supplied `Client`.
They cover each location default (unset, env, `global` with its
adjustment, explicit other region, and `gemini.New` still `global`), the
golden request body, each rejection, each poll and error row above
(including a project-number name and a done reply with no output), usage
from fixtures, and the token headers. Fixture MP4s are built in the tests by
`internal/mp4/mp4test`; no third-party files are checked in. `internal/mp4`
also has a fuzz test. One live test (`//go:build live`, about $0.96) makes
an 8 s 1080p Fast video with audio and checks the usage.

**Live result (2026-11-02):** passed in 94 s. The operation name used the
project ID. The done operation held inline bytes. Usage: 8 s, 1920×1080,
"1080p", audio true; ffprobe agrees (h264 8.000 s, AAC 8.000 s). The run
used a gcloud access token in a supplied `Client`, because the local ADC
needed reauthentication; Dashi's ADC token path is covered by contract
tests only.

## Tradeoffs

- A 4k succeeded poll holds tens of MB; the docs say to persist the handle.
- An MP4 parser reads provider bytes; bounds and a fuzz test contain it.
- Usage reports an audio track, not what Google bills. If Google strips
  audio after a safety filter and still bills it, usage under-reports.
- Lenient checks (no 8 s rule, Fast 4k) can lead to a rejected submit,
  which Google does not bill.
- New root names (`DisableAudio`, `VideoUsage`, `internal/mp4`) need a root
  release before the gemini module can tag.

## Sources (read 2026-11-02)

- [Veo 3.1 model page](https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/veo/3-1-generate):
  model IDs, durations, aspect ratios, resolutions, us-central1.
- [Generate videos from text](https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/video/generate-videos-from-text):
  REST URLs, `fetchPredictOperation` body, "4k (Veo 3.1 Preview models only)".
- [VideoGenerationModelParams](https://docs.cloud.google.com/gemini-enterprise-agent-platform/reference/rest/Shared.Types/VideoGenerationModelParams):
  `generateAudio` defaults to true; duration "may vary slightly".
- [Long-running operations](https://docs.cloud.google.com/vertex-ai/docs/general/long-running-operations):
  no retention period stated.
- [Pricing](https://cloud.google.com/model-garden/pricing): per second by
  resolution and audio.
- genai v1.71.0 `videoFromVertex` and Dive `providers/google/media.go`:
  inline `bytesBase64Encoded` in production.
