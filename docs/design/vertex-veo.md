# Vertex Veo video jobs

**Status:** Draft · **Author:** Claude · **Date:** 2026-11-02\
**Workflow:** Spec, then wait for Curtis's review of the PRD PR, then
build.\ **PRD:** [vertex-veo](../prds/vertex-veo.md)

## Context

`gemini.MediaClient` runs Veo jobs only on the Gemini API with an API
key, and Google shut that Veo 3.1 route down on 2026-10-22. Mobius runs
Veo on Vertex under its own project and bills per second by resolution
and audio. Vertex returns the video inline as base64 but says nothing
about the video's length, size, or audio. Goal: a Vertex video job
client that reuses the Vertex credentials of `gemini.Config`, returns
the bytes, and reports usage measured from the MP4.

## Goals

- `NewVertexVideoJobs` runs prepare, submit, poll, and fetch against
  Vertex with ADC and no API key.
- `generateAudio` is always explicit for Veo 3.x; callers can turn it
  off.
- Each fetched video has a `media.VideoUsage` read from the file.
- Contract tests pin the wire; one live test passes on Vertex.

## Non-goals

`storageUri`, edit, extend, reference images, last frame, Vertex images,
prices, and changes to the Gemini API Veo route other than rejecting
`DisableAudio`.

## Proposal

### 1. API shape

Package `providers/google/gemini`, new file `media_vertex_video.go`:

```go
// VertexVideoConfig describes one Vertex project and location for Veo jobs.
// It resolves Project, Location, and credentials as a Vertex Config does.
type VertexVideoConfig struct {
    Provider        string // "" means "google-vertex"
    Project         string // "" means GOOGLE_CLOUD_PROJECT
    // "" means GOOGLE_CLOUD_LOCATION, then GOOGLE_CLOUD_REGION, then
    // "us-central1", the region on Google's Veo model page.
    Location        string
    CredentialScope string // "" means "project:" + Project
    Endpoint        string // "" means the documented predictLongRunning URL
    // Client supplies transport and credentials. When it is set, Dashi adds
    // no token; when it is nil, Dashi uses ADC.
    Client           *http.Client
    VideoModels      map[string]media.ModelInfo // added to the built-in models
    CapabilityPolicy model.CapabilityPolicy
}

type VertexVideoJobClient struct{ /* config, models, credentials */ }

var _ media.VideoJobClient = (*VertexVideoJobClient)(nil)

func NewVertexVideoJobs(config VertexVideoConfig) (*VertexVideoJobClient, error)
func (*VertexVideoJobClient) Provider() string
func (*VertexVideoJobClient) VideoModelInfo(name string) (media.ModelInfo, bool)
// PrepareVideo, SubmitVideo, PollVideo, FetchVideo as media.VideoJobClient.
```

Shared `media` changes:

```go
type VideoSettings struct {
    // …existing fields…
    // DisableAudio asks for a video without audio. False means the
    // model's default, which is audio for models that make it.
    DisableAudio bool `json:"disable_audio,omitempty"`
}

// VideoUsage describes one generated video in the units that video APIs
// bill by, as measured from the file. ContentIndex names its block in
// Result.Content.
type VideoUsage struct {
    ContentIndex int           `json:"content_index"`
    Duration     time.Duration `json:"duration"`
    Width        int           `json:"width"`
    Height       int           `json:"height"`
    Resolution   string        `json:"resolution,omitempty"` // "720p", "1080p", "4k"
    Audio        bool          `json:"audio"`
}

type Result struct {
    // …existing fields…
    // VideoUsage has one entry for each video that the adapter could
    // measure. A video with no entry was not measured; bill it from the
    // recorded request.
    VideoUsage []VideoUsage `json:"video_usage,omitempty"`
}
```

`Result.Validate` checks each entry: index in range, block is video, no
duplicate index, nonnegative values. `Clone` copies the slice.
`Resolution` is the tier of the short edge: 720 → "720p", 1080 →
"1080p", 2160 → "4k", other → "".

