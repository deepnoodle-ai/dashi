# PRD: attachments — stored references and `@file` mentions

| Field | Value |
| --- | --- |
| Status | Draft for review, 2026-09-28 (revised from the first draft) |
| Roadmap | [Item 16](../remaining-work.md), size M / high difficulty, plus the CLI attachment helper |
| Evidence | [Source review](16-attachment-references-research.md), [history storage](../history-storage.md), Dive CLI `attachments.go` |
| Demo | Wednesday 2026-09-30. Slice 1 ships by Tuesday night. |

## Problem

Two users cannot attach files today.

1. **The `dashi` CLI user** types `explain @internal/wake/wake.go` or `what is wrong in ./err.png`. Dashi has no code that turns those words into content the model can read. Dive's CLI has this code, and the Dashi CLI needs it back.
2. **A Dashi host** already stores uploads. To attach one, the host must copy its bytes into `Start.Input` or a tool result. The bytes enter the Turn record and grow every checkpoint and log. A URL or provider file ID cannot fix this: Dashi cannot check who may read it, or prove which bytes it names.

If we do nothing, the CLI demo has no file mentions, and each host writes its own conversion layer.

## Goals and metrics

- A CLI user attaches a workspace file or image with one word. Time to attach: no extra step.
- A host records a stored attachment as an ID, a SHA-256 digest, and a size. The Turn record holds no attachment bytes. Test: a Turn with a 5 MiB ref serializes to under 1 KiB.
- Each model call re-checks access and bytes. Test: revoked, missing, changed, and oversized content each stop the Turn before any model request.
- Guardrails: inline `Source` behavior does not change. `Project`, replay, and inspection never call the host.

## Users

**Primary:** the CLI user (demo path). **Secondary:** the host developer who stores uploads and runs Turns for many tenants.

## Part A: content references (library)

### Public API

```go
// package content
type Ref struct {
    ID     string `json:"id"`     // opaque host ID; never a URL or path
    SHA256 string `json:"sha256"` // lowercase hex digest of the bytes
    Size   int64  `json:"size"`   // byte count of the bytes
}
// Source gains: Ref *Ref `json:"ref,omitempty"`
// Ref excludes Data, URL, ID, URI, Origin, and ExpiresAt.
// Ref requires MIMEType, because Dashi cannot detect it without the bytes.

// package dashi
type ContentResolver interface {
    // Resolve returns the bytes of ref. It checks that turnID may read ref.
    Resolve(ctx context.Context, turnID string, ref content.Ref) ([]byte, error)
}
// Runner gains:
//   Content ContentResolver // nil: a Turn that holds a Ref stops before its first model call
//   ContentLimits ContentLimits

type ContentLimits struct {
    MaxRefBytes   int64 // per ref; zero means 32 MiB; negative means no limit
    MaxTotalBytes int64 // per model request; zero means 64 MiB; negative means no limit
}
```

### Host flow

```go
source := content.Source{
    Ref:      &content.Ref{ID: "upl_1", SHA256: sum, Size: 812},
    MIMEType: "image/png",
}
turn, err := runner.Run(ctx, dashi.Start{Input: []content.Block{
    content.TextBlock("What is wrong here?"), content.ImageBlock(source),
}})
```

The record holds only the ref. Before each model call, Dashi does this on a copy of the input:

1. Check each ref's declared size against `ContentLimits`. This needs no I/O.
2. Call `Content.Resolve` with the run context. The host uses that context to check tenant and session.
3. Check that the returned length equals `Size` and the SHA-256 equals `SHA256`.
4. Put the bytes in `Source.Data`, then call `Prepare`.

`ModelFact.RequestDigest` covers the request body with the bytes. Facts store no bytes. Every retry and resume runs steps 1 to 4 again. Dashi keeps no cache across calls, so a revoked ref stops delivery at the next call.

### Failure behavior

A ref problem stops the Turn as failed before Dashi records a model intent. The failure names the ref ID and one reason: `no_resolver`, `unavailable` (any resolver error, including denied and deleted), `too_large`, `size_mismatch`, or `digest_mismatch`. It never includes bytes. Dashi never drops, replaces, or falls back to another location. This is a known failure, not an uncertain one.

## Part B: `@file` mentions (helper for the CLI)

A new stdlib-only package, `content/mention`. Any terminal host can use it. Policy comes from `Options`.

```go
type Options struct {
    Root          string // workspace root; zero means the current directory
    MaxFileBytes  int64  // per file; zero means 10 MiB
    MaxTextBytes  int64  // text file; zero means 256 KiB
    MaxTotalBytes int64  // all files in one message; zero means 32 MiB
}
type Result struct {
    Text     string          // input with each attached mention replaced by a label
    Blocks   []content.Block // one block per attached file, in order
    Attached []Attachment    // Path, MIMEType, Size, for the screen
    Warnings []string        // one plain sentence per skipped mention
}
func Expand(input string, opt Options) Result
```

### What the user sees

| Input | Result |
| --- | --- |
| `explain @internal/wake/wake.go` | Text becomes `explain [file: internal/wake/wake.go]`. Blocks hold one text document titled with the path. |
| `what is wrong in ./err.png` | The path stays in the text. Blocks hold one image. |
| `see @docs/spec.pdf` | Blocks hold one PDF document. |
| `mail me@example.com` | No change. `@` starts a mention only at the start of the text or after a space or opening bracket. |
| `read @../secret.txt` | The text stays. Warning: `Skipped @../secret.txt: outside the workspace.` |
| `read @huge.bin` | Warning: `Skipped @huge.bin: 48 MiB is over the 10 MiB limit.` |

Rules:

