package media

import (
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/model"
)

// ImageSizeBounds describes a continuous set of valid exact image sizes.
// Evidence must establish that omitted bounds are unrestricted. Multiple
// applies to both edges; MaxAspectRatio is the larger edge divided by the
// smaller edge.
type ImageSizeBounds struct {
	MinPixels      uint64 `json:"min_pixels,omitempty"`
	MaxPixels      uint64 `json:"max_pixels,omitempty"`
	MaxEdge        int    `json:"max_edge,omitempty"`
	Multiple       int    `json:"multiple,omitempty"`
	MaxAspectRatio int    `json:"max_aspect_ratio,omitempty"`
}

// Validate checks the bounds' internal shape.
func (b ImageSizeBounds) Validate() error {
	if b.MaxEdge < 0 || b.Multiple < 0 || b.MaxAspectRatio < 0 ||
		(b.MaxPixels > 0 && b.MinPixels > b.MaxPixels) ||
		(b.MinPixels == 0 && b.MaxPixels == 0 && b.MaxEdge == 0 &&
			b.Multiple == 0 && b.MaxAspectRatio == 0) {
		return errors.New("media: invalid image size bounds")
	}
	return nil
}

// Check reports whether an exact size meets every known bound.
func (b ImageSizeBounds) Check(size Size) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if size.Width <= 0 || size.Height <= 0 {
		return errors.New("media: image size requires two positive edges")
	}
	if b.MaxEdge > 0 && (size.Width > b.MaxEdge || size.Height > b.MaxEdge) {
		return errors.New("media: image size exceeds maximum edge")
	}
	if b.Multiple > 0 && (size.Width%b.Multiple != 0 || size.Height%b.Multiple != 0) {
		return errors.New("media: image size edges require a supported multiple")
	}
	hi, pixels := bits.Mul64(uint64(size.Width), uint64(size.Height))
	if hi != 0 || (b.MaxPixels > 0 && pixels > b.MaxPixels) {
		return errors.New("media: image size exceeds maximum pixels")
	}
	if b.MinPixels > 0 && pixels < b.MinPixels {
		return errors.New("media: image size is below minimum pixels")
	}
	if b.MaxAspectRatio > 0 {
		larger, smaller := max(size.Width, size.Height), min(size.Width, size.Height)
		if larger/smaller > b.MaxAspectRatio ||
			(larger/smaller == b.MaxAspectRatio && larger%smaller != 0) {
			return errors.New("media: image aspect ratio exceeds maximum")
		}
	}
	return nil
}

// ImageCapabilities states known operation support and known output limits.
// Empty constraint lists mean unknown, not unrestricted.
type ImageCapabilities struct {
	Generate        model.Support    `json:"generate,omitempty"`
	Edit            model.Support    `json:"edit,omitempty"`
	Mask            model.Support    `json:"mask,omitempty"`
	Sizes           []Size           `json:"sizes,omitempty"`
	SizeBounds      *ImageSizeBounds `json:"size_bounds,omitempty"`
	Resolutions     []string         `json:"resolutions,omitempty"`
	AspectRatios    []string         `json:"aspect_ratios,omitempty"`
	Qualities       []string         `json:"qualities,omitempty"`
	OutputMIMETypes []string         `json:"output_mime_types,omitempty"`
	MaxCount        int              `json:"max_count,omitempty"`
}

// Clone detaches constraint lists.
func (c ImageCapabilities) Clone() ImageCapabilities {
	c.Sizes = slices.Clone(c.Sizes)
	if c.SizeBounds != nil {
		bounds := *c.SizeBounds
		c.SizeBounds = &bounds
	}
	c.Resolutions = slices.Clone(c.Resolutions)
	c.AspectRatios = slices.Clone(c.AspectRatios)
	c.Qualities = slices.Clone(c.Qualities)
	c.OutputMIMETypes = slices.Clone(c.OutputMIMETypes)
	return c
}

// VideoCapabilities states known video operation and reference-frame support.
// A provider may still reject a request for content policy reasons.
type VideoCapabilities struct {
	Generate        model.Support   `json:"generate,omitempty"`
	Edit            model.Support   `json:"edit,omitempty"`
	Extend          model.Support   `json:"extend,omitempty"`
	ReferenceFrames model.Support   `json:"reference_frames,omitempty"`
	Sizes           []Size          `json:"sizes,omitempty"`
	Resolutions     []string        `json:"resolutions,omitempty"`
	Durations       []time.Duration `json:"durations,omitempty"`
	MinDuration     time.Duration   `json:"min_duration,omitempty"`
	MaxDuration     time.Duration   `json:"max_duration,omitempty"`
	AspectRatios    []string        `json:"aspect_ratios,omitempty"`
	OutputMIMETypes []string        `json:"output_mime_types,omitempty"`
}

