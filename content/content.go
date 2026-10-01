// Package content defines shared text, media, and references without model or
// tool execution dependencies. Source handling is adapted from Dive's
// llm/content.go.
package content

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Kind identifies the payload carried by a Block.
type Kind string

// Block kinds.
const (
	// KindText carries display text and optional citations.
	KindText Kind = "text"
	// KindImage carries an image source and optional processing detail.
	KindImage Kind = "image"
	// KindDocument carries a document source and optional title.
	KindDocument Kind = "document"
	// KindAudio carries an audio source without implying provider support.
	KindAudio Kind = "audio"
	// KindVideo carries a video source without implying provider support.
	KindVideo Kind = "video"
	// KindResource carries a URI without resolving the referenced content.
	KindResource Kind = "resource"
)

// Source contains exactly one of inline bytes, a URL, a provider file ID, a
// provider-managed URI, or an application reference. It never fetches external
// data. Inline bytes may
// omit MIMEType: every model and media preparation path that sends them
// detects it with DetectMIMEType on its own copy.
type Source struct {
	// Data holds inline bytes; it excludes URL, ID, and URI.
	Data []byte `json:"data,omitempty"`

	// URL references remote content without fetching it; it excludes Data and
	// ID.
	URL string `json:"url,omitempty"`

	// ID references a provider-managed file; adapters decide how to encode it.
	ID string `json:"id,omitempty"`

	// URI names provider-managed media such as a Gemini Files API object.
	// Unlike URL, it requires MIME type and origin; Dashi never fetches it.
	URI string `json:"uri,omitempty"`

	// Origin binds ID or URI to its provider, API, and endpoint/credential
	// scope. Model is optional. Data and URL must not set Origin.
	Origin Origin `json:"origin,omitzero"`

	// MIMEType describes the content. It is required with URI. For inline
	// bytes it is optional: when empty, model and media preparation detect it
	// from the bytes and, for a document, its title's file extension.
	MIMEType string `json:"mime_type,omitempty"`

	// ExpiresAt reports a known expiration of a remote reference. Inline bytes
	// do not expire; this does not promise remote acceptance until that instant.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	// Ref names bytes that the application stores. It requires MIMEType and
	// excludes every other location, Origin, and ExpiresAt. A recorded turn
	// keeps only the reference; the runner asks the application's
	// ContentLoader for the bytes before each model request.
	Ref *Ref `json:"ref,omitempty"`
}

// Ref names stored bytes by application ID and proves which bytes it names
// with a digest and a size. It is not a URL or a path: the ID carries no
// authority, and only the application's resolver can turn it into bytes. The
// digest and size let Dashi verify what the resolver returns, so a changed
// file cannot reach the model unnoticed.
type Ref struct {
	// ID is an opaque application identifier.
	ID string `json:"id"`

	// SHA256 is the lowercase hex digest of the bytes.
	SHA256 string `json:"sha256"`

	// Size is the byte count of the bytes. It must be positive, like inline
	// data.
	Size int64 `json:"size"`
}

// Validate reports whether r has an ID, a well-formed digest, and a positive
// size.
func (r Ref) Validate() error {
	if r.ID == "" || !utf8.ValidString(r.ID) {
		return errors.New("content: ref requires a valid UTF-8 ID")
	}
	if len(r.SHA256) != 64 || strings.ToLower(r.SHA256) != r.SHA256 {
		return errors.New("content: ref requires a lowercase hex SHA-256")
	}
	if _, err := hex.DecodeString(r.SHA256); err != nil {
		return errors.New("content: ref requires a lowercase hex SHA-256")
	}
	if r.Size <= 0 {
		return errors.New("content: ref requires a positive size")
	}
	return nil
}

