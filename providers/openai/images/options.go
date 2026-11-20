package images

import (
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

// Options holds controls specific to OpenAI's Images API. An unset field
// leaves the provider default in effect. OutputCompression uses a pointer so
// zero can be requested explicitly.
type Options struct {
	Background        string `json:"background,omitempty"`
	Moderation        string `json:"moderation,omitempty"`
	InputFidelity     string `json:"input_fidelity,omitempty"`
	OutputCompression *int   `json:"output_compression,omitempty"`
	// Complexity is "low", "medium", or "high" and sets how much detail
	// GPT Image 2.6 plans into the image. OpenAI defaults to high.
	Complexity string `json:"complexity,omitempty"`
	// FrameCount is the number of GIF frames, 2 to 16 (OpenAI default 9).
	// It requires GIF output.
	FrameCount *int `json:"frame_count,omitempty"`
	// FrameDurationMS is how long each GIF frame shows, at least 20 ms
	// (OpenAI default 200). It requires GIF output.
	FrameDurationMS *int `json:"frame_duration_ms,omitempty"`
	// SVG chooses the model that writes SVG output and its reasoning effort.
	// SVG output requires it, and it requires SVG output.
	SVG *SVGOptions `json:"svg,omitempty"`
	// Markup is a transparent PNG drawn over the first reference image to
	// show where and how to edit it. It must match that image's size, is
	// for edits only, and cannot be combined with a mask.
	Markup *content.Block `json:"markup,omitempty"`
}

// SVGOptions selects how OpenAI authors SVG output. The image model in the
// request still renders the reference image the SVG is drawn from.
type SVGOptions struct {
	// Model names the model that writes the SVG, such as gpt-5.5.
	Model string `json:"model"`
	// ReasoningEffort is "none", "minimal", "low", "medium", "high",
	// "xhigh", or "max". Empty uses the author model's default.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

func (o Options) clone() Options {
	if o.OutputCompression != nil {
		value := *o.OutputCompression
		o.OutputCompression = &value
	}
	if o.FrameCount != nil {
		value := *o.FrameCount
		o.FrameCount = &value
	}
	if o.FrameDurationMS != nil {
		value := *o.FrameDurationMS
		o.FrameDurationMS = &value
	}
	if o.SVG != nil {
		value := *o.SVG
		o.SVG = &value
	}
	if o.Markup != nil {
		value := o.Markup.Clone()
		o.Markup = &value
	}
	return o
}

func (o Options) fields(input media.ImageInput) (map[string]any, error) {
	fields := make(map[string]any)
	switch o.Background {
	case "":
	case "auto", "opaque", "transparent":
		if o.Background == "transparent" &&
			input.Settings.OutputMIMEType != "" &&
			input.Settings.OutputMIMEType != "image/png" &&
			input.Settings.OutputMIMEType != "image/webp" {
			return nil, errors.New("images: transparent background requires PNG or WebP output")
		}
		fields["background"] = o.Background
	default:
		return nil, fmt.Errorf("images: unsupported background %q", o.Background)
	}
	switch o.Moderation {
	case "":
	case "auto", "low":
		fields["moderation"] = o.Moderation
	default:
		return nil, fmt.Errorf("images: unsupported moderation %q", o.Moderation)
	}
	switch o.InputFidelity {
	case "":
	case "high", "low":
		if input.Operation != media.ImageEdit {
			return nil, errors.New("images: input fidelity requires an image edit")
		}
		switch gptImageModels[input.Model].fidelity {
		case "fixed":
			return nil, errors.New("images: GPT Image 2 input fidelity is fixed at high")
		case "low":
			if o.InputFidelity != "low" {
				return nil, fmt.Errorf(
					"images: %s accepts only low input fidelity",
					input.Model,
				)
			}
		}
		fields["input_fidelity"] = o.InputFidelity
	default:
		return nil, fmt.Errorf(
			"images: unsupported input fidelity %q",
			o.InputFidelity,
		)
	}
	switch o.Complexity {
	case "":
	case "low", "medium", "high":
		if known, ok := gptImageModels[input.Model]; ok && !known.complexity {
			return nil, errors.New("images: complexity requires GPT Image 2.6")
		}
		fields["complexity"] = o.Complexity
	default:
		return nil, fmt.Errorf("images: unsupported complexity %q", o.Complexity)
	}
	if o.OutputCompression != nil {
		if *o.OutputCompression < 0 || *o.OutputCompression > 100 {
			return nil, errors.New("images: output compression must be between 0 and 100")
		}
		if input.Settings.OutputMIMEType != "image/jpeg" &&
			input.Settings.OutputMIMEType != "image/webp" {
			return nil, errors.New("images: output compression requires JPEG or WebP output")
		}
		fields["output_compression"] = *o.OutputCompression
	}
	if err := o.gifFields(input, fields); err != nil {
		return nil, err
	}
	if err := o.svgFields(input, fields); err != nil {
		return nil, err
	}
	if err := o.markupFields(input, fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func (o Options) gifFields(input media.ImageInput, fields map[string]any) error {
	gif := input.Settings.OutputMIMEType == "image/gif"
	if o.FrameCount != nil {
		if !gif {
			return errors.New("images: frame count requires GIF output")
		}
		if *o.FrameCount < 2 || *o.FrameCount > 16 {
			return errors.New("images: frame count must be between 2 and 16")
		}
		fields["frame_count"] = *o.FrameCount
	}
	if o.FrameDurationMS != nil {
		if !gif {
			return errors.New("images: frame duration requires GIF output")
		}
		if *o.FrameDurationMS < 20 {
			return errors.New("images: frame duration must be at least 20 ms")
		}
		fields["frame_duration_ms"] = *o.FrameDurationMS
	}
	return nil
}

func (o Options) svgFields(input media.ImageInput, fields map[string]any) error {
	svgOutput := input.Settings.OutputMIMEType == svgMIMEType
	if o.SVG == nil {
		if svgOutput {
			// The API answers missing_required_parameter for svg.model.
			return errors.New("images: SVG output requires SVG options with an author model")
		}
		return nil
	}
	if !svgOutput {
		return errors.New("images: SVG options require SVG output")
	}
	if o.SVG.Model == "" {
		return errors.New("images: SVG options require an author model")
	}
	svg := map[string]any{"model": o.SVG.Model}
	switch o.SVG.ReasoningEffort {
	case "":
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		svg["reasoning"] = map[string]any{"effort": o.SVG.ReasoningEffort}
	default:
		return fmt.Errorf(
			"images: unsupported SVG reasoning effort %q",
			o.SVG.ReasoningEffort,
		)
	}
	fields["svg"] = svg
	return nil
}

func (o Options) markupFields(input media.ImageInput, fields map[string]any) error {
	if o.Markup == nil {
		return nil
	}
	if input.Operation != media.ImageEdit {
		return errors.New("images: markup requires an image edit")
	}
	if input.Mask != nil {
		return errors.New("images: markup cannot be combined with a mask")
	}
	if known, ok := gptImageModels[input.Model]; ok && !known.markup {
		return errors.New("images: markup requires GPT Image 2.5 or 2.6")
	}
	if o.Markup.Kind != content.KindImage || o.Markup.Image == nil {
		return errors.New("images: markup must be an image block")
	}
	if o.Markup.Image.Detail != "" {
		return errors.New("images: markup does not accept image detail")
	}
	source := o.Markup.Image.Source.WithDetectedMIMEType("")
	if source.MIMEType != "" && source.MIMEType != "image/png" {
		return errors.New("images: markup must be a transparent PNG")
	}
	ref, err := jsonReference(*o.Markup)
	if err != nil {
		return err
	}
	fields["markup"] = ref
	return nil
}