### 2. Credentials and token flow

No new credential source. Two helpers move out of `gemini.New` and
`newSDKClient` so both clients share them:

- `vertexHost(location)`: global → `aiplatform.googleapis.com`; us, eu →
  `aiplatform.{loc}.rep.googleapis.com`; other → `{loc}-aiplatform.googleapis.com`.
- `vertexCredentials()`: `credentials.DetectDefault` with the
  `https://www.googleapis.com/auth/cloud-platform` scope, the call genai's
  `UseDefaultCredentials` makes today. `newSDKClient` sets
  `ClientConfig.Credentials` from it. Vertex documents no narrower
  scope.

The video client detects credentials on its first send and keeps them;
a failed detection is not kept, so the next send tries again.
`auth.Credentials` caches and refreshes the token. Each request gets
`Authorization: Bearer <token>` and, when the credential has a quota
project, `X-Goog-User-Project` (the existing `withQuotaProject` rule).

Token safety: Dashi attaches the ADC token only when `Client` is nil and
the endpoint is `https` on a `*.googleapis.com` host. A custom endpoint
on another host needs a `Client`, or construction fails. Redirects are
refused. Project fallback stays env-only, as in `gemini.Config`.
`cloud.google.com/go/auth` becomes a direct dependency of the gemini
module; it is already an indirect one through genai.

### 3. Endpoints and validation

Default submit URL, from the Vertex REST sample:

```
https://{host}/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:predictLongRunning
```

The poll URL replaces `:predictLongRunning` with `:fetchPredictOperation`.
Construction checks a custom `Endpoint` as `validateMediaEndpoint` does,
plus the `/projects/{p}/locations/{l}/publishers/google` scope (as
`gemini.New` does), and requires `Provider` when it differs from the
default. The origin scope hashes `[VideoAPI, endpoint, CredentialScope]`.

`SubmitVideo` accepts an operation name only in this form:
`projects/{any one segment}/locations/{location}/publishers/google/models/{model}/operations/{id}`,
with `{id}` one segment of `[A-Za-z0-9_-]`. Vertex may return the
project number instead of the ID, so the project segment is not matched.
The name goes only into the poll body; the poll URL always comes from
the configuration, so a name cannot send the token elsewhere.

### 4. Request

```json
{"instances": [{"prompt": "…",
                "image": {"bytesBase64Encoded": "…", "mimeType": "image/png"}}],
 "parameters": {"sampleCount": 1, "generateAudio": true, "durationSeconds": 8,
                "resolution": "1080p", "aspectRatio": "16:9"}}
```