// Clone detaches constraint lists.
func (c VideoCapabilities) Clone() VideoCapabilities {
	c.Sizes = slices.Clone(c.Sizes)
	c.Resolutions = slices.Clone(c.Resolutions)
	c.Durations = slices.Clone(c.Durations)
	c.AspectRatios = slices.Clone(c.AspectRatios)
	c.OutputMIMETypes = slices.Clone(c.OutputMIMETypes)
	return c
}

// AudioCapabilities describes non-speech audio generation, including known
// output formats and maximum duration.
type AudioCapabilities struct {
	Generate        model.Support         `json:"generate,omitempty"`
	MaxDuration     time.Duration         `json:"max_duration,omitempty"`
	OutputMIMETypes []string              `json:"output_mime_types,omitempty"`
	FrameFormats    []content.AudioFormat `json:"frame_formats,omitempty"`
}

// Clone detaches constraint lists.
func (c AudioCapabilities) Clone() AudioCapabilities {
	c.OutputMIMETypes = slices.Clone(c.OutputMIMETypes)
	c.FrameFormats = slices.Clone(c.FrameFormats)
	return c
}

// ModelInfo scopes media capability claims to one model and endpoint. Pricing
// and usage remain operation-specific until a shared billing contract exists.
type ModelInfo struct {
	Image    *ImageCapabilities  `json:"image,omitempty"`
	Video    *VideoCapabilities  `json:"video,omitempty"`
	Audio    *AudioCapabilities  `json:"audio,omitempty"`
	Evidence model.ModelEvidence `json:"evidence"`
}