- A mention is `@path` or `@"path with spaces"`. Trailing `?!,.:;)]}` is not part of the path.
- A bare word attaches only when it names an existing image or PDF inside the workspace. A bare text path stays prose.
- The helper resolves symbolic links first. A path that leaves `Root` after resolution is outside the workspace. Absolute paths are outside unless they resolve inside `Root`.
- Image types: PNG, JPEG, GIF, WebP. PDF is a document. A text file is a document with `text/plain` and its path as title. A file that is neither, or not valid UTF-8, is skipped with a warning.
- A skipped mention never fails the send. The CLI shows the warnings and sends the message.
- The same file mentioned twice attaches once.

## User stories

- **US-1.** As a CLI user, I type `@path` so the model reads that file.
  - [ ] Test: a text file becomes a titled `text/plain` document block.
  - [ ] Test: an email address is not a mention.
- **US-2.** As a CLI user, I name an image or PDF path so the model sees it.
  - [ ] Test: `./err.png` yields an image block and keeps the text.
- **US-3.** As a CLI user, I see why a file was not attached.
  - [ ] Test: outside path, symbolic-link escape, oversized file, binary file, and missing file each yield one warning and no block.
- **US-4.** As a host developer, I start a Turn with a ref and the record stays small.
  - [ ] Test: `Source` JSON round-trips a ref; `Validate` rejects a ref with any other location.
  - [ ] Test: replay and `Project` make no `Resolve` call.
- **US-5.** As a host developer, I revoke a file and the next model call stops.
  - [ ] Tests: no resolver, resolver error, size mismatch, digest mismatch, and oversized ref each stop the Turn before a model intent.
  - [ ] Test: the same ref twice in one request resolves twice. Each call checks access.
  - [ ] Test: a paused Turn calls `Resolve` again on resume.
  - [ ] Test: a provider that rejects the media type returns its normal `Prepare` error.
  - [ ] Test: a ref in a tool result works like a ref in input.

## Decisions

1. **The CLI stores attachments inline in slice 1, not by ref.** Reason: a ref needs a resolver, an attachment store, and a file that cannot change. The CLI has none of these. It reads the file at send time and the bytes enter the session file, capped by `MaxTotalBytes`. Slice 2 moves them to a content-addressed store under the workspace state directory and uses `Ref`. This also proves the API with a second host.
2. **`Runner.Content` is a Runner field and receives the run context and Turn ID.** Reason: the host already sets the context per `Run` call, so one shared Runner cannot resolve under the wrong tenant unless the host passes the wrong context. The opaque ID carries no authority.
3. **The resolver returns bytes only.** Reason: Dashi can verify bytes. It cannot verify a signed URL or a provider file ID. A later contract can add those with explicit attestations.
4. **A ref failure is a terminal failed stop in slice 1.** Reason: this matches every other pre-dispatch failure. Resume after the host restores the same bytes is later work.
5. **Size limits check the declared `Size` before any I/O.** Reason: a host that stores a 2 GiB file is rejected without a read. The digest check catches a lie about `Size`. The host must bound its own read.
6. **`mention` is a library package, not CLI code.** Reason: it uses only stdlib and `content`, and other terminal hosts need it. CLI colors, autocomplete, and labels stay in `cmd/dashi`.
7. **Text files become document blocks.** Reason: the Anthropic and OpenAI providers both accept `text/plain` documents, and the record keeps the file path as a title. Inline XML in the prompt is not needed.
8. **Skipped mentions warn; they do not fail.** Reason: the user may have typed `@` on purpose in prose. The zero-value rule says warn and adjust.
9. **Dive's path check is not copied.** Dive tests the workspace with a string prefix and does not resolve symbolic links. A sibling directory named `repo-evil` passes a check for `repo`. The new helper compares resolved paths by path element.

## Slices

**Slice 1 (ships Tuesday night).** Order matters. If time runs short, cut from the bottom.

1. `content/mention` with tests and a doc comment. The CLI track needs it first.
2. `content.Ref`, `Source.Ref`, validation, clone, JSON tests.
3. `ContentResolver`, `Runner.Content`, limits, hydration before `Prepare`, failure tests.
4. Example `examples/attachment-refs` (host storage, tenant check, retention duty).
5. Guide `docs/guides/attachments.md` and the roadmap update.

**Later.** CLI store by ref (slice 2). Files dropped from outside the workspace with explicit consent. Resume after restored bytes. Resolver results that are signed URLs or provider file IDs. Resize or compress images. Streaming resolver for very large documents. Audio and video mentions.

## Out of scope

Dashi artifact storage or upload endpoints. Automatic externalization of inline content. Fetching a URL. A resource browser. A history store; caller-owned `History` is unchanged. Autocomplete for `@` (a CLI screen feature).

## Technical notes

- The hydrated input must reach `Client.Prepare`, but `runner.go` compares `snapshot.Input` with `input` after `Prepare`. Compare against the hydrated input. Keep the ref-bearing input for `BeforeModel`, facts, and inspection.
- `llm.RequestSnapshot` holds bytes in memory. The record must not store it.
- Hydrate on a clone. `Source.Clone` already detaches `Data`.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| The `snapshot.Input` equality check fails on hydrated input | Every ref Turn fails | Add a test first; compare against the hydrated copy |
| A model call with 64 MiB of bytes exceeds provider limits | Provider error | The provider's `Prepare` error passes through unchanged; document the limits |
| A host cache skips the access check | Revoked content still reaches the model | The guide says the resolver runs on every call; Dashi caches nothing |
| Slice 1 runs late | Demo lacks mentions | Item 1 ships alone; items 2 to 5 follow |

## Open questions

- Should `Resolve` receive the step or attempt number? The Turn ID suffices for tenant checks. Add it only if a host needs it.
