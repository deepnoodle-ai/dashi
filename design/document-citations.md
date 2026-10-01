# Documents, citations, and content construction

**Status:** Accepted · **Date:** 2026-09-28 · **Addresses:**
[gaps/F](gaps/F-example-ports.md) 9.1, 9.2, 9.3, 9.6, 9.7, 9.8, 5.1

## Problem

Dive's `citations_example` asked Claude to cite a plain-text document and
printed each citation. Dashi could not: nothing requested citations, inline text
documents were sent as base64 (Anthropic accepts that only for PDFs) and failed
at send, and decoding read `title` where document citations carry
`document_title`. Around this, content took three nested constructors, inline
bytes needed a hand-detected MIME type, and structured output needed a
hand-built schema.

## Decisions

### Citations are a request setting (9.1)

```go
// llm.Settings
Citations bool `json:"citations,omitempty"`
```

Set through `Agent.Settings` or `Input.Settings`. Anthropic requires citations
on all documents in a request or none, and no other supported API has a
per-document switch, so a request-level setting is the honest shape; a
per-document flag would invite mixed requests Anthropic rejects.

- **Anthropic** marks every document in the request, including documents in tool
  results, with `"citations": {"enabled": true}`.
- **Other APIs** declare the capability unsupported. `Fit` omits the setting
  through the existing path: an `omit` adjustment with a `setting_omitted`
  warning, or an Unsupported error under `CapabilityStrict`. It does so only
  when the request contains a document; otherwise the setting has no effect and
  nothing is warned.

Citations cost a little more input and split the reply into several text
blocks, one per cited passage. The reply accessor must join adjacent text
blocks without inserting separators.

Decoded document citations carry their title and a location:

```go
type CitationLocation struct {
	RequestDocumentIndex int          // see below
	Unit                 LocationUnit // LocationPage or LocationChar
	Start, End           int          // half-open; pages from 1, chars from 0
}
```

`RequestDocumentIndex` counts the document blocks of the request that produced
the citation, in the order sent: every message, then each tool result's blocks.
It is not stable across calls, since next-call-only context and new tool results
change the order. Translating it to caller coordinates is not clean: provider
tool results such as web fetch can contain documents that the provider counts and
Dashi does not author. Callers who need a stable key should give documents
titles and use `Citation.Title`. Search result citations map their `source` to
`Citation.URL`; other citation kinds stay in `Raw`.

The raw-field escape hatch is unchanged: replay data is for content a provider
produced, and its origin check keeps one destination's data from reaching
another. Provider options on caller-authored blocks (prompt caching, a
document's `context`) need their own design.

### Media types are checked and detected at prepare (9.2, 9.8)

`Source.MIMEType` becomes optional for inline bytes. When it is empty,
preparation fills it on its working copy with `content.DetectMIMEType(data,
name)`: a fixed extension table (pdf, txt, md, csv, html, json, png, jpg, jpeg,
gif, webp) for a document title, then `http.DetectContentType` with parameters
stripped. The extension table is fixed; sniffed results could change with a Go
release, so the recorded change's reason, which names the type, is the record of
what a past request sent. Each position is recorded as a `detect_mime_type`
content change on every call; the caller's input and the recorded turn keep the
empty value. One change covers a whole tool result, so two documents in one
result do not collide. When detection finds only `application/octet-stream`, a
second change, `unknown_mime_type`, becomes a `mime_type_unknown` warning. Media
preparation (OpenAI Images, Grok Imagine, Gemini media) detects the same way on
its own copy through `Source.WithDetectedMIMEType`, and rejects a type its API
does not accept.

Anthropic then encodes inline documents by type: `application/pdf` as `base64`,
any `text/*` type, and JSON, XML, and YAML, as a `text` source with `media_type:
text/plain` (valid UTF-8 required), and anything else is an Unsupported error at
prepare. A URL document with a non-PDF `MIMEType` and an inline image outside
JPEG, PNG, GIF, and WebP fail at prepare too. Other APIs send what they get;
their accepted types are not documented precisely enough to check. Responses no
longer names every inline file `document.pdf`: a title without an extension
gains one from the media type.

### Flat constructors (9.3, 9.7)

```go
func Image(source content.Source) Content                // llm
func Document(source content.Source, title string) Content // llm
func DocumentBlock(source Source, title string) Block     // content
```

Audio and video stay on `llm.Block`.

```go
llm.UserMessage(
	llm.Document(content.Source{Data: notes}, "notes.txt"),
	llm.Text("What color is the sky?"),
)
```

### Structured output helpers (9.6)

```go
func MustOutputSchema[T any]() json.RawMessage
func DecodeOutput[T any](o RunResult) (T, error)
```

`MustOutputSchema` generates the schema with `tool.Func`'s rules and runs the
same compile check as agent validation, so a non-object `T` panics on first use,
like `regexp.MustCompile`. `DecodeOutput` decodes a value Dashi already
validated against the schema before recording it; when there is none it returns
an error wrapping `ErrNoOutput` that names why (the stop kind and reason, or a
completed turn without a schema). Both live in the root package, where
`Agent.OutputSchema` is.

### Typed context origin (5.1)

`ContextItem.Origin` becomes `ContextOrigin` with `ContextUser`,
`ContextRuntime`, `ContextTool`, and `ContextOperator`. `ContextRuntime`
replaces the undefined `"library"`: context the application's runtime adds on
its own, such as reminders and environment state, at user trust. An empty origin
means `ContextUser`, the least trusted. This stays separate from `StartOrigin`:
the sets differ, and one type would let each field accept the other's values.

## Recorded format

New optional fields `settings.citations` and `citation.location`; content change
actions `detect_mime_type` and `unknown_mime_type`; warning code
`mime_type_unknown`; inline sources may omit `mime_type`; context origin
`library` becomes `runtime`. No migration: Dashi has no users.

## Tests

- Anthropic wire: text and PDF sources, citations on every document including
  tool results, prepare-time media errors, and decoding of page and character
  citations from the documented response shapes.
- Preparation: citations omitted with a warning on other APIs only when
  documents are present, strict errors, MIME detection and its warning, and one
  change per tool result.
- Helpers: constructors, `DetectMIMEType`, `MustOutputSchema`, `DecodeOutput`,
  and context origins; the three examples run live.