// Validate reports whether s sets exactly one location, with non-empty inline
// data and a MIME type for a provider URI or a reference.
func (s Source) Validate() error {
	if count(
		s.Data != nil,
		s.URL != "",
		s.ID != "",
		s.URI != "",
		s.Ref != nil,
	) != 1 {
		return errors.New(
			"content: source requires one of data, URL, ID, URI, or ref",
		)
	}
	if s.Ref != nil {
		if s.MIMEType == "" {
			return errors.New("content: ref requires MIME type")
		}
		if s.Origin != (Origin{}) || s.ExpiresAt != nil {
			return errors.New("content: ref excludes origin and expiry")
		}
		return s.Ref.Validate()
	}
	if s.Data != nil && len(s.Data) == 0 {
		return errors.New("content: inline source requires data")
	}
	if s.Data != nil && s.ExpiresAt != nil {
		return errors.New("content: inline source cannot expire")
	}
	if s.URI != "" && s.MIMEType == "" {
		return errors.New("content: provider URI requires MIME type")
	}
	if s.ID != "" || s.URI != "" {
		return s.Origin.Validate()
	}
	if s.Origin != (Origin{}) {
		return errors.New("content: source origin requires a provider ID or URI")
	}
	return nil
}

// Clone detaches inline bytes, expiry, and the reference while preserving the
// location and provenance fields.
func (s Source) Clone() Source {
	s.Data = slices.Clone(s.Data)
	s.ExpiresAt = cloneTime(s.ExpiresAt)
	if s.Ref != nil {
		ref := *s.Ref
		s.Ref = &ref
	}
	return s
}

// WithDetectedMIMEType returns s with MIMEType set by DetectMIMEType when s
// holds inline bytes without one, and s unchanged otherwise. name is an
// optional file name whose extension guides detection. The result is a copy,
// so the caller's source keeps its empty type.
func (s Source) WithDetectedMIMEType(name string) Source {
	if s.Data != nil && s.MIMEType == "" {
		s.MIMEType = DetectMIMEType(s.Data, name)
	}
	return s
}

// DataURL returns the inline data encoded as a base64 data URL. It uses
// MIMEType as given; preparation fills an empty one before encoding.
func (s Source) DataURL() string {
	return "data:" + s.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(s.Data)
}

