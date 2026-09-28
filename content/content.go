// Package content defines shared text, media, and references without model or
// tool execution dependencies. Source handling is adapted from Dive's
// llm/content.go.
package content

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
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

// Source contains exactly one of inline bytes, a URL, a provider file ID, or
// a provider-managed URI.
// It never fetches external data. MIMEType is required for inline bytes.
type Source struct {
	// Data holds inline bytes and requires MIMEType; it excludes URL and ID.
	Data []byte `json:"data,omitempty"`

	// URL references remote content without fetching it; it excludes Data and
	// ID.
	URL string `json:"url,omitempty"`

	// ID references a provider-managed file; adapters decide how to encode it.
	ID string `json:"id,omitempty"`

	// URI names provider-managed media such as a Gemini Files API object.
	// Unlike URL, it requires MIME type and origin; Kernel never fetches it.
	URI string `json:"uri,omitempty"`

	// Origin binds ID or URI to its provider, API, and endpoint/credential
	// scope. Model is optional. Data and URL must not set Origin.
	Origin Origin `json:"origin,omitzero"`

	// MIMEType describes inline bytes and is required when Data is set.
	MIMEType string `json:"mime_type,omitempty"`

	// ExpiresAt reports a known expiration of a remote reference. Inline bytes
	// do not expire; this does not promise remote acceptance until that instant.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Validate reports whether s sets exactly one location and, for inline data,
// a MIME type.
func (s Source) Validate() error {
	if count(s.Data != nil, s.URL != "", s.ID != "", s.URI != "") != 1 {
		return errors.New("content: source requires one of data, URL, ID, or URI")
	}
	if s.Data != nil && (len(s.Data) == 0 || s.MIMEType == "") {
		return errors.New("content: inline source requires data and MIME type")
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

// Clone detaches inline bytes and expiry while preserving the location and
// provenance fields.
func (s Source) Clone() Source {
	s.Data = slices.Clone(s.Data)
	s.ExpiresAt = cloneTime(s.ExpiresAt)
	return s
}

// DataURL returns the inline data encoded as a base64 data URL.
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

	// Origin restricts where Raw may be replayed and is required with Raw.
	Origin Origin `json:"origin,omitzero"`

	// Raw preserves the provider citation object; use Block.Clone before
	// editing it.
	Raw json.RawMessage `json:"raw,omitempty"`
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

	// Title is an optional display name passed to APIs that support it.
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

// DocumentBlock retains source as supplied without inventing a display title.
func DocumentBlock(source Source) Block {
	return Block{Kind: KindDocument, Document: &Document{Source: source}}
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
