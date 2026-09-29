# PRD: attachments — stored references and `@file` mentions

| Field | Value |
| --- | --- |
| Status | Implemented, 2026-09-28: PR 1 (Part B) and PR 2 (Part A) |
| Roadmap | [Item 16](../remaining-work.md), size M / high difficulty, plus the CLI attachment helper |
| Evidence | [Source review](16-attachment-references-research.md), [history storage](../history-storage.md), Dive CLI `attachments.go` |
| Demo | Wednesday 2026-09-30. PR 1 (Part B) ships by Tuesday night. PR 2 (Part A) follows on its own branch. |

## Problem

Two users cannot attach files today.

1. **The `dashi` CLI user** types `explain @internal/wake/wake.go` or `what is wrong in ./err.png`. Dashi has no code that turns those words into content the model can read. Dive's CLI has this code, and the Dashi CLI needs it back.
2. **A Dashi host** already stores uploads. To attach one, the host must copy its bytes into `Start.Input` or a tool result. The bytes enter the Turn record and grow every checkpoint and log. A URL or provider file ID cannot fix this: Dashi cannot check who may read it, or prove which bytes it names.

If we do nothing, the CLI has no file mentions, and each host writes its own conversion layer.

## Goals and metrics

- A CLI user attaches a workspace file or image with one word. Time to attach: no extra step.
- A host records a stored attachment as an ID, a SHA-256 digest, and a size. The Turn record holds no attachment bytes. Test: a whole Turn with a 5 MiB ref serializes to under 4 KiB, and the size does not depend on the file.
- Each model call re-checks access and bytes. Test: revoked, missing, changed, and oversized content each stop the Turn before any model request.
- A CLI user drags a screenshot from the Desktop into the terminal and it attaches. No extra step.
- Guardrails: inline `Source` behavior does not change. Every Turn runs the model-request path that Part A changes, so that path gets the most tests. `Project`, replay, and inspection never call the host.

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
//   ContentResolver ContentResolver // nil: a Turn that holds a Ref stops before its first model call
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
2. Call `ContentResolver.Resolve` with the run context. The host uses that context to check tenant and session.
3. Check that the returned length equals `Size` and the SHA-256 equals `SHA256`.
4. Put the bytes in `Source.Data`, then call `Prepare`.

`ModelFact.RequestDigest` covers the request body with the bytes. Facts store no bytes. Every retry and resume runs steps 1 to 4 again. Dashi keeps no cache across calls, so a revoked ref stops delivery at the next call.

### Failure behavior

A ref problem stops the Turn as failed before Dashi records a model intent. `Stop.Reason` holds one exported code and `Stop.RefID` names the ref: `StopReasonContentNoResolver`, `StopReasonContentUnavailable` (any resolver error, including denied and deleted), `StopReasonContentTooLarge`, `StopReasonContentSizeMismatch`, or `StopReasonContentDigestMismatch`. A host branches on the code. The stop never includes bytes. The stop is not terminal: after the host fixes the cause, `Continue` calls the resolver again and resumes the Turn. Dashi never drops, replaces, or falls back to another location. This is a known failure, not an uncertain one.

## Part B: `@file` mentions (helper for the CLI)

A new package, `content/mention`. It uses only the standard library and `content`. It does not depend on the `workspace` package (Decision 8). Policy comes from `Options`.

```go
type Options struct {
    Root          string // base for relative paths; zero means the current directory
    MaxImageBytes int64  // per image; zero means 3.75 MB (Decision 4)
    MaxFileBytes  int64  // per PDF; zero means 10 MiB
    MaxTextBytes  int64  // per text file; zero means 256 KiB
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
| `look at ~/Desktop/Screen\ Shot\ 1.png` | Attaches the image. A terminal drop inserts this form. |
| `look at '/tmp/a b.png'` or `file:///tmp/a.png` | Attaches the image. Quotes and `file://` URLs work. |
| `read @big.png` (8 MB) | Warning: `Skipped @big.png: 8 MB is over the 3.75 MB image limit.` |
| `read @app.bin` | Warning: `Skipped @app.bin: not text, an image, or a PDF.` |

Rules:

- A mention is `@path` or `@"path with spaces"`. Trailing `?!,.:;)]}` is not part of the path.
- The user chooses every attachment. It is not a model action, so no workspace boundary applies. A relative path resolves against `Root`. An absolute path, a `~` path, and a `file://` URL attach from anywhere the user can read. The model's tools keep the workspace boundary.
- The parser reads shell quotes and backslash escapes the way a terminal inserts a dropped path.
- A bare word (no `@`) attaches only when it names an existing image or PDF file. A bare text path stays prose, so a pasted stack trace is safe.
- Image types: PNG, JPEG, GIF, WebP. PDF is a document. A text file is a document with `text/plain` and its path as title. Any other file, or invalid UTF-8, is skipped with a warning.
- A skipped mention never fails the send. The CLI shows the warnings and sends the message.
- The same file mentioned twice attaches once.