// Citation preserves provider-specific citation data as well as display fields.
// Raw may only be replayed to Origin; a URL alone does not replace that data.
type Citation struct {
	// URL is the display link; it does not replace provider replay data.
	URL string `json:"url,omitempty"`

	// Title is an optional display label for the cited source.
	Title string `json:"title,omitempty"`

	// Text is an optional excerpt reported by the provider.
	Text string `json:"text,omitempty"`

	// Location says where in a document the cited passage is. It is nil for
	// web and URL citations and for citation kinds Dashi does not interpret,
	// whose details stay in Raw.
	Location *CitationLocation `json:"location,omitempty"`

	// Origin restricts where Raw may be replayed and is required with Raw.
	Origin Origin `json:"origin,omitzero"`

	// Raw preserves the provider citation object; use Block.Clone before
	// editing it.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// LocationUnit names what a CitationLocation's range counts.
type LocationUnit string

// Location units.
const (
	// LocationPage counts PDF pages from 1.
	LocationPage LocationUnit = "page"
	// LocationChar counts characters of a text document from 0.
	LocationChar LocationUnit = "char"
)

// CitationLocation points at a passage of a cited document. The range
// [Start, End) is half-open.
type CitationLocation struct {
	// RequestDocumentIndex counts the document blocks of the request that
	// produced the citation, from 0, in the order sent: through every message
	// in order, including the blocks of each tool result. It is not stable
	// across calls, because context and tool results change which documents a
	// request carries; give documents titles and use Citation.Title as a
	// stable key.
	RequestDocumentIndex int `json:"request_document_index"`

	// Unit says whether Start and End count pages or characters.
	Unit LocationUnit `json:"unit"`

	// Start is the first page or character of the passage.
	Start int `json:"start"`

	// End is one past the last page or character of the passage.
	End int `json:"end"`
}

// Validate requires a known unit and a well-formed range.
func (l CitationLocation) Validate() error {
	switch {
	case l.Unit != LocationPage && l.Unit != LocationChar:
		return fmt.Errorf("content: unknown citation location unit %q", l.Unit)
	case l.RequestDocumentIndex < 0 || l.Start < 0 || l.End < l.Start:
		return errors.New("content: invalid citation location range")
	case l.Unit == LocationPage && l.Start < 1:
		return errors.New("content: citation pages count from 1")
	}
	return nil
}

// Text keeps display text beside provider citations. Block.Clone detaches
// citation slices and their raw replay data.
type Text struct {
	// Text holds display text independently of citation replay data.
	Text string `json:"text"`

	// Citations retain source attribution in provider order; Block.Clone copies
	// them.
	Citations []Citation `json:"citations,omitempty"`
}

// Image describes an image source without fetching or decoding it.
type Image struct {
	// Source selects inline bytes, a URL, or a provider file ID.
	Source Source `json:"source"`

	// Detail requests provider-specific image processing; unsupported values
	// may fail preparation.
	Detail string `json:"detail,omitempty"`
}

// Document is a document payload, such as a PDF.
type Document struct {
	// Source selects inline bytes, a URL, or a provider file ID.
	Source Source `json:"source"`

	// Title is an optional display name passed to APIs that support it. A
	// file extension in it, such as "notes.md", also guides MIME detection
	// for inline bytes without a MIME type.
	Title string `json:"title,omitempty"`
}

// AudioFormat describes audio frames when MIME type alone is insufficient.
// A zero value means the format has not been specified.
type AudioFormat struct {
	Encoding     string `json:"encoding"`
	SampleRateHz int    `json:"sample_rate_hz"`
	Channels     int    `json:"channels"`
}

// Validate requires all format fields when frame format is specified.
func (f AudioFormat) Validate() error {
	if f.Encoding == "" || f.SampleRateHz <= 0 || f.Channels <= 0 {
		return errors.New("content: audio format requires encoding, sample rate, and channels")
	}
	return nil
}

// Audio describes audio data without fetching or decoding it.
type Audio struct {
	// Source selects audio data without fetching or decoding it.
	Source Source `json:"source"`

	// Format describes sample layout when known, including raw PCM.
	Format *AudioFormat `json:"format,omitempty"`
}

// Video describes video data without fetching or decoding it.
type Video struct {
	// Source selects inline bytes or a remote reference without resolving it.
	Source Source `json:"source"`

	// Duration is measured or provider-reported output length. Zero means it is
	// unknown; JSON encodes time.Duration as nanoseconds.
	Duration time.Duration `json:"duration,omitempty"`
}

// Resource is a reference to content identified by URI.
type Resource struct {
	// URI is required and identifies a resource without resolving it.
	URI string `json:"uri"`

	// MIMEType optionally describes the referenced resource.
	MIMEType string `json:"mime_type,omitempty"`

	// Title is an optional label for displaying the resource.
	Title string `json:"title,omitempty"`

	// Origin identifies a provider-private link; a public link omits it.
	Origin Origin `json:"origin,omitzero"`

	// ExpiresAt reports a known expiration without fetching the link.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Block is a tagged union. Exactly one payload must be set, matching Kind.
type Block struct {
	// Kind selects exactly one of the payload fields below.
	Kind Kind `json:"kind"`

	// Text is required only for KindText; every other payload must be nil.
	Text *Text `json:"text,omitempty"`

	// Image is required only for KindImage; every other payload must be nil.
	Image *Image `json:"image,omitempty"`

	// Document is required only for KindDocument; every other payload must be
	// nil.
	Document *Document `json:"document,omitempty"`

	// Audio is required only for KindAudio; every other payload must be nil.
	Audio *Audio `json:"audio,omitempty"`

	// Video is required only for KindVideo; every other payload must be nil.
	Video *Video `json:"video,omitempty"`

	// Resource is required only for KindResource; every other payload must be
	// nil.
	Resource *Resource `json:"resource,omitempty"`
}

// TextBlock constructs a text payload without citations or provider data.
func TextBlock(text string) Block {
	return Block{Kind: KindText, Text: &Text{Text: text}}
}

// ImageBlock retains source as supplied. Use Block.Clone to detach inline bytes.
func ImageBlock(source Source) Block {
	return Block{Kind: KindImage, Image: &Image{Source: source}}
}

// DocumentBlock retains source as supplied. Title may be empty.
func DocumentBlock(source Source, title string) Block {
	return Block{
		Kind:     KindDocument,
		Document: &Document{Source: source, Title: title},
	}
}

// AudioBlock retains source as supplied without decoding it.
func AudioBlock(source Source) Block {
	return Block{Kind: KindAudio, Audio: &Audio{Source: source}}
}

// VideoBlock retains source as supplied without fetching it.
func VideoBlock(source Source) Block {
	return Block{Kind: KindVideo, Video: &Video{Source: source}}
}

// Validate reports whether b carries exactly one payload that matches its Kind
// and whether that payload is well formed.
func (b Block) Validate() error {
	if count(
		b.Text != nil,
		b.Image != nil,
		b.Document != nil,
		b.Audio != nil,
		b.Video != nil,
		b.Resource != nil,
	) != 1 {
		return errors.New("content: block requires one payload")
	}
	switch {
	case b.Kind == KindText && b.Text != nil:
		for _, c := range b.Text.Citations {
			if c.Location != nil {
				if err := c.Location.Validate(); err != nil {
					return err
				}
			}
			if len(c.Raw) > 0 &&
				(c.Origin.Validate() != nil ||
					c.Origin.Model == "" ||
					!json.Valid(c.Raw)) {
				return errors.New("content: invalid citation provenance or JSON")
			}
		}
		return nil
	case b.Kind == KindImage && b.Image != nil:
		return b.Image.Source.Validate()
	case b.Kind == KindDocument && b.Document != nil:
		return b.Document.Source.Validate()
	case b.Kind == KindAudio && b.Audio != nil:
		if err := b.Audio.Source.Validate(); err != nil {
			return err
		}
		if b.Audio.Format != nil {
			return b.Audio.Format.Validate()
		}
		return nil
	case b.Kind == KindVideo && b.Video != nil:
		if err := b.Video.Source.Validate(); err != nil {
			return err
		}
		if b.Video.Duration < 0 {
			return errors.New("content: video duration cannot be negative")
		}
		return nil
	case b.Kind == KindResource &&
		b.Resource != nil &&
		b.Resource.URI != "":
		if b.Resource.Origin != (Origin{}) {
			return b.Resource.Origin.Validate()
		}
		return nil
	}
	return fmt.Errorf("content: invalid %q block", b.Kind)
}

// Clone returns an independent value, including bytes and citation payloads.
func (b Block) Clone() Block {
	if b.Text != nil {
		t := *b.Text
		t.Citations = slices.Clone(t.Citations)
		for i := range t.Citations {
			t.Citations[i].Raw = slices.Clone(t.Citations[i].Raw)
			if l := t.Citations[i].Location; l != nil {
				location := *l
				t.Citations[i].Location = &location
			}
		}
		b.Text = &t
	}
	if b.Image != nil {
		v := *b.Image
		v.Source = v.Source.Clone()
		b.Image = &v
	}
	if b.Document != nil {
		v := *b.Document
		v.Source = v.Source.Clone()
		b.Document = &v
	}
	if b.Audio != nil {
		v := *b.Audio
		v.Source = v.Source.Clone()
		if v.Format != nil {
			format := *v.Format
			v.Format = &format
		}
		b.Audio = &v
	}
	if b.Video != nil {
		v := *b.Video
		v.Source = v.Source.Clone()
		b.Video = &v
	}
	if b.Resource != nil {
		v := *b.Resource
		v.ExpiresAt = cloneTime(v.ExpiresAt)
		b.Resource = &v
	}
	return b
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

func count(conds ...bool) int {
	n := 0
	for _, c := range conds {
		if c {
			n++
		}
	}
	return n
}
