// Package media defines values for image, video, and non-speech audio
// generation. Its values do not assign a fixed call or job lifecycle to any
// medium. Provider clients and wire plans are qualified separately; this
// package does not select providers or store artifacts.
package media

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/model"
)

// Kind identifies the output medium, independently of execution shape.
type Kind string

const (
	// KindImage marks a generated image result or job.
	KindImage Kind = "image"
	// KindVideo marks a generated video result or job.
	KindVideo Kind = "video"
	// KindAudio marks generated non-speech audio.
	KindAudio Kind = "audio"
)

// ImageOperation distinguishes creation from an edit with reference images.
type ImageOperation string

const (
	// ImageGenerate creates new images from a prompt.
	ImageGenerate ImageOperation = "generate"
	// ImageEdit modifies one or more reference images.
	ImageEdit ImageOperation = "edit"
)

// Size requests exact pixel dimensions when both fields are set. A zero value
// leaves dimensions unspecified; provider preparation checks known limits.
type Size struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Validate rejects partial or negative dimensions.
func (s Size) Validate() error {
	if (s.Width == 0 && s.Height == 0) || (s.Width > 0 && s.Height > 0) {
		return nil
	}
	return errors.New("media: size requires positive width and height together")
}

// ImageSettings contains controls meaningful to image generation or editing.
// Zero values leave controls to provider preparation.
type ImageSettings struct {
	Size           Size   `json:"size,omitzero"`
	Resolution     string `json:"resolution,omitempty"`
	AspectRatio    string `json:"aspect_ratio,omitempty"`
	Count          int    `json:"count,omitempty"`
	Quality        string `json:"quality,omitempty"`
	OutputMIMEType string `json:"output_mime_type,omitempty"`
}

// Validate checks provider-independent setting invariants.
func (s ImageSettings) Validate() error {
	if err := s.Size.Validate(); err != nil {
		return err
	}
	if s.Size.Width > 0 && s.AspectRatio != "" {
		return errors.New("media: choose image size or aspect ratio")
	}
	if s.Size.Width > 0 && s.Resolution != "" {
		return errors.New("media: choose image size or resolution")
	}
	if s.Count < 0 {
		return errors.New("media: image count cannot be negative")
	}
	return nil
}

// ImageInput keeps reference images and a mask distinct from the prompt, so
// editing intent does not have to be inferred from an arbitrary content block.
type ImageInput struct {
	Model           string          `json:"model"`
	Operation       ImageOperation  `json:"operation"`
	Prompt          []content.Block `json:"prompt"`
	ReferenceImages []content.Block `json:"reference_images,omitempty"`
	Mask            *content.Block  `json:"mask,omitempty"`
	Settings        ImageSettings   `json:"settings,omitzero"`
}

// Validate checks shape and content only. A client must also check the model's
// evidence and its own wire capabilities during preparation.
func (in ImageInput) Validate() error {
	if in.Model == "" {
		return errors.New("media: image input requires a model")
	}
	if in.Operation != ImageGenerate && in.Operation != ImageEdit {
		return errors.New("media: image operation must be generate or edit")
	}
	if in.Operation == ImageGenerate && len(in.Prompt) == 0 {
		return errors.New("media: image generation requires a prompt")
	}
	if in.Operation == ImageEdit && len(in.ReferenceImages) == 0 {
		return errors.New("media: image edit requires a reference image")
	}
	if in.Mask != nil && in.Operation != ImageEdit {
		return errors.New("media: image mask requires edit operation")
	}
	if err := validateBlocks(in.Prompt); err != nil {
		return err
	}
	for _, block := range in.ReferenceImages {
		if block.Kind != content.KindImage || block.Validate() != nil {
			return errors.New("media: reference images require valid image blocks")
		}
	}
	if in.Mask != nil {
		if in.Mask.Kind != content.KindImage || in.Mask.Validate() != nil {
			return errors.New("media: image mask requires a valid image block")
		}
	}
	return in.Settings.Validate()
}

// Clone detaches prompt and image bytes before a caller changes the input.
func (in ImageInput) Clone() ImageInput {
	in.Prompt = cloneBlocks(in.Prompt)
	in.ReferenceImages = cloneBlocks(in.ReferenceImages)
	if in.Mask != nil {
		mask := in.Mask.Clone()
		in.Mask = &mask
	}
	return in
}

// VideoSettings contains controls for generated video. Duration is requested
// duration, not a claim about the length the provider will return. JSON
// encodes time.Duration as nanoseconds.
type VideoSettings struct {
	Duration    time.Duration `json:"duration,omitempty"`
	Size        Size          `json:"size,omitzero"`
	Resolution  string        `json:"resolution,omitempty"`
	AspectRatio string        `json:"aspect_ratio,omitempty"`
}

// Validate checks provider-independent setting invariants.
func (s VideoSettings) Validate() error {
	if s.Duration < 0 {
		return errors.New("media: video duration cannot be negative")
	}
	if err := s.Size.Validate(); err != nil {
		return err
	}
	if s.Size.Width > 0 && s.AspectRatio != "" {
		return errors.New("media: choose video size or aspect ratio")
	}
	if s.Size.Width > 0 && s.Resolution != "" {
		return errors.New("media: choose video size or resolution")
	}
	return nil
}