## User stories

- **US-1.** As a CLI user, I type `@path` so the model reads that file.
  - [ ] Test: a text file becomes a titled `text/plain` document block.
  - [ ] Test: `me@example.com` is not a mention.
  - [ ] Test: trailing punctuation, a quoted path with spaces, and a duplicate mention behave as the rules say.
- **US-2.** As a CLI user, I name an image or PDF anywhere on disk so the model sees it.
  - [ ] One test per path form: `./err.png`, absolute, `~`, backslash-escaped, quoted, and `file://`.
  - [ ] Test: a bare text path stays prose.
- **US-3.** As a CLI user, I see why a file was not attached.
  - [ ] Test: missing file, directory, binary file, oversized image, oversized text, and the total limit each yield one warning and no block.
- **US-4.** As a CLI user, my attachment reaches the model.
  - [ ] Test: `Expand` output passes `Validate` and `Prepare` for the default OpenAI model and for Anthropic, for text, image, and PDF.
- **US-5.** As a host developer, I start a Turn with a ref and the record stays small.
  - [ ] Test: `Source` JSON round-trips a ref; `Validate` rejects a ref with any other location.
  - [ ] Test: replay and `Project` make no `Resolve` call.
- **US-6.** As a host developer, I revoke a file and the next model call stops.
  - [ ] Tests: no resolver, resolver error, size mismatch, digest mismatch, and oversized ref each stop the Turn before a model intent.
  - [ ] Test: the same ref twice in one request resolves twice. Each call checks access.
  - [ ] Test: a paused Turn calls `Resolve` again on resume.
  - [ ] Test: a provider that rejects the media type returns its normal `Prepare` error.
  - [ ] Test: a ref in a tool result works like a ref in input.
- **US-7.** As any Dashi user, a Turn with no ref runs exactly as before.
  - [ ] The existing runner, hook, budget, steering, and provider suites pass unchanged.
  - [ ] Tests: `BeforeModel` sees ref-bearing input; `BeforeSend` sees hydrated input; `RequestDigest` covers hydrated bytes; a hook that adds a ref-bearing block gets hydrated; streaming and non-streaming runs both hydrate.

## Decisions

1. **The CLI stores attachments inline, not by ref.** Reason: a ref needs a resolver, an attachment store, and a file that cannot change. The CLI has none of these. It reads the file at send time and the bytes enter the session file, capped by `MaxTotalBytes`. A later change can move them to a content-addressed store and use `Ref`.
2. **`Runner.ContentResolver` is a Runner field and receives the run context and Turn ID.** Reason: the host sets the context on every `Run` call, so a shared Runner cannot resolve under the wrong tenant unless the host passes the wrong context. The opaque ID carries no authority. The name avoids `Content`, which already means blocks (review 7, accepted).
3. **The resolver returns bytes only.** Reason: Dashi can verify bytes. It cannot verify a signed URL or a provider file ID. This replaces the text in `history-storage.md` that allowed a signed URL or file ID, a `content_unavailable` reason, and a substitution policy. PR 2 updates that document so the design has one answer (review 6, accepted).
4. **The image default is 3.75 MB of file size.** Reason: Anthropic rejects an image above 5 MB, and it counts the base64 text, which is one third larger than the file. The default is thus 5 MB x 3/4. A file that passes is accepted by every main provider (OpenAI and Google accept more). An image over the limit gets a warning and is not sent, so the provider never fails the whole message. Dashi does not resize. A host can set `MaxImageBytes` higher for a provider that has a higher limit (review 4, accepted; implementation review 1, accepted).
5. **A ref failure is a failed stop that `Continue` can resume.** Reason: the failure happens before a model request, like a hook failure. The stop is not terminal, so a host can restore the bytes or grant access, then call `Continue`. The runner asks the resolver again and the Turn goes on. This works by default and needs no new command. A host that will not recover can leave the Turn failed. The size limit checks the declared `Size` before any I/O; the digest check catches a wrong `Size`. The host must bound its own read.
6. **Text files become document blocks.** Reason: the Anthropic and OpenAI providers both accept `text/plain` documents, and the record keeps the path as a title.
7. **Skipped mentions warn; they do not fail.** Reason: the user may have typed `@` on purpose in prose. Zero-value rules say warn and adjust.
8. **User attachments have no workspace boundary, and `mention` does not use the `workspace` package.** Reason: the user picks the file and can already read it, so a boundary protects nothing (review 1, accepted). The model's tools keep their boundary in `workspace`. This removes the duplicate path check and any signature dependency on the codetools track (review 2, accepted). Symbolic links need no special handling for the same reason. Dive's string-prefix check is not copied.
9. **Delivery splits in two PRs. Both stay in scope.** PR 1 is Part B and ships Tuesday night. PR 2 is Part A, on a second branch cut from the PR 1 branch after PR 1 opens. Reason: Part A changes the request path that every Turn runs. It gets its own review and the tests in US-6 and US-7 (review 3, accepted in part: no deferral, separate PR).
10. **Review 5 is accepted.** The stories above add the missing parsing, limit, and `Prepare` tests.

