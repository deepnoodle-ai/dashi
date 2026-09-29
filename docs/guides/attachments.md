# Attachments

A model can read a file that a person or a host attaches. Dashi has two
tools for this. A host that stores uploads names them with a `content.Ref`.
A terminal front end turns `@file` words into content with `content/mention`.

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
   can read the reference.
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
recorded. The stop reason names the reference and one cause:

| Cause | Meaning |
| --- | --- |
| `no_resolver` | `Runner.ContentResolver` is nil. |
| `unavailable` | The resolver returned an error. Denied and deleted files look the same. |
| `too_large` | The declared size is over a limit. |
| `size_mismatch` | The resolver returned a different number of bytes. |
| `digest_mismatch` | The bytes do not match the SHA-256. |

The reason never contains bytes or the resolver's error text. Dashi never
drops the block or falls back to another location. After the host restores
the same bytes, it can start a new turn.

## Files from a terminal: `content/mention`

`mention.Expand` reads a message typed by a person and attaches files. The
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
| `me@example.com`, `main.go:12` | No change. |

A word without `@` attaches only when it names an existing image or PDF.
Images are PNG, JPEG, GIF, and WebP. The default limits are 5 MB for an image,
10 MB for a PDF, 256 KB for text, and 32 MB for one message. A skipped file
gives a warning and never fails the message. The same file attaches once.

The blocks hold the bytes inline. A front end that keeps sessions can store
them in the session file. Storing them by `content.Ref` needs a resolver and
a file that cannot change.
