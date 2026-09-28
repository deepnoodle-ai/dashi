# Documents, citations, and content construction

**Status:** Proposal · **Date:** 2026-09-28 · **Addresses:**
[gaps/F](gaps/F-example-ports.md) 9.1, 9.2, 9.3, 9.6, 9.7, 9.8, 5.1

## Problem

Dive's `citations_example` asked Claude to cite a plain-text document and
printed each citation. Dashi cannot do this today:

- `content.Document` has no way to request citations, and Anthropic returns
  document citations only when a document enables them. Supplying the flag
  through replay data needs the destination's private scope hash, so a caller
  cannot do it either.
- Inline documents are always sent to Anthropic as base64, which it accepts only
  for PDFs. A text document fails at send, not at prepare.
- Decoding reads a citation's `title`, but document citations carry
  `document_title`, so their titles come back empty. Where a citation points
  (page or character range) is only in `Raw`.

Around this, building content takes too many layers
(`llm.UserMessage(llm.Block(content.ImageBlock(src)))`), inline bytes need a
hand-detected MIME type, and structured output needs a hand-built schema.

## Proposal

### Requesting citations (9.1)

```go
// content
type Document struct {
	Source Source `json:"source"`
	Title  string `json:"title,omitempty"`
	// Citations asks the model to cite passages of this document. APIs that
	// cannot honor it send the document without it and warn.
	Citations bool `json:"citations,omitempty"`
}

// Location says where a document citation points. Ranges are half-open:
// [Start, End). Pages count from 1; characters and blocks from 0.
type CitationLocation struct {
	Document int          `json:"document"` // index among the request's documents
	Unit     LocationUnit `json:"unit"`     // "page", "char", or "block"
	Start    int          `json:"start"`
	End      int          `json:"end"`
}

type Citation struct {
	URL, Title, Text string
	Location *CitationLocation // new; nil for web and URL citations
	Origin   Origin
	Raw      json.RawMessage
}
```

**Anthropic** sends `"citations": {"enabled": true}` on each requesting document
and decodes `document_title`, `cited_text`, and the location of `char_location`,
`page_location`, and `content_block_location` citations. Anthropic requires
citations on all documents in a request or none. When some documents request
them and others do not (common once a conversation adds a second document),
Dashi enables them on all and records each promoted document as a content change
(`enable_citations`), without a warning, since nothing the caller asked for is
lost.

**Every other API** (Responses, Chat Completions, Gemini) drops the request.
`providercontent.Adapt` does this for all of them, controlled by a new
`AdaptOptions.DocumentCitations` that only Anthropic sets. Each dropped request
is a content change (`omit_citations`) at the document's original position, and
a warning. Under `CapabilityStrict` it is an Unsupported error instead.

**Warnings from content changes.** Today warnings come only from setting
adjustments. `ContentChange` gains a `Warning()` method like
`Adjustment.Warning()`; `NewRequest` appends the warnings of content changes
that lose something the caller asked for (so far only `omit_citations`, code
`citations_omitted`). They reach `Request.Warnings()` and `Result.Warnings`
through the existing path. `Warning.Field` stays empty; the message names the
message and content position.

**Raw-field escape hatch.** Not changed. Replay data (`Content.Replay`) is for
content a provider produced, and its origin check is what stops a record from
one destination leaking into another. Exposing the scope hash so callers can
author replay would weaken that. Provider options on caller-authored blocks,
such as Anthropic prompt caching (`cache_control`, which Dive's example also
used) and a document's `context` field, deserve their own design; not in this
change.

Citations and Anthropic's native structured-output format cannot be combined,
but Dashi's structured output uses a reserved tool, so the two work together.

### Plain-text documents and media types at prepare (9.2)

Anthropic encodes inline documents by media type:

| Inline media type                          | Anthropic source                     |
| ------------------------------------------ | ------------------------------------ |
| `application/pdf`                          | `base64` (unchanged)                 |
| `text/*` (plain, markdown, csv, html, ...) | `text` with `media_type: text/plain` |
| anything else                              | Unsupported error at prepare         |

Text must be valid UTF-8, else an error at prepare. A URL document with a
non-PDF `MIMEType` set is also rejected at prepare (Anthropic fetches only
PDFs). Anthropic images get the same check against JPEG, PNG, GIF, and WebP.
Other APIs keep sending what they are given; their accepted types are not
documented precisely enough to check. Responses stops naming every inline file
`document.pdf` and derives the extension from the media type.

### MIME detection for inline bytes (9.8)

