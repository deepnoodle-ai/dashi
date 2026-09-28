# 16. Turn-owned attachment references — draft PRD

**Status:** Draft for review, 2026-09-28. **Size/difficulty:** M / High (roadmap estimate). **Research:** [comparative findings](16-attachment-references-research.md). **Existing proposal:** `design/history-storage.md:155-187`; this draft narrows its first slice to verifiable inline bytes.

## User problem

A host that stores an uploaded image or document already has a durable, authorized artifact identity. Today it must put the bytes into Dashi's `Start.Input` or a tool result. Those bytes enter the Turn record and can grow checkpoints and logs. If the host substitutes a URL or provider file ID, Dashi has no way to recheck host authorization or prove the bytes behind that location. Mobius solves this with a costly conversion around Dive's content types. A Dashi user should be able to say “this Turn includes this stored attachment” once, inspect the Turn without loading it, and have each model request use only content that the host still authorizes.

## Goals and non-goals

**Goals:** A small application-owned reference in existing media `content.Source`; immutable byte identity (size and SHA-256); host-controlled resolution and access check before each `Prepare`; exact request evidence without persisting resolved bytes; explicit errors for missing, revoked, changed, oversized, or unsupported content. The same mechanism should work for current-turn input, delivered context, and tool-result media.

**Non-goals:** Dashi artifact storage, upload endpoints, automatic externalization of inline content, private URL fetch, a generic resource browser, signed-URL/provider-file-ID resolution, or a new Session/history store. Prior `History` remains caller-owned. Existing public URLs and provider IDs keep their current explicit semantics.

## Proposed smallest contract

```go
// Sketch, names subject to Go API review.
type Ref struct {
    ID     string // opaque host identifier; never treated as a URL/path
    SHA256 string // canonical hex digest of original bytes
    Size   int64
}
// content.Source gains Ref *content.Ref, mutually exclusive with Data/URL/ID/URI.
// Existing Source.MIMEType and document title remain the media metadata.

type ContentResolver interface {
    Resolve(ctx context.Context, turnID string, ref content.Ref) ([]byte, error)
}
// Runner.Content binds the application resolver for model preparation.
```

The host saves and validates the attachment first, computes the digest from the stored bytes, and records a `Source{Ref: ..., MIMEType: ...}`. The ref is an identifier, **not** an access token. The host resolver uses trusted invocation context plus Turn/session/tenant policy to re-authorize and read it, including after resume. It must retain bytes for as long as that Turn may legitimately need another model call. Deletion or revocation is allowed and causes a visible pre-dispatch failure.

Dashi keeps the ref in Turn facts. After final message projection and `BeforeModel` adjustments, it resolves every app ref on a **detached** input immediately before `Client.Prepare`. It checks exact `Size` and SHA-256 of returned bytes, rejects inputs exceeding configured limits, and passes ordinary inline `Source.Data` to the existing provider preparation path. The host resolver must bound its own storage read. The original facts and policy-visible Turn view remain ref-bearing. `BeforeSend` sees the actual prepared request; the existing `ModelFact.RequestDigest` pins its exact body. Replay and inspection never call the resolver. A retry or resumed model call resolves and re-authorizes again; no cached bytes may bypass that check. Providers can still report unsupported media through their normal `Prepare` error.

For the first slice, resolver output is **bytes only**. Returning a signed URL or provider ID cannot be checked against `Ref.SHA256` by Dashi and has separate expiry/origin behavior. That may be a later contract with explicit attestations and content-change evidence. No arbitrary URI is implicitly fetched.

### Developer flow

```go
saved := artifactStore.Save(ctx, tenantID, upload) // host-owned, durable
source := content.Source{
    Ref: &content.Ref{ID: saved.ID, SHA256: saved.SHA256, Size: saved.Size},
    MIMEType: saved.MIMEType,
}
// Put source in an image/document input block or a tool-result block.
runner.Content = contentResolver // host uses trusted ctx + turnID to authorize
```

The example is deliberately host-shaped: Dashi offers no storage client or upload policy. API review should settle whether the resolver is a `Runner` field or per-`Apply` dependency so a shared Runner cannot accidentally resolve under the wrong tenant.

## Acceptance criteria

1. Source validation accepts exactly one location; a ref requires nonempty opaque ID, positive size, valid SHA-256, and media metadata required by its content kind. Existing `Data`, `URL`, `ID`, and `URI` behavior remains compatible.
2. Starting a Turn or committing a tool result with `Ref` records only the ref and metadata. Project/replay/inspection produce the same ref without I/O. Inline content still works; Dashi never auto-externalizes it.
3. A model call with refs requires a resolver. It resolves only on an in-memory copy after the final model input is assembled and before `Prepare`; all refs in current-turn input, context, tool results, and caller-supplied history are handled uniformly without changing ownership of history.
4. A resolver failure, authorization denial, deletion, size/digest mismatch, or configured size limit stops **before model intent/dispatch** with a typed, actionable error naming the ref and cause without leaking bytes. It never omits or replaces the content silently. A retry must not accept different bytes under the same ref.
5. Successful preparation records the digest of the exact resolved wire request and no hydrated bytes in Turn facts. `BeforeSend` can inspect/authorize that prepared request. Paused/resumed Turns re-authorize on the next model call; deleting or unbinding an artifact prevents further delivery.
6. Tests cover input and tool-result refs, duplicate refs, no resolver, revoked/missing/changed bytes, bounded oversized input, resume, and a provider that rejects the media type. A small example shows host storage and tenant/session authorization, including retention responsibility.

## Risks and open decisions

- **Failure lifecycle:** Current `runner.go:1086-1089` turns `Prepare` errors into a failure. Decide the exact typed stop/error and whether a host can resume after restoring the *same* authorized bytes. No automatic substitution or silent retry is proposed.
- **Resolver binding:** A global Runner can serve many tenants. Prefer a per-invocation trusted context or explicit scoped resolver binding; do not infer authority from the opaque ID or rely on an arbitrary user string inside the ref.
- **Limits and memory:** The resolver returns bytes, so Dashi needs a clear per-object and aggregate ceiling before accepting them; large streaming documents or provider uploads can be a later extension. A duplicate-ref cache within one preparation may save I/O only after each access check.
- **Request evidence:** `RequestDigest` proves the encoded body Dashi prepared, not provider receipt. Existing interruption/uncertainty handling remains authoritative. Do not persist `RequestSnapshot.Body`, which includes hydrated bytes (`llm/request.go:18-32`).