// Validate checks the evidence and internal shape, not the truth of claims.
func (m ModelInfo) Validate() error {
	if err := m.Evidence.Validate(); err != nil {
		return err
	}
	if m.Image == nil && m.Video == nil && m.Audio == nil {
		return errors.New("media: model info requires a capability area")
	}
	if c := m.Image; c != nil {
		if !c.Generate.Valid() || !c.Edit.Valid() ||
			!c.Mask.Valid() || c.MaxCount < 0 {
			return errors.New("media: invalid image capabilities")
		}
		for _, size := range c.Sizes {
			if size.Width <= 0 || size.Validate() != nil {
				return errors.New("media: invalid supported image size")
			}
		}
		if c.SizeBounds != nil {
			if len(c.Sizes) != 0 {
				return errors.New("media: image sizes and size bounds are mutually exclusive")
			}
			if err := c.SizeBounds.Validate(); err != nil {
				return err
			}
		}
	}
	if c := m.Video; c != nil {
		if !c.Generate.Valid() || !c.Edit.Valid() || !c.Extend.Valid() ||
			!c.ReferenceFrames.Valid() {
			return errors.New("media: invalid video capabilities")
		}
		for _, size := range c.Sizes {
			if size.Width <= 0 || size.Validate() != nil {
				return errors.New("media: invalid supported video size")
			}
		}
		for _, duration := range c.Durations {
			if duration <= 0 {
				return errors.New("media: supported video duration must be positive")
			}
		}
		if c.MinDuration < 0 || c.MaxDuration < 0 ||
			(c.MaxDuration > 0 && c.MinDuration > c.MaxDuration) {
			return errors.New("media: invalid video duration limits")
		}
	}
	if c := m.Audio; c != nil {
		if !c.Generate.Valid() || c.MaxDuration < 0 {
			return errors.New("media: invalid audio capabilities")
		}
		for _, format := range c.FrameFormats {
			if err := format.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Check validates an image request against known claims. Under the default
// adjust policy it returns the explicit features whose support remains
// unverified; the caller records them in the prepared request. It never
// overrides known rejection.
func (c ImageCapabilities) Check(
	input ImageInput,
	policy model.CapabilityPolicy,
) ([]string, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	check, err := newCapabilityCheck(policy)
	if err != nil {
		return nil, err
	}
	switch input.Operation {
	case ImageGenerate:
		if err := check.support("image.generate", c.Generate); err != nil {
			return nil, err
		}
	case ImageEdit:
		if err := check.support("image.edit", c.Edit); err != nil {
			return nil, err
		}
	}
	if input.Mask != nil {
		if err := check.support("image.mask", c.Mask); err != nil {
			return nil, err
		}
	}
	s := input.Settings
	for _, requested := range []struct {
		name   string
		value  string
		values []string
	}{
		{"image.resolution", s.Resolution, c.Resolutions},
		{"image.aspect_ratio", s.AspectRatio, c.AspectRatios},
		{"image.quality", s.Quality, c.Qualities},
		{"image.output_mime_type", s.OutputMIMEType, c.OutputMIMETypes},
	} {
		if requested.value != "" {
			if err := check.string(requested.name, requested.value, requested.values); err != nil {
				return nil, err
			}
		}
	}
	if s.Size.Width > 0 {
		if c.SizeBounds != nil {
			if err := c.SizeBounds.Check(s.Size); err != nil {
				return nil, err
			}
		} else if len(c.Sizes) == 0 {
			if err := check.unknown("image.size"); err != nil {
				return nil, err
			}
		} else if !slices.Contains(c.Sizes, s.Size) {
			return nil, errors.New("media: requested image size is unsupported")
		}
	}
	if s.Count > 0 {
		switch {
		case c.MaxCount == 0:
			if err := check.unknown("image.count"); err != nil {
				return nil, err
			}
		case s.Count > c.MaxCount:
			return nil, errors.New("media: image count exceeds model limit")
		}
	}
	return check.unverified, nil
}

// Check validates a video request against known claims and returns features
// whose support is unknown, which only the adjust policy allows.
func (c VideoCapabilities) Check(
	input VideoInput,
	policy model.CapabilityPolicy,
) ([]string, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	check, err := newCapabilityCheck(policy)
	if err != nil {
		return nil, err
	}
	switch input.Operation {
	case "", VideoGenerate:
		if err := check.support("video.generate", c.Generate); err != nil {
			return nil, err
		}
	case VideoEdit:
		if err := check.support("video.edit", c.Edit); err != nil {
			return nil, err
		}
	case VideoExtend:
		if err := check.support("video.extend", c.Extend); err != nil {
			return nil, err
		}
	}
	if len(input.ReferenceFrames) > 0 {
		if err := check.support("video.reference_frames", c.ReferenceFrames); err != nil {
			return nil, err
		}
	}
	s := input.Settings
	for _, requested := range []struct {
		name   string
		value  string
		values []string
	}{
		{"video.resolution", s.Resolution, c.Resolutions},
		{"video.aspect_ratio", s.AspectRatio, c.AspectRatios},
	} {
		if requested.value != "" {
			if err := check.string(requested.name, requested.value, requested.values); err != nil {
				return nil, err
			}
		}
	}
	if s.Size.Width > 0 {
		if len(c.Sizes) == 0 {
			if err := check.unknown("video.size"); err != nil {
				return nil, err
			}
		} else if !slices.Contains(c.Sizes, s.Size) {
			return nil, errors.New("media: requested video size is unsupported")
		}
	}
	if s.Duration > 0 {
		switch {
		case len(c.Durations) > 0 && !slices.Contains(c.Durations, s.Duration):
			return nil, errors.New("media: requested video duration is unsupported")
		case c.MinDuration > 0 && s.Duration < c.MinDuration:
			return nil, errors.New("media: requested video duration is too short")
		case c.MaxDuration > 0 && s.Duration > c.MaxDuration:
			return nil, errors.New("media: requested video duration is too long")
		case len(c.Durations) == 0 &&
			(c.MinDuration == 0 || c.MaxDuration == 0):
			if err := check.unknown("video.duration"); err != nil {
				return nil, err
			}
		}
	}
	return check.unverified, nil
}

type capabilityCheck struct {
	policy     model.CapabilityPolicy
	unverified []string
}

func newCapabilityCheck(policy model.CapabilityPolicy) (*capabilityCheck, error) {
	if !policy.Valid() {
		return nil, errors.New("media: invalid capability policy")
	}
	return &capabilityCheck{policy: policy}, nil
}

func (c *capabilityCheck) support(name string, support model.Support) error {
	switch support {
	case model.SupportSupported:
		return nil
	case model.SupportUnsupported:
		return fmt.Errorf("media: %s is unsupported", name)
	case model.SupportUnknown:
		return c.unknown(name)
	default:
		return fmt.Errorf("media: invalid support value for %s", name)
	}
}

func (c *capabilityCheck) string(name, value string, values []string) error {
	if len(values) == 0 {
		return c.unknown(name)
	}
	if !slices.Contains(values, value) {
		return fmt.Errorf("media: %s value %q is unsupported", name, value)
	}
	return nil
}

func (c *capabilityCheck) unknown(name string) error {
	if c.policy == model.CapabilityStrict {
		return fmt.Errorf("media: support for %s is unknown", name)
	}
	c.unverified = append(c.unverified, name)
	return nil
}

// Clone detaches all optional capability records and their constraint lists.
func (m ModelInfo) Clone() ModelInfo {
	if m.Image != nil {
		image := m.Image.Clone()
		m.Image = &image
	}
	if m.Video != nil {
		video := m.Video.Clone()
		m.Video = &video
	}
	if m.Audio != nil {
		audio := m.Audio.Clone()
		m.Audio = &audio
	}
	return m
}
