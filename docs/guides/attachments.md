# Attachments

A model can read a file that a person or a host attaches. Dashi has two
tools for this. A host that stores uploads names them with a `content.Ref`.
A terminal front end turns `@file` words into content with `content/mention`.

## CLI selections

In the full-screen and inline editors, type `explain @notes.txt` and press
Enter. The first Enter selects the file and shows a numbered preview. Check
its path, type, size, and any outside-workspace notice. Press Enter again to
send the unchanged draft. Selection reads the file once; later source changes
or deletion do not change the snapshot.

Use `/attach PATH` to select a file before composing, `/attachments` to inspect
selections, and `/detach N` to remove one. A failed selection stays visible and
blocks submission until you correct or remove it. Detach removes the matching
`@path` spans too. Names can use quotes or escaped spaces. A line suffix such
as `@main.go:12` selects the whole file and keeps the suffix in the prompt.

Plain line mode uses the same preview: repeat the prompt, or enter a blank
line, to confirm. Removing the last selection keeps the request text; a blank
line still sends that retained request. Print mode needs no second Enter:

```sh
dashi -p 'Compare @notes.txt with the screenshot' --attach screen.png
```

Repeat `--attach` for more files; flags precede prompt mentions in selection
order. The shell handles flag-path quotes and escapes; Dashi uses each flag
argument literally. All selections must succeed before print mode starts work. Diagnostics
go to stderr; the answer goes to stdout.

In fullscreen and inline mode, a terminal can deliver a saved-file drop as
bracketed paste containing only absolute, `~/`, or local `file://` paths.
Quoted or escaped spaces and multiple paths are supported. Dashi consumes
that path segment, preserves the draft and cursor, and shows numbered selections.
Once ready, ordinary Enter sends; loading or error entries block submission.
Mixed prose stays text. For unbracketed terminals or plain mode, use `/attach`
or `@path`. Clipboard image bytes require saving a file first; SSH paths must
exist on Dashi's host and transfer no desktop bytes.

Recognition accepts at most 64 KiB of UTF-8 paste and 64 candidate paths,
with at most 64 resulting draft entries. Count overflow selects nothing and
preserves the draft; use `/attachments`, `/detach N`, or a smaller batch.
Larger paste remains ordinary text with a notice to undo it and select fewer
files or use `/attach`. Existing file-byte limits still apply.

The CLI accepts UTF-8 text without NUL bytes, PNG, JPEG, GIF, WebP, and PDF.
Defaults are 256 KiB per text file, 3.75 MB per image, 10 MB per PDF, and 32 MB
of encoded content per submission. It checks the opened descriptor is a regular
file, rejects special files, and checks the bytes against the claimed format.
Selecting outside the workspace grants no tool access there.

Native image or document content can make the active context estimate
unavailable. The CLI shows that warning and leaves acceptance to the provider.
Cost limits still apply; an unavailable estimate does not prove the request fits.

Before an ordinary Start, the CLI publishes private immutable snapshots in
that session. Turn records contain only IDs, digests, sizes, and basenames.
Resume resolves those snapshots under the same session, checks their size and
digest, and never falls back to the source path. Secure local storage currently
requires macOS or Linux; other platforms can still open attachment-free work.

Only acknowledgement of the matching ordinary Start consumes the draft.
Compaction and Continue do not consume independent selections. A rejected
Start keeps a retryable draft. An uncertain Start freezes it until recorded
facts reconcile the outcome. Attachments cannot become a steer, `/queue`
text, or a delayed send during a running Turn. Exit and session changes warn
before losing unsent selections. Published snapshots, including orphans, are
retained; `/discard` does not delete them.

## Stored files by reference

Without a reference, the bytes of an upload enter the Turn record. They grow
every checkpoint and log. With a reference, the record holds an ID, a SHA-256
digest, and a size. The host keeps the bytes.

```go
ref := content.Ref{ID: "upl_1", SHA256: sum, Size: 812}
source := content.Source{Ref: &ref, MIMEType: "image/png"}
out, err := runner.Run(ctx, turn, dashi.Start{Input: []llm.Message{
	llm.UserMessage(llm.Text("What is wrong here?"), llm.Image(source)),
}})
```

Rules for a reference:

- `ID` is opaque. It is not a URL or a path, and it gives no access.
- `MIMEType` is required, because Dashi cannot detect the type without bytes.
- A reference excludes `Data`, `URL`, `ID`, `URI`, `Origin`, and `ExpiresAt`.
- A reference can appear in a start message, in a message from a hook or a
  steer, and in a tool result.