`Source.MIMEType` becomes optional for inline bytes. When it is empty,
preparation detects it (`content.DetectMIMEType(data, name)`: a document title's
file extension first, then `http.DetectContentType`, parameters stripped) on the
adapted copy and records a content change (`detect_mime_type`, reason naming the
type). The caller's input and the recorded turn keep the empty value; detection
is deterministic, so a replayed request matches. If detection finds nothing
better than `application/octet-stream`, the request warns (`mime_type_unknown`)
and sends that type, and APIs with a known list (above) fail at prepare with a
message saying to set `MIMEType`. `content.DetectMIMEType` is exported for
callers who want the value up front.

### Flat constructors (9.3, 9.7)

```go
// llm, beside Text and Block
func Image(source content.Source) Content
func Document(source content.Source, title string) Content
func CitedDocument(source content.Source, title string) Content // Citations: true

// content: DocumentBlock takes the title
func DocumentBlock(source Source, title string) Block
```

Audio and video stay on `llm.Block`; they are rarely model input. No source
constructors: `content.Source{URL: u}` and `content.Source{Data: b}` are already
short once the MIME type is optional.

```go
llm.UserMessage(
	llm.CitedDocument(content.Source{Data: data}, "notes.txt"),
	llm.Text("What color is the sky? Cite the document."),
)
```

`CitedDocument` rather than a boolean argument, which reads poorly at a call
site, or a two-line field assignment on the returned union.

### Output schema from a struct (9.6)

```go
// package dashi, new file
func MustOutputSchema[T any]() json.RawMessage
func DecodeOutput[T any](out *StructuredOutput) (T, error)
```

`MustOutputSchema` uses the generator and options `tool.Func` uses (fields are
required unless `omitempty`; `description` and `enum` tags are read; extra
properties are disallowed). It panics only when the type cannot be expressed as
a schema, a programming error found on first run, like `regexp.MustCompile`; a
non-object type is still rejected by agent validation. `DecodeOutput` returns an
error for a nil output. Both live in the root package because
`Agent.OutputSchema` does, and they leave `tool` untouched (another stream owns
it); sharing the generator between the two is a later cleanup.

```go
runner.Agent.OutputSchema = dashi.MustOutputSchema[Person]()
person, err := dashi.DecodeOutput[Person](outcome.Output)
```

### Typed context origin (5.1)

```go
type ContextOrigin string

const (
	ContextUser     ContextOrigin = "user"
	ContextLibrary  ContextOrigin = "library"
	ContextTool     ContextOrigin = "tool"
	ContextOperator ContextOrigin = "operator"
)
```

`ContextItem.Origin` changes from `string` to `ContextOrigin` (one line in
`turn.go`; constants in a new file). The names parallel `StartUser` and
`StartOperator`. [gaps/B](gaps/B-developer-experience.md) suggests one
`Authority` type for both; the two sets differ (`scheduled` versus `library` and
`tool`), and one type would let each field accept the other's values, so this
keeps two typed sets.

## Recorded format

New optional JSON fields: `document.citations`, `citation.location`. New content
change actions `enable_citations`, `omit_citations`, `detect_mime_type`, and
warning codes `citations_omitted`, `mime_type_unknown`. Inline sources may omit
`mime_type`. Context origin strings are unchanged. No migration: Dashi has no
users.

## Tests

- Anthropic wire: text source for `text/*`, base64 for PDF; citations flag;
  all-or-none promotion recorded; unsupported media types fail at prepare;
  decoding char, page, and block citations with title and location (fixtures
  from the documented response shapes).
- Adapt: `omit_citations` for Responses, Chat Completions, and Gemini, in
  messages and tool results; strict policy error; warnings on the request and
  result.
- `DetectMIMEType` table (PDF, PNG, JPEG, text, markdown by title, unknown);
  prepare records `detect_mime_type` and leaves the input unchanged.
- `llm.Image`, `Document`, `CitedDocument`; `MustOutputSchema` and
  `DecodeOutput`; `validateContext` with the constants.
- Examples: `documents` requests and prints citations (with an embedded text
  default so it shows character ranges; `-pdf` still works); `vision` drops its
  own MIME sniffing; `structured_output` uses the two helpers. Run live with a
  cheap model.

## Risks

- Promoting citations to all documents changes output format for a conversation
  whose later document asked for them. It is recorded; the alternative, failing
  or dropping, loses more.
- Relaxing `Source.Validate` means an inline source without a type is no longer
  caught at construction; preparation catches it for APIs with a known list.
- `turn.go` is shared: only the `ContextItem.Origin` type changes there.

## Files

`content/content.go`, new `content/mime.go`; `llm/llm.go`,
`llm/content_change.go`, `llm/adjustment.go`, `llm/request.go`;
`internal/providercontent/adapt.go`; `providers/anthropic/messages/messages.go`,
`providers/openai/responses/responses.go`; new `structured.go` and
`context_origin.go`; one line in `turn.go`; `state.go` (`validateContext`);
`examples/documents`, `vision`, `structured_output`;
`docs/guides/model-calls.md`.
