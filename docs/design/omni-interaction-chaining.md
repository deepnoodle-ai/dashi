# Omni interaction chaining

Status: not built, 2026-11-17. Omni edit and extension work today by
resending the source video.

## What Google offers

Omni can edit or extend a video it made earlier without the caller sending it
again. The new request sets `previous_interaction_id` to the earlier
interaction's `id` and carries only a text prompt. Google keeps the video
server-side. This works only when the earlier call stored its interaction
(`store` not false).

## What Dashi does now

`media.VideoInput.SourceVideo` with the edit or extend operation sends the
video in the request: inline bytes, or a Files API URI such as a URI-delivered
Omni output. Inline Omni calls send `store: false`, so nothing is kept on
Google's side and each request is complete on its own. URI-delivered jobs
already send `store: true`, because Google needs it to create the File.

## Why it is more than a field

- **A handle, not media.** An interaction ID names provider state, not a
  video. It needs an origin (provider, API, credential scope) so a request
  cannot send it to another destination, as `content.Source.URI` does for
  Files. No existing `media` type carries such a handle.
- **Storage changes for every call.** Chaining needs the earlier call to store
  its interaction. Turning `store` on for inline calls would keep every
  prompt and video at Google for an undocumented time, which is a privacy
  change callers would need to opt into.
- **Unclear lifetime and failure modes.** Google does not document how long a
  stored interaction lasts, or what an expired ID returns. A host that
  restores a recorded request later would need that to decide whether to
  resend the video instead.
- **Hard to review.** A prepared request would no longer hold everything it
  sends. The model's input would partly live at Google.

## Options

1. **Keep resending (current).** Simple and stateless. It costs upload
   bandwidth and input tokens for long videos.
2. **Add a provider option** on the Interactions client, such as
   `PrepareVideoWithOptions(ctx, in, InteractionsVideoOptions{Previous: id})`,
   plus a client setting that stores inline interactions. This keeps the
   shared `media` types unchanged.
3. **Add a shared "previous result" handle** to `media.VideoInput`. This is
   worth it only if another provider documents the same kind of chaining.

## Recommendation

Stay with option 1 until a caller needs chained edits of long videos. Then
build option 2, with storage off by default, and qualify the expiry behavior
live before relying on it.