- `sampleCount: 1` always: one video, one charge.
- `generateAudio: !DisableAudio` for every model not prefixed `veo-2`
  (Veo 2 rejects the field; Dive's `veoGeneratesAudio` rule). Always
  sent, because Google's default changed once already.
- `durationSeconds`, `resolution`, `aspectRatio` only when set. Unset
  duration is Google's default, 8 s.
- `image` only for one reference frame: inline JPEG or PNG, at most
  20 MB. There is no `gcsUri` input.

PrepareVideo rejects, with `ErrorUnsupported` and a message that names
the value: an operation other than generate, `Size`, more than one
frame, aspect ratios other than 16:9 and 9:16, durations other than 4, 6,
or 8 s, and resolutions other than 720p, 1080p, and 4k. It does **not**
apply the Gemini API rule "1080p and 4k need 8 s"; no Vertex doc states
it, and a wrong rejection would block a valid video. Then the model
info check runs (`VideoCapabilities.Check`).

Built-in model info (`EvidenceDocumentation`, owner "dashi", dated
2026-11-02, endpoint resolved for the configured project and location):

| Model | Resolutions | Frames |
| --- | --- | --- |
| `veo-3.1-generate-001` | 720p, 1080p, 4k | supported |
| `veo-3.1-fast-generate-001` | 720p, 1080p, 4k | supported |
| `veo-3.1-lite-generate-001` | 720p, 1080p | unknown |

All: generate supported; edit and extend unsupported (not built);
durations 4, 6, 8 s; 16:9 and 9:16; video/mp4. Fast lists 4k because
Google prices it, although its model page omits it. Lite's frames are
unknown because its page contradicts itself. `VideoModels` entries win
over built-in ones.

### 5. Poll and response

`POST …:fetchPredictOperation` with `{"operationName": job.ID}`. The
read limit is `maxVideoBytes*4/3 + 1 MiB` (base64 of a 512 MiB video).
Decoding:

| Response | JobPoll |
| --- | --- |
| `done` false | `JobPending` |
| `error` set | `JobFailed`, message or "code N" |
| `videos[]` with `bytesBase64Encoded` | `JobSucceeded`, inline `VideoBlock`s |
| no videos, `raiMediaFilteredCount` > 0 | `JobFailed`, "blocked by safety filters: <reasons>" |
| a video with only `gcsUri` | `ErrorProtocol` (Dashi never sets `storageUri`) |
| `name` differs from the job | `ErrorProtocol` |

A missing `mimeType` means video/mp4; a non-video type is
`ErrorProtocol`. The succeeded poll carries the bytes, so a host that
records polls should store the bytes once, from the result.

### 6. Fetch and usage

FetchVideo checks the poll belongs to this destination and holds inline
video blocks. It sends nothing. For each block it calls
`mp4.Probe(data)`, a new generic helper in `internal/mp4`:

```go
type Info struct { Duration time.Duration; Width, Height int; Audio bool }
func Probe(data []byte) (Info, error)
```

`Probe` walks the boxes to `moov`: `mvhd` (version 0 or 1) gives
duration over timescale; each `trak`'s `mdia/hdlr` gives the handler;
the first `vide` track's `tkhd` gives width and height (16.16); any
`soun` track sets `Audio`. It handles 64-bit and to-end box sizes,
limits depth and box count, and returns an error for a missing `moov`,
a missing video track, or a zero timescale. On success FetchVideo sets
`Video.Duration` and adds a `VideoUsage`; on error it adds neither and
still returns the video.

### 7. Other adapters

- `gemini.MediaClient` (Gemini API Veo) and the Interactions video
  clients reject `DisableAudio` with `ErrorUnsupported`: they cannot
  encode it.
- Grok `imagine` maps `DisableAudio` to `generate_audio: false` under the
  existing grok-imagine-video-1.5 rule. With `VideoOptions.GenerateAudio`
  set to true as well, PrepareVideo fails.

### 8. Errors

| Case | Result |
| --- | --- |
| No project | construction error "gemini: Vertex video requires a project: set Project or GOOGLE_CLOUD_PROJECT" |
| ADC not found, token fetch fails | `ErrorAuthentication`; submit is `SubmissionRejected` (nothing sent) |
| HTTP 401, 403 | `ErrorAuthentication`, as `doGeminiMedia` maps today |
| 400, 404 on submit | `ErrorInvalidRequest`, `SubmissionRejected` |
| 408, 409, 425, 429, 5xx, transport error on submit | `SubmissionUnknown` |
| Undecodable body, bad operation name | `ErrorProtocol` |

The HTTP send reuses `doGeminiMedia`'s status mapping and size limits,
with a header function in place of the API-key header.

## Test plan

Contract tests (`media_vertex_video_test.go`, httptest server with a
supplied `Client`):

- Construction: env fallbacks, default host per location, us-central1
  default, endpoint scope check, custom host without `Client` refused,
  built-in models present and overridable.
- Request body golden JSON: text, frame, `DisableAudio`, Veo 2 omits
  `generateAudio`; rejections from section 4; 4 s 1080p accepted.
- Submit: accepted name with project ID and with project number;
  foreign location, model, or extra segment rejected; status → state.
- Poll: each row of the section 5 table; poll body names the job; URL is
  the configured one.
- Fetch: usage from fixture MP4s (720p silent, 1080p with audio, 9:16);
  an unparsable file returns the video without usage; no request sent.
- Credentials: a fake token source sets `Authorization` and
  `X-Goog-User-Project`; a supplied `Client` gets no token.
- `internal/mp4`: fixtures, truncated and looping boxes, and a fuzz test.
- `media`: `VideoUsage` validation and clone; Grok and Gemini API
  `DisableAudio` handling.

Live test (`media_vertex_video_live_test.go`, `//go:build live`), one
paid call of about $0.96: skips unless `DASHI_LIVE_VERTEX_VIDEO_TEST=1`;
needs `GOOGLE_CLOUD_PROJECT` (run with
`GOOGLE_CLOUD_PROJECT=$(gcloud config get-value project)`), ADC, and
optional `GOOGLE_CLOUD_LOCATION`. It submits `veo-3.1-fast-generate-001`,
8 s, 1080p, 16:9, audio on; polls every 10 s for up to 6 minutes; and
checks usage: 7.5–8.5 s, 1920×1080, "1080p", `Audio` true. It logs the
operation name form. `DASHI_LIVE_VERTEX_VEO_JOB_ID` resumes a job
without a new submission. Run once, before the implementation PR.

## Alternatives considered

- **`MediaConfig.Vertex`.** One constructor, but image calls on that
  client would fail, and API-key fields would sit beside Vertex fields.
- **The genai SDK** (as Dive and `gemini.Client` do). It hides the
  request body, so Prepare could not record the exact wire bytes, and it
  does not expose the operation name check.
- **Usage from the request.** No parser, but it bills the request, not
  the video; Google says the length can differ.
- **Poll again in Fetch.** Keeps polls small, but downloads each video
  twice and makes Fetch depend on the operation still existing.

## Tradeoffs and consequences

- A 4k succeeded poll holds tens of MB; hosts that persist polls pay for
  it.
- An MP4 parser reads provider bytes; limits and a fuzz test contain it.
- Usage reports an audio track, not what Google bills. If Google strips
  audio after a safety filter and still bills it, usage under-reports.
- Lenient checks (no 8 s rule, Fast 4k) can lead to a rejected submit;
  a rejected submit is not billed.
- New root names (`DisableAudio`, `VideoUsage`) need a root release
  before the gemini module can tag.

## Rollout

Pre-release library; no compatibility shims. CHANGELOG under
Unreleased: Vertex Veo client, `DisableAudio`, `VideoUsage`. Update the
media row in `design/media-provider-implementation.md` with the live
result.

## Open questions

Product questions are in the PRD. The live test settles the operation
name form and whether an audio request yields a `soun` track.

## Sources (read 2026-11-02)

- [Veo 3.1 model page](https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/veo/3-1-generate):
  model IDs, launch stage, durations, aspect ratios, resolutions,
  us-central1.
- [Generate videos from text](https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/video/generate-videos-from-text):
  REST URLs, `fetchPredictOperation` body, response sample, inline bytes
  without `storageUri`, "4k (Veo 3.1 Preview models only)".
- [VideoGenerationModelParams](https://docs.cloud.google.com/gemini-enterprise-agent-platform/reference/rest/Shared.Types/VideoGenerationModelParams):
  `generateAudio` defaults to true; duration "may vary slightly".
- [VideoGenerationModelInstance](https://docs.cloud.google.com/gemini-enterprise-agent-platform/reference/rest/Shared.Types/VideoGenerationModelInstance):
  `image` with `bytesBase64Encoded` or `gcsUri`, JPEG or PNG.
- [Pricing](https://cloud.google.com/model-garden/pricing): Veo per
  second by resolution and audio.
- [Gemini API video](https://aistudio.google.com/docs/video): Veo 3.1
  Preview shut down 2026-10-22.
- genai v1.71.0 `models.go` (`videoFromVertex`): response field names;
  `client.go`: the `cloud-platform` scope.