11. **Implementation review of PR 1 (2026-09-28).**
    - The provider tests use the default capability policy and check that the text document, the image, and the PDF are in each request body (accepted).
    - `@main.go:42` and `@main.go:42-50` attach `main.go`. The line reference stays in the text (accepted).
    - A mention in backticks or quotes attaches, for example `` `@a.go` `` (accepted).
    - The duplicate `scanWords` comment is deleted (accepted).
    - A warning prints sizes with up to two decimals, rounded up, so a file that is over the limit never prints as equal to it (accepted).
    - `design/history-storage.md` changed in PR 1 (commit 02161a1), not in PR 2. The PR 1 description says so.

12. **Implementation calls (2026-09-28).** A `Ref` needs a positive `Size`, like inline data. `llm.NewRequest` rejects an input that still holds a `Ref`, so no provider can send an unresolved reference. The stop carries a code and the reference ID, and omits the resolver's error text (see decision 13). Mention labels are `[file: path]` and `[image: path]`. Size limits use binary units (1 MB is 1 MiB) and print as MB and KB. A skipped mention prints the text as typed, for example `@big.png`.

13. **Implementation review of PR 2 (2026-09-28).**
    - The stop codes are exported: `StopReasonContentNoResolver`, `...Unavailable`, `...TooLarge`, `...SizeMismatch`, and `...DigestMismatch`. `Stop.RefID` holds the reference ID. The message text `content reference "ID": cause` is removed. The codes are reserved, so a hook cannot fake them (accepted).
    - `Continue` resolves again and resumes. The guide and decision 5 now say so. Tests cover the failure, the fix, and the resume (accepted; the reviewer's finding 2 was resolved toward the code, not away from it).
    - A ref in history or an earlier tool result resolves under the ID of the current turn. Reason: Dashi keeps no record of which turn first held the ref, and a per-turn cache would break the rule that a revoked ref stops at the next call. The guide tells hosts to check tenant or session from `ctx`. A test fixes this behavior (decided and documented).
    - `Block.Sources` and `Content.Sources` are removed from the public API. They live in `internal/contentsrc`, and `Input.HasRef` stays public. No caller outside the module can alias a recorded block now (accepted).
    - Media edits (OpenAI images, Grok imagine) reject a ref with an error that names it: `content reference "ID" cannot be an edit image; pass the bytes`. `Source.Validate` still accepts a ref, because a message needs it (accepted).
    - A test runs a whole Turn with a 5 MiB ref and measures `json.Marshal(out.Turn)`. It is about 1.9 KB, all of it steps, and it does not grow with the file. The PRD metric now reads "under 4 KiB for a whole Turn" (accepted; the metric changed because a Turn has steps as well as the ref).

## Delivery

**PR 1: mention helper.** `content/mention`, its tests (US-1 to US-4), and a doc comment. Branch `content/attachment-refs`.

**PR 2: content references.** Branch cut from PR 1. `content.Ref`, `Source.Ref`, validation, `ContentResolver`, limits, hydration before `Prepare`, tests (US-5 to US-7), example `examples/attachment-refs`, guide `docs/guides/attachments.md`, the `history-storage.md` update, and the roadmap update.

**Later.** CLI store by ref. Resolver results that are signed URLs or provider file IDs. Image resize. Streaming resolver for large documents. Audio and video mentions.

## Out of scope

Dashi artifact storage or upload endpoints. Automatic externalization of inline content. Fetching a URL. A resource browser. A history store; caller-owned `History` is unchanged. `@` autocomplete (a CLI screen feature).

## Technical notes

- `runner.go` compares `snapshot.Input` with `input` after `Prepare`. Compare against the hydrated input. Keep the ref-bearing input for `BeforeModel`, facts, and inspection.
- `llm.RequestSnapshot` holds bytes in memory. The record must not store it.
- Hydrate on a clone. `Source.Clone` already detaches `Data`.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| The `snapshot.Input` equality check fails on hydrated input | Every ref Turn fails | Write that test first; compare against the hydrated copy |
| Part A breaks the ordinary request path | Every Turn fails | Separate PR; US-7 tests; no-ref path takes no new branch |
| A host cache skips the access check | Revoked content still reaches the model | The guide says the resolver runs on every call; Dashi caches nothing |
| A request with 64 MiB of bytes exceeds a provider limit | Provider error | The provider's `Prepare` error passes through; the guide lists limits |
| The user attaches a secret file by mistake | The model reads it | The screen lists every attachment before the send |

## Open questions

- Should `Resolve` receive the step or attempt number? The Turn ID suffices for tenant checks. Add it only if a host needs it.