// VideoInput holds prompt and reference frames without assuming that every
// video provider accepts both.
type VideoInput struct {
	Model           string          `json:"model"`
	Prompt          []content.Block `json:"prompt"`
	ReferenceFrames []content.Block `json:"reference_frames,omitempty"`
	Settings        VideoSettings   `json:"settings,omitzero"`
}

// Validate checks shape and content before provider preparation.
func (in VideoInput) Validate() error {
	if in.Model == "" || (len(in.Prompt) == 0 && len(in.ReferenceFrames) == 0) {
		return errors.New("media: video input requires model and prompt or reference frame")
	}
	if err := validateBlocks(in.Prompt); err != nil {
		return err
	}
	for _, block := range in.ReferenceFrames {
		if block.Kind != content.KindImage || block.Validate() != nil {
			return errors.New("media: reference frames require valid image blocks")
		}
	}
	return in.Settings.Validate()
}

// Clone detaches prompt and reference bytes.
func (in VideoInput) Clone() VideoInput {
	in.Prompt = cloneBlocks(in.Prompt)
	in.ReferenceFrames = cloneBlocks(in.ReferenceFrames)
	return in
}

// AudioSettings contains controls for non-speech audio generation. Format
// describes sample layout when requested; OutputMIMEType names a container or
// media type. JSON encodes Duration as nanoseconds.
type AudioSettings struct {
	Duration       time.Duration        `json:"duration,omitempty"`
	OutputMIMEType string               `json:"output_mime_type,omitempty"`
	Format         *content.AudioFormat `json:"format,omitempty"`
}

// Validate checks the requested duration and any explicit frame format.
func (s AudioSettings) Validate() error {
	if s.Duration < 0 {
		return errors.New("media: audio duration cannot be negative")
	}
	if s.Format != nil {
		return s.Format.Validate()
	}
	return nil
}

// AudioInput describes generated non-speech audio such as music or sound.
type AudioInput struct {
	Model    string          `json:"model"`
	Prompt   []content.Block `json:"prompt"`
	Settings AudioSettings   `json:"settings,omitzero"`
}

// Validate checks shape and content before provider preparation.
func (in AudioInput) Validate() error {
	if in.Model == "" || len(in.Prompt) == 0 {
		return errors.New("media: audio input requires model and prompt")
	}
	if err := validateBlocks(in.Prompt); err != nil {
		return err
	}
	return in.Settings.Validate()
}

// Clone detaches prompt bytes and the optional format.
func (in AudioInput) Clone() AudioInput {
	in.Prompt = cloneBlocks(in.Prompt)
	if in.Settings.Format != nil {
		format := *in.Settings.Format
		in.Settings.Format = &format
	}
	return in
}

// Result holds generated content and reported identity. Kind names the primary
// output medium; ancillary content may use another kind. The host chooses
// durable artifact storage and any smaller model-facing representation.
type Result struct {
	Kind       Kind            `json:"kind"`
	Content    []content.Block `json:"content"`
	Requested  model.ModelRef  `json:"requested"`
	Served     model.ModelRef  `json:"served"`
	API        string          `json:"api"`
	ResponseID string          `json:"response_id,omitempty"`
	// RawUsage preserves provider-reported usage without claiming shared billing
	// units across image and video APIs. Nil means the provider did not report it.
	RawUsage json.RawMessage `json:"raw_usage,omitempty"`
}

// Validate requires at least one block of the declared medium and requested
// provenance; ancillary content remains a valid, typed block.
func (r Result) Validate() error {
	if !knownKind(r.Kind) || r.Requested.Provider == "" ||
		r.Requested.Model == "" || r.API == "" || len(r.Content) == 0 {
		return errors.New("media: result requires kind, content, and request origin")
	}
	if r.Served != (model.ModelRef{}) &&
		(r.Served.Provider != r.Requested.Provider || r.Served.Model == "") {
		return errors.New("media: served model must identify the configured provider")
	}
	if err := validateRawUsage(r.RawUsage); err != nil {
		return err
	}
	var hasPrimary bool
	for _, block := range r.Content {
		if err := block.Validate(); err != nil {
			return err
		}
		hasPrimary = hasPrimary || block.Kind == content.Kind(r.Kind)
	}
	if !hasPrimary {
		return fmt.Errorf("media: result requires a primary %s block", r.Kind)
	}
	return nil
}

// Clone detaches result content and its nested bytes.
func (r Result) Clone() Result {
	r.Content = cloneBlocks(r.Content)
	r.RawUsage = slices.Clone(r.RawUsage)
	return r
}

func validateRawUsage(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	if !json.Valid(raw) || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte{'{'}) {
		return errors.New("media: raw usage requires a JSON object")
	}
	return nil
}

func knownKind(kind Kind) bool {
	return kind == KindImage || kind == KindVideo || kind == KindAudio
}

func validateBlocks(blocks []content.Block) error {
	for _, block := range blocks {
		if err := block.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func cloneBlocks(blocks []content.Block) []content.Block {
	copy := slices.Clone(blocks)
	for i := range copy {
		copy[i] = copy[i].Clone()
	}
	return copy
}