Set `Runner.ContentResolver` to turn a reference into bytes:

```go
type ContentResolver interface {
	Resolve(ctx context.Context, turnID string, ref content.Ref) ([]byte, error)
}
```

Before each model request, Dashi does these steps on a copy of the input:

1. It compares the declared size of each reference with `Runner.ContentLimits`.
   This step does no I/O.
2. It calls `Resolve` with the context of the `Run` or `Stream` call. Put the
   tenant and session in that context. The resolver must check that the turn
   can read the reference. Check the tenant or session, not only the turn.
3. It checks that the length and the SHA-256 of the result match the reference.
4. It puts the bytes in the copy and prepares the request.

Dashi keeps no cache. It calls `Resolve` again on every model call, every
retry, and every resume. A reference that the host revoked stops the turn at
the next call. `Project`, replay, and inspection never call the resolver.

`BeforeModel` hooks see the reference. `BeforeSend` hooks see the prepared
request with bytes. `ModelFact.RequestDigest` covers those bytes. The
resolver must bound its own read, because Dashi checks the length only after
the read.

### Limits

`ContentLimits.MaxRefBytes` is 32 MiB by default. `MaxTotalBytes` is 64 MiB
by default and counts each use of a reference. Inline data is not counted. A
negative value turns a limit off. A provider can have a lower limit and can
reject the request. Its normal preparation error then ends the turn.

### Failures

A problem with a reference ends the turn as failed. No model intent is
recorded. `Stop.Reason` holds a stable code, and `Stop.RefID` names the
reference. A host can branch on the code:

| Code | Meaning |
| --- | --- |
| `StopReasonContentNoResolver` | `Runner.ContentResolver` is nil. |
| `StopReasonContentUnavailable` | The resolver returned an error. Denied and deleted files look the same. |
| `StopReasonContentTooLarge` | The declared size is over a limit. |
| `StopReasonContentSizeMismatch` | The resolver returned a different number of bytes. |
| `StopReasonContentDigestMismatch` | The bytes do not match the SHA-256. |

The stop never contains bytes or the resolver's error text. Dashi never drops
the block or falls back to another location.

The stop is not terminal. To recover, fix the cause, then call `Run` with
`Continue` on the same turn. Restore the bytes, grant access, raise
`ContentLimits`, or set a resolver. `Continue` calls `Resolve` again and
resumes the turn where it stopped. A turn that will not recover can stay
failed.

### References in earlier messages

A reference in `Start.History` or in a tool result from an earlier turn is
resolved again on every later turn. It resolves under the ID of the current
turn, not the turn that created it. The resolver must not check access by the
turn ID alone. Check the tenant or session that it reads from `ctx`. Each use
also counts toward `MaxTotalBytes`.

## Files from a terminal: `content/mention`

`mention.Scan` finds explicit mention spans without reading files.
`mention.ReadFileContext` makes one bounded, cancelable selection; `ReadFile`
is its context-free wrapper. A front end can show a preview before dispatch.

The legacy `mention.Expand` reads a message typed by a person and attaches files. The
person picks each file, so there is no workspace boundary. A model tool that
reads files must enforce its own boundary.

```go
res := mention.Expand("explain @internal/wake/wake.go", mention.Options{})
parts := []llm.Content{llm.Text(res.Text)}
for _, b := range res.Blocks {
	parts = append(parts, llm.Block(b))
}
for _, w := range res.Warnings {
	fmt.Println(w) // Show why a file was skipped.
}
```

| Input | Result |
| --- | --- |
| `@src/main.go` | A text document block. The text becomes `[file: src/main.go]`. |
| `./err.png`, `spec.pdf` | An image or PDF block. The path stays in the text. |
| `~/Desktop/Screen\ Shot.png`, `'/tmp/a b.png'`, `file:///tmp/a.png` | The forms a terminal inserts when a person drops a file. |
| `@main.go:12` | The file `main.go`. The text becomes `[file: main.go]:12`. |
| `me@example.com`, `main.go:12` | No change. |

A word without `@` attaches only when it names an existing image or PDF.
Images are PNG, JPEG, GIF, and WebP. The default limits are 3.75 MB for an image (Anthropic counts the base64
text against a 5 MB limit),
10 MB for a PDF, 256 KB for text, and 32 MB for one message. A skipped file
gives a warning and never fails the message. The same file attaches once.

The blocks hold the bytes inline. A front end that keeps sessions can store
them in the session file. Storing them by `content.Ref` needs a resolver and
a file that cannot change.
